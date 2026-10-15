package main

import (
	"bufio"
	tea "github.com/charmbracelet/bubbletea"
	"io"
	"strings"
	"testing"
)

// testPrompter answers prompts from input and returns password for the
// password prompt, counting how often the password was asked for.
func testPrompter(input, password string, asked *int) sonicPrompter {
	return sonicPrompter{
		in:  bufio.NewReader(strings.NewReader(input)),
		out: io.Discard,
		readPassword: func() (string, error) {
			*asked++
			return password, nil
		},
	}
}

func TestResolveSonicDetailsPromptsWhenUnconfigured(t *testing.T) {
	var asked int
	d, err := resolveSonicDetails(&Config{}, "", "", testPrompter("music.example.com\nderp\n", "pw", &asked))
	if err != nil {
		t.Fatal(err)
	}
	if d.server != "https://music.example.com" || d.user != "derp" || d.password != "pw" || !d.changed {
		t.Fatalf("got %+v", d)
	}
}

func TestResolveSonicDetailsUsesSavedConfig(t *testing.T) {
	cfg := &Config{SubsonicServer: "https://s", SubsonicUser: "derp", SubsonicPassword: "pw"}
	var asked int
	d, err := resolveSonicDetails(cfg, "", "", testPrompter("", "", &asked))
	if err != nil {
		t.Fatal(err)
	}
	if asked != 0 || d.changed || d.server != "https://s" || d.password != "pw" {
		t.Fatalf("saved config should be used without prompting: %+v, asked=%d", d, asked)
	}
}

func TestResolveSonicDetailsNewServerAsksAgain(t *testing.T) {
	cfg := &Config{SubsonicServer: "https://old", SubsonicUser: "derp", SubsonicPassword: "oldpw"}
	var asked int
	d, err := resolveSonicDetails(cfg, "https://new", "", testPrompter("other\n", "newpw", &asked))
	if err != nil {
		t.Fatal(err)
	}
	if d.server != "https://new" || d.user != "other" || d.password != "newpw" || !d.changed || asked != 1 {
		t.Fatalf("got %+v, asked=%d", d, asked)
	}
}

func TestResolveSonicDetailsNewUserAsksPassword(t *testing.T) {
	cfg := &Config{SubsonicServer: "https://s", SubsonicUser: "derp", SubsonicPassword: "pw"}
	var asked int
	d, err := resolveSonicDetails(cfg, "", "other", testPrompter("", "pw2", &asked))
	if err != nil {
		t.Fatal(err)
	}
	if d.user != "other" || d.password != "pw2" || asked != 1 {
		t.Fatalf("got %+v, asked=%d", d, asked)
	}
}

func TestResolveSonicDetailsNoInput(t *testing.T) {
	var asked int
	if _, err := resolveSonicDetails(&Config{}, "", "", testPrompter("", "", &asked)); err == nil {
		t.Fatal("expected an error when stdin is empty")
	}
}

func TestSubsonicPlaylistFilter(t *testing.T) {
	songs := []subsonicSong{
		{ID: "1", Path: "Jazz/Piano/a.flac"},
		{ID: "2", Artist: "Blues Guy", Album: "Delta", Title: "b"},
		{ID: "3", Path: "Rock/c.mp3"},
	}
	all, err := subsonicPlaylist(songs, "")
	if err != nil || len(all) != 3 || all[0] != "subsonic:1" {
		t.Fatalf("no filter: %v %v", all, err)
	}
	got, err := subsonicPlaylist(songs, "jazz OR delta")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0] != "subsonic:1" || got[1] != "subsonic:2" {
		t.Fatalf("filtered: %v", got)
	}
	if _, err := subsonicPlaylist(songs, "(jazz"); err == nil {
		t.Fatal("expected a parse error")
	}
}

func TestZenTrackDisplayWhileLoadingSubsonic(t *testing.T) {
	m := &PlayerModel{playlist: []string{subsonicPrefix + "tr-42"}, loading: true}
	if got := m.zenTrackDisplay(); got != "Waiting for Sonic server..." {
		t.Fatalf("loading: got %q", got)
	}
	m.loading = false
	if got := m.zenTrackDisplay(); got != "Unknown track" {
		t.Fatalf("loaded without metadata: got %q, want no raw id", got)
	}
	m.artist, m.title = "A", "T"
	if got := m.zenTrackDisplay(); got != "A — T" {
		t.Fatalf("loaded: got %q", got)
	}
}

func TestLoadCurrentTrackResetsDisplayAndDropsStaleResults(t *testing.T) {
	m := NewPlayerModel([]string{subsonicPrefix + "a", subsonicPrefix + "b"})
	m.playing, m.artist, m.title, m.album = true, "Old", "Song", "Record"

	_ = m.loadCurrentTrack()
	first := m.loadSeq
	if !m.loading || m.playing || m.title != "" || m.album != "" {
		t.Fatalf("load start should clear the old track: %+v", m)
	}

	m.currentIndex = 1
	_ = m.loadCurrentTrack()

	// A result from the superseded load must not overwrite the new one.
	m.Update(trackLoadedMsg{seq: first, title: "stale"})
	if m.title == "stale" || !m.loading {
		t.Fatal("stale trackLoadedMsg was applied")
	}
	m.Update(trackLoadedMsg{seq: m.loadSeq, title: "fresh", album: "LP"})
	if m.title != "fresh" || m.album != "LP" || m.loading {
		t.Fatalf("current trackLoadedMsg not applied: title=%q loading=%v", m.title, m.loading)
	}
}

func TestFavouriteKeyOnLocalTrack(t *testing.T) {
	m := NewPlayerModel([]string{"/music/a.mp3"})
	m.playing = true
	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'f'}})
	if cmd != nil || !strings.Contains(m.nostrStatus, "only available for Sonic") {
		t.Fatalf("local track: cmd=%v status=%q", cmd != nil, m.nostrStatus)
	}
}

func TestFavouriteMsgUpdatesCurrentTrackOnly(t *testing.T) {
	m := NewPlayerModel([]string{subsonicPrefix + "a", subsonicPrefix + "b"})
	m.Update(favouriteMsg{id: "a", favourite: true})
	if !m.favourite || !strings.Contains(m.nostrStatus, "Added") {
		t.Fatalf("current track: favourite=%v status=%q", m.favourite, m.nostrStatus)
	}
	if !strings.Contains(m.zenHome(""), "♥") {
		t.Error("heart not shown for a favourite")
	}

	// A result for a track the user already skipped past must not mark the
	// current one.
	m.favourite = false
	m.Update(favouriteMsg{id: "b", favourite: true})
	if m.favourite {
		t.Error("favouriteMsg for another track changed the current track")
	}
}
