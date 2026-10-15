package main

import (
	"context"
	"crypto/md5"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"
)

// subsonicPrefix marks a playlist entry as a track on the Subsonic server
// rather than a local file. The entry is "subsonic:<song id>". Credentials
// never appear in the entry, so it is safe to show in errors and to store as
// an earmark path.
const subsonicPrefix = "subsonic:"

// subsonicAPIVersion is the Subsonic REST API version derpy speaks. 1.13.0 is
// the first version with token authentication, which keeps the password off
// the wire.
const subsonicAPIVersion = "1.13.0"

// subsonicPageSize is the number of songs requested per search3 page.
const subsonicPageSize = 500

// decodableSuffixes are the formats AudioPlayer can decode natively. Songs in
// any other format are requested from the server transcoded to mp3.
var decodableSuffixes = map[string]bool{
	"mp3":  true,
	"flac": true,
	"ogg":  true,
	"wav":  true,
}

// subsonicSession is the server the current run plays from. It is set by the
// 'sonic' command and read by AudioPlayer.LoadTrack to resolve "subsonic:"
// playlist entries. It is nil for local playback.
var subsonicSession *SubsonicClient

// subsonicSong is the subset of a Subsonic song entry derpy uses.
type subsonicSong struct {
	ID     string `json:"id"`
	Title  string `json:"title"`
	Artist string `json:"artist"`
	Album  string `json:"album"`
	Suffix string `json:"suffix"`
	Path   string `json:"path"`
}

// matchText is the string keyword expressions are matched against. Servers
// that expose the file path get the same matching as local playback; others
// fall back to "artist/album/title".
func (s subsonicSong) matchText() string {
	if s.Path != "" {
		return s.Path
	}
	return s.Artist + "/" + s.Album + "/" + s.Title
}

// subsonicResponse is the JSON envelope every Subsonic endpoint returns.
type subsonicResponse struct {
	Response struct {
		Status string `json:"status"`
		Error  *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
		SearchResult3 struct {
			Song []subsonicSong `json:"song"`
		} `json:"searchResult3"`
		RandomSongs struct {
			Song []subsonicSong `json:"song"`
		} `json:"randomSongs"`
	} `json:"subsonic-response"`
}

// SubsonicClient talks to a Subsonic-compatible server (Navidrome, Gonic,
// Airsonic, ...) and downloads tracks to a session temp directory for
// playback.
type SubsonicClient struct {
	baseURL  string
	user     string
	password string
	http     *http.Client

	mu      sync.Mutex
	songs   map[string]subsonicSong // song id → metadata, filled by Songs
	tempDir string                  // created on first download
	next    *subsonicPrefetch       // background download of the next track, if any
}

// subsonicPrefetch is one background download. done is closed when path,
// song and err are set; they must not be read before then.
type subsonicPrefetch struct {
	id     string
	cancel context.CancelFunc
	done   chan struct{}
	path   string
	song   subsonicSong
	err    error
}

// discard cancels the download and deletes its file once it finishes.
func (p *subsonicPrefetch) discard() {
	p.cancel()
	go func() {
		<-p.done
		if p.path != "" {
			os.Remove(p.path)
		}
	}()
}

// NewSubsonicClient returns a client for the server at baseURL.
func NewSubsonicClient(baseURL, user, password string) *SubsonicClient {
	return &SubsonicClient{
		baseURL:  normalizeServerURL(baseURL),
		user:     user,
		password: password,
		http:     &http.Client{Timeout: 10 * time.Minute},
		songs:    make(map[string]subsonicSong),
	}
}

// normalizeServerURL trims whitespace and trailing slashes and defaults the
// scheme to https when the user typed a bare host.
func normalizeServerURL(raw string) string {
	s := strings.TrimRight(strings.TrimSpace(raw), "/")
	if s != "" && !strings.Contains(s, "://") {
		s = "https://" + s
	}
	return s
}

