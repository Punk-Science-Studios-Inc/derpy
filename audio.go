package main

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/dhowden/tag"
	"github.com/gopxl/beep"
	"github.com/gopxl/beep/flac"
	"github.com/gopxl/beep/mp3"
	"github.com/gopxl/beep/vorbis"
	"github.com/gopxl/beep/wav"
)

// deviceSampleRate is the fixed rate the output device is opened at. Every
// track is resampled to this rate before playback so that files whose native
// sample rate differs from the device do not play at the wrong speed/pitch.
// 44100 Hz is chosen because the overwhelming majority of music is already at
// that rate, so the common case incurs no resampling cost or quality loss.
const deviceSampleRate beep.SampleRate = 44100

// resampleQuality is the beep.Resample quality factor (higher = better quality,
// more CPU). 4 is a good balance for music playback.
const resampleQuality = 4

// resampleForDevice wraps streamer so its output runs at deviceSampleRate.
// When the source already matches the device rate the streamer is returned
// unchanged — no resampler is inserted, so there is no cost or quality loss
// for the common case.
func resampleForDevice(streamer beep.Streamer, srcRate beep.SampleRate) beep.Streamer {
	if srcRate == deviceSampleRate {
		return streamer
	}
	return beep.Resample(resampleQuality, srcRate, deviceSampleRate, streamer)
}

// CompletionStreamer wraps a streamer to detect when it completes
type CompletionStreamer struct {
	beep.Streamer
	completed bool
}

func (cs *CompletionStreamer) Stream(samples [][2]float64) (n int, ok bool) {
	n, ok = cs.Streamer.Stream(samples)
	if !ok {
		cs.completed = true
	}
	return n, ok
}

func (cs *CompletionStreamer) Err() error {
	if s, ok := cs.Streamer.(interface{ Err() error }); ok {
		return s.Err()
	}
	return nil
}

func (cs *CompletionStreamer) IsCompleted() bool {
	return cs.completed
}

// AudioPlayer manages audio playback
type AudioPlayer struct {
	streamer           beep.StreamSeekCloser
	playbackStreamer   beep.Streamer
	ctrl               *beep.Ctrl
	format             beep.Format
	playing            bool
	file               *os.File
	duration           time.Duration
	currentPos         time.Duration
	artist             string
	title              string
	album              string
	startTime          time.Time
	hasEnded           bool
	completionStream   *CompletionStreamer
	speakerInitialized bool
	ensureOutputReady  func() error
	outputAvailable    func() bool
	outputClockRunning bool
	scrobbleTracker    *ScrobbleTracker
	lbClient           *ListenBrainzClient
}

// NewAudioPlayer creates a new audio player instance
func NewAudioPlayer() *AudioPlayer {
	return &AudioPlayer{
		ensureOutputReady: speakerEnsureReady,
		outputAvailable:   speakerOutputAvailable,
		lbClient:          NewListenBrainzClient(),
	}
}

// isReadFailure reports whether err was ultimately caused by the operating
// system being unable to read the file's bytes, rather than by the audio data
// itself being malformed or in an unsupported format.
//
// The distinction matters because every beep decoder labels its failures with
// the codec name — a dead network mount surfaces as "mp3: read ...: input/output
// error", which reads like a broken MP3 and sends you debugging the decoder
// instead of the filesystem. Network-backed mounts (pCloud, NFS, SMB) fail this
// way in bulk whenever the mount goes offline or a file is not cached locally.
//
// Detection unwraps rather than matching strings: os.File boxes kernel errors
// in *fs.PathError, and the beep decoders wrap with github.com/pkg/errors,
// which implements Unwrap. Both layers are therefore transparent to errors.As.
// Any errno at the bottom of the chain — not just EIO — means the OS refused
// the read, so all of them classify as a read failure.
func isReadFailure(err error) bool {
	var pathErr *fs.PathError
	if errors.As(err, &pathErr) {
		return true
	}
	var errno syscall.Errno
	return errors.As(err, &errno)
}

