package main

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"
	"unicode"

	tea "github.com/charmbracelet/bubbletea"

	core "github.com/punkscience/earmark/earmark-core"
)

// PlayerModel represents the state of the music player TUI
type PlayerModel struct {
	playlist     []string
	currentIndex int
	player       *AudioPlayer
	playing      bool
	paused       bool
	position     time.Duration
	duration     time.Duration
	width        int
	height       int
	err          error
	artist       string
	title        string
	album        string
	tickInterval time.Duration
	// mpris is the optional MPRIS2 D-Bus service.  It is nil when the session
	// D-Bus is unavailable or when running in --no-tui mode.
	mpris *MPRISService

	// Nostr key-entry state: when the user presses [E] or [P] and no key is
	// configured, the TUI switches into a one-time key-entry mode.
	nostrKeyEntry   bool   // true while waiting for the user to type their key
	nostrKeyBuffer  string // accumulates characters typed by the user
	nostrKeyForPost bool   // true when key entry was triggered by [P] (public post) vs [E] (earmark)
	nostrStatus     string // last Nostr result message shown to the user


	// Channel state. Channels are loaded once in the background at startup so
	// pressing [E] never blocks on a relay round-trip; an empty list simply
	// means [E] behaves exactly as it did before channels existed.
	channels        []core.Channel
	channelPicker   bool            // true while the [E] target overlay is up
	channelCursor   int             // 0 = personal, 1..n = channels
	channelPersonal bool            // personal list selected as a target (toggleable)
	channelTargets  map[string]bool // channel ids selected as targets

	// Channel feed browser, opened with [C]. Posts are fetched on demand
	// rather than at startup — the feed is a deliberate detour, not something
	// that should cost a relay round-trip on every launch.
	channelFeed       bool
	channelFeedLoad   bool // true while the fetch is in flight
	channelPosts      []core.ChannelPost
	channelFeedCursor int
	channelFeedErr    string

	// loadFailures counts consecutive track load/play failures. It is reset
	// to zero on every successful load. When it reaches the playlist length
	// the whole playlist has failed in a row (e.g. the music drive was
	// unmounted), which is treated as fatal rather than looping forever.
	loadFailures int

	// Track-load state. Each loadCurrentTrack call bumps loadSeq and cancels
	// the previous load, so skipping during a slow Subsonic download abandons
	// it rather than queueing behind it. loadMu keeps two loads from driving
	// the player at once; trackLoadedMsg results from older loads are dropped.
	loading    bool
	favourite  bool // current Subsonic track is a favourite; always false for local files
	loadSeq    int
	loadCancel context.CancelFunc
	loadMu     sync.Mutex

	// help shows the Zen keys card. It carries key discoverability so the
	// home hint can stay minimal; esc/? dismisses it, and any other key
	// dismisses it first and then processes normally.
	help bool
}


// Messages for the TUI
type tickMsg time.Time
type positionMsg time.Duration
type trackEndedMsg struct{}
type playErrorMsg error
type trackLoadedMsg struct {
	seq      int // loadSeq of the load that produced this; stale ones are ignored
	duration time.Duration
	artist   string
	title    string
	album    string
}

// channelsLoadedMsg carries the user's channel list, fetched in the background
// at startup. A failure is not reported: channels are an enhancement to [E],
// and a relay being down should not produce an error the user cannot act on.
type channelsLoadedMsg struct {
	channels []core.Channel
}

// channelFeedMsg carries the result of a channel feed fetch.
type channelFeedMsg struct {
	posts    []core.ChannelPost
	channels []core.Channel
	err      error
}

// channelTrackReadyMsg carries a channel post that has been downloaded,
// decrypted and reassembled to a local file, ready to play.
type channelTrackReadyMsg struct {
	path string
	err  error
}

// nostrPublishedMsg is sent after a Nostr publish attempt completes.
type nostrPublishedMsg struct {
	err       error  // nil on success
	action    string // "earmark" or "post" — drives the status display
	queued    bool   // true when the earmark was saved locally but not yet published (offline)
	duplicate bool   // true when the track was already earmarked
	shared    bool   // true when the track went to channels only (personal unticked)
}

// socialPublishMsg combines the results of a dual-post to Nostr and Bluesky.
// It is sent by publishToSocial after both platform calls complete.
type socialPublishMsg struct {
	nostrErr error // nil when Nostr succeeded or wasn't attempted
	bskyErr  error // nil when Bluesky succeeded or wasn't attempted
	nostrOK  bool  // true if Nostr was configured and succeeded
	bskyOK   bool  // true if Bluesky was configured and succeeded
}

// queueFlushedMsg is sent after a background queue-flush attempt completes.
type queueFlushedMsg struct {
	count int // number of earmarks successfully published from the queue
}

// cleanupMsg is sent after the startup old-earmark cleanup completes.
type cleanupMsg struct {
	removed int // number of earmarks older than core.EarmarkMaxAge that were purged
}

type trackDeletedMsg struct {
	err error
}

// favouriteMsg is sent after a favourite toggle on the Subsonic server.
type favouriteMsg struct {
	id        string
	favourite bool
	err       error
}

