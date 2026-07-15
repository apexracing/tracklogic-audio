package audio

import (
	"encoding/binary"
	"fmt"
	"math"
	"sync"

	"github.com/gen2brain/malgo"
)

// DefaultCaptureSampleRate is the ASR-friendly sample rate used when
// RecorderConfig.SampleRate is zero.
const DefaultCaptureSampleRate uint32 = 16000

// RecorderConfig selects the input device and output sample rate.
// Capture is always mono float32 PCM. An empty DeviceID selects the
// system default input device.
type RecorderConfig struct {
	DeviceID   string
	SampleRate uint32
}

// Recording contains mono float32 PCM suitable for tracklogic-asr's
// Recognizer.Transcribe method.
type Recording struct {
	Samples    []float32
	SampleRate int
}

// Recorder captures one in-memory audio segment at a time. Start begins a
// fresh segment and Stop returns an independent copy of its samples.
// Recorder lifecycle methods are safe to call from different goroutines.
type Recorder struct {
	owner      *AudioPlayerEngine
	device     *malgo.Device
	sampleRate uint32

	opMu   sync.Mutex
	closed bool

	samplesMu sync.Mutex
	recording bool
	samples   []float32
}

// NewRecorder creates an initialized but stopped capture device. The engine
// must have been initialized first. Close the Recorder when it is no longer
// needed; Destroy also closes any remaining Recorders.
func (e *AudioPlayerEngine) NewRecorder(config RecorderConfig) (*Recorder, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.ctx == nil {
		return nil, ErrNotInitialized
	}

	config = normalizeRecorderConfig(config)
	recorder, err := newRecorder(e, e.ctx, config)
	if err != nil {
		return nil, err
	}
	e.recorders[recorder] = struct{}{}
	return recorder, nil
}

func newRecorder(owner *AudioPlayerEngine, ctx *malgo.AllocatedContext, config RecorderConfig) (*Recorder, error) {
	deviceConfig := malgo.DefaultDeviceConfig(malgo.Capture)
	deviceConfig.Capture.Format = malgo.FormatF32
	deviceConfig.Capture.Channels = 1
	deviceConfig.SampleRate = config.SampleRate

	if config.DeviceID != "" {
		rawID, err := resolveDeviceID(ctx, malgo.Capture, config.DeviceID)
		if err != nil {
			return nil, err
		}
		deviceConfig.Capture.DeviceID = rawID.Pointer()
	}

	recorder := &Recorder{
		owner:      owner,
		sampleRate: config.SampleRate,
	}
	callbacks := malgo.DeviceCallbacks{Data: recorder.dataCallback}
	device, err := malgo.InitDevice(ctx.Context, deviceConfig, callbacks)
	if err != nil {
		return nil, fmt.Errorf("initialize capture device: %w", err)
	}
	recorder.device = device
	return recorder, nil
}

func normalizeRecorderConfig(config RecorderConfig) RecorderConfig {
	if config.SampleRate == 0 {
		config.SampleRate = DefaultCaptureSampleRate
	}
	return config
}

// Start clears any previous segment and starts recording. It is non-blocking.
func (r *Recorder) Start() error {
	r.opMu.Lock()
	defer r.opMu.Unlock()
	if r.closed {
		return ErrRecorderClosed
	}

	r.samplesMu.Lock()
	if r.recording {
		r.samplesMu.Unlock()
		return ErrAlreadyRecording
	}
	r.samples = r.samples[:0]
	r.recording = true
	r.samplesMu.Unlock()

	if err := r.device.Start(); err != nil {
		r.samplesMu.Lock()
		r.recording = false
		r.samplesMu.Unlock()
		return fmt.Errorf("start capture device: %w", err)
	}
	return nil
}

// Stop stops recording and returns the complete captured segment. The returned
// slice does not share storage with the Recorder and remains valid after the
// next Start or Close.
func (r *Recorder) Stop() (Recording, error) {
	r.opMu.Lock()
	defer r.opMu.Unlock()
	if r.closed {
		return Recording{}, ErrRecorderClosed
	}

	r.samplesMu.Lock()
	recording := r.recording
	r.samplesMu.Unlock()
	if !recording {
		return Recording{}, ErrNotRecording
	}
	if err := r.device.Stop(); err != nil {
		return Recording{}, fmt.Errorf("stop capture device: %w", err)
	}

	r.samplesMu.Lock()
	r.recording = false
	samples := append([]float32(nil), r.samples...)
	r.samplesMu.Unlock()
	return Recording{Samples: samples, SampleRate: int(r.sampleRate)}, nil
}

// SampleRate returns the configured output sample rate.
func (r *Recorder) SampleRate() int {
	return int(r.sampleRate)
}

// Close stops capture if necessary and releases the input device. It is safe
// to call Close more than once. Call Stop first if the final samples are needed.
func (r *Recorder) Close() error {
	err := r.close()
	if r.owner != nil {
		r.owner.mu.Lock()
		delete(r.owner.recorders, r)
		r.owner.mu.Unlock()
	}
	return err
}

func (r *Recorder) close() error {
	r.opMu.Lock()
	defer r.opMu.Unlock()
	if r.closed {
		return nil
	}

	r.samplesMu.Lock()
	recording := r.recording
	r.samplesMu.Unlock()
	var stopErr error
	if recording {
		stopErr = r.device.Stop()
	}
	r.samplesMu.Lock()
	r.recording = false
	r.samplesMu.Unlock()
	r.device.Uninit()
	r.closed = true
	return stopErr
}

func (r *Recorder) dataCallback(_ []byte, input []byte, _ uint32) {
	r.samplesMu.Lock()
	defer r.samplesMu.Unlock()
	if !r.recording {
		return
	}
	for len(input) >= 4 {
		bits := binary.NativeEndian.Uint32(input[:4])
		r.samples = append(r.samples, math.Float32frombits(bits))
		input = input[4:]
	}
}
