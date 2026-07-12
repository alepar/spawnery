package spawnlet

import (
	"context"
	"testing"

	"spawnery/internal/runtime"
)

// StopAll tears down every spawn the Manager tracks — the destructive bulk path. NOTE: since SE3
// (sp-2tx8.3.2) this is NOT what a SIGTERM'd node runs; process shutdown goes through DetachAll and
// leaves the pods running. This test pins that StopAll itself still destroys.
func TestStopAllTearsDownEverySpawn(t *testing.T) {
	rt := runtime.NewFake()
	m := NewManager(rt, ManagerConfig{AgentImage: "a", SidecarImage: "s", DataRoot: t.TempDir(), NodeID: "n"})
	ctx := context.Background()

	for _, id := range []string{"sp-a", "sp-b"} {
		if _, err := m.Create(ctx, id, "../../examples/secret-app", "model", "", "", 0); err != nil {
			t.Fatalf("Create %s: %v", id, err)
		}
	}

	if n := m.StopAll(ctx); n != 2 {
		t.Fatalf("StopAll returned %d, want 2", n)
	}

	// Every container of both spawns is torn down...
	stopped := 0
	for _, gone := range rt.Stopped {
		if gone {
			stopped++
		}
	}
	if stopped != 4 { // 2 spawns x (sidecar + agent)
		t.Fatalf("want all 4 containers stopped, got %d (stopped=%v)", stopped, rt.Stopped)
	}
	// ...and the store is empty (no inventory left to report).
	if inv := m.RunningInventory(); len(inv) != 0 {
		t.Fatalf("RunningInventory after StopAll = %+v, want empty", inv)
	}
}
