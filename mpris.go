//go:build linux

package main

// mpris.go implements the MPRIS2 D-Bus interface for derpy.
//
// MPRIS2 spec: https://specifications.freedesktop.org/mpris-spec/latest/
//
// Two D-Bus interfaces are exposed on the object path /org/mpris/MediaPlayer2:
//   - org.mpris.MediaPlayer2          (identity / capabilities)
//   - org.mpris.MediaPlayer2.Player   (playback control and state)
//
// Commands from external clients (playerctl, Waybar, etc.) are forwarded into
// the Bubble Tea event loop via tea.Program.Send(), so they are handled safely
// on the same goroutine as all other model updates.

import (
	"fmt"
	"log"
	"net/url"
	"strings"
	"sync"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/godbus/dbus/v5"
	"github.com/godbus/dbus/v5/introspect"
)

// ---- D-Bus introspection XML ------------------------------------------------

const mprisIntrospectXML = `
<node>
  <interface name="org.mpris.MediaPlayer2">
    <property name="CanQuit"             type="b"  access="read"/>
    <property name="CanRaise"            type="b"  access="read"/>
    <property name="HasTrackList"        type="b"  access="read"/>
    <property name="Identity"            type="s"  access="read"/>
    <property name="SupportedMimeTypes"  type="as" access="read"/>
    <property name="SupportedUriSchemes" type="as" access="read"/>
  </interface>
  <interface name="org.mpris.MediaPlayer2.Player">
    <property name="PlaybackStatus" type="s"     access="read"/>
    <property name="LoopStatus"     type="s"     access="readwrite"/>
    <property name="Rate"           type="d"     access="readwrite"/>
    <property name="Shuffle"        type="b"     access="readwrite"/>
    <property name="Metadata"       type="a{sv}" access="read"/>
    <property name="Volume"         type="d"     access="readwrite"/>
    <property name="Position"       type="x"     access="read"/>
    <property name="MinimumRate"    type="d"     access="read"/>
    <property name="MaximumRate"    type="d"     access="read"/>
    <property name="CanGoNext"      type="b"     access="read"/>
    <property name="CanGoPrevious"  type="b"     access="read"/>
    <property name="CanPlay"        type="b"     access="read"/>
    <property name="CanPause"       type="b"     access="read"/>
    <property name="CanSeek"        type="b"     access="read"/>
    <property name="CanControl"     type="b"     access="read"/>
    <method name="Next"/>
    <method name="Previous"/>
    <method name="Pause"/>
    <method name="PlayPause"/>
    <method name="Stop"/>
    <method name="Play"/>
    <method name="Seek">
      <arg direction="in"  type="x" name="Offset"/>
    </method>
    <method name="SetPosition">
      <arg direction="in"  type="o" name="TrackId"/>
      <arg direction="in"  type="x" name="Position"/>
    </method>
    <method name="OpenUri">
      <arg direction="in"  type="s" name="Uri"/>
    </method>
    <signal name="Seeked">
      <arg type="x" name="Position"/>
    </signal>
  </interface>
  ` + introspect.IntrospectDataString + `
</node>`

// ---- MPRISService -----------------------------------------------------------

// mprisStateSnapshot holds a point-in-time copy of the player state fields
// that D-Bus goroutines need to read.  The snapshot is updated on the Bubble
// Tea goroutine (single writer) and read from D-Bus goroutines (multiple
// readers), protected by mu.  This eliminates the data race between the
// Bubble Tea Update loop writing PlayerModel fields and godbus goroutines
// reading them concurrently.
type mprisStateSnapshot struct {
	playing      bool
	paused       bool
	artist       string
	title        string
	album        string
	position     time.Duration
	duration     time.Duration
	currentIndex int
	trackPath    string // current file path; used as title fallback
}

// MPRISService registers derpy on the session D-Bus and handles MPRIS2
// method calls by forwarding them as messages into the Bubble Tea program.
type MPRISService struct {
	conn    *dbus.Conn
	program *tea.Program

	// mu protects snap from concurrent reads (D-Bus goroutines) and writes
	// (Bubble Tea goroutine via NotifyStateChanged / EmitSeeked).
	mu   sync.RWMutex
	snap mprisStateSnapshot
}