// NewPlayerModel creates a new player model.
func NewPlayerModel(playlist []string) *PlayerModel {
	return &PlayerModel{
		playlist:     playlist,
		currentIndex: 0,
		player:       NewAudioPlayer(),
		tickInterval: 100 * time.Millisecond, // Make tick interval configurable
	}
}

// channelPickerSelected counts the picker's selected targets: the personal
// list when ticked plus each ticked channel. Enter only confirms when this is
// non-zero — with nothing checked there is nowhere for the earmark to go.
func (m *PlayerModel) channelPickerSelected() int {
	n := 0
	if m.channelPersonal {
		n++
	}
	for _, c := range m.channels {
		if m.channelTargets[c.Descriptor.ID] {
			n++
		}
	}
	return n
}

// Init initializes the model
func (m *PlayerModel) Init() tea.Cmd {
	return tea.Batch(
		m.loadCurrentTrack(),
		m.tickCmd(),
		m.flushQueueCmd(),   // retry any earmarks that failed to publish last session
		m.cleanupCmd(),      // purge earmarks older than 30 days + their Blossom chunks
		m.loadChannelsCmd(), // so [E] can offer channel targets without blocking
	)
}

// Update handles messages and updates the model
func (m *PlayerModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height

	case tea.KeyMsg:
		// The Zen help card dismisses on esc/?; any other key closes it
		// first and then processes normally.
		if m.help {
			if msg.String() == "esc" || msg.String() == "?" {
				m.help = false
				return m, nil
			}
			m.help = false
		}
		// While the channel feed browser is up, consume all keystrokes.
		if m.channelFeed {
			switch msg.String() {
			case "esc", "ctrl+c", "c":
				m.channelFeed = false
				return m, nil
			case "up", "k":
				if m.channelFeedCursor > 0 {
					m.channelFeedCursor--
				}
				return m, nil
			case "down", "j":
				if m.channelFeedCursor < len(m.channelPosts)-1 {
					m.channelFeedCursor++
				}
				return m, nil
			case "enter":
				if m.channelFeedLoad || len(m.channelPosts) == 0 {
					return m, nil
				}
				post := m.channelPosts[m.channelFeedCursor]
				m.channelFeed = false
				m.nostrStatus = fmt.Sprintf("Fetching %s...", post.Earmark.Title)
				return m, m.playChannelPostCmd(post)
			}
			return m, nil
		}

		// While the channel-target overlay is up, consume all keystrokes.
		if m.channelPicker {
			switch msg.String() {
			case "esc", "ctrl+c":
				m.channelPicker = false
				return m, nil
			case "up", "k":
				if m.channelCursor > 0 {
					m.channelCursor--
				}
				return m, nil
			case "down", "j":
				if m.channelCursor < len(m.channels) {
					m.channelCursor++
				}
				return m, nil
			case " ":
				// Every row is a toggle: personal (row 0) and each channel.
				// The earmark goes to whatever is selected when enter is hit.
				if m.channelCursor == 0 {
					m.channelPersonal = !m.channelPersonal
				} else if m.channelCursor-1 < len(m.channels) {
					id := m.channels[m.channelCursor-1].Descriptor.ID
					m.channelTargets[id] = !m.channelTargets[id]
				}
				return m, nil
			case "enter":
				// At least one target must stay selected — with nothing
				// checked there is nowhere for the earmark to go, so stay
				// in the picker (the hint says as much).
				if m.channelPickerSelected() == 0 {
					return m, nil
				}
				var targets []string
				for _, c := range m.channels {
					if m.channelTargets[c.Descriptor.ID] {
						targets = append(targets, c.Descriptor.ID)
					}
				}
				m.channelPicker = false
				hexKey := resolveNostrKey()
				if hexKey == "" {
					return m, nil
				}
				switch {
				case m.channelPersonal && len(targets) > 0:
					m.nostrStatus = fmt.Sprintf("Nostr: earmarking and sharing to %d channel(s)...", len(targets))
				case len(targets) > 0:
					m.nostrStatus = fmt.Sprintf("Nostr: sharing to %d channel(s)...", len(targets))
				default:
					m.nostrStatus = "Nostr: saving earmark..."
				}
				return m, m.saveEarmarkCmd(hexKey, m.channelPersonal, targets)
			}
			return m, nil
		}


		// While in Nostr key-entry mode, consume all keystrokes for input.
		if m.nostrKeyEntry {
			switch msg.String() {
			case "esc":
				// Cancel key entry without saving.
				m.nostrKeyEntry = false
				m.nostrKeyBuffer = ""
				m.nostrStatus = "Nostr key entry cancelled."
			case "enter":
				// User submitted their key — validate, save, then perform the
				// action that triggered key entry ([E] earmark or [P] post).
				key := m.nostrKeyBuffer
				forPost := m.nostrKeyForPost
				m.nostrKeyEntry = false
				m.nostrKeyBuffer = ""
				m.nostrKeyForPost = false
				if forPost {
					m.nostrStatus = "Nostr: searching for links and posting..."
					return m, m.saveKeyAndPublish(key)
				}
				m.nostrStatus = "Nostr: saving earmark..."
				return m, m.saveKeyAndAddEarmark(key)
			case "backspace", "ctrl+h":
				if len(m.nostrKeyBuffer) > 0 {
					m.nostrKeyBuffer = m.nostrKeyBuffer[:len(m.nostrKeyBuffer)-1]
				}
			default:
				// Append printable characters only (ignore control sequences and null
				// bytes, which Windows terminals can inject during clipboard paste).
				if len(msg.Runes) == 1 && unicode.IsPrint(msg.Runes[0]) {
					m.nostrKeyBuffer += string(msg.Runes)
				}
			}
			return m, nil
		}

		switch msg.String() {
		case "?":
			m.help = true
			return m, nil

		case "q", "esc", "ctrl+c":
			m.player.Close()
			return m, tea.Quit

		case " ":
			// Toggle pause/play
			if m.playing {
				if m.paused {
					if err := m.player.Resume(); err != nil {
						m.err = err
						return m, nil
					}
					m.paused = false
					m.mpris.NotifyStateChanged(m)
					// Restart ticking when resuming
					return m, m.tickCmd()
				} else {
					m.player.Pause()
					m.paused = true
					m.mpris.NotifyStateChanged(m)
					// Stop ticking when paused (handled by tickMsg case)
				}
			}

		case "left":
			// Previous track
			m.player.Stop()
			m.currentIndex--
			if m.currentIndex < 0 {
				m.currentIndex = len(m.playlist) - 1 // Loop to last track
			}
			return m, m.loadCurrentTrack()

		case "right":
			// Next track
			m.player.Stop()
			m.currentIndex++
			if m.currentIndex >= len(m.playlist) {
				m.currentIndex = 0 // Loop back to first track
			}
			return m, m.loadCurrentTrack()

		case "e":
			// Add the current track to the private Nostr earmark list.
			// The earmark is written to the local queue first (guarantees no
			// data loss), then a Nostr publish is attempted in the background.
			if m.playing {
				hexKey := resolveNostrKey()
				if hexKey == "" {
					// No key configured — enter inline key-entry mode.
					m.nostrKeyEntry = true
					m.nostrKeyBuffer = ""
					m.nostrStatus = ""
					return m, nil
				}
				// With channels available, ask where this should go. The
				// overlay opens with personal selected, so the old one-key
				// flow is still [E] then enter; untick personal to share
				// to channels only.
				if len(m.channels) > 0 {
					m.channelPicker = true
					m.channelCursor = 0
					m.channelPersonal = true
					m.channelTargets = map[string]bool{}
					m.nostrStatus = ""
					return m, nil
				}
				m.nostrStatus = "Nostr: saving earmark..."
				return m, m.saveEarmarkCmd(hexKey, true, nil)
			}

		case "c":
			// Browse tracks other people have posted to your channels.
			if resolveNostrKey() == "" {
				m.nostrStatus = "Nostr: no key configured"
				return m, nil
			}
			m.channelFeed = true
			m.channelFeedLoad = true
			m.channelFeedCursor = 0
			m.channelFeedErr = ""
			return m, m.loadChannelFeedCmd()

		case "p":
			// Publish the current track as a public post. When both Nostr
			// and Bluesky are configured, both are fired in parallel.
			if m.playing {
				hexKey := resolveNostrKey()
				bskyHandle, bskyPassword := resolveBskyConfig()

				if hexKey == "" && bskyHandle == "" {
					// Neither platform configured — inline Nostr key entry.
					m.nostrKeyEntry = true
					m.nostrKeyBuffer = ""
					m.nostrKeyForPost = true
					m.nostrStatus = ""
					return m, nil
				}
				m.nostrStatus = "Posting..."
				return m, m.publishToSocial(hexKey, bskyHandle, bskyPassword, m.artist, m.title, m.album)
			}

		case "d":
			// Delete current track from disk and advance to next
			if m.playing && m.currentIndex < len(m.playlist) {
				// Subsonic has no delete API, and the server copy is not ours to remove.
				if _, remote := subsonicEntryID(m.playlist[m.currentIndex]); remote {
					m.nostrStatus = "Delete is not available for Subsonic tracks"
					return m, nil
				}
				return m, m.deleteCurrentTrack()
			}

		case "f":
			// Toggle the current Subsonic track as a favourite on the server.
			if m.playing && m.currentIndex < len(m.playlist) {
				id, remote := subsonicEntryID(m.playlist[m.currentIndex])
				if !remote || subsonicSession == nil {
					m.nostrStatus = "Favourites are only available for Sonic tracks"
					return m, nil
				}
				return m, m.toggleFavouriteCmd(id, !m.favourite)
			}
		}

	// ---- MPRIS2 command messages injected by MPRISService -------------------

	case mprisPlayMsg:
		if m.playing && m.paused {
			// Resume from pause.
			if err := m.player.Resume(); err != nil {
				m.err = err
				return m, nil
			}
			m.paused = false
			m.mpris.NotifyStateChanged(m)
			return m, m.tickCmd()
		} else if !m.playing && !m.loading {
			// Nothing loaded yet — load and play.
			return m, m.loadCurrentTrack()
		}

	case mprisPauseMsg:
		if m.playing && !m.paused {
			m.player.Pause()
			m.paused = true
			m.mpris.NotifyStateChanged(m)
		}

	case mprisPlayPauseMsg:
		if m.playing {
			if m.paused {
				if err := m.player.Resume(); err != nil {
					m.err = err
					return m, nil
				}
				m.paused = false
				m.mpris.NotifyStateChanged(m)
				return m, m.tickCmd()
			} else {
				m.player.Pause()
				m.paused = true
				m.mpris.NotifyStateChanged(m)
			}
		}

	case mprisStopMsg:
		if m.playing {
			m.player.Stop()
			m.playing = false
			m.paused = false
			m.mpris.NotifyStateChanged(m)
		}

	case mprisNextMsg:
		m.player.Stop()
		m.currentIndex++
		if m.currentIndex >= len(m.playlist) {
			m.currentIndex = 0
		}
		return m, m.loadCurrentTrack()

	case mprisPreviousMsg:
		m.player.Stop()
		m.currentIndex--
		if m.currentIndex < 0 {
			m.currentIndex = len(m.playlist) - 1
		}
		return m, m.loadCurrentTrack()

	case mprisSeekMsg:
		if m.playing {
			newPos := m.position + msg.offset
			if newPos < 0 {
				newPos = 0
			}
			if newPos > m.duration {
				newPos = m.duration
			}
			if err := m.player.Seek(newPos); err == nil {
				m.position = newPos
				m.mpris.EmitSeeked(m)
			}
		}

	case mprisSetPositionMsg:
		if m.playing {
			pos := msg.pos
			if pos < 0 {
				pos = 0
			}
			if pos > m.duration {
				pos = m.duration
			}
			if err := m.player.Seek(pos); err == nil {
				m.position = pos
				m.mpris.EmitSeeked(m)
			}
		}

	case tickMsg:
		// Get current position from player directly
		m.position = m.player.GetPosition()

		// Check if track ended using the new HasEnded method
		if m.playing && m.player.HasEnded() {
			return m, func() tea.Msg {
				return trackEndedMsg{}
			}
		}

		// Only continue ticking if we're actually playing
		if m.playing && !m.paused {
			return m, m.tickCmd()
		}

		return m, nil

	case trackEndedMsg:
		m.player.Stop()
		m.currentIndex++
		if m.currentIndex >= len(m.playlist) {
			m.currentIndex = 0 // Loop back to the first track
		}
		return m, m.loadCurrentTrack()

	case trackLoadedMsg:
		if msg.seq != m.loadSeq {
			return m, nil // superseded by a later skip
		}
		m.loading = false
		m.favourite = false
		if id, ok := subsonicEntryID(m.playlist[m.currentIndex]); ok && subsonicSession != nil {
			m.favourite = subsonicSession.IsFavourite(id)
		}
		m.loadFailures = 0 // A track loaded — reset the consecutive-failure guard.
		m.playing = true
		m.paused = false
		m.position = 0
		m.duration = msg.duration
		m.artist = msg.artist
		m.title = msg.title
		m.album = msg.album
		// Notify MPRIS clients that metadata and status have changed.
		m.mpris.NotifyStateChanged(m)
		m.mpris.EmitSeeked(m) // Position jumped to 0 on track change.
		// Start downloading the next Subsonic track now, so skipping to it
		// or reaching it naturally doesn't wait on the server.
		if len(m.playlist) > 1 {
			prefetchEntry(m.playlist[(m.currentIndex+1)%len(m.playlist)])
		}
		// Restart the tick cycle for position updates
		return m, m.tickCmd()

	case favouriteMsg:
		if msg.err != nil {
			m.nostrStatus = "Favourite failed: " + msg.err.Error()
			return m, nil
		}
		// Only reflect the change if the user hasn't moved on to another track.
		if id, ok := subsonicEntryID(m.playlist[m.currentIndex]); ok && id == msg.id {
			m.favourite = msg.favourite
		}
		if msg.favourite {
			m.nostrStatus = "♥ Added to favourites"
		} else {
			m.nostrStatus = "Removed from favourites"
		}
		return m, nil

	case trackDeletedMsg:
		if msg.err != nil {
			m.err = msg.err
			return m, nil
		}
		// Playlist was already updated in deleteCurrentTrack; load the next track.
		// currentIndex was adjusted to stay in bounds.
		if len(m.playlist) == 0 {
			return m, tea.Quit
		}
		return m, m.loadCurrentTrack()

	case channelsLoadedMsg:
		m.channels = msg.channels

	case channelFeedMsg:
		m.channelFeedLoad = false
		if msg.err != nil {
			m.channelFeedErr = msg.err.Error()
		} else {
			m.channelPosts = msg.posts
			m.channels = msg.channels
			if m.channelFeedCursor >= len(m.channelPosts) {
				m.channelFeedCursor = 0
			}
		}

	case channelTrackReadyMsg:
		if msg.err != nil {
			m.nostrStatus = "Could not fetch that track: " + msg.err.Error()
			return m, nil
		}
		// Slot the fetched file in after the current track and jump to it, so
		// the surrounding playlist order is left intact.
		at := m.currentIndex + 1
		if at > len(m.playlist) {
			at = len(m.playlist)
		}
		m.playlist = append(m.playlist[:at], append([]string{msg.path}, m.playlist[at:]...)...)
		m.currentIndex = at
		m.nostrStatus = ""
		return m, m.loadCurrentTrack()

	case nostrPublishedMsg:
		if msg.err != nil {
			m.nostrStatus = fmt.Sprintf("Nostr: %s failed — %v", msg.action, msg.err)
		} else if msg.action == "post" {
			m.nostrStatus = "Nostr: posted!"
		} else if msg.duplicate {
			m.nostrStatus = "Nostr: already earmarked"
		} else if msg.queued {
			m.nostrStatus = "Nostr: earmark queued (offline — will sync later)"
		} else if msg.shared {
			m.nostrStatus = "Nostr: shared to channel(s)!"
		} else {
			m.nostrStatus = "Nostr: earmark saved!"
		}
		return m, nil

	case socialPublishMsg:
		m.nostrStatus = renderSocialStatus(msg)
		return m, nil

	case queueFlushedMsg:
		if msg.count > 0 {
			m.nostrStatus = fmt.Sprintf("Nostr: synced %d queued earmark(s)", msg.count)
		}
		return m, nil


	case cleanupMsg:
		if msg.removed > 0 {
			m.nostrStatus = fmt.Sprintf("Nostr: removed %d expired earmark(s)", msg.removed)
		}
		return m, nil

	case playErrorMsg:
		// A track failed to load or play. Rather than halting the whole
		// session, record the failure to ~/derpy-errors.log and skip to the
		// next track so playback continues. If the failures span the entire
		// playlist without a single success in between (e.g. the music drive
		// was unmounted), treat it as fatal so we don't spin forever.
		if logErr := logTrackError(error(msg)); logErr != nil {
			m.nostrStatus = fmt.Sprintf("Skipped a track (log write failed: %v)", logErr)
		} else {
			m.nostrStatus = fmt.Sprintf("Skipped unreadable track — see %s", errorLogPath())
		}

		m.loadFailures++
		if m.loadFailures >= len(m.playlist) {
			m.err = fmt.Errorf("no playable tracks: %d consecutive load failures (see %s)", m.loadFailures, errorLogPath())
			return m, nil
		}

		m.player.Stop()
		m.currentIndex++
		if m.currentIndex >= len(m.playlist) {
			m.currentIndex = 0 // Loop back to the first track
		}
		return m, m.loadCurrentTrack()

	case positionMsg:
		m.position = time.Duration(msg)
	}

	return m, nil
}


