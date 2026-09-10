package audio

import (
	"bytes"
	"encoding/binary"
	"github.com/gen2brain/malgo"
	"sync"
	"testing"
	"time"
)

func TestLoopLifecycleNullBackend(t *testing.T) {
	ctx, err := malgo.InitContext([]malgo.Backend{malgo.BackendNull}, malgo.ContextConfig{}, nil)
	if err != nil {
		t.Skipf("Null audio backend unavailable in this malgo build: %v", err)
	}
	defer ctx.Free()
	defer ctx.Uninit()
	p, err := newPlayerFromSound(ctx, &preloadedSound{pcmData: monoS16LE([]int16{1200, -1200, 500, -500}), numChannels: 1, bitsPerSample: 16, sampleRate: 48000}, "")
	if err != nil {
		t.Fatal(err)
	}
	defer p.device.Uninit()
	p.SetLoop(true)
	done := make(chan error, 1)
	go func() {
		for i := 0; i < 30; i++ {
			if err := p.Replay(); err != nil {
				done <- err
				return
			}
			p.SetPlaybackRate(.5 + float64(i%6)*.3)
			p.SetVolume(.2)
			if err := p.Stop(); err != nil {
				done <- err
				return
			}
		}
		done <- nil
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(8 * time.Second):
		t.Fatal("device lifecycle deadlocked against audio callback")
	}
}

type callbackDevice struct {
	owner   *Player
	started bool
}

func (d *callbackDevice) Start() error {
	d.started = true
	d.owner.dataCallback(make([]byte, 8), nil, 4)
	return nil
}
func (d *callbackDevice) Stop() error                              { d.started = false; d.owner.stopCallback(); return nil }
func (d *callbackDevice) IsStarted() bool                          { return d.started }
func (d *callbackDevice) Uninit()                                  {}
func (d *callbackDevice) PlaybackInternalFormat() malgo.FormatType { return malgo.FormatS16 }
func (d *callbackDevice) PlaybackInternalChannels() uint32         { return 1 }
func (d *callbackDevice) PlaybackInternalSampleRate() uint32       { return 48000 }

func TestLifecycleDoesNotHoldCallbackLock(t *testing.T) {
	p := &Player{done: make(chan struct{}), bytesPerSample: 2, bytesPerFrame: 2, numChannels: 1}
	p.device = &callbackDevice{owner: p}
	done := make(chan error, 1)
	go func() {
		for i := 0; i < 30; i++ {
			if err := p.Replay(); err != nil {
				done <- err
				return
			}
			if err := p.Stop(); err != nil {
				done <- err
				return
			}
		}
		done <- nil
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("Start/Stop deadlocked on callback lock")
	}
}

func TestConcurrentDoneResetAndCallback(t *testing.T) {
	p := &Player{done: make(chan struct{}), bytesPerFrame: 2, bytesPerSample: 2, numChannels: 1}
	var group sync.WaitGroup
	for i := 0; i < 3; i++ {
		group.Add(1)
		go func(n int) {
			defer group.Done()
			for k := 0; k < 200; k++ {
				switch n {
				case 0:
					p.Reset()
				case 1:
					p.Done()
				case 2:
					p.dataCallback(make([]byte, 4), nil, 2)
					p.stopCallback()
				}
			}
		}(i)
	}
	group.Wait()
}

func TestFractionalCursorStaysFrameAlignedWhenRateReturnsToOne(t *testing.T) {
	p := &Player{done: make(chan struct{}), pcmData: monoS16LE([]int16{0, 2000, 4000}), bytesPerFrame: 2, bytesPerSample: 2, numChannels: 1, sourceFrames: 3}
	p.SetVolume(1)
	p.SetGain(1)
	p.SetPlaybackRate(.5)
	p.dataCallback(make([]byte, 2), nil, 1)
	p.SetPlaybackRate(1)
	out := make([]byte, 2)
	p.dataCallback(out, nil, 1)
	got := decodeMonoS16LE(out)[0]
	if got < 999 || got > 1001 {
		t.Fatalf("interpolated sample = %d, want 1000", got)
	}
}

func TestU8EndOfSoundIsSilent(t *testing.T) {
	p := &Player{done: make(chan struct{}), bytesPerFrame: 1, bytesPerSample: 1, numChannels: 1}
	out := make([]byte, 4)
	p.dataCallback(out, nil, 4)
	if !bytes.Equal(out, []byte{128, 128, 128, 128}) {
		t.Fatalf("U8 silence = %v", out)
	}
}

func TestPreloadWAVReader(t *testing.T) {
	var wav bytes.Buffer
	wav.WriteString("RIFF")
	binary.Write(&wav, binary.LittleEndian, uint32(40))
	wav.WriteString("WAVEfmt ")
	for _, value := range []any{uint32(16), uint16(1), uint16(1), uint32(48000), uint32(96000), uint16(2), uint16(16)} {
		binary.Write(&wav, binary.LittleEndian, value)
	}
	wav.WriteString("data")
	binary.Write(&wav, binary.LittleEndian, uint32(4))
	wav.Write(monoS16LE([]int16{1, -1}))
	sound, err := loadSoundReader(bytes.NewReader(wav.Bytes()))
	if err != nil || sound.numChannels != 1 || sound.sampleRate != 48000 {
		t.Fatalf("decode: %+v %v", sound, err)
	}
	if _, err := loadSoundReader(bytes.NewReader(wav.Bytes()[:wav.Len()-1])); err == nil {
		t.Fatal("accepted truncated WAV")
	}
	if _, err := loadSoundReader(nil); err == nil {
		t.Fatal("accepted nil reader")
	}
}