// NewMPRISService creates and registers the MPRIS2 service.  If the session
// D-Bus is unavailable it returns (nil, nil) so the caller can continue
// without MPRIS support.
func NewMPRISService(program *tea.Program) (*MPRISService, error) {
	conn, err := dbus.SessionBus()
	if err != nil {
		log.Printf("MPRIS: could not connect to session D-Bus, continuing without MPRIS support: %v", err)
		return nil, nil //nolint:nilerr // graceful degradation
	}

	svc := &MPRISService{
		conn:    conn,
		program: program,
	}

	// Request the well-known service name.
	reply, err := conn.RequestName(
		"org.mpris.MediaPlayer2.derpy",
		dbus.NameFlagDoNotQueue,
	)
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("MPRIS: RequestName failed: %w", err)
	}
	if reply != dbus.RequestNameReplyPrimaryOwner {
		conn.Close()
		return nil, fmt.Errorf("MPRIS: name already taken (reply=%d)", reply)
	}

	// Export this object for method calls.  godbus dispatches incoming D-Bus
	// method calls to matching exported interfaces; we implement both the
	// org.freedesktop.DBus.Properties interface (for property reads) and the
	// actual MPRIS2 player interface (for playback methods) on the same type.
	objectPath := dbus.ObjectPath("/org/mpris/MediaPlayer2")

	// org.mpris.MediaPlayer2 — identity/capability properties (no methods need renaming)
	conn.Export(svc, objectPath, "org.mpris.MediaPlayer2")

	// org.mpris.MediaPlayer2.Player — playback control methods.
	// We map the D-Bus "Seek" method to the Go method "SeekByOffset" to avoid
	// a false-positive from go vet's stdmethods check, which expects any method
	// named "Seek" to match the io.Seeker signature.
	playerMethodMap := map[string]string{
		"Seek": "SeekByOffset",
	}
	conn.ExportWithMap(svc, playerMethodMap, objectPath, "org.mpris.MediaPlayer2.Player")

	conn.Export(svc, objectPath, "org.freedesktop.DBus.Properties")

	// Export introspection so that tools like d-feet and playerctl can
	// discover the interfaces without trial-and-error.
	conn.Export(introspect.Introspectable(mprisIntrospectXML), objectPath,
		"org.freedesktop.DBus.Introspectable")

	return svc, nil
}

// Close releases the D-Bus connection.
func (s *MPRISService) Close() {
	if s != nil && s.conn != nil {
		s.conn.Close()
	}
}

// ---- org.freedesktop.DBus.Properties ----------------------------------------
//
// playerctl and most MPRIS clients use the standard Properties interface rather
// than per-interface property accessors, so we implement Get/GetAll/Set here.

// Get returns a single property value.  iface is one of the two MPRIS2
// interface names; prop is the property name.
func (s *MPRISService) Get(iface, prop string) (dbus.Variant, *dbus.Error) {
	props := s.allProperties(iface)
	if props == nil {
		return dbus.Variant{}, dbus.NewError(
			"org.freedesktop.DBus.Error.UnknownInterface",
			[]interface{}{fmt.Sprintf("unknown interface %q", iface)},
		)
	}
	v, ok := props[prop]
	if !ok {
		return dbus.Variant{}, dbus.NewError(
			"org.freedesktop.DBus.Error.InvalidArgs",
			[]interface{}{fmt.Sprintf("unknown property %q on %q", prop, iface)},
		)
	}
	return v, nil
}

// GetAll returns all properties for the given interface as a map.
func (s *MPRISService) GetAll(iface string) (map[string]dbus.Variant, *dbus.Error) {
	props := s.allProperties(iface)
	if props == nil {
		return nil, dbus.NewError(
			"org.freedesktop.DBus.Error.UnknownInterface",
			[]interface{}{fmt.Sprintf("unknown interface %q", iface)},
		)
	}
	return props, nil
}

// Set is a no-op for read-only properties; writable properties (Rate, Volume,
// LoopStatus, Shuffle) are accepted silently so clients do not error.
func (s *MPRISService) Set(iface, prop string, value dbus.Variant) *dbus.Error {
	// All writable properties are acknowledged but not applied — derpy does
	// not yet support runtime rate/volume/loop changes via MPRIS.
	return nil
}

