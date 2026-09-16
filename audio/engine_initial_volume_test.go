package audio

import (
	"errors"
	"testing"
)

func TestPlayLoopWithVolumeDoesNotInitializeAnotherEngine(t *testing.T) {
	var engine AudioPlayerEngine
	if _, err := engine.PlayLoopWithVolume("layer", "", 0); !errors.Is(err, ErrNotInitialized) {
		t.Fatalf("silent voice must use an already initialized shared engine: %v", err)
	}
}
