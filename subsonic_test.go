package main

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// fakeSubsonic is an httptest Subsonic server holding a fixed song list.
type fakeSubsonic struct {
	user, password string
	songs          []subsonicSong
	noSearch       bool              // search3 returns nothing for an empty query
	streams        map[string]string // song id → body served by stream
	lastFormat     string            // format param of the last stream call
	streamCalls    atomic.Int32      // number of stream requests served
	gate           chan struct{}     // when set, stream blocks until it is closed
	starCalls      []string          // "star:<id>" / "unstar:<id>" in call order
}

func (f *fakeSubsonic) handler(t *testing.T) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		sum := md5.Sum([]byte(f.password + q.Get("s")))
		if q.Get("u") != f.user || q.Get("t") != hex.EncodeToString(sum[:]) {
			writeEnvelope(w, map[string]any{"status": "failed", "error": map[string]any{"code": 40, "message": "Wrong username or password"}})
			return
		}
		if q.Get("p") != "" {
			t.Errorf("plain password sent on the wire")
		}

		switch r.URL.Path {
		case "/rest/ping":
			writeEnvelope(w, map[string]any{"status": "ok"})
		case "/rest/search3":
			var page []subsonicSong
			if !f.noSearch {
				count, _ := strconv.Atoi(q.Get("songCount"))
				offset, _ := strconv.Atoi(q.Get("songOffset"))
				for i := offset; i < len(f.songs) && i < offset+count; i++ {
					page = append(page, f.songs[i])
				}
			}
			writeEnvelope(w, map[string]any{"status": "ok", "searchResult3": map[string]any{"song": page}})
		case "/rest/star", "/rest/unstar":
			f.starCalls = append(f.starCalls, strings.TrimPrefix(r.URL.Path, "/rest/")+":"+q.Get("id"))
			writeEnvelope(w, map[string]any{"status": "ok"})
		case "/rest/getStarred2":
			var starred []subsonicSong
			for _, s := range f.songs {
				if s.Starred != "" {
					starred = append(starred, s)
				}
			}
			writeEnvelope(w, map[string]any{"status": "ok", "starred2": map[string]any{"song": starred}})
		case "/rest/getRandomSongs":
			writeEnvelope(w, map[string]any{"status": "ok", "randomSongs": map[string]any{"song": f.songs}})
		case "/rest/stream":
			f.streamCalls.Add(1)
			if f.gate != nil {
				select {
				case <-f.gate:
				case <-r.Context().Done():
					return
				}
			}
			f.lastFormat = q.Get("format")
			body, ok := f.streams[q.Get("id")]
			if !ok {
				writeEnvelope(w, map[string]any{"status": "failed", "error": map[string]any{"code": 70, "message": "Song not found"}})
				return
			}
			w.Header().Set("Content-Type", "audio/mpeg")
			fmt.Fprint(w, body)
		default:
			http.NotFound(w, r)
		}
	})
}

func writeEnvelope(w http.ResponseWriter, body map[string]any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"subsonic-response": body})
}

func newFakeClient(t *testing.T, f *fakeSubsonic) *SubsonicClient {
	t.Helper()
	srv := httptest.NewServer(f.handler(t))
	t.Cleanup(srv.Close)
	c := NewSubsonicClient(srv.URL, f.user, f.password)
	t.Cleanup(c.Close)
	return c
}

func TestSubsonicPingAuth(t *testing.T) {
	f := &fakeSubsonic{user: "derp", password: "s3cret"}
	if err := newFakeClient(t, f).Ping(); err != nil {
		t.Fatalf("Ping with valid credentials: %v", err)
	}

	srv := httptest.NewServer(f.handler(t))
	defer srv.Close()
	err := NewSubsonicClient(srv.URL, "derp", "wrong").Ping()
	if err == nil || !strings.Contains(err.Error(), "Wrong username or password") {
		t.Fatalf("Ping with bad password: got %v, want auth error", err)
	}
}

