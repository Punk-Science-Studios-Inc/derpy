package main

import (
	"bytes"
	"errors"
	"io"
	"io/fs"
	"syscall"
	"testing"

	"github.com/gopxl/beep"
	"github.com/gopxl/beep/flac"
	"github.com/gopxl/beep/mp3"
	"github.com/gopxl/beep/vorbis"
	"github.com/gopxl/beep/wav"
)

// fixedStreamer emits a set number of stereo frames, then reports completion.
// It stands in for a decoded track of known length at a known sample rate.
type fixedStreamer struct {
	remaining int
}

func (s *fixedStreamer) Stream(samples [][2]float64) (n int, ok bool) {
	if s.remaining <= 0 {
		return 0, false
	}
	n = len(samples)
	if n > s.remaining {
		n = s.remaining
	}
	for i := 0; i < n; i++ {
		samples[i][0] = 0.25
		samples[i][1] = 0.25
	}
	s.remaining -= n
	return n, true
}

func (s *fixedStreamer) Err() error { return nil }

// drain pulls every frame from s in fixed-size chunks and returns the total.
func drain(s beep.Streamer) int {
	buf := make([][2]float64, 512)
	total := 0
	for {
		n, ok := s.Stream(buf)
		total += n
		if !ok {
			return total
		}
	}
}

// A track already at the device rate must pass through untouched — no
// resampler inserted, frame count unchanged.
func TestResampleForDevicePassthrough(t *testing.T) {
	src := &fixedStreamer{remaining: 1000}

	got := resampleForDevice(src, deviceSampleRate)

	if beep.Streamer(src) != got {
		t.Fatalf("matching-rate track should not be wrapped in a resampler")
	}
	if frames := drain(got); frames != 1000 {
		t.Fatalf("passthrough changed frame count: got %d, want 1000", frames)
	}
}

// A track whose native rate is lower than the device rate is the chipmunk
// case: without resampling its samples play too fast. After resampleForDevice
// the output frame count must scale up to the device rate.
func TestResampleForDeviceUpsamples(t *testing.T) {
	const srcRate beep.SampleRate = 22050
	const inFrames = int(srcRate) // one second of audio

	out := drain(resampleForDevice(&fixedStreamer{remaining: inFrames}, srcRate))

	want := float64(deviceSampleRate) / float64(srcRate) // 2.0
	got := float64(out) / float64(inFrames)
	if diff := got - want; diff < -0.01 || diff > 0.01 {
		t.Fatalf("22050Hz track not resampled to %dHz: %d in -> %d out (ratio %.4f, want %.4f)",
			deviceSampleRate, inFrames, out, got, want)
	}
}

// A track whose native rate is higher than the device rate scales down to the
// device rate (otherwise it would play slightly slow/low).
func TestResampleForDeviceDownsamples(t *testing.T) {
	const srcRate beep.SampleRate = 48000
	const inFrames = int(srcRate) // one second of audio

	out := drain(resampleForDevice(&fixedStreamer{remaining: inFrames}, srcRate))

	want := float64(deviceSampleRate) / float64(srcRate)
	got := float64(out) / float64(inFrames)
	if diff := got - want; diff < -0.01 || diff > 0.01 {
		t.Fatalf("48000Hz track not resampled to %dHz: %d in -> %d out (ratio %.4f, want %.4f)",
			deviceSampleRate, inFrames, out, got, want)
	}
}

// --- read-failure classification ---------------------------------------

// eioReadSeekCloser fails every Read the way *os.File does when the kernel
// reports an error: os.File.wrapErr boxes the errno in an *fs.PathError. This
// reproduces a file on an offline network mount without needing one.
type eioReadSeekCloser struct {
	path string
	err  error
}

func (e eioReadSeekCloser) Read(p []byte) (int, error) {
	return 0, &fs.PathError{Op: "read", Path: e.path, Err: e.err}
}
func (e eioReadSeekCloser) Seek(int64, int) (int64, error) { return 0, nil }
func (e eioReadSeekCloser) Close() error                   { return nil }

