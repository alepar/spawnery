package node

// attach_detach_test.go: process-shutdown detach (SE3 §4.1/§4.6). The node closes its pumps, relays and
// session registries and EXITS WITH THE PODS RUNNING. The trap this pins: session-0's pump exitFn calls
// mgr.Stop (agent-death reclaim), which would destroy the very pod we are trying to preserve.

import (
	"context"
	"strings"
	"testing"
	"time"

	nodev1 "spawnery/gen/node/v1"
	"spawnery/internal/runtime/fakepod"
)

func TestDetachAllClosesPumpsAndLeavesPodRunning(t *testing.T) {
	be := fakeBackend(t, fakepod.WithAttachScript(scriptGoose))
	mgr := newGooseManager(t, be)
	cp := &fakeCPStream{}
	a := newAttacher(mgr, cp)

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

	// The strongest check: exitFn itself must never have run. Deleting the pump from a.pumps before
	// closing it already makes exitFn's internal mgr.Stop guard a no-op (the "mine" check fails), so
	// "the pod is still running" alone does NOT prove exitFn didn't fire — exitFn also reports ERROR
	// over the CP stream, asynchronously (readLoop's goroutine notices the closed conn on its own
	// schedule). A regression that closes the pump conn WITHOUT going through Pump.stop() (e.g. calling
	// att.Close directly) still leaves the pod running — be.State alone would not catch it — but does
	// spuriously mark the spawn ERROR on every graceful restart, on a delay: poll instead of a single
	// immediate check, which would race the exitFn goroutine and pass even against that regression.
	deadline := time.Now().Add(500 * time.Millisecond)
	for {
		phases := cp.phasesFor("sp1")
		for _, ph := range phases {
			if ph == nodev1.SpawnPhase_ERROR {
				t.Fatalf("detachAll caused exitFn to report ERROR for sp1, phases = %v", phases)
			}
		}
		if time.Now().After(deadline) {
			break // no ERROR ever showed up: exitFn did not fire
		}
		time.Sleep(20 * time.Millisecond)
	}
}