func TestSubsonicSongsPages(t *testing.T) {
	f := &fakeSubsonic{user: "u", password: "p"}
	for i := range subsonicPageSize + 7 {
		f.songs = append(f.songs, subsonicSong{ID: fmt.Sprint(i)})
	}
	songs, err := newFakeClient(t, f).Songs(nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(songs) != len(f.songs) {
		t.Fatalf("got %d songs, want %d", len(songs), len(f.songs))
	}
}

func TestSubsonicSongsFallsBackToRandom(t *testing.T) {
	f := &fakeSubsonic{user: "u", password: "p", noSearch: true,
		songs: []subsonicSong{{ID: "a"}, {ID: "b"}}}
	songs, err := newFakeClient(t, f).Songs(nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(songs) != 2 {
		t.Fatalf("got %d songs, want 2 from getRandomSongs", len(songs))
	}
}

func TestSubsonicFetch(t *testing.T) {
	f := &fakeSubsonic{user: "u", password: "p",
		songs: []subsonicSong{
			{ID: "flac1", Suffix: "flac", Title: "T"},
			{ID: "aac1", Suffix: "m4a"},
		},
		streams: map[string]string{"flac1": "FLACDATA", "aac1": "MP3DATA"},
	}
	c := newFakeClient(t, f)
	if _, err := c.Songs(nil); err != nil {
		t.Fatal(err)
	}

	local, song, err := c.Fetch(context.Background(), "flac1")
	if err != nil {
		t.Fatal(err)
	}
	if f.lastFormat != "raw" || !strings.HasSuffix(local, ".flac") || song.Title != "T" {
		t.Errorf("decodable song: format=%q path=%q title=%q", f.lastFormat, local, song.Title)
	}
	if b, _ := os.ReadFile(local); string(b) != "FLACDATA" {
		t.Errorf("downloaded %q, want FLACDATA", b)
	}

	local, _, err = c.Fetch(context.Background(), "aac1")
	if err != nil {
		t.Fatal(err)
	}
	if f.lastFormat != "mp3" || !strings.HasSuffix(local, ".mp3") {
		t.Errorf("undecodable song should be transcoded: format=%q path=%q", f.lastFormat, local)
	}

	c.Close()
	if _, err := os.Stat(local); !os.IsNotExist(err) {
		t.Errorf("Close left %s behind", local)
	}
}

func TestSubsonicFetchServerError(t *testing.T) {
	f := &fakeSubsonic{user: "u", password: "p"}
	_, _, err := newFakeClient(t, f).Fetch(context.Background(), "missing")
	if err == nil || !strings.Contains(err.Error(), "Song not found") {
		t.Fatalf("got %v, want server error surfaced", err)
	}
}

func TestLoadTrackSubsonicWithoutSession(t *testing.T) {
	subsonicSession = nil
	err := NewAudioPlayer().LoadTrack(subsonicPrefix + "x")
	if err == nil || !strings.Contains(err.Error(), "no Subsonic server") {
		t.Fatalf("got %v, want missing-session error", err)
	}
}

func TestNormalizeServerURL(t *testing.T) {
	cases := map[string]string{
		"music.example.com":         "https://music.example.com",
		" http://host:4533/ ":       "http://host:4533",
		"https://host/navidrome///": "https://host/navidrome",
		"":                          "",
	}
	for in, want := range cases {
		if got := normalizeServerURL(in); got != want {
			t.Errorf("normalizeServerURL(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSafeFileName(t *testing.T) {
	if got := safeFileName("../a/b c"); got != "___a_b_c" {
		t.Errorf("got %q", got)
	}
}

func TestSubsonicFetchCancelled(t *testing.T) {
	f := &fakeSubsonic{user: "u", password: "p", streams: map[string]string{"a": "DATA"}}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := newFakeClient(t, f).Fetch(ctx, "a"); err == nil {
		t.Fatal("expected a cancelled fetch to fail")
	}
}

func TestLoadTrackContextCancelledSkipsDecode(t *testing.T) {
	f := &fakeSubsonic{user: "u", password: "p", streams: map[string]string{"a": "DATA"}}
	subsonicSession = newFakeClient(t, f)
	defer func() { subsonicSession = nil }()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := NewAudioPlayer().LoadTrackContext(ctx, subsonicPrefix+"a")
	if err == nil {
		t.Fatal("expected a cancelled load to fail")
	}
}

func TestSubsonicPrefetchIsReused(t *testing.T) {
	f := &fakeSubsonic{user: "u", password: "p", streams: map[string]string{"a": "DATA"}}
	c := newFakeClient(t, f)
	c.Prefetch("a")
	c.Prefetch("a") // already in flight: no second download

	local, _, err := c.Fetch(context.Background(), "a")
	if err != nil {
		t.Fatal(err)
	}
	if n := f.streamCalls.Load(); n != 1 {
		t.Errorf("stream called %d times, want 1", n)
	}
	if b, _ := os.ReadFile(local); string(b) != "DATA" {
		t.Errorf("got %q", b)
	}
}

func TestSubsonicPrefetchReplacedIsDeleted(t *testing.T) {
	f := &fakeSubsonic{user: "u", password: "p", streams: map[string]string{"a": "A", "b": "B"}}
	c := newFakeClient(t, f)
	c.Prefetch("a")
	c.mu.Lock()
	first := c.next
	c.mu.Unlock()
	<-first.done

	c.Prefetch("b")
	deadline := time.Now().Add(2 * time.Second)
	for {
		if _, err := os.Stat(first.path); os.IsNotExist(err) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("replaced prefetch %s was not deleted", first.path)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if _, _, err := c.Fetch(context.Background(), "b"); err != nil {
		t.Fatal(err)
	}
	if n := f.streamCalls.Load(); n != 2 {
		t.Errorf("stream called %d times, want 2", n)
	}
}

func TestSubsonicFetchRetriesFailedPrefetch(t *testing.T) {
	f := &fakeSubsonic{user: "u", password: "p", streams: map[string]string{}}
	c := newFakeClient(t, f)
	c.Prefetch("a") // fails: song not served yet
	c.mu.Lock()
	p := c.next
	c.mu.Unlock()
	<-p.done
	f.streams["a"] = "DATA"

	if _, _, err := c.Fetch(context.Background(), "a"); err != nil {
		t.Fatalf("Fetch should download directly after a failed prefetch: %v", err)
	}
	if n := f.streamCalls.Load(); n != 2 {
		t.Errorf("stream called %d times, want 2", n)
	}
}

func TestSubsonicFetchCancelledWhileWaitingOnPrefetch(t *testing.T) {
	f := &fakeSubsonic{user: "u", password: "p", streams: map[string]string{"a": "A"}, gate: make(chan struct{})}
	c := newFakeClient(t, f)
	defer close(f.gate)
	c.Prefetch("a")

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, _, err := c.Fetch(ctx, "a"); err == nil {
		t.Fatal("expected the wait to end with the context")
	}
}

func TestSubsonicFavourites(t *testing.T) {
	f := &fakeSubsonic{user: "u", password: "p", songs: []subsonicSong{
		{ID: "a", Starred: "2026-10-01T00:00:00Z"},
		{ID: "b"},
	}}
	c := newFakeClient(t, f)

	favs, err := c.Favourites()
	if err != nil {
		t.Fatal(err)
	}
	if len(favs) != 1 || favs[0].ID != "a" || !c.IsFavourite("a") || c.IsFavourite("b") {
		t.Fatalf("favourites: %+v", favs)
	}

	if err := c.SetFavourite("b", true); err != nil {
		t.Fatal(err)
	}
	if err := c.SetFavourite("a", false); err != nil {
		t.Fatal(err)
	}
	if !c.IsFavourite("b") || c.IsFavourite("a") {
		t.Error("SetFavourite did not update local state")
	}
	if got := strings.Join(f.starCalls, ","); got != "star:b,unstar:a" {
		t.Errorf("server calls: %s", got)
	}
}
