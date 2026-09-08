//go:build linux

package main

// speaker.go provides a PulseAudio-based audio output backend.
// This replaces gopxl/beep/speaker (which requires libasound2-dev ALSA headers)
// with a pure-Go PulseAudio client that works on any standard Debian desktop
// running PulseAudio or PipeWire (with PulseAudio compatibility layer).

import (
	"fmt"
	"sync"
	"time"

	"github.com/gopxl/beep"
	"github.com/jfreymuth/pulse"
	"github.com/jfreymuth/pulse/proto"
)

var (
	spkMu          sync.Mutex
	spkLifecycleMu sync.Mutex
	spkBuf         [][2]float64
	spkPlayer      beep.Streamer
	spkClient      *pulse.Client
	spkStream      *pulse.PlaybackStream
	spkSampleRate  beep.SampleRate
	spkBufferSize  int
	spkMonitorStop chan struct{}
)

// speakerInit connects to PulseAudio and starts a continuous audio output stream.
// The stream can later be replaced by speakerEnsureReady when the output route
// changes.
func speakerInit(sampleRate beep.SampleRate, bufferSize int) error {
	spkLifecycleMu.Lock()
	defer spkLifecycleMu.Unlock()

	spkSampleRate = sampleRate
	spkBufferSize = bufferSize
	return speakerReopenLocked()
}

// speakerReopenLocked replaces the PulseAudio stream without touching the
// decoded player. The caller must hold spkLifecycleMu.
func speakerReopenLocked() error {
	spkMu.Lock()
	player := spkPlayer
	spkPlayer = nil
	oldClient := spkClient
	oldStream := spkStream
	spkClient = nil
	spkStream = nil
	spkMu.Unlock()

	closePulseOutput(oldClient, oldStream)

	client, stream, err := newPulseOutput(spkSampleRate, spkBufferSize)
	if err != nil {
		spkMu.Lock()
		spkPlayer = player
		spkMu.Unlock()
		return err
	}

	spkMu.Lock()
	spkClient = client
	spkStream = stream
	spkPlayer = player
	startRouteMonitorLocked()
	spkMu.Unlock()
	return nil
}

func newPulseOutput(sampleRate beep.SampleRate, bufferSize int) (*pulse.Client, *pulse.PlaybackStream, error) {
	client, err := pulse.NewClient(pulse.ClientApplicationName("derpy"))
	if err != nil {
		return nil, nil, fmt.Errorf("failed to connect to PulseAudio: %w", err)
	}

	// Fail before creating a stream when the audio server has no current
	// default sink. The caller can retry without consuming the track.
	if _, err := client.DefaultSink(); err != nil {
		client.Close()
		return nil, nil, fmt.Errorf("no default audio sink: %w", err)
	}

	spkMu.Lock()
	spkBuf = make([][2]float64, bufferSize)
	spkMu.Unlock()

	stream, err := client.NewPlayback(
		pulse.Float32Reader(speakerRead),
		pulse.PlaybackStereo,
		pulse.PlaybackSampleRate(int(sampleRate)),
		pulse.PlaybackLatency(0.1),
	)
	if err != nil {
		client.Close()
		return nil, nil, fmt.Errorf("failed to create PulseAudio playback stream: %w", err)
	}

	stream.Start()
	return client, stream, nil
}

func speakerRead(out []float32) (int, error) {
	spkMu.Lock()
	defer spkMu.Unlock()

	numFrames := len(out) / 2
	if numFrames > len(spkBuf) {
		spkBuf = make([][2]float64, numFrames)
	}

	if spkPlayer == nil {
		for i := range out {
			out[i] = 0
		}
		return len(out), nil
	}

	n, ok := spkPlayer.Stream(spkBuf[:numFrames])
	if !ok {
		spkPlayer = nil
	}

	for i := 0; i < n; i++ {
		out[i*2] = float32(spkBuf[i][0])
		out[i*2+1] = float32(spkBuf[i][1])
	}
	for i := n * 2; i < len(out); i++ {
		out[i] = 0
	}
	return len(out), nil
}

