package spawnlet

// adopt_test.go: Manager.Adopt — rebuilding the in-memory Spawn for a pod that survived a spawnlet
// restart (SE3 §4.2). "Restart" here is literal and hermetic: a SECOND Manager over the SAME fakepod
// backend, whose container state outlives the first Manager.

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"spawnery/internal/runtime"
	"spawnery/internal/runtime/fakepod"
	"spawnery/internal/spawnlet/firewall"
)

// recordingApplier records the egress-floor calls in order, so a test can pin Remove-then-Apply.
type recordingApplier struct{ calls []string }

func (a *recordingApplier) Apply(_ context.Context, rules []firewall.Rule) error {
	a.calls = append(a.calls, "apply")
	return nil
}
func (a *recordingApplier) Remove(_ context.Context, rules []firewall.Rule) error {
	a.calls = append(a.calls, "remove")
	return nil
}

// restart returns a NEW Manager over the same backend + data root — the hermetic equivalent of
// `systemctl restart spawnery-spawnlet`: fresh in-memory store, same containers, same node-local dirs.
func restart(t *testing.T, be runtime.PodBackend, fw firewall.Applier, dataRoot string) *Manager {
	t.Helper()
	return NewManagerWithBackend(be, fw, ManagerConfig{
		AgentImage: "a", SidecarImage: "s", DataRoot: dataRoot, NodeID: "n1",
	})
}

// The adopted Spawn must be complete enough to drive every later lifecycle op: the container ids and pod
// IP from the runtime, the mount table re-derived from the manifest + the CP's spec, and — critically —
// the mount host dirs must still hold the agent's data (Adopt must never re-Prepare them).
func TestAdoptRebuildsSpawnWithoutRepreparingMounts(t *testing.T) {
	ctx := context.Background()
	be := fakepod.New()
	t.Cleanup(be.Close)
	dataRoot := t.TempDir()

	m1 := restart(t, be, &recordingApplier{}, dataRoot)
	sp, err := m1.Create(ctx, "sp1", "../../examples/secret-app", "model", "friendly", "app1", 3)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if len(sp.MountDirs) == 0 {
		t.Fatal("setup: the app has no mounts — pick an app that does")
	}
	// The agent's work, on the host side of a live bind mount.
	work := filepath.Join(sp.MountDirs[len(sp.MountDirs)-1], "work.txt")
	if err := os.WriteFile(work, []byte("AGENT WORK"), 0o644); err != nil {
		t.Fatal(err)
	}

	// --- the restart ---
	m2 := restart(t, be, &recordingApplier{}, dataRoot)
	pods, err := m2.UntrackedPods(ctx)
	if err != nil || len(pods) != 1 {
		t.Fatalf("UntrackedPods = %+v, %v; want the one surviving pod", pods, err)
	}

	got, err := m2.Adopt(ctx, pods[0], AdoptSpec{
		AppRef: "../../examples/secret-app", Model: "model", Name: "friendly", AppID: "app1",
	})
	if err != nil {
		t.Fatalf("Adopt: %v", err)
	}

	if got.ID != "sp1" || got.Generation != 3 {
		t.Fatalf("adopted spawn = %s gen %d, want sp1 gen 3", got.ID, got.Generation)
	}
	if got.AgentID != pods[0].AgentID || got.SidecarID != pods[0].SidecarID || got.PodIP != pods[0].PodIP {
		t.Fatalf("adopted ids/IP = %+v, want %+v", got, pods[0])
	}
	if len(got.MountDirs) != len(sp.MountDirs) {
		t.Fatalf("adopted MountDirs = %v, want the same table as Create's %v", got.MountDirs, sp.MountDirs)
	}
	for i := range sp.MountDirs {
		if got.MountDirs[i] != sp.MountDirs[i] {
			t.Fatalf("adopted MountDirs[%d] = %q, want %q", i, got.MountDirs[i], sp.MountDirs[i])
		}
	}
	if len(got.MountTargets) != len(sp.MountTargets) {
		t.Fatalf("adopted MountTargets = %v, want Create's %v (the delta-scrub guard reads these)",
			got.MountTargets, sp.MountTargets)
	}
	if len(got.MountFinalizers) != len(sp.MountFinalizers) {
		t.Fatalf("adopted MountFinalizers = %d, want %d (teardown finalizes through them)",
			len(got.MountFinalizers), len(sp.MountFinalizers))
	}
	// The agent's data is UNTOUCHED — Adopt must not have re-Prepared (re-seeded) the mount.
	b, err := os.ReadFile(work)
	if err != nil || string(b) != "AGENT WORK" {
		t.Fatalf("mount data after adopt = %q, %v — Adopt re-prepared a live mount", b, err)
	}
	// The pod was never touched.
	for _, role := range []string{"sandbox", "sidecar", "agent"} {
		if st := be.State("sp1", role); st != fakepod.StateRunning {
			t.Fatalf("after Adopt, sp1/%s = %v, want running", role, st)
		}
	}
	// And the Manager tracks it again: the CP's next heartbeat carries it.
	if inv := m2.RunningInventory(); len(inv) != 1 || inv[0].SpawnID != "sp1" || inv[0].Generation != 3 {
		t.Fatalf("RunningInventory after adopt = %+v", inv)
	}
}