// allProperties builds the property map for the given interface by reading
// from the snapshot (safe for D-Bus goroutines).  Returns nil for unknown
// interfaces.
func (s *MPRISService) allProperties(iface string) map[string]dbus.Variant {
	switch iface {
	case "org.mpris.MediaPlayer2":
		return map[string]dbus.Variant{
			"CanQuit":             dbus.MakeVariant(false),
			"CanRaise":            dbus.MakeVariant(false),
			"HasTrackList":        dbus.MakeVariant(false),
			"Identity":            dbus.MakeVariant("derpy"),
			"SupportedMimeTypes":  dbus.MakeVariant([]string{"audio/mpeg", "audio/flac", "audio/ogg", "audio/wav"}),
			"SupportedUriSchemes": dbus.MakeVariant([]string{"file"}),
		}
	case "org.mpris.MediaPlayer2.Player":
		// Take a read lock for the whole property build so we see a
		// consistent snapshot across all fields.
		s.mu.RLock()
		snap := s.snap
		s.mu.RUnlock()

		return map[string]dbus.Variant{
			"PlaybackStatus": dbus.MakeVariant(playbackStatusFromSnap(snap)),
			"LoopStatus":     dbus.MakeVariant("None"),
			"Rate":           dbus.MakeVariant(float64(1.0)),
			"Shuffle":        dbus.MakeVariant(false),
			"Metadata":       dbus.MakeVariant(metadataFromSnap(snap)),
			"Volume":         dbus.MakeVariant(float64(1.0)),
			"Position":       dbus.MakeVariant(snap.position.Microseconds()),
			"MinimumRate":    dbus.MakeVariant(float64(1.0)),
			"MaximumRate":    dbus.MakeVariant(float64(1.0)),
			"CanGoNext":      dbus.MakeVariant(true),
			"CanGoPrevious":  dbus.MakeVariant(true),
			"CanPlay":        dbus.MakeVariant(true),
			"CanPause":       dbus.MakeVariant(true),
			"CanSeek":        dbus.MakeVariant(true),
			"CanControl":     dbus.MakeVariant(true),
		}
	}
	return nil
}

// ---- org.mpris.MediaPlayer2 -------------------------------------------------

// Raise is a no-op (terminal UI has no window to raise).
func (s *MPRISService) Raise() *dbus.Error { return nil }

// Quit is a no-op; derpy is controlled via the TUI keyboard.
func (s *MPRISService) Quit() *dbus.Error { return nil }

// ---- org.mpris.MediaPlayer2.Player methods ----------------------------------

// Play starts playback.
func (s *MPRISService) Play() *dbus.Error {
	s.program.Send(mprisPlayMsg{})
	return nil
}

// Pause pauses playback.
func (s *MPRISService) Pause() *dbus.Error {
	s.program.Send(mprisPauseMsg{})
	return nil
}

// PlayPause toggles play/pause.
func (s *MPRISService) PlayPause() *dbus.Error {
	s.program.Send(mprisPlayPauseMsg{})
	return nil
}

// Stop stops playback.
func (s *MPRISService) Stop() *dbus.Error {
	s.program.Send(mprisStopMsg{})
	return nil
}

// Next advances to the next track.
func (s *MPRISService) Next() *dbus.Error {
	s.program.Send(mprisNextMsg{})
	return nil
}

// Previous goes to the previous track.
func (s *MPRISService) Previous() *dbus.Error {
	s.program.Send(mprisPreviousMsg{})
	return nil
}

// SeekByOffset moves the playback position by offsetMicros microseconds relative
// to the current position.  A negative offset seeks backward.
//
// This method is exported to D-Bus under the name "Seek" via ExportWithMap to
// avoid a go vet stdmethods false positive (go vet expects any Go method named
// "Seek" to match the io.Seeker signature).
func (s *MPRISService) SeekByOffset(offsetMicros int64) *dbus.Error {
	offset := time.Duration(offsetMicros) * time.Microsecond
	s.program.Send(mprisSeekMsg{offset: offset})
	return nil
}

// SetPosition seeks to an absolute position (in microseconds).
// trackId is the D-Bus object path of the track; if it does not match the
// currently playing track the seek is ignored per the MPRIS2 spec.
func (s *MPRISService) SetPosition(trackId dbus.ObjectPath, positionMicros int64) *dbus.Error {
	// Validate that the trackId matches the currently playing track.
	s.mu.RLock()
	currentIndex := s.snap.currentIndex
	s.mu.RUnlock()
	expected := dbus.ObjectPath(fmt.Sprintf("/org/derpy/track/%d", currentIndex))
	if trackId != expected {
		// Per spec: "If the TrackId argument is not the same as the current
		// TrackId, this request should be ignored."
		return nil
	}
	pos := time.Duration(positionMicros) * time.Microsecond
	s.program.Send(mprisSetPositionMsg{pos: pos})
	return nil
}

// OpenUri is a no-op; derpy plays from a directory, not individual URIs.
func (s *MPRISService) OpenUri(uri string) *dbus.Error { return nil }

// ---- State change notifications ---------------------------------------------