// renderProgressBar renders a progress bar
// renderControls returns the controls hint string, omitting [E], [C] and [P]
// when no Nostr key is available from any source (env, config, or inline).
// [P] is shown when either Nostr or Bluesky is configured; [E] and [C] need
// Nostr specifically, since earmarks and channels are Nostr constructs.
func renderControls(hasNostr bool, hasBluesky bool) string {
	if hasNostr {
		return "← prev  → next  space pause  e earmark  c channels  p post  d delete  esc quit"
	}
	if hasBluesky {
		return "← prev  → next  space pause  p post  d delete  esc quit"
	}
	return "← prev  → next  space pause  d delete  esc quit"
}

func (m *PlayerModel) renderProgressBar(width int) string {
	if m.duration == 0 {
		return strings.Repeat("─", width)
	}

	progress := float64(m.position) / float64(m.duration)
	filled := int(progress * float64(width))

	if filled > width {
		filled = width
	}

	bar := strings.Repeat("█", filled) + strings.Repeat("─", width-filled)
	return fmt.Sprintf("[%s]", bar)
}

// tickCmd returns a command to send tick messages
func (m *PlayerModel) tickCmd() tea.Cmd {
	return tea.Tick(m.tickInterval, func(t time.Time) tea.Msg {
		return tickMsg(t)
	})
}