// The egress floor is re-established as Remove-then-Apply: the previous process never removed its rules
// (DetachAll deliberately doesn't), and both appliers insert with `-I`, so a bare re-Apply would leave a
// DUPLICATE rule set behind after teardown's single `-D` pass (plan decision 2). FloorIP must be recorded
// so teardown can remove it at all.
func TestAdoptReestablishesEgressFloorExactlyOnce(t *testing.T) {
	ctx := context.Background()
	be := fakepod.New()
	t.Cleanup(be.Close)
	dataRoot := t.TempDir()
	fw := &recordingApplier{}

	m1 := NewManagerWithBackend(be, fw, ManagerConfig{
		AgentImage: "a", SidecarImage: "s", DataRoot: dataRoot, NodeID: "n1",
		NodeClass: "cloud", EgressEnforce: true,
	})
	if _, err := m1.Create(ctx, "sp1", "../../examples/secret-app", "model", "", "", 1); err != nil {
		t.Fatalf("Create: %v", err)
	}
	fw2 := &recordingApplier{}
	m2 := NewManagerWithBackend(be, fw2, ManagerConfig{
		AgentImage: "a", SidecarImage: "s", DataRoot: dataRoot, NodeID: "n1",
		NodeClass: "cloud", EgressEnforce: true,
	})
	pods, err := m2.UntrackedPods(ctx)
	if err != nil || len(pods) != 1 {
		t.Fatalf("UntrackedPods = %+v, %v", pods, err)
	}
	sp, err := m2.Adopt(ctx, pods[0], AdoptSpec{AppRef: "../../examples/secret-app", Model: "model"})
	if err != nil {
		t.Fatalf("Adopt: %v", err)
	}
	if got := fw2.calls; len(got) != 2 || got[0] != "remove" || got[1] != "apply" {
		t.Fatalf("floor calls on adopt = %v, want [remove apply] (exactly one rule set survives)", got)
	}
	if sp.FloorIP != pods[0].PodIP || sp.FloorIP == "" {
		t.Fatalf("FloorIP = %q, want the pod IP %q (teardown removes by it)", sp.FloorIP, pods[0].PodIP)
	}
}

// A rebuild that cannot complete leaves NOTHING behind: no store entry, no floor. The caller then
// capture-before-reaps. (Here the app ref is gone — the manifest cannot be parsed.)
func TestAdoptFailureLeavesNoHalfAdoptedState(t *testing.T) {
	ctx := context.Background()
	be := fakepod.New()
	t.Cleanup(be.Close)
	dataRoot := t.TempDir()
	fw := &recordingApplier{}

	m1 := restart(t, be, fw, dataRoot)
	if _, err := m1.Create(ctx, "sp1", "../../examples/secret-app", "model", "", "", 1); err != nil {
		t.Fatalf("Create: %v", err)
	}
	m2 := restart(t, be, fw, dataRoot)
	pods, _ := m2.UntrackedPods(ctx)

	if _, err := m2.Adopt(ctx, pods[0], AdoptSpec{AppRef: filepath.Join(dataRoot, "no-such-app"), Model: "m"}); err == nil {
		t.Fatal("Adopt with a missing app ref must fail")
	}
	if _, ok := m2.Store().Get("sp1"); ok {
		t.Fatal("a failed Adopt left the spawn in the store — half-adopted state")
	}
	if inv := m2.RunningInventory(); len(inv) != 0 {
		t.Fatalf("RunningInventory after a failed adopt = %+v, want empty", inv)
	}
}

// The node re-checks the CP's fence: a decision for a generation that is not the one on the pod is not
// adoptable (plan decision 4).
func TestAdoptRejectsGenerationMismatch(t *testing.T) {
	ctx := context.Background()
	be := fakepod.New()
	t.Cleanup(be.Close)
	dataRoot := t.TempDir()

	m1 := restart(t, be, &recordingApplier{}, dataRoot)
	if _, err := m1.Create(ctx, "sp1", "../../examples/secret-app", "model", "", "", 2); err != nil {
		t.Fatalf("Create: %v", err)
	}
	m2 := restart(t, be, &recordingApplier{}, dataRoot)
	pods, _ := m2.UntrackedPods(ctx)
	mp := pods[0]
	mp.Generation = 99 // the pod's labels say 2; pretend the caller handed us a mismatched fence

	if _, err := m2.Adopt(ctx, mp, AdoptSpec{AppRef: "../../examples/secret-app", Model: "model", Generation: 2}); err == nil {
		t.Fatal("Adopt must reject a spec whose generation disagrees with the observed pod")
	}
}