// endpoint builds a signed request URL for a Subsonic REST method. Each call
// uses a fresh random salt, as the token-auth spec requires.
func (c *SubsonicClient) endpoint(method string, params url.Values) string {
	salt := randomSalt()
	sum := md5.Sum([]byte(c.password + salt))

	q := url.Values{}
	maps.Copy(q, params)
	q.Set("u", c.user)
	q.Set("t", hex.EncodeToString(sum[:]))
	q.Set("s", salt)
	q.Set("v", subsonicAPIVersion)
	q.Set("c", "derpy")
	q.Set("f", "json")
	return c.baseURL + "/rest/" + method + "?" + q.Encode()
}

// randomSalt returns a 12-character hex salt for token authentication.
func randomSalt() string {
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// call performs a JSON API request and returns the decoded envelope, turning
// a "failed" status into an error.
func (c *SubsonicClient) call(method string, params url.Values) (*subsonicResponse, error) {
	resp, err := c.http.Get(c.endpoint(method, params))
	if err != nil {
		return nil, fmt.Errorf("%s: %w", method, redactURLError(err))
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s: server returned %s", method, resp.Status)
	}
	var out subsonicResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("%s: unexpected response (is this a Subsonic server?): %w", method, err)
	}
	if out.Response.Status != "ok" {
		if e := out.Response.Error; e != nil {
			return nil, fmt.Errorf("%s: %s (code %d)", method, e.Message, e.Code)
		}
		return nil, fmt.Errorf("%s: request failed", method)
	}
	return &out, nil
}

// redactURLError strips the request URL from a transport error. The URL
// carries the auth token and salt, which must not end up in error logs.
func redactURLError(err error) error {
	if ue, ok := err.(*url.Error); ok {
		return ue.Err
	}
	return err
}

// Ping checks that the server is reachable and the credentials are valid.
func (c *SubsonicClient) Ping() error {
	_, err := c.call("ping", nil)
	return err
}

// Songs returns every song on the server. It pages through search3 with an
// empty query, which Navidrome, Gonic and other OpenSubsonic servers treat as
// "match everything". Servers that return nothing for an empty query fall back
// to one batch of random songs.
func (c *SubsonicClient) Songs(progress func(n int)) ([]subsonicSong, error) {
	var all []subsonicSong
	for offset := 0; ; offset += subsonicPageSize {
		out, err := c.call("search3", url.Values{
			"query":       {""},
			"artistCount": {"0"},
			"albumCount":  {"0"},
			"songCount":   {fmt.Sprint(subsonicPageSize)},
			"songOffset":  {fmt.Sprint(offset)},
		})
		if err != nil {
			if offset == 0 {
				break // fall through to getRandomSongs
			}
			return nil, err
		}
		page := out.Response.SearchResult3.Song
		all = append(all, page...)
		if progress != nil {
			progress(len(all))
		}
		if len(page) < subsonicPageSize {
			break
		}
	}

	if len(all) == 0 {
		out, err := c.call("getRandomSongs", url.Values{"size": {fmt.Sprint(subsonicPageSize)}})
		if err != nil {
			return nil, err
		}
		all = out.Response.RandomSongs.Song
	}

	c.mu.Lock()
	for _, s := range all {
		c.songs[s.ID] = s
	}
	c.mu.Unlock()
	return all, nil
}

// Prefetch starts downloading a song in the background so a later Fetch of
// the same id returns at once. Only one prefetch is kept: starting one for a
// different song discards the previous download. Prefetching the song that is
// already in flight does nothing.
func (c *SubsonicClient) Prefetch(id string) {
	ctx, cancel := context.WithCancel(context.Background())
	p := &subsonicPrefetch{id: id, cancel: cancel, done: make(chan struct{})}

	c.mu.Lock()
	if c.next != nil && c.next.id == id {
		c.mu.Unlock()
		cancel()
		return
	}
	old := c.next
	c.next = p
	c.mu.Unlock()

	if old != nil {
		old.discard()
	}
	go func() {
		p.path, p.song, p.err = c.download(ctx, id)
		close(p.done)
	}()
}