// loadCurrentTrack loads and plays the current track.
//
// It runs on the Update goroutine, so it also resets the display: the old
// track's details are cleared and m.loading is set until trackLoadedMsg
// arrives, which is what lets a slow Subsonic download show a waiting
// message instead of the previous song.
func (m *PlayerModel) loadCurrentTrack() tea.Cmd {
	if m.currentIndex >= len(m.playlist) {
		return nil
	}
	track := m.playlist[m.currentIndex]

	if m.loadCancel != nil {
		m.loadCancel()
	}
	ctx, cancel := context.WithCancel(context.Background())
	m.loadCancel = cancel
	m.loadSeq++
	seq := m.loadSeq

	m.loading = true
	m.playing = false
	m.paused = false
	m.position = 0
	m.duration = 0
	m.artist, m.title, m.album = "", "", ""

	return func() tea.Msg {
		m.loadMu.Lock()
		defer m.loadMu.Unlock()
		if ctx.Err() != nil {
			return nil // skipped before this load started
		}

		// Load the track. Include the track path so a fatal decode/load
		// error tells the user which file was playing when it crashed.
		if err := m.player.LoadTrackContext(ctx, track); err != nil {
			if ctx.Err() != nil {
				return nil // skipped mid-download; not a failure
			}
			return playErrorMsg(fmt.Errorf("failed to load track %q: %w", track, err))
		}

		// Start playing
		if err := m.player.Play(); err != nil {
			return playErrorMsg(fmt.Errorf("failed to play track %q: %w", track, err))
		}

		return trackLoadedMsg{
			seq:      seq,
			duration: m.player.GetDuration(),
			artist:   m.player.GetArtist(),
			title:    m.player.GetTitle(),
			album:    m.player.GetAlbum(),
		}
	}
}