// TestIsReadFailureThroughDecoders is the regression test for the bug where an
// unreadable file was reported as "failed to decode". Each beep decoder wraps
// the underlying error with its own codec prefix, so the test asserts the
// unwrap chain survives that wrapping for every format derpy supports.
func TestIsReadFailureThroughDecoders(t *testing.T) {
	r := eioReadSeekCloser{path: "/mnt/cloud/track", err: syscall.EIO}

	decoders := map[string]func() error{
		"mp3": func() error {
			_, _, err := mp3.Decode(r)
			return err
		},
		"flac": func() error {
			_, _, err := flac.Decode(r)
			return err
		},
		"vorbis": func() error {
			_, _, err := vorbis.Decode(r)
			return err
		},
		"wav": func() error {
			_, _, err := wav.Decode(r)
			return err
		},
	}

	for name, decode := range decoders {
		t.Run(name, func(t *testing.T) {
			err := decode()
			if err == nil {
				t.Fatalf("expected %s decode of an unreadable file to fail", name)
			}
			if !isReadFailure(err) {
				t.Errorf("isReadFailure(%v) = false, want true — an I/O error must not be reported as a decode failure", err)
			}
		})
	}
}

// TestIsReadFailureRejectsDecodeErrors guards the other direction: a genuinely
// malformed file must still be classified as a decode failure, or the fix would
// simply relabel every error and lose the distinction it exists to draw.
func TestIsReadFailureRejectsDecodeErrors(t *testing.T) {
	// Readable bytes that are not valid audio in any supported format.
	garbage := func() io.ReadSeekCloser {
		return nopCloser{bytes.NewReader(make([]byte, 8192))}
	}

	cases := map[string]func() error{
		"mp3": func() error {
			_, _, err := mp3.Decode(garbage())
			return err
		},
		"flac": func() error {
			_, _, err := flac.Decode(garbage())
			return err
		},
		"vorbis": func() error {
			_, _, err := vorbis.Decode(garbage())
			return err
		},
		"wav": func() error {
			_, _, err := wav.Decode(garbage())
			return err
		},
	}

	for name, decode := range cases {
		t.Run(name, func(t *testing.T) {
			err := decode()
			if err == nil {
				t.Skipf("%s decoder accepted the zero-filled input; nothing to classify", name)
			}
			if isReadFailure(err) {
				t.Errorf("isReadFailure(%v) = true, want false — a malformed file is not an I/O failure", err)
			}
		})
	}
}

// TestIsReadFailureNonEIOErrno covers mounts that report something other than
// EIO (a stale NFS handle, a vanished file); any errno means the OS refused
// the read, so all of them must classify the same way.
func TestIsReadFailureNonEIOErrno(t *testing.T) {
	for _, errno := range []syscall.Errno{syscall.EIO, syscall.ESTALE, syscall.EACCES, syscall.ENODEV} {
		_, _, err := mp3.Decode(eioReadSeekCloser{path: "/mnt/cloud/track", err: errno})
		if err == nil {
			t.Fatalf("expected decode to fail for errno %v", errno)
		}
		if !isReadFailure(err) {
			t.Errorf("isReadFailure for errno %v = false, want true", errno)
		}
	}
}

func TestIsReadFailureNil(t *testing.T) {
	if isReadFailure(nil) {
		t.Error("isReadFailure(nil) = true, want false")
	}
}

func TestResumeEnsuresOutputBeforeUnpausing(t *testing.T) {
	player := &AudioPlayer{}
	player.ctrl = &beep.Ctrl{Streamer: &fixedStreamer{remaining: 10}, Paused: true}
	player.playing = true
	called := false
	player.ensureOutputReady = func() error {
		called = true
		if !player.ctrl.Paused {
			t.Fatal("output readiness was checked after unpausing")
		}
		return nil
	}

	if err := player.Resume(); err != nil {
		t.Fatalf("Resume() returned unexpected error: %v", err)
	}
	if !called {
		t.Fatal("Resume() did not ensure output readiness")
	}
	if player.ctrl.Paused {
		t.Fatal("Resume() left the controller paused")
	}
}

func TestResumeKeepsPlaybackPausedWhenOutputUnavailable(t *testing.T) {
	player := &AudioPlayer{}
	player.ctrl = &beep.Ctrl{Streamer: &fixedStreamer{remaining: 10}, Paused: true}
	player.playing = true
	wantErr := errors.New("no default audio sink")
	player.ensureOutputReady = func() error { return wantErr }

	if err := player.Resume(); !errors.Is(err, wantErr) {
		t.Fatalf("Resume() error = %v, want %v", err, wantErr)
	}
	if !player.ctrl.Paused {
		t.Fatal("Resume() unpaused playback after output recovery failed")
	}
}

// nopCloser adapts a *bytes.Reader to io.ReadSeekCloser.
type nopCloser struct{ *bytes.Reader }

func (nopCloser) Close() error { return nil }
