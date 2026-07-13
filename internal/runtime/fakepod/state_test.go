package fakepod_test

import (
	"testing"

	"spawnery/internal/runtime/fakepod"
)

// The state machine is exercised end-to-end through the Backend in later tasks; this pins the
// exported State values, which the contract suite and the spawnlet tests assert against.
func TestStateValues(t *testing.T) {
	got := []fakepod.State{
		fakepod.StateAbsent, fakepod.StateCreated, fakepod.StateRunning,
		fakepod.StatePaused, fakepod.StateStopped, fakepod.StateRemoved,
	}
	want := []string{"absent", "created", "running", "paused", "stopped", "removed"}
	for i, s := range got {
		if string(s) != want[i] {
			t.Fatalf("State[%d] = %q, want %q", i, s, want[i])
		}
	}
}