// publishToNostr returns a Bubble Tea command that searches for listen links
// and publishes a public kind-1 Nostr post about the current track.
func (m *PlayerModel) publishToNostr(hexKey string) tea.Cmd {
	artist, title, album := m.artist, m.title, m.album
	return func() tea.Msg {
		err := PublishNostrTrackNote(hexKey, artist, title, album)
		return nostrPublishedMsg{action: "post", err: err}
	}
}

// publishToSocial fires Nostr and Bluesky post attempts in parallel, collecting
// results into a single socialPublishMsg. Each platform is skipped when its
// credentials are empty (corresponding *_OK fields will be false). The listen
// link search is done once and shared, saving a duplicate HTTP round-trip.
func (m *PlayerModel) publishToSocial(hexKey, bskyHandle, bskyPassword, artist, title, album string) tea.Cmd {
	return func() tea.Msg {
		// Search for a listen link once — shared between platforms.
		link := FindBestLink(artist, title, album)

		result := socialPublishMsg{}

		// Run both platforms in parallel when configured.
		done := make(chan struct{}, 2)

		if hexKey != "" {
			go func() {
				result.nostrErr = PublishNostrTrackNote(hexKey, artist, title, album)
				result.nostrOK = result.nostrErr == nil
				done <- struct{}{}
			}()
		} else {
			done <- struct{}{}
		}

		if bskyHandle != "" {
			go func() {
				result.bskyErr = PublishBskyPost(bskyHandle, bskyPassword, artist, title, link)
				result.bskyOK = result.bskyErr == nil
				done <- struct{}{}
			}()
		} else {
			done <- struct{}{}
		}

		// Wait for both goroutines to finish.
		<-done
		<-done

		return result
	}
}

