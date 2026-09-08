//go:build darwin

package main

// speaker_darwin.go provides a CoreAudio-backed audio output for macOS.
// Uses github.com/ebitengine/oto/v3 which accesses CoreAudio — no CGo
// or C compiler required.
//
// The public surface (speakerInit/Play/Clear/Lock/Unlock) mirrors speaker.go
// exactly so audio.go is fully platform-agnostic.

import (
	"encoding/binary"
	"fmt"
	"math"
	"sync"
	"time"

	"github.com/ebitengine/oto/v3"
	"github.com/gopxl/beep"
)

var (
	spkMu     sync.Mutex
	spkBuf    [][2]float64
	spkPlayer beep.Streamer
	otoCtx    *oto.Context
	otoPlayer *oto.Player
)

// beepReader implements io.Reader by pulling samples from the active
// beep.Streamer, converting float64 stereo to interleaved float32 LE bytes.
// When no streamer is set it outputs silence so the CoreAudio stream stays open.
type beepReader struct{}

const (
	numChannels    = 2
	bytesPerSample = 4 // float32
	frameBytes     = numChannels * bytesPerSample
)

func (r *beepReader) Read(out []byte) (int, error) {
	frames := len(out) / frameBytes
	if frames == 0 {
		return 0, nil
	}

	spkMu.Lock()
	if len(spkBuf) < frames {
		spkBuf = make([][2]float64, frames)
	}

	filled := 0
	if spkPlayer != nil {
		n, ok := spkPlayer.Stream(spkBuf[:frames])
		if !ok {
			spkPlayer = nil
		}
		filled = n
	}
	spkMu.Unlock()

	for i := 0; i < frames; i++ {
		var l, r float64
		if i < filled {
			l = math.Max(-1, math.Min(1, spkBuf[i][0]))
			r = math.Max(-1, math.Min(1, spkBuf[i][1]))
		}
		binary.LittleEndian.PutUint32(out[i*frameBytes:], math.Float32bits(float32(l)))
		binary.LittleEndian.PutUint32(out[i*frameBytes+4:], math.Float32bits(float32(r)))
	}

	return frames * frameBytes, nil
}

func speakerInit(sampleRate beep.SampleRate, bufferSize int) error {
	spkBuf = make([][2]float64, bufferSize)

	opts := &oto.NewContextOptions{
		SampleRate:   int(sampleRate),
		ChannelCount: numChannels,
		Format:       oto.FormatFloat32LE,
		BufferSize:   200 * time.Millisecond,
	}

	ctx, ready, err := oto.NewContext(opts)
	if err != nil {
		return err
	}
	<-ready

	otoCtx = ctx
	otoPlayer = otoCtx.NewPlayer(&beepReader{})
	otoPlayer.Play()
	return nil
}

func speakerPlay(s beep.Streamer) {
	spkMu.Lock()
	defer spkMu.Unlock()
	spkPlayer = s
}

func speakerClear() {
	spkMu.Lock()
	defer spkMu.Unlock()
	spkPlayer = nil
}

func speakerLock() {
	spkMu.Lock()
}

func speakerUnlock() {
	spkMu.Unlock()
}

// speakerEnsureReady verifies the native output reader before resuming. The
// macOS backend owns device recovery, so there is no Pulse stream to rebind.
func speakerEnsureReady() error {
	spkMu.Lock()
	defer spkMu.Unlock()
	if otoPlayer == nil {
		return fmt.Errorf("audio output is not initialized")
	}
	return otoPlayer.Err()
}

func speakerClose() {
	spkMu.Lock()
	defer spkMu.Unlock()
	spkPlayer = nil
	if otoPlayer != nil {
		_ = otoPlayer.Close()
		otoPlayer = nil
	}
}
