package main

// zen_test.go — tests for the Zen presentation (see zen.go).
//
// Contract under test: playback is a centered column (hero, album whisper,
// hairline progress, position whisper, hint); lines with nothing to
// show collapse without leaving blanks; every overlay borrows the column.

import (
	"strings"
	"testing"
	"time"
)

// TestZenHomeShowsAlbum verifies the hero carries artist, title AND album.
func TestZenHomeShowsAlbum(t *testing.T) {
	m := NewPlayerModel([]string{"a.flac"})
	m.Update(trackLoadedMsg{
		artist:   "Talking Heads",
		title:    "This Must Be the Place",
		album:    "Speaking in Tongues",
		duration: 5*time.Minute + 12*time.Second,
	})
	m.width = 0 // unpadded: assert on raw lines

	out := m.View()
	for _, want := range []string{
		"Talking Heads — This Must Be the Place",
		"Speaking in Tongues",
		"●",             // hairline head
		"00:00 / 05:12", // position / duration
		"space",         // hint survives
	} {
		if !strings.Contains(out, want) {
			t.Errorf("Zen home missing %q: %q", want, out)
		}
	}
}

// TestZenHomeCollapsesAlbumWhenMissing verifies the album line leaves no
// blank behind when metadata has no album.
func TestZenHomeCollapsesAlbumWhenMissing(t *testing.T) {
	with := NewPlayerModel([]string{"a.flac"})
	with.Update(trackLoadedMsg{artist: "Y", title: "X", album: "A"})
	with.width = 0

	without := NewPlayerModel([]string{"a.flac"})
	without.Update(trackLoadedMsg{artist: "Y", title: "X"})
	without.width = 0

	diff := len(strings.Split(with.View(), "\n")) - len(strings.Split(without.View(), "\n"))
	if diff != 1 {
		t.Errorf("missing album should collapse exactly one line, collapsed %d", diff)
	}
}

// TestZenWhisperShowsPosition verifies the whisper is just the playlist
// position — no dangling separator.
func TestZenWhisperShowsPosition(t *testing.T) {
	m := NewPlayerModel([]string{"a.flac", "b.flac"})
	m.Update(trackLoadedMsg{title: "X", artist: "Y"})
	m.currentIndex = 1

	got := m.zenWhisper()
	if !strings.Contains(got, "2 / 2") {
		t.Errorf("zenWhisper should carry the position, got %q", got)
	}
	if strings.Contains(got, "·") {
		t.Errorf("zenWhisper should have no separator, got %q", got)
	}
}

// TestZenTrackDisplayFallsBackToFilename verifies metadata gaps degrade to
// the bare filename without its extension.
func TestZenTrackDisplayFallsBackToFilename(t *testing.T) {
	m := NewPlayerModel([]string{"/music/unknown track.flac"})
	if got := m.zenTrackDisplay(); got != "unknown track" {
		t.Errorf("zenTrackDisplay = %q, want %q", got, "unknown track")
	}
}

// TestZenHelpOpensAndCloses verifies "?" raises the keys card (which carries
// discoverability so the home hint can stay minimal) and esc lowers it.
func TestZenHelpOpensAndCloses(t *testing.T) {
	m := NewPlayerModel([]string{"a.flac"})
	m.Update(trackLoadedMsg{title: "X", artist: "Y"})

	m.Update(keyMsg("?"))
	if !m.help {
		t.Fatalf("expected help=true after [?]")
	}
	out := m.View()
	for _, want := range []string{"keys", "earmark", "channel", "delete"} {
		if !strings.Contains(out, want) {
			t.Errorf("help card missing %q: %q", want, out)
		}
	}

	m.Update(keyMsg("esc"))
	if m.help {
		t.Errorf("help still active after esc")
	}
}

// TestZenOtherKeyDismissesHelp verifies any other key closes the card first
// and still processes normally (here: space pauses).
func TestZenOtherKeyDismissesHelp(t *testing.T) {
	m := NewPlayerModel([]string{"a.flac"})
	m.Update(trackLoadedMsg{title: "X", artist: "Y"})
	m.playing = true
	m.paused = false
	m.Update(keyMsg("?"))

	m.Update(keyMsg(" "))
	if m.help {
		t.Errorf("help still active after space")
	}
	if !m.paused {
		t.Errorf("space should have paused after dismissing help")
	}
}

// TestZenCenterPads verifies centering pads (not truncates) to the width.
func TestZenCenterPads(t *testing.T) {
	got := zenCenter(20, "hi")
	if len([]rune(got)) != 20 {
		t.Errorf("zenCenter width = %d runes, want 20: %q", len([]rune(got)), got)
	}
	if strings.TrimSpace(got) != "hi" {
		t.Errorf("zenCenter mangled content: %q", got)
	}
	if got := zenCenter(0, "hi"); got != "hi" {
		t.Errorf("zenCenter(0) should not pad: %q", got)
	}
}

// TestZenTransientHiddenWhenIdle verifies the system line is empty when
// nothing transient is happening.
func TestZenTransientHiddenWhenIdle(t *testing.T) {
	m := NewPlayerModel([]string{"a.flac"})
	if got := m.zenTransient(); got != "" {
		t.Errorf("zenTransient = %q, want empty when idle", got)
	}
}
