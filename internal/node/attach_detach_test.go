package node

// attach_detach_test.go: process-shutdown detach (SE3 §4.1/§4.6). The node closes its pumps, relays and
// session registries and EXITS WITH THE PODS RUNNING. The trap this pins: session-0's pump exitFn calls
// mgr.Stop (agent-death reclaim), which would destroy the very pod we are trying to preserve.

import (
	"context"
	"strings"
	"testing"

	nodev1 "spawnery/gen/node/v1"
	"spawnery/internal/runtime/fakepod"
)

func TestDetachAllClosesPumpsAndLeavesPodRunning(t *testing.T) {
	be := fakeBackend(t, fakepod.WithAttachScript(scriptGoose))
	mgr := newGooseManager(t, be)
	a := newAttacher(mgr, &fakeCPStream{})

	a.startSpawn(context.Background(), &nodev1.StartSpawn{SpawnId: "sp1", AppRef: writeNodeApp(t), Model: "m"})
	a.mu.Lock()
	pumped := a.pumps[zeroKey("sp1")] != nil
	a.mu.Unlock()
	if !pumped {
		t.Fatal("setup: pump not registered after startSpawn")
	}

	if n := a.detachAll(); n != 1 {
		t.Fatalf("detachAll closed %d pump(s), want 1", n)
	}

	// The node's per-connection state is gone...
	a.mu.Lock()
	np, nr, ns := len(a.pumps), len(a.tmuxRelays), len(a.sessions)
	a.mu.Unlock()
	if np != 0 || nr != 0 || ns != 0 {
		t.Fatalf("after detachAll: pumps=%d relays=%d sessions=%d, want 0/0/0", np, nr, ns)
	}

	// ...and the pod is STILL RUNNING: the pump's exitFn (which calls mgr.Stop) must not have fired.
	if got := be.State("sp1", "agent"); got != fakepod.StateRunning {
		t.Fatalf("agent state after detachAll = %v, want %v (exitFn reclaimed the pod)", got, fakepod.StateRunning)
	}
	for _, op := range be.Ops() {
		if strings.HasPrefix(op, "stop:") {
			t.Fatalf("detachAll must not stop the pod, ops = %v", be.Ops())
		}
	}
	// The Manager still tracks the spawn: detach relinquishes supervision, it does not delete.
	if inv := mgr.RunningInventory(); len(inv) != 1 {
		t.Fatalf("RunningInventory after detachAll = %+v, want 1 spawn", inv)
	}
}
