package spawnlet

// manager_detach_test.go: DetachAll — the process-shutdown path (SE3 §4.1). It must leave every pod
// RUNNING (a SIGTERM'd node is upgraded, not a spawn-killer) while stopping the per-spawn journal
// watchers so nothing races process exit.

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"spawnery/internal/runtime/fakepod"
)

// DetachAll must leave every container of every spawn running and must never call pod.Stop —
// this is the load-bearing guarantee of the whole re-adoption epic.
func TestDetachAllLeavesPodsRunning(t *testing.T) {
	ctx := context.Background()
	be := fakepod.New()
	t.Cleanup(be.Close)
	m := NewManagerWithBackend(be, &fakeApplier{}, ManagerConfig{
		AgentImage: "a", SidecarImage: "s", DataRoot: t.TempDir(), NodeID: "n1",
	})

	for _, id := range []string{"sp-a", "sp-b"} {
		if _, err := m.Create(ctx, id, "../../examples/secret-app", "model", "", "", 0); err != nil {
			t.Fatalf("Create %s: %v", id, err)
		}
	}

	if n := m.DetachAll(); n != 2 {
		t.Fatalf("DetachAll returned %d, want 2", n)
	}

	for _, id := range []string{"sp-a", "sp-b"} {
		for _, role := range []string{"sandbox", "sidecar", "agent"} {
			if got := be.State(id, role); got != fakepod.StateRunning {
				t.Fatalf("after DetachAll, %s/%s = %v, want %v", id, role, got, fakepod.StateRunning)
			}
		}
	}
	for _, op := range be.Ops() {
		if strings.HasPrefix(op, "stop:") {
			t.Fatalf("DetachAll must not stop any pod, ops = %v", be.Ops())
		}
	}
	if h := be.LastStopHandle(); h != nil {
		t.Fatalf("DetachAll called pod.Stop with %+v", h)
	}
}

// DetachAll stops the continuous journal watchers: after it returns, a write into a journaled mount's
// host dir must NOT drive a RequestSnapshot (mirror of TestWatcherDrivesRequestSnapshotOnWrite, which
// proves the watcher was live before the detach).
func TestDetachAllStopsJournalWatchers(t *testing.T) {
	ctx := context.Background()
	app := writeJournalApp(t)
	fj := newFakeJournal("manifest-detach")
	be := fakepod.New()
	t.Cleanup(be.Close)
	m := NewManagerWithBackend(be, &fakeApplier{}, ManagerConfig{
		AgentImage: "a", SidecarImage: "s", DataRoot: t.TempDir(), NodeID: "n1",
	})
	m.SetJournal(fj, t.TempDir())

	sp, err := m.Create(ctx, "spw", app, "model", "", "", 0)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if len(sp.JournalMounts) != 1 {
		t.Fatalf("expected 1 journaled mount, got %d", len(sp.JournalMounts))
	}
	hostDir := sp.JournalMounts[0].HostDir

	// The watcher is live before the detach.
	if err := os.WriteFile(filepath.Join(hostDir, "before.txt"), []byte("EDIT"), 0o644); err != nil {
		t.Fatal(err)
	}
	select {
	case <-fj.requested:
	case <-time.After(5 * time.Second):
		t.Fatal("watcher did not fire before DetachAll — test setup is wrong")
	}

	if n := m.DetachAll(); n != 1 {
		t.Fatalf("DetachAll returned %d, want 1", n)
	}

	// Drain anything already queued, then write again: a stopped watcher must not fire.
	for drained := false; !drained; {
		select {
		case <-fj.requested:
		default:
			drained = true
		}
	}
	if err := os.WriteFile(filepath.Join(hostDir, "after.txt"), []byte("EDIT2"), 0o644); err != nil {
		t.Fatal(err)
	}
	select {
	case mt := <-fj.requested:
		t.Fatalf("watcher still firing after DetachAll (mount %q) — watchers were not stopped", mt.Name)
	case <-time.After(500 * time.Millisecond):
		// no snapshot requested: the watcher is stopped (the periodic fallback is 60s away)
	}
	// The pod is still running — a journaled spawn is detached, not torn down.
	if got := be.State("spw", "agent"); got != fakepod.StateRunning {
		t.Fatalf("agent state after DetachAll = %v, want %v", got, fakepod.StateRunning)
	}
}
