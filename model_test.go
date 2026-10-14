package main

import (
	"fmt"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

// TestTrackLoadedMsgAppliesMetadata verifies the Update handler for
// trackLoadedMsg copies the track metadata onto the model.
func TestTrackLoadedMsgAppliesMetadata(t *testing.T) {
	m := NewPlayerModel([]string{"a.flac"})

	msg := trackLoadedMsg{
		duration: 3 * time.Minute,
		artist:   "Aphex Twin",
		title:    "Xtal",
		album:    "Selected Ambient Works 85-92",
	}
	m.Update(msg)

	if m.artist != "Aphex Twin" || m.title != "Xtal" {
		t.Errorf("metadata not applied: artist=%q title=%q", m.artist, m.title)
	}
}

// TestPlayErrorSkipsToNextTrack verifies a single load/play failure is
// non-fatal: the model logs it, advances to the next track, and returns a
// reload command instead of setting m.err (which would freeze the UI).
func TestPlayErrorSkipsToNextTrack(t *testing.T) {
	t.Setenv("USERPROFILE", t.TempDir()) // keep the log out of the real home
	t.Setenv("HOME", t.TempDir())

	m := NewPlayerModel([]string{"a.mp3", "b.mp3", "c.mp3"})
	m.currentIndex = 0

	_, cmd := m.Update(playErrorMsg(fmt.Errorf("failed to load track %q: boom", "a.mp3")))

	if m.err != nil {
		t.Fatalf("m.err set on a single failure; skip should be non-fatal: %v", m.err)
	}
	if m.currentIndex != 1 {
		t.Errorf("currentIndex = %d, want 1 (advanced to next track)", m.currentIndex)
	}
	if m.loadFailures != 1 {
		t.Errorf("loadFailures = %d, want 1", m.loadFailures)
	}
	if cmd == nil {
		t.Errorf("expected a reload command to load the next track, got nil")
	}
}

// TestPlayErrorFatalAfterWholePlaylistFails verifies the runaway guard: once
// consecutive failures reach the playlist length (nothing plays at all), the
// model surfaces a fatal error rather than looping forever.
func TestPlayErrorFatalAfterWholePlaylistFails(t *testing.T) {
	t.Setenv("USERPROFILE", t.TempDir())
	t.Setenv("HOME", t.TempDir())

	m := NewPlayerModel([]string{"a.mp3", "b.mp3"})

	m.Update(playErrorMsg(fmt.Errorf("fail 1")))
	if m.err != nil {
		t.Fatalf("first failure should not be fatal: %v", m.err)
	}
	_, cmd := m.Update(playErrorMsg(fmt.Errorf("fail 2")))
	if m.err == nil {
		t.Fatalf("second failure (== playlist length) should be fatal")
	}
	if cmd != nil {
		t.Errorf("no reload command expected once fatal, got one")
	}
}

// TestTrackLoadedResetsFailureGuard verifies a successful load clears the
// consecutive-failure counter so a bad track earlier in the run doesn't push
// a later, unrelated failure over the fatal threshold.
func TestTrackLoadedResetsFailureGuard(t *testing.T) {
	m := NewPlayerModel([]string{"a.mp3", "b.mp3"})
	m.loadFailures = 1
	m.Update(trackLoadedMsg{title: "X", artist: "Y"})
	if m.loadFailures != 0 {
		t.Errorf("loadFailures = %d after a successful load, want 0", m.loadFailures)
	}
}


// keyMsg builds a tea.KeyMsg for a single character or named key. Mirrors
// Bubble Tea's internal construction so msg.String() and msg.Runes both
// look right to the handler.
func keyMsg(s string) tea.KeyMsg {
	switch s {
	case "enter":
		return tea.KeyMsg{Type: tea.KeyEnter}
	case "esc":
		return tea.KeyMsg{Type: tea.KeyEsc}
	case "backspace":
		return tea.KeyMsg{Type: tea.KeyBackspace}
	default:
		runes := []rune(s)
		return tea.KeyMsg{Type: tea.KeyRunes, Runes: runes}
	}
}

// TestRenderControls verifies that [E] and [P] appear only when a key is available.
func TestRenderControls(t *testing.T) {
	tests := []struct {
		name   string
		hasKey bool
		want   string
		not    string
	}{
		{
			name:   "with key",
			hasKey: true,
			want:   "e earmark",
			not:    "",
		},
		{
			name:   "without key",
			hasKey: false,
			want:   "",
			not:    "e earmark  p post",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := renderControls(tt.hasKey, false)
			if tt.want != "" && !strings.Contains(got, tt.want) {
				t.Errorf("renderControls(%v) missing %q: %q", tt.hasKey, tt.want, got)
			}
			if tt.not != "" && strings.Contains(got, tt.not) {
				t.Errorf("renderControls(%v) should not contain %q: %q", tt.hasKey, tt.not, got)
			}
		})
	}
}

// TestViewControlsHideWhenNoKey verifies that renderControls(false, false)
// omits [E] and [P] while preserving all other controls.
func TestViewControlsHideWhenNoKey(t *testing.T) {
	got := renderControls(false, false)

	if strings.Contains(got, "e earmark") || strings.Contains(got, "p post") {
		t.Errorf("renderControls(false) should not contain [E]/[P]: %q", got)
	}
	if !strings.Contains(got, "← prev") || !strings.Contains(got, "esc quit") {
		t.Errorf("renderControls(false) missing basic controls: %q", got)
	}
}

// TestRenderSocialStatus verifies the combined post-status messages.
func TestRenderSocialStatus(t *testing.T) {
	tests := []struct {
		name string
		msg  socialPublishMsg
		want string
	}{
		{
			name: "both succeed",
			msg:  socialPublishMsg{nostrOK: true, bskyOK: true},
			want: "Posted to Bluesky + Nostr!",
		},
		{
			name: "nostr only succeeds",
			msg:  socialPublishMsg{nostrOK: true},
			want: "Nostr: posted!",
		},
		{
			name: "bsky only succeeds",
			msg:  socialPublishMsg{bskyOK: true},
			want: "Bluesky: posted!",
		},
		{
			name: "nostr fails bsky succeeds",
			msg: socialPublishMsg{
				nostrErr: fmt.Errorf("timeout"),
				bskyOK:   true,
			},
			want: "Nostr: failed — timeout  Bluesky: posted!",
		},
		{
			name: "neither attempted",
			msg:  socialPublishMsg{},
			want: "Post: nothing to do",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := renderSocialStatus(tt.msg)
			if got != tt.want {
				t.Errorf("renderSocialStatus() = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestRenderControlsDualPlatform verifies controls display with Bluesky-only.
func TestRenderControlsDualPlatform(t *testing.T) {
	// Bluesky-only: shows [P] but not [E].
	got := renderControls(false, true)
	if !strings.Contains(got, "p post") {
		t.Errorf("renderControls(false, true) should contain [P]: %q", got)
	}
	if strings.Contains(got, "e earmark") {
		t.Errorf("renderControls(false, true) should not contain [E]: %q", got)
	}

	// Both: shows both [E] and [P].
	got = renderControls(true, true)
	if !strings.Contains(got, "e earmark") || !strings.Contains(got, "p post") {
		t.Errorf("renderControls(true, true) should contain [E] and [P]: %q", got)
	}

	// Neither: hides both.
	got = renderControls(false, false)
	if strings.Contains(got, "e earmark") || strings.Contains(got, "p post") {
		t.Errorf("renderControls(false, false) should hide [E]/[P]: %q", got)
	}
}