// Fetch returns a local copy of a song and its metadata. A matching prefetch
// is used when there is one, waiting for it to finish if needed; otherwise,
// or if the prefetch failed, the song is downloaded now. Cancelling ctx
// abandons the wait or the download.
func (c *SubsonicClient) Fetch(ctx context.Context, id string) (string, subsonicSong, error) {
	c.mu.Lock()
	p := c.next
	if p != nil && p.id == id {
		c.next = nil // claimed: the caller now owns the file
	} else {
		p = nil
	}
	c.mu.Unlock()

	if p != nil {
		select {
		case <-p.done:
			if p.err == nil {
				return p.path, p.song, nil
			}
		case <-ctx.Done():
			p.discard()
			return "", subsonicSong{ID: id}, ctx.Err()
		}
	}
	return c.download(ctx, id)
}

// download fetches a song to the session temp directory and returns the local
// path with its metadata. Songs in a format derpy can't decode are transcoded
// to mp3 by the server.
func (c *SubsonicClient) download(ctx context.Context, id string) (string, subsonicSong, error) {
	c.mu.Lock()
	song, ok := c.songs[id]
	if !ok {
		song = subsonicSong{ID: id}
	}
	if c.tempDir == "" {
		dir, err := os.MkdirTemp("", "derpy-sonic-*")
		if err != nil {
			c.mu.Unlock()
			return "", song, fmt.Errorf("could not create temp dir: %w", err)
		}
		c.tempDir = dir
	}
	dir := c.tempDir
	c.mu.Unlock()

	suffix := strings.ToLower(song.Suffix)
	params := url.Values{"id": {id}}
	if decodableSuffixes[suffix] {
		params.Set("format", "raw")
	} else {
		params.Set("format", "mp3")
		suffix = "mp3"
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.endpoint("stream", params), nil)
	if err != nil {
		return "", song, fmt.Errorf("stream: %w", redactURLError(err))
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return "", song, fmt.Errorf("stream: %w", redactURLError(err))
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", song, fmt.Errorf("stream: server returned %s", resp.Status)
	}
	// Errors come back as a JSON or XML envelope with a 200 status.
	if ct := resp.Header.Get("Content-Type"); strings.Contains(ct, "json") || strings.Contains(ct, "xml") {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return "", song, fmt.Errorf("stream: server refused the track: %s", strings.TrimSpace(string(body)))
	}

	// A unique name per download, so a discarded prefetch cleaning up can
	// never delete a later copy of the same song.
	f, err := os.CreateTemp(dir, safeFileName(id)+"-*."+suffix)
	if err != nil {
		return "", song, fmt.Errorf("could not create temp file: %w", err)
	}
	local := f.Name()
	if _, err := io.Copy(f, resp.Body); err != nil {
		f.Close()
		os.Remove(local)
		return "", song, fmt.Errorf("download interrupted: %w", err)
	}
	if err := f.Close(); err != nil {
		os.Remove(local)
		return "", song, err
	}
	return local, song, nil
}

// safeFileName replaces characters that are not safe in a file name. Song ids
// are opaque server strings and may contain path separators.
func safeFileName(id string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			return r
		}
		return '_'
	}, id)
}

// Close stops any prefetch and deletes every downloaded track.
func (c *SubsonicClient) Close() {
	c.mu.Lock()
	p := c.next
	c.next = nil
	c.mu.Unlock()
	if p != nil {
		p.cancel()
		<-p.done
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	if c.tempDir != "" {
		os.RemoveAll(c.tempDir)
		c.tempDir = ""
	}
}

// prefetchEntry starts a background download of a playlist entry when it is
// a track on the connected Subsonic server. Local entries are ignored.
func prefetchEntry(entry string) {
	if subsonicSession == nil {
		return
	}
	if id, ok := subsonicEntryID(entry); ok {
		subsonicSession.Prefetch(id)
	}
}

// subsonicEntryID returns the song id of a "subsonic:" playlist entry.
func subsonicEntryID(entry string) (string, bool) {
	if !strings.HasPrefix(entry, subsonicPrefix) {
		return "", false
	}
	return strings.TrimPrefix(entry, subsonicPrefix), true
}