// renderSocialStatus builds the status-line text from a socialPublishMsg.
func renderSocialStatus(msg socialPublishMsg) string {
	var parts []string

	switch {
	case msg.nostrOK && msg.bskyOK:
		return "Posted to Bluesky + Nostr!"
	case msg.nostrOK:
		parts = append(parts, "Nostr: posted!")
	case msg.nostrErr != nil:
		parts = append(parts, fmt.Sprintf("Nostr: failed — %v", msg.nostrErr))
	}

	switch {
	case msg.bskyOK:
		parts = append(parts, "Bluesky: posted!")
	case msg.bskyErr != nil:
		parts = append(parts, fmt.Sprintf("Bluesky: failed — %v", msg.bskyErr))
	}

	if len(parts) == 0 {
		return "Post: nothing to do"
	}
	return strings.Join(parts, "  ")
}

// saveEarmarkCmd returns a Bubble Tea command that earmarks the current track.
//
// keepPersonal controls whether the track lands in the personal Nostr list;
// channelTargets names the channels it is shared to. At least one of the two
// is always set — the picker guarantees it.
//
// Correct sequencing — chunk identities (SHA-256s) must be known before the
// earmark is published so the manifest is complete from the start:
//
//  1. Duplicate check (local queue, personal keeps only)
//  2. Encrypt file into chunks — all SHA-256s now known, no network yet
//  3. Write earmark WITH manifest to local queue (data safety, personal only)
//  4. Upload encrypted chunks to Blossom servers — fills in server lists
//  5. Publish earmark WITH complete manifest to Nostr in one shot (personal)
//  6. Remove from local queue on success (personal only)
//  7. Share to the picked channels (both modes — members download the same
//     chunks, so the upload in step 4 always runs)
//
// If offline at step 4/5, a personal earmark (with manifest) stays in the
// queue and will be synced the next time the app starts with connectivity.
// A channel-only share has no queue to fall back on, so step-4 failure is a
// hard error there.
func (m *PlayerModel) saveEarmarkCmd(hexKey string, keepPersonal bool, channelTargets []string) tea.Cmd {
	artist, title, album := m.artist, m.title, m.album
	path := m.playlist[m.currentIndex]
	// The file to encrypt. For Subsonic tracks this is the downloaded copy;
	// path stays the "subsonic:" entry so the earmark never names a temp file.
	local := m.player.LocalPath()

	return func() tea.Msg {
		// Step 1: duplicate check before any work. The local queue is the
		// personal outbox, so this only applies when keeping personally —
		// channel-only shares leave no local record to dedupe against.
		if keepPersonal {
			existing, _ := LoadQueue()
			stub := core.Earmark{Path: path, Artist: artist, Title: title, Album: album}
			if core.IsDuplicateEarmark(existing, stub) {
				return nostrPublishedMsg{action: "earmark", duplicate: true}
			}
		}

		// Step 2: encrypt locally — gives us all chunk SHA-256s immediately.
		prepared, manifest, err := core.PrepareUpload(local)
		if err != nil {
			return nostrPublishedMsg{action: "earmark", err: fmt.Errorf("could not encrypt file: %w", err)}
		}

		e := core.Earmark{
			Artist:    artist,
			Album:     album,
			Title:     title,
			Path:      path,
			Timestamp: time.Now().Unix(),
			Blossom:   manifest,
		}

		// Step 3: when keeping personally, persist the earmark with manifest
		// to the local queue first (data safety).
		if keepPersonal {
			if err := AppendToQueue(e); err != nil {
				return nostrPublishedMsg{action: "earmark", err: fmt.Errorf("could not save to local queue: %w", err)}
			}
		}

		// Step 4: upload chunks — fills in confirmed server lists.
		servers, err := core.ResolveBlossomServers(hexKey)
		if err != nil || len(servers) == 0 {
			servers = core.BlossomServers()
		}
		uploadCtx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
		defer cancel()
		if err := core.UploadPrepared(uploadCtx, hexKey, prepared, manifest, servers, nil); err != nil {
			if !keepPersonal {
				// Nothing was queued, so report the failure outright.
				return nostrPublishedMsg{action: "earmark", err: fmt.Errorf("could not upload file: %w", err)}
			}
			// Upload failed — earmark is in queue without server lists.
			// It will be retried on next startup via FlushQueue.
			return nostrPublishedMsg{action: "earmark", queued: true}
		}

		if keepPersonal {
			// Step 5: publish earmark with complete manifest to Nostr.
			if err := core.AddEarmark(hexKey, e); err != nil {
				return nostrPublishedMsg{action: "earmark", queued: true}
			}

			// Step 6: all done — remove from outbox.
			_ = RemoveFromQueue(e)
		}

		// Step 7: share to any channels the user picked. On a personal keep
		// this is a separate concern from the earmark, which is already
		// safely published — a channel failure must not read as a failed
		// earmark.
		for _, chanID := range channelTargets {
			postCtx, postCancel := context.WithTimeout(context.Background(), 90*time.Second)
			err := core.PostToChannel(postCtx, hexKey, chanID, e)
			postCancel()
			if err != nil {
				if keepPersonal {
					return nostrPublishedMsg{
						action: "earmark",
						err:    fmt.Errorf("earmarked, but could not share to channel: %w", err),
					}
				}
				return nostrPublishedMsg{
					action: "earmark",
					err:    fmt.Errorf("could not share to channel: %w", err),
				}
			}
		}
		return nostrPublishedMsg{action: "earmark", shared: !keepPersonal}
	}
}