func closePulseOutput(client *pulse.Client, stream *pulse.PlaybackStream) {
	if stream != nil {
		stream.Stop()
		stream.Close()
	}
	if client != nil {
		client.Close()
	}
}

// speakerEnsureReady reopens the output stream on the current default route.
// Reopening on every resume is deliberate: the Pulse client does not expose
// route-loss events, and a stream can remain apparently open after its sink
// disappears.
func speakerEnsureReady() error {
	spkLifecycleMu.Lock()
	defer spkLifecycleMu.Unlock()
	return speakerReopenLocked()
}

func startRouteMonitorLocked() {
	if spkMonitorStop != nil {
		return
	}

	stop := make(chan struct{})
	spkMonitorStop = stop
	go monitorRoute(stop)
}

func monitorRoute(stop <-chan struct{}) {
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()

	var retryAt time.Time
	for {
		select {
		case <-stop:
			return
		case <-ticker.C:
			if time.Now().Before(retryAt) || speakerRouteHealthy() {
				continue
			}

			spkLifecycleMu.Lock()
			spkMu.Lock()
			active := spkMonitorStop == stop
			spkMu.Unlock()
			if active {
				_ = speakerReopenLocked()
				retryAt = time.Now().Add(2 * time.Second)
			}
			spkLifecycleMu.Unlock()
		}
	}
}

func speakerRouteHealthy() bool {
	spkMu.Lock()
	client := spkClient
	stream := spkStream
	player := spkPlayer
	spkMu.Unlock()

	if player == nil {
		return true
	}
	if client == nil || stream == nil || stream.Closed() || stream.Error() != nil {
		return false
	}

	var input proto.GetSinkInputInfoReply
	if err := client.RawRequest(&proto.GetSinkInputInfo{SinkInputIndex: stream.StreamInputIndex()}, &input); err != nil {
		return false
	}

	var sink proto.GetSinkInfoReply
	if err := client.RawRequest(&proto.GetSinkInfo{SinkIndex: input.SinkIndex}, &sink); err != nil {
		return false
	}

	// PulseAudio sink state 3 is SUSPENDED. PipeWire's PulseAudio
	// compatibility layer reports the same protocol state.
	return sink.State != 3
}

func speakerOutputAvailable() bool {
	spkMu.Lock()
	defer spkMu.Unlock()
	return spkClient != nil && spkStream != nil && !spkStream.Closed() && spkStream.Error() == nil
}

func speakerClose() {
	spkLifecycleMu.Lock()
	defer spkLifecycleMu.Unlock()

	spkMu.Lock()
	spkPlayer = nil
	client := spkClient
	stream := spkStream
	monitorStop := spkMonitorStop
	spkClient = nil
	spkStream = nil
	spkMonitorStop = nil
	spkMu.Unlock()

	if monitorStop != nil {
		close(monitorStop)
	}
	closePulseOutput(client, stream)
}

// speakerPlay sets the active streamer. The pulse callback will begin pulling
// samples from s on the next audio buffer fill.
func speakerPlay(s beep.Streamer) {
	spkMu.Lock()
	defer spkMu.Unlock()
	spkPlayer = s
}

// speakerClear stops audio output by removing the active streamer.
// The stream itself stays open and outputs silence until the next speakerPlay call.
func speakerClear() {
	spkMu.Lock()
	defer spkMu.Unlock()
	spkPlayer = nil
}

// speakerLock acquires the speaker mutex, blocking the audio callback from
// running. Use this to make atomic state changes (e.g. toggling pause) that
// must not race with sample delivery.
func speakerLock() {
	spkMu.Lock()
}

// speakerUnlock releases the speaker mutex.
func speakerUnlock() {
	spkMu.Unlock()
}