// LoadTrack loads an audio file for playback
func (ap *AudioPlayer) LoadTrack(filePath string) error {
	// Stop any current playback and reset state
	ap.Stop()

	// Wait a moment for resources to be fully released
	time.Sleep(50 * time.Millisecond)

	// Open the audio file
	file, err := os.Open(filePath)
	if err != nil {
		return fmt.Errorf("failed to open %s: %w", filePath, err)
	}

	ap.file = file

	// Read metadata tags
	tags, err := tag.ReadFrom(file)
	if err == nil {
		ap.artist = tags.Artist()
		ap.title = tags.Title()
		ap.album = tags.Album()
	} else {
		// Fallback to filename if no tags
		ap.title = filepath.Base(filePath)
		ap.artist = "Unknown Artist"
		ap.album = "Unknown Album"
	}

	// Reset file pointer for audio decoding
	if _, err := file.Seek(0, 0); err != nil {
		return fmt.Errorf("failed to seek %s: %w", filePath, err)
	}

	// Decode based on file extension
	ext := strings.ToLower(filepath.Ext(filePath))
	var streamer beep.StreamSeekCloser
	var format beep.Format

	switch ext {
	case ".mp3":
		streamer, format, err = mp3.Decode(file)
	case ".wav":
		streamer, format, err = wav.Decode(file)
	case ".flac":
		streamer, format, err = flac.Decode(file)
	case ".ogg":
		streamer, format, err = vorbis.Decode(file)
	default:
		return fmt.Errorf("unsupported audio format: %s", ext)
	}

	if err != nil {
		file.Close()
		if isReadFailure(err) {
			return fmt.Errorf("failed to read %s (filesystem I/O error — the file's bytes could not be fetched; not a corrupt or unsupported audio file): %w", filePath, err)
		}
		return fmt.Errorf("failed to decode %s: %w", filePath, err)
	}

	ap.streamer = streamer
	ap.format = format

	// Resample to the fixed device rate. Seek/Len/duration math below all
	// operate on the native streamer, so they stay correct; only the bytes
	// handed to the speaker are rate-converted.
	ap.playbackStreamer = resampleForDevice(streamer, format.SampleRate)

	// Calculate duration
	streamLen := streamer.Len()
	ap.duration = format.SampleRate.D(streamLen)
	ap.currentPos = 0

	// Initialize ListenBrainz scrobble tracker
	if ap.lbClient.IsEnabled() {
		ap.scrobbleTracker = NewScrobbleTracker(ap.lbClient, ap.artist, ap.title, ap.album, ap.duration)
	}

	// Initialize speaker only once per application lifecycle
	if !ap.speakerInitialized {
		if err := speakerInit(deviceSampleRate, deviceSampleRate.N(time.Second/10)); err != nil {
			return fmt.Errorf("failed to initialize speaker: %w", err)
		}
		ap.speakerInitialized = true
	}

	return nil
}

// Play starts or resumes playback
func (ap *AudioPlayer) Play() error {
	if ap.streamer == nil {
		return fmt.Errorf("no track loaded")
	}

	if ap.playing {
		return nil // Already playing
	}

	// Clear any existing audio from speaker
	speakerClear()

	// Give speaker time to fully clear
	time.Sleep(10 * time.Millisecond)

	// Create completion detector wrapper. Wrap the (possibly resampled)
	// playback streamer so the speaker receives audio at the device rate.
	ap.completionStream = &CompletionStreamer{
		Streamer: ap.playbackStreamer,
	}

	// Create control wrapper for pause/resume functionality
	ap.ctrl = &beep.Ctrl{
		Streamer: ap.completionStream,
		Paused:   false,
	}

	// Record start time for position tracking and reset position
	ap.startTime = time.Now()
	ap.currentPos = 0
	ap.outputClockRunning = ap.isOutputAvailable()

	// Start playback
	speakerPlay(ap.ctrl)
	ap.playing = true

	return nil
}

// Pause pauses playback
func (ap *AudioPlayer) Pause() {
	if ap.ctrl != nil && ap.playing {
		outputAvailable := ap.isOutputAvailable()
		speakerLock()
		ap.advancePositionLocked(outputAvailable)
		ap.ctrl.Paused = true
		speakerUnlock()
	}
}

// Resume resumes playback after ensuring that the output route is ready.
func (ap *AudioPlayer) Resume() error {
	if ap.ctrl == nil || !ap.playing {
		return nil
	}

	speakerLock()
	paused := ap.ctrl.Paused
	speakerUnlock()
	if !paused {
		return nil
	}

	ensureOutputReady := ap.ensureOutputReady
	if ensureOutputReady == nil {
		ensureOutputReady = speakerEnsureReady
	}
	if err := ensureOutputReady(); err != nil {
		return fmt.Errorf("failed to restore audio output: %w", err)
	}

	speakerLock()
	defer speakerUnlock()
	ap.ctrl.Paused = false
	ap.startTime = time.Now() // Reset start time on resume
	ap.outputClockRunning = true
	return nil
}

// IsPaused returns true if playback is paused
func (ap *AudioPlayer) IsPaused() bool {
	if ap.ctrl == nil {
		return false
	}
	speakerLock()
	paused := ap.ctrl.Paused
	speakerUnlock()
	return paused
}

