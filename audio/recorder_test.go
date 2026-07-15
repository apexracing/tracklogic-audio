package audio

import (
	"encoding/binary"
	"errors"
	"math"
	"reflect"
	"testing"
)

func TestNormalizeRecorderConfigUsesASRDefault(t *testing.T) {
	config := normalizeRecorderConfig(RecorderConfig{})
	if config.SampleRate != DefaultCaptureSampleRate {
		t.Fatalf("SampleRate = %d, want %d", config.SampleRate, DefaultCaptureSampleRate)
	}

	config = normalizeRecorderConfig(RecorderConfig{SampleRate: 48000})
	if config.SampleRate != 48000 {
		t.Fatalf("SampleRate = %d, want 48000", config.SampleRate)
	}
}

func TestRecorderDataCallbackCapturesFloat32(t *testing.T) {
	want := []float32{-1, -0.25, 0, 0.5, 1}
	input := make([]byte, len(want)*4)
	for i, sample := range want {
		binary.NativeEndian.PutUint32(input[i*4:], math.Float32bits(sample))
	}

	recorder := &Recorder{recording: true}
	recorder.dataCallback(nil, input, uint32(len(want)))
	if !reflect.DeepEqual(recorder.samples, want) {
		t.Fatalf("samples = %v, want %v", recorder.samples, want)
	}
}

func TestRecorderDataCallbackIgnoresDataWhileStopped(t *testing.T) {
	recorder := &Recorder{}
	recorder.dataCallback(nil, make([]byte, 8), 2)
	if len(recorder.samples) != 0 {
		t.Fatalf("captured %d samples while stopped", len(recorder.samples))
	}
}

func TestRecorderStopStateErrors(t *testing.T) {
	recorder := &Recorder{}
	if _, err := recorder.Stop(); !errors.Is(err, ErrNotRecording) {
		t.Fatalf("Stop error = %v, want ErrNotRecording", err)
	}

	recorder.closed = true
	if _, err := recorder.Stop(); !errors.Is(err, ErrRecorderClosed) {
		t.Fatalf("Stop error = %v, want ErrRecorderClosed", err)
	}
	if err := recorder.Start(); !errors.Is(err, ErrRecorderClosed) {
		t.Fatalf("Start error = %v, want ErrRecorderClosed", err)
	}
}

func TestRecordingSamplesCanBeCopiedIndependently(t *testing.T) {
	recorder := &Recorder{samples: []float32{0.25, 0.5}}
	samples := append([]float32(nil), recorder.samples...)
	recorder.samples[0] = 1
	if samples[0] != 0.25 {
		t.Fatalf("copied sample changed to %v", samples[0])
	}
}
