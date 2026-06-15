package cp

import (
	"fmt"
	"sort"

	"spawnery/internal/cp/store"
)

func sortedRootfsArtifactPins(pins []store.RootfsArtifactPin) []store.RootfsArtifactPin {
	if len(pins) == 0 {
		return nil
	}
	out := append([]store.RootfsArtifactPin(nil), pins...)
	sort.SliceStable(out, func(i, j int) bool {
		return out[i].Sequence < out[j].Sequence
	})
	return out
}

func validateRootfsArtifactPinChain(pins []store.RootfsArtifactPin) error {
	for i, pin := range pins {
		if pin.Sequence <= 0 {
			return fmt.Errorf("rootfs artifact chain has missing sequence for artifact %s", pin.ArtifactID)
		}
		if i > 0 && pin.Sequence == pins[i-1].Sequence {
			return fmt.Errorf("rootfs artifact chain has duplicate sequence %d", pin.Sequence)
		}
		want := i + 1
		if pin.Sequence != want {
			return fmt.Errorf("rootfs artifact chain has sequence gap: got %d want %d", pin.Sequence, want)
		}
	}
	return nil
}