// Stop stops playback
func (ap *AudioPlayer) Stop() {
	if ap.playing {
		// Clear the speaker to stop any audio
		speakerClear()

		// Wait for speaker to fully stop
		time.Sleep(20 * time.Millisecond)

		// Update currentPos to where we stopped before changing playing state.
		if ap.ctrl != nil && !ap.ctrl.Paused {
			ap.currentPos = ap.GetPosition()
		}
		ap.playing = false
		ap.hasEnded = false
	}

	// Clean up resources
	if ap.streamer != nil {
		ap.streamer.Close()
		ap.streamer = nil
	}

	if ap.file != nil {
		ap.file.Close()
		ap.file = nil
	}

	// Clear references to prevent accumulation
	ap.ctrl = nil
	ap.completionStream = nil
	ap.playbackStreamer = nil
	ap.scrobbleTracker = nil
}

// Close closes the audio player and releases resources
func (ap *AudioPlayer) Close() {
	ap.Stop()

	// Final cleanup - release the output stream and client.
	speakerClose()

	// Reset speaker initialization flag if needed for restart
	ap.speakerInitialized = false
}

// Shutdown completely shuts down the audio player
func (ap *AudioPlayer) Shutdown() {
	ap.Close()
}

// GetDuration returns the total duration of the current track
func (ap *AudioPlayer) GetDuration() time.Duration {
	return ap.duration
}

// GetPosition returns the current playback position.
func (ap *AudioPlayer) GetPosition() time.Duration {
	outputAvailable := ap.isOutputAvailable()

	speakerLock()
	defer speakerUnlock()

	if ap.ctrl != nil && ap.ctrl.Paused {
		return ap.currentPos
	}

	if !ap.playing {
		return ap.currentPos
	}

	pos := ap.advancePositionLocked(outputAvailable)
	if outputAvailable && ap.scrobbleTracker != nil {
		ap.scrobbleTracker.Update(pos)
	}
	return pos
}

func (ap *AudioPlayer) isOutputAvailable() bool {
	if ap.outputAvailable != nil {
		return ap.outputAvailable()
	}
	return speakerOutputAvailable()
}

// advancePositionLocked commits only time during which output was available.
// The caller must hold the speaker mutex.
func (ap *AudioPlayer) advancePositionLocked(outputAvailable bool) time.Duration {
	if ap.outputClockRunning {
		ap.currentPos += time.Since(ap.startTime)
		ap.startTime = time.Now()
	}

	if !outputAvailable {
		ap.outputClockRunning = false
	} else if !ap.outputClockRunning {
		ap.startTime = time.Now()
		ap.outputClockRunning = true
	}

	if ap.currentPos > ap.duration {
		ap.currentPos = ap.duration
	}
	return ap.currentPos
}

// Seek seeks to a specific position in the track
func (ap *AudioPlayer) Seek(pos time.Duration) error {
	if ap.streamer == nil {
		return fmt.Errorf("no track loaded")
	}

	// Convert time to sample position
	samples := ap.format.SampleRate.N(pos)

	// Seek to position
	if err := ap.streamer.Seek(samples); err != nil {
		return fmt.Errorf("failed to seek: %w", err)
	}

	speakerLock()
	ap.currentPos = pos
	ap.startTime = time.Now()
	ap.outputClockRunning = !ap.ctrlPaused()
	speakerUnlock()
	return nil
}

func (ap *AudioPlayer) ctrlPaused() bool {
	return ap.ctrl != nil && ap.ctrl.Paused
}

// IsPlaying returns true if audio is currently playing
func (ap *AudioPlayer) IsPlaying() bool {
	return ap.playing && !ap.IsPaused()
}

// GetArtist returns the artist of the current track
func (ap *AudioPlayer) GetArtist() string {
	return ap.artist
}

// GetTitle returns the title of the current track
func (ap *AudioPlayer) GetTitle() string {
	return ap.title
}

// GetAlbum returns the album of the current track
func (ap *AudioPlayer) GetAlbum() string {
	return ap.album
}

// HasEnded returns true if the current track has finished playing
func (ap *AudioPlayer) HasEnded() bool {
	if ap.duration == 0 || !ap.playing {
		return false
	}

	// First check if our completion streamer detected the end
	if ap.completionStream != nil && ap.completionStream.IsCompleted() {
		ap.hasEnded = true
		return true
	}

	// Also check if position has reached the end (fallback)
	currentPos := ap.GetPosition()

	if currentPos >= ap.duration {
		ap.hasEnded = true
		return true
	}

	return ap.hasEnded
}