// NotifyStateChanged updates the internal snapshot from the current model
// state (must be called on the Bubble Tea goroutine) and emits a
// PropertiesChanged D-Bus signal so that clients (Waybar, playerctl, etc.)
// pick up the new status and metadata immediately without polling.
//
// Call this from the Bubble Tea Update() method after any state transition
// (track load, pause, resume, stop, next, previous).
func (s *MPRISService) NotifyStateChanged(m *PlayerModel) {
	if s == nil || s.conn == nil {
		return
	}

	// Build a safe snapshot of the model state.  All fields are copied under
	// the write lock so D-Bus goroutines always see a consistent view.
	trackPath := ""
	if m.currentIndex < len(m.playlist) {
		trackPath = m.playlist[m.currentIndex]
	}

	s.mu.Lock()
	s.snap = mprisStateSnapshot{
		playing:      m.playing,
		paused:       m.paused,
		artist:       m.artist,
		title:        m.title,
		album:        m.album,
		position:     m.position,
		duration:     m.duration,
		currentIndex: m.currentIndex,
		trackPath:    trackPath,
	}
	s.mu.Unlock()

	// Build the signal payload from the freshly-committed snapshot.
	s.mu.RLock()
	snap := s.snap
	s.mu.RUnlock()

	changedProps := map[string]dbus.Variant{
		"PlaybackStatus": dbus.MakeVariant(playbackStatusFromSnap(snap)),
		"Metadata":       dbus.MakeVariant(metadataFromSnap(snap)),
	}

	err := s.conn.Emit(
		"/org/mpris/MediaPlayer2",
		"org.freedesktop.DBus.Properties.PropertiesChanged",
		"org.mpris.MediaPlayer2.Player",
		changedProps,
		[]string{}, // invalidated properties — none
	)
	if err != nil {
		log.Printf("MPRIS: failed to emit PropertiesChanged: %v", err)
	}
}

// EmitSeeked updates the position in the snapshot and emits the Seeked signal
// with the current position.  Call this after a seek operation completes.
func (s *MPRISService) EmitSeeked(m *PlayerModel) {
	if s == nil || s.conn == nil {
		return
	}

	s.mu.Lock()
	s.snap.position = m.position
	pos := s.snap.position
	s.mu.Unlock()

	err := s.conn.Emit(
		"/org/mpris/MediaPlayer2",
		"org.mpris.MediaPlayer2.Player.Seeked",
		pos.Microseconds(),
	)
	if err != nil {
		log.Printf("MPRIS: failed to emit Seeked: %v", err)
	}
}

// ---- Helpers ----------------------------------------------------------------

// playbackStatusFromSnap returns the MPRIS2 PlaybackStatus string from a snapshot.
func playbackStatusFromSnap(snap mprisStateSnapshot) string {
	if !snap.playing {
		return "Stopped"
	}
	if snap.paused {
		return "Paused"
	}
	return "Playing"
}

// metadataFromSnap builds the MPRIS2 Metadata map from a snapshot.
// The xesam:artist field must be a []string per the MPRIS2 spec.
func metadataFromSnap(snap mprisStateSnapshot) map[string]dbus.Variant {
	trackId := dbus.ObjectPath(fmt.Sprintf("/org/derpy/track/%d", snap.currentIndex))

	artist := snap.artist
	if artist == "" {
		artist = "Unknown Artist"
	}
	title := snap.title
	if title == "" {
		// Fall back to the bare filename without extension.
		if snap.trackPath != "" {
			title = basenameWithoutExt(snap.trackPath)
		}
	}
	album := snap.album

	return map[string]dbus.Variant{
		"mpris:trackid": dbus.MakeVariant(trackId),
		"mpris:length":  dbus.MakeVariant(snap.duration.Microseconds()),
		"xesam:url":     dbus.MakeVariant(fileUri(snap.trackPath)),
		"xesam:title":   dbus.MakeVariant(title),
		"xesam:artist":  dbus.MakeVariant([]string{artist}),
		"xesam:album":   dbus.MakeVariant(album),
	}
}


// fileUri renders an absolute track path as a file:// URI with the path
// percent-encoded, the shape MPRIS xesam:url expects. Empty paths stay
// empty so clients can tell "no file" from a malformed one.
func fileUri(path string) string {
	if path == "" {
		return ""
	}
	return (&url.URL{Scheme: "file", Path: path}).String()
}

// basenameWithoutExt returns the filename component of a path, with its
// extension stripped.
func basenameWithoutExt(path string) string {
	// Walk backward to the last slash.
	idx := strings.LastIndexByte(path, '/')
	base := path[idx+1:]
	// Strip the extension.
	if dot := strings.LastIndexByte(base, '.'); dot > 0 {
		base = base[:dot]
	}
	return base
}
