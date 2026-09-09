package audio

import (
	"encoding/binary"
	"math"
	"testing"
)

func TestPlayerLoopAndPlaybackRateHelpers(t *testing.T) {
	p := &Player{
		done: make(chan struct{}),
	}
	p.volume.Store(math.Float32bits(1.0))
	p.gain.Store(math.Float32bits(1.0))

	p.SetLoop(true)
	if !p.Loop() {
		t.Fatal("expected loop=true")
	}

	p.SetLoop(false)
	if p.Loop() {
		t.Fatal("expected loop=false")
	}

	p.SetPlaybackRate(0.1)
	if got := p.PlaybackRate(); got != 0.25 {
		t.Fatalf("playback rate = %v, want 0.25", got)
	}

	p.SetPlaybackRate(10)
	if got := p.PlaybackRate(); got != 4.0 {
		t.Fatalf("playback rate = %v, want 4.0", got)
	}
}

func TestPlayerDataCallbackLoopsAndNonLoopDone(t *testing.T) {
	samples := []int16{1000, -1000, 2000, -2000}

	p := &Player{
		pcmData:        monoS16LE(samples),
		positionFrames: 0,
		done:           make(chan struct{}),
		numChannels:    1,
		bytesPerSample: 2,
		bytesPerFrame:  2,
		sourceFrames:   len(samples),
	}
	p.volume.Store(math.Float32bits(1.0))
	p.gain.Store(math.Float32bits(1.0))
	p.SetLoop(true)

	out := make([]byte, 12) // 6 frames
	p.dataCallback(out, nil, 6)
	got := decodeMonoS16LE(out)
	want := []int16{1000, -1000, 2000, -2000, 1000, -1000}
	for i, v := range want {
		if got[i] != v {
			t.Fatalf("loop out[%d] = %d, want %d", i, got[i], v)
		}
	}
	select {
	case <-p.Done():
		t.Fatal("looped playback should not close Done")
	default:
	}

	p2 := &Player{
		pcmData:        monoS16LE(samples),
		positionFrames: 0,
		done:           make(chan struct{}),
		numChannels:    1,
		bytesPerSample: 2,
		bytesPerFrame:  2,
		sourceFrames:   len(samples),
	}
	p2.volume.Store(math.Float32bits(1.0))
	p2.gain.Store(math.Float32bits(1.0))
	out2 := make([]byte, 12) // 6 frames
	p2.dataCallback(out2, nil, 6)
	got2 := decodeMonoS16LE(out2)
	for i := 0; i < 4; i++ {
		if got2[i] != samples[i] {
			t.Fatalf("non-loop out2[%d] = %d, want %d", i, got2[i], samples[i])
		}
	}
	for i := 4; i < len(got2); i++ {
		if got2[i] != 0 {
			t.Fatalf("non-loop out2[%d] = %d, want 0", i, got2[i])
		}
	}
	select {
	case <-p2.Done():
	default:
		t.Fatal("non-loop playback should close Done at end of sample")
	}
}

func monoS16LE(samples []int16) []byte {
	out := make([]byte, len(samples)*2)
	for i, s := range samples {
		binary.LittleEndian.PutUint16(out[i*2:], uint16(s))
	}
	return out
}

func decodeMonoS16LE(data []byte) []int16 {
	count := len(data) / 2
	out := make([]int16, count)
	for i := 0; i < count; i++ {
		out[i] = int16(binary.LittleEndian.Uint16(data[i*2:]))
	}
	return out
}
