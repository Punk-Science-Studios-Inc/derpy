package main

// zen.go — the Zen presentation of the derpy TUI.
//
// One room, many visitors: playback is a centered column (hero line, album
// whisper, hairline progress, position whisper, hint); every overlay —
// key entry, channel feed, earmark picker, help — borrows that
// same column inside a hairline card and gives it back. Transient system
// state (Nostr receipts) is a single muted line
// that appears only while it has something to say.
//
// Collapsing rule: a line with nothing to show leaves no blank behind. No
// album → no album line. No transient state → no transient line.

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/charmbracelet/lipgloss"
)

// zenBarMaxWidth caps the progress hairline so it stays a whisper on wide
// terminals; zenBarMinWidth keeps it legible on narrow ones.
const (
	zenBarMaxWidth = 48
	zenBarMinWidth = 16
)

// zenTrackDisplay renders "Artist — Title", falling back to the bare
// filename when embedded metadata is missing. A Subsonic track that is still
// downloading shows a waiting message instead of its opaque "subsonic:" id.
func (m *PlayerModel) zenTrackDisplay() string {
	_, remote := subsonicEntryID(m.playlist[m.currentIndex])
	if m.loading && remote {
		return "Waiting for Sonic server..."
	}
	if m.title != "" && m.artist != "" {
		return fmt.Sprintf("%s — %s", m.artist, m.title)
	}
	if m.title != "" {
		return m.title
	}
	if remote {
		return "Unknown track"
	}
	trackDisplay := filepath.Base(m.playlist[m.currentIndex])
	if ext := filepath.Ext(trackDisplay); ext != "" {
		trackDisplay = strings.TrimSuffix(trackDisplay, ext)
	}
	return trackDisplay
}

// zenCenter centers every line of s in a field width. A non-positive width
// (tests, --no-tui) renders unpadded.
func zenCenter(width int, s string) string {
	if width <= 0 {
		return s
	}
	return lipgloss.PlaceHorizontal(width, lipgloss.Center, s)
}

// zenBarWidth adapts the hairline to the terminal, clamped to the
// zenBarMin/MaxWidth constants.
func (m *PlayerModel) zenBarWidth() int {
	if m.width <= 0 {
		return 32
	}
	w := m.width - 30 // room for glyph, headroom and "pos / dur"
	if w < zenBarMinWidth {
		return zenBarMinWidth
	}
	if w > zenBarMaxWidth {
		return zenBarMaxWidth
	}
	return w
}

// zenProgressLine renders the status glyph, the hairline with its ● head at
// the playhead, and the position/duration pair — e.g.
// "▶ ───────●─────────────  1:02 / 3:59".
func (m *PlayerModel) zenProgressLine() string {
	status := "■"
	if m.playing {
		if m.paused {
			status = "⏸"
		} else {
			status = "▶"
		}
	}
	width := m.zenBarWidth()
	head := 0
	if m.duration > 0 {
		head = int(float64(m.position) / float64(m.duration) * float64(width))
		if head > width {
			head = width
		}
	}
	elapsed := psProgress.Render(strings.Repeat("─", head) + "●")
	rest := psStatus.Render(strings.Repeat("─", width-head))
	times := "  " + strings.TrimSpace(psStatus.Render(fmt.Sprintf("%s / %s", formatDuration(m.position), formatDuration(m.duration))))
	return fmt.Sprintf("%s %s%s%s",
		strings.TrimSpace(psStatus.Render(status)),
		strings.TrimSpace(elapsed),
		strings.TrimSpace(rest),
		times)
}

// zenWhisper renders the playlist position line: "3 / 12".
func (m *PlayerModel) zenWhisper() string {
	whisper := fmt.Sprintf("%d / %d", m.currentIndex+1, len(m.playlist))
	return strings.TrimSpace(psStatus.Render(whisper))
}

// zenTransient renders the single system line — the Nostr receipt — or ""
// when there is nothing to say.
func (m *PlayerModel) zenTransient() string {
	if m.nostrStatus != "" {
		return strings.TrimSpace(psNostr.Render(m.nostrStatus))
	}
	return ""
}

// zenCard wraps overlay rows in a hairline card, centered in the column.
// hint is rendered below the card in the same centered column.
func (m *PlayerModel) zenCard(title string, rows []string, hint string) string {
	var body strings.Builder
	if title != "" {
		body.WriteString(strings.TrimSpace(psPrompt.Render(title)))
		body.WriteString("\n")
	}
	for _, r := range rows {
		body.WriteString(strings.TrimSpace(r))
		body.WriteString("\n")
	}
	card := lipgloss.NewStyle().
		Border(lipgloss.NormalBorder()).
		BorderForeground(lipgloss.Color("#3A3A3A")).
		Padding(0, 2).
		Render(strings.TrimSuffix(body.String(), "\n"))
	var out strings.Builder
	out.WriteString(card)
	if hint != "" {
		out.WriteString("\n")
		out.WriteString(strings.TrimSpace(psStatus.Render(hint)))
	}
	return zenCenter(m.width, out.String())
}

