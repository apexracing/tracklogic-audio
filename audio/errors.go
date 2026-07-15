package audio

import "errors"

var (
	// ErrNotInitialized is returned when an operation is attempted before Init.
	ErrNotInitialized = errors.New("AudioPlayerEngine 未初始化,请先调用 Init()")
	// ErrAlreadyRecording is returned when Start is called on an active Recorder.
	ErrAlreadyRecording = errors.New("audio: recorder is already recording")
	// ErrNotRecording is returned when Stop is called before Start or after Stop.
	ErrNotRecording = errors.New("audio: recorder is not recording")
	// ErrRecorderClosed is returned when a closed Recorder is used.
	ErrRecorderClosed = errors.New("audio: recorder is closed")
)