// loadChannelFeedCmd syncs channel messages and returns the live posts.
func (m *PlayerModel) loadChannelFeedCmd() tea.Cmd {
	return func() tea.Msg {
		hexKey := resolveNostrKey()
		if hexKey == "" {
			return channelFeedMsg{err: fmt.Errorf("no Nostr key configured")}
		}
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		posts, st, err := core.SyncChannels(ctx, hexKey)
		if err != nil {
			return channelFeedMsg{err: err}
		}
		return channelFeedMsg{posts: posts, channels: st.Channels}
	}
}

// playChannelPostCmd downloads, decrypts and reassembles a posted track so it
// can be played locally. Nothing is added to the user's own earmark list —
// listening is not keeping. Use the earmark CLI's "channel keep" to adopt it.
func (m *PlayerModel) playChannelPostCmd(post core.ChannelPost) tea.Cmd {
	return func() tea.Msg {
		hexKey := resolveNostrKey()
		if hexKey == "" {
			return channelTrackReadyMsg{err: fmt.Errorf("no Nostr key configured")}
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
		defer cancel()
		path, err := core.DownloadAndReassemble(ctx, post.Earmark.Blossom, hexKey, nil)
		if err != nil {
			return channelTrackReadyMsg{err: err}
		}
		return channelTrackReadyMsg{path: path}
	}
}

// loadChannelsCmd fetches the user's channel list in the background at startup.
// Errors are swallowed deliberately: channels only add targets to [E], and a
// relay being down is not something the user can act on mid-session.
func (m *PlayerModel) loadChannelsCmd() tea.Cmd {
	return func() tea.Msg {
		hexKey := resolveNostrKey()
		if hexKey == "" {
			return channelsLoadedMsg{}
		}
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		st, err := core.LoadChannelState(ctx, hexKey)
		if err != nil {
			return channelsLoadedMsg{}
		}
		return channelsLoadedMsg{channels: st.Channels}
	}
}

// saveKeyAndPublish validates the key the user typed inline, persists it, and
// then publishes a public Nostr post about the current track.
func (m *PlayerModel) saveKeyAndPublish(rawKey string) tea.Cmd {
	artist, title, album := m.artist, m.title, m.album
	return func() tea.Msg {
		rawKey = strings.TrimSpace(rawKey)
		if rawKey == "" {
			return nostrPublishedMsg{action: "post", err: fmt.Errorf("no key entered")}
		}

		hexKey, err := core.ResolvePrivateKey(rawKey)
		if err != nil {
			return nostrPublishedMsg{action: "post", err: fmt.Errorf("invalid key: %w", err)}
		}

		cfg, loadErr := LoadConfig()
		if loadErr != nil {
			cfg = &Config{}
		}
		cfg.NostrPrivateKey = hexKey
		_ = SaveConfig(cfg)

		err = PublishNostrTrackNote(hexKey, artist, title, album)
		return nostrPublishedMsg{action: "post", err: err}
	}
}

// saveKeyAndAddEarmark validates the key the user typed inline, persists it,
// and then runs the same encrypt → upload → earmark sequence as saveEarmarkCmd.
func (m *PlayerModel) saveKeyAndAddEarmark(rawKey string) tea.Cmd {
	artist, title, album := m.artist, m.title, m.album
	path := m.playlist[m.currentIndex]
	local := m.player.LocalPath()
	return func() tea.Msg {
		rawKey = strings.TrimSpace(rawKey)
		if rawKey == "" {
			return nostrPublishedMsg{action: "earmark", err: fmt.Errorf("no key entered")}
		}

		hexKey, err := core.ResolvePrivateKey(rawKey)
		if err != nil {
			return nostrPublishedMsg{action: "earmark", err: fmt.Errorf("invalid key: %w", err)}
		}

		cfg, loadErr := LoadConfig()
		if loadErr != nil {
			cfg = &Config{}
		}
		cfg.NostrPrivateKey = hexKey
		_ = SaveConfig(cfg)

		// Duplicate check.
		existingQ, _ := LoadQueue()
		stub := core.Earmark{Path: path, Artist: artist, Title: title, Album: album}
		if core.IsDuplicateEarmark(existingQ, stub) {
			return nostrPublishedMsg{action: "earmark", duplicate: true}
		}

		// Encrypt → upload → earmark (same sequence as saveEarmarkCmd).
		prepared, manifest, err := core.PrepareUpload(local)
		if err != nil {
			return nostrPublishedMsg{action: "earmark", err: fmt.Errorf("could not encrypt file: %w", err)}
		}

		e := core.Earmark{
			Artist:    artist,
			Album:     album,
			Title:     title,
			Path:      path,
			Timestamp: time.Now().Unix(),
			Blossom:   manifest,
		}
		if err := AppendToQueue(e); err != nil {
			return nostrPublishedMsg{action: "earmark", err: fmt.Errorf("could not save to local queue: %w", err)}
		}

		servers, sErr := core.ResolveBlossomServers(hexKey)
		if sErr != nil || len(servers) == 0 {
			servers = core.BlossomServers()
		}
		uploadCtx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
		defer cancel()
		if err := core.UploadPrepared(uploadCtx, hexKey, prepared, manifest, servers, nil); err != nil {
			return nostrPublishedMsg{action: "earmark", queued: true}
		}

		if err = core.AddEarmark(hexKey, e); err != nil {
			return nostrPublishedMsg{action: "earmark", queued: true}
		}

		_ = RemoveFromQueue(e)
		return nostrPublishedMsg{action: "earmark"}
	}
}

// flushQueueCmd returns a Bubble Tea command that attempts to publish any
// earmarks left in the local offline queue from previous sessions.
// It is run once at startup so earmarks queued while offline are synced as
// soon as the app starts with connectivity.
func (m *PlayerModel) flushQueueCmd() tea.Cmd {
	return func() tea.Msg {
		hexKey := resolveNostrKey()
		if hexKey == "" {
			return queueFlushedMsg{}
		}
		count, _ := FlushQueue(hexKey)
		return queueFlushedMsg{count: count}
	}
}

// cleanupCmd runs at startup to purge earmarks older than core.EarmarkMaxAge (30
// days) and delete their Blossom chunks from all associated servers.
func (m *PlayerModel) cleanupCmd() tea.Cmd {
	return func() tea.Msg {
		hexKey := resolveNostrKey()
		if hexKey == "" {
			return cleanupMsg{}
		}
		removed, _ := core.CleanupOldEarmarks(hexKey)
		return cleanupMsg{removed: removed}
	}
}

// deleteCurrentTrack stops playback, removes the file from disk, removes it
// from the playlist, and signals the TUI to load the next track.
func (m *PlayerModel) deleteCurrentTrack() tea.Cmd {
	// Capture path and index before the goroutine runs
	filePath := m.playlist[m.currentIndex]
	idx := m.currentIndex

	// Stop playback immediately (synchronous)
	m.player.Stop()
	m.playing = false

	// Remove the entry from the playlist
	m.playlist = append(m.playlist[:idx], m.playlist[idx+1:]...)

	// Clamp index so it points to the next track (or wraps around)
	if len(m.playlist) > 0 && m.currentIndex >= len(m.playlist) {
		m.currentIndex = 0
	}

	return func() tea.Msg {
		if err := os.Remove(filePath); err != nil {
			return trackDeletedMsg{err: fmt.Errorf("delete failed: %w", err)}
		}
		return trackDeletedMsg{}
	}
}

// toggleFavouriteCmd sets a Subsonic track's favourite state on the server.
func (m *PlayerModel) toggleFavouriteCmd(id string, favourite bool) tea.Cmd {
	client := subsonicSession
	return func() tea.Msg {
		err := client.SetFavourite(id, favourite)
		return favouriteMsg{id: id, favourite: favourite, err: err}
	}
}

// formatDuration formats a time.Duration as mm:ss
func formatDuration(d time.Duration) string {
	minutes := int(d.Minutes())
	seconds := int(d.Seconds()) % 60
	return fmt.Sprintf("%02d:%02d", minutes, seconds)
}
