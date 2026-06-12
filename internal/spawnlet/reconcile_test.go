package spawnlet

import (
	"context"
	"testing"

	"spawnery/internal/runtime"
)

// TestCreateLabelsInventoryAndReap covers sp-8hf: Create stamps managed/spawn-id/generation/node-id
// labels on both pod containers; RunningInventory reflects the live spawn; ReapOrphans keeps a
// spawn that's still in the store and reaps one that isn't (a leftover from a previous node process).
func TestCreateLabelsInventoryAndReap(t *testing.T) {
	rt := runtime.NewFake()
	m := NewManager(rt, ManagerConfig{AgentImage: "a", SidecarImage: "s", DataRoot: t.TempDir(), NodeID: "node-9"})
	ctx := context.Background()

	sp, err := m.Create(ctx, "spawn-A", "../../examples/secret-app", "model", "", "", 7)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if sp.Generation != 7 {
		t.Fatalf("Generation = %d, want 7", sp.Generation)
	}

	// Both containers (sidecar + agent) carry the reconcile labels.
	if len(rt.Started) != 2 {
		t.Fatalf("want 2 containers started, got %d", len(rt.Started))
	}
	roles := map[string]bool{}
	for _, c := range rt.Started {
		if c.Labels[runtime.LabelManaged] != "true" || c.Labels[runtime.LabelSpawnID] != "spawn-A" ||
			c.Labels[runtime.LabelGeneration] != "7" || c.Labels[runtime.LabelNodeID] != "node-9" {
			t.Fatalf("container missing/wrong labels: %v", c.Labels)
		}
		roles[c.Labels[runtime.LabelRole]] = true
	}
	if !roles["sidecar"] || !roles["agent"] {
		t.Fatalf("want both sidecar+agent roles, got %v", roles)
	}

	// RunningInventory reflects the live spawn (id + generation).
	inv := m.RunningInventory()
	if len(inv) != 1 || inv[0].SpawnID != "spawn-A" || inv[0].Generation != 7 {
		t.Fatalf("RunningInventory = %+v", inv)
	}

	// ReapOrphans with the spawn still tracked stops nothing.
	stoppedBefore := len(rt.Stopped)
	if err := m.ReapOrphans(ctx); err != nil {
		t.Fatalf("ReapOrphans: %v", err)
	}
	if len(rt.Stopped) != stoppedBefore {
		t.Fatalf("ReapOrphans stopped a still-managed spawn (stopped=%v)", rt.Stopped)
	}

	// Simulate a node restart: the in-mem store forgets the spawn, but the runtime still has the
	// containers -> ReapOrphans must reap both.
	m.store.Delete("spawn-A")
	if err := m.ReapOrphans(ctx); err != nil {
		t.Fatalf("ReapOrphans (orphan): %v", err)
	}
	stopped := 0
	for id := range rt.Stopped {
		if rt.Stopped[id] {
			stopped++
		}
	}
	if stopped != 2 {
		t.Fatalf("ReapOrphans should have reaped both containers; stopped=%v", rt.Stopped)
	}
	// A second pass is a no-op (they're gone from the inventory now).
	if err := m.ReapOrphans(ctx); err != nil {
		t.Fatalf("ReapOrphans (second pass): %v", err)
	}
}

// TestReapOrphansScopedToNodeID covers the shared-daemon hazard (sp-5v03 fallout): a second
// spawnlet on the same Docker daemon (dev stack + e2e run, multi-node host) must NOT reap
// another node's live pods — only pods carrying its own node-id label (or none, pre-label).
func TestReapOrphansScopedToNodeID(t *testing.T) {
	rt := runtime.NewFake()
	ctx := context.Background()

	m1 := NewManager(rt, ManagerConfig{AgentImage: "a", SidecarImage: "s", DataRoot: t.TempDir(), NodeID: "node-9"})
	if _, err := m1.Create(ctx, "spawn-A", "../../examples/secret-app", "model", "", "", 1); err != nil {
		t.Fatalf("Create: %v", err)
	}

	// A different node's manager (empty store) sharing the same runtime: spawn-A is not in ITS
	// store, but it is node-9's pod — reap must skip it.
	m2 := NewManager(rt, ManagerConfig{AgentImage: "a", SidecarImage: "s", DataRoot: t.TempDir(), NodeID: "node-other"})
	if err := m2.ReapOrphans(ctx); err != nil {
		t.Fatalf("ReapOrphans: %v", err)
	}
	for id, stopped := range rt.Stopped {
		if stopped {
			t.Fatalf("a foreign node reaped node-9's container %s", id)
		}
	}

	// node-9's own restarted manager (empty store, same node id) DOES reap it.
	m3 := NewManager(rt, ManagerConfig{AgentImage: "a", SidecarImage: "s", DataRoot: t.TempDir(), NodeID: "node-9"})
	if err := m3.ReapOrphans(ctx); err != nil {
		t.Fatalf("ReapOrphans (own restart): %v", err)
	}
	stopped := 0
	for _, s := range rt.Stopped {
		if s {
			stopped++
		}
	}
	if stopped != 2 {
		t.Fatalf("restarted node-9 should reap its own 2 containers; stopped=%v", rt.Stopped)
	}
}