// zenHome renders the playback room all overlays borrow: hero, album,
// progress, whisper, transient, hint.
func (m *PlayerModel) zenHome(hint string) string {
	var lines []string
	lines = append(lines, strings.TrimSpace(psStatus.Render("derpy")))
	lines = append(lines, "")
	hero := strings.TrimSpace(psTrack.Render(m.zenTrackDisplay()))
	if m.favourite && !m.loading {
		hero += " " + strings.TrimSpace(psProgress.Render("♥"))
	}
	lines = append(lines, hero)
	if m.album != "" {
		lines = append(lines, strings.TrimSpace(psAlbum.Render(m.album)))
	}
	lines = append(lines, "")
	lines = append(lines, m.zenProgressLine())
	lines = append(lines, m.zenWhisper())
	if t := m.zenTransient(); t != "" {
		lines = append(lines, t)
	}
	lines = append(lines, "")
	lines = append(lines, strings.TrimSpace(psStatus.Render(hint)))
	return zenCenter(m.width, strings.Join(lines, "\n"))
}

// zenHelpRows lists every key. Shown in the "?" card; the home hint stays
// minimal because this card carries discoverability.
func zenHelpRows() []string {
	return []string{
		"← / →        previous / next track",
		"space        pause / resume",
		"e            earmark this track",
		"c            browse channel feed",
		"p            post to nostr + bluesky",

		"d            delete file from disk",
		"f            favourite (Sonic tracks)",
		"? / esc      close this card",
		"q            quit",
	}
}

// View renders the Zen TUI. Overlay precedence mirrors the old view:
// key entry, channel feed, picker, help, then home.
func (m *PlayerModel) View() string {
	if m.err != nil {
		return zenCenter(m.width, fmt.Sprintf("error: %v\nq or esc to quit", m.err))
	}

	if len(m.playlist) == 0 {
		return zenCenter(m.width, "silence.\nq or esc to quit")
	}

	context := strings.TrimSpace(psStatus.Render(m.zenTrackDisplay()))

	// Nostr key-entry overlay.
	if m.nostrKeyEntry {
		masked := strings.Repeat("*", len(m.nostrKeyBuffer))
		return zenCenter(m.width, context) + "\n" + m.zenCard("nostr key",
			[]string{fmt.Sprintf("> %s", masked)},
			"paste nsec1... or hex key — enter to save, esc to cancel")
	}

	// Channel feed browser.
	if m.channelFeed {
		var rows []string
		switch {
		case m.channelFeedLoad:
			rows = []string{"fetching..."}
		case m.channelFeedErr != "":
			rows = []string{m.channelFeedErr}
		case len(m.channels) == 0:
			rows = []string{
				"you are not in any channels yet.",
				"create one:   earmark channel create <name>",
				"then invite:  earmark channel invite <name> <npub>",
			}
		case len(m.channelPosts) == 0:
			// No backfill is by design, so say so rather than leaving the
			// user staring at an empty list wondering what broke.
			rows = []string{
				"nothing posted yet.",
				"tracks shared from now on appear here — channels do not backfill.",
			}
		default:
			names := map[string]string{}
			for _, c := range m.channels {
				names[c.Descriptor.ID] = c.Descriptor.Name
			}
			for i, post := range m.channelPosts {
				marker := "  "
				if m.channelFeedCursor == i {
					marker = "› "
				}
				desc := post.Earmark.Title
				if post.Earmark.Artist != "" {
					desc = post.Earmark.Artist + " — " + desc
				}
				name := names[post.Chan]
				if name == "" {
					name = post.Chan[:8]
				}
				row := fmt.Sprintf("%s%s  %s", marker, truncateZen(desc, 32), name)
				if m.channelFeedCursor == i {
					row = psPrompt.Render(row)
				}
				rows = append(rows, row)
			}
		}
		return zenCenter(m.width, context) + "\n" + m.zenCard("", rows, "enter play · esc close")
	}

	// Channel-target overlay: personal and each channel are toggles, and the
	// earmark goes to whatever is selected. At least one must stay checked.
	if m.channelPicker {
		personalMarker := "  "
		if m.channelCursor == 0 {
			personalMarker = "› "
		}
		personalBox := "[ ]"
		if m.channelPersonal {
			personalBox = "[x]"
		}
		personalRow := fmt.Sprintf("%s%s personal — on every device", personalMarker, personalBox)
		if m.channelCursor == 0 {
			personalRow = psPrompt.Render(personalRow)
		}
		rows := []string{personalRow}
		for i, c := range m.channels {
			marker := "  "
			if m.channelCursor == i+1 {
				marker = "› "
			}
			box := "[ ]"
			if m.channelTargets[c.Descriptor.ID] {
				box = "[x]"
			}
			row := fmt.Sprintf("%s%s %s", marker, box, c.Descriptor.Name)
			if m.channelCursor == i+1 {
				row = psPrompt.Render(row)
			}
			rows = append(rows, row)
		}
		hint := "space toggle · enter keep · esc cancel"
		if m.channelPickerSelected() == 0 {
			hint = "space toggle · select at least one target · esc cancel"
		}
		return zenCenter(m.width, context) + "\n" + m.zenCard("keep this?",
			rows, hint)
	}

	// Help overlay: carries key discoverability so the home hint can stay
	// to four glyphs.
	if m.help {
		return zenCenter(m.width, context) + "\n" + m.zenCard("keys", zenHelpRows(), "? / esc close")
	}


	return m.zenHome("space · ← → · e · ?")
}

// truncateZen clips s to at most width runes with an ellipsis, keeping feed
// and picker rows inside the centered card.
func truncateZen(s string, width int) string {
	if width <= 1 {
		return s
	}
	runes := []rune(s)
	if len(runes) <= width {
		return s
	}
	return string(runes[:width-1]) + "…"
}
