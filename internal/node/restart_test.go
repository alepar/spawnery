package node

// restart_test.go: the WHOLE restart cycle, end to end and hermetically (SE3 §5, bead sp-2tx8.3.6).
//
// 3.2 tested the shutdown half (detachAll leaves the pods running) and 3.4 the startup half
// (reconcileManagedPods re-adopts them). Nothing yet composes them — and the composition is the product
// claim: `systemctl restart spawnery-node` must cost zero spawns. A "restart" here is literal: the
// fakepod.Backend's container state OUTLIVES the Manager, so process #2 is just a new Manager + attacher
// over the same backend and the same data root.
//
// The four properties, in order of how much they matter:
//
//  1. detach → re-adopt round-trips a live spawn: the pod is never stopped, the agent's file content is
//     intact, the spawn is back in the store at the same generation, and the CP sees it ACTIVE again with
//     no operator action (spec §6);
//  2. a CP that is UNREACHABLE (the send itself fails — not merely silent) destroys NOTHING and does not
//     open the serve gate;
//  3. a REAP verdict captures BEFORE it destroys (assert the ops ORDER, not just the outcome);
//  4. a rebuild that fails AFTER Manager.Adopt succeeded (fault-injected Attach) falls back to
//     capture-then-destroy and leaves nothing half-adopted.

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	nodev1 "spawnery/gen/node/v1"
	"spawnery/internal/runtime/fakepod"
	"spawnery/internal/spawnlet"
)

// firstNode is the process that is ABOUT to be restarted: same shape as restartedNode (same backend, same
// data root, same node id), but with the serve gate opened so it can start spawns.
func firstNode(t *testing.T, be *fakepod.Backend, dataRoot string, fs cpStream) *attacher {
	t.Helper()
	a := restartedNode(t, be, dataRoot, fs)
	a.readopt.openGate()
	return a
}

// firstNodeCfg is firstNode with a ManagerConfig tweak (e.g. DeltaCapture).
func firstNodeCfg(t *testing.T, be *fakepod.Backend, dataRoot string, fs cpStream, opt func(*spawnlet.ManagerConfig)) *attacher {
	t.Helper()
	a := restartedNodeCfg(t, be, dataRoot, fs, opt)
	a.readopt.openGate()
	return a
}

// opIndex returns the position of entry in the ops log, or -1.
func opIndex(ops []string, entry string) int {
	for i, op := range ops {
		if op == entry {
			return i
		}
	}
	return -1
}

// hasStopOp reports whether any pod was stopped.
func hasStopOp(ops []string) bool {
	for _, op := range ops {
		if strings.HasPrefix(op, "stop:") {
			return true
		}
	}
	return false
}

// shutdown is what cmd/spawnlet/main.go does on SIGTERM: node.Run's detach (pumps/relays/sessions), then
// gracefulDetachAll (journal watchers). Neither touches the runtime.
func shutdown(a *attacher) {
	a.detachAll()
	a.mgr.DetachAll()
}

// THE test: a running spawn survives a full process restart — pod alive, content intact, ACTIVE again.
func TestRestartCycleDetachThenReadopt(t *testing.T) {
	ctx := context.Background()
	be := fakepod.New(fakepod.WithAttachScript(scriptGoose))
	t.Cleanup(be.Close)
	dataRoot := t.TempDir()
	app := writeNodeApp(t)

	// --- process #1: start a spawn and let the agent write into its mount ---
	a1 := firstNode(t, be, dataRoot, &fakeCPStream{})
	a1.startSpawn(ctx, &nodev1.StartSpawn{SpawnId: "sp1", AppRef: app, Model: "m", Generation: 1})

	sp1, ok := a1.mgr.Store().Get("sp1")
	if !ok {
		t.Fatal("setup: startSpawn did not put sp1 in the store")
	}
	if len(sp1.MountDirs) == 0 {
		t.Fatal("setup: the app has no mounts — writeNodeApp must declare one")
	}
	work := filepath.Join(sp1.MountDirs[len(sp1.MountDirs)-1], "work.txt")
	if err := os.WriteFile(work, []byte("AGENT WORK"), 0o644); err != nil {
		t.Fatalf("setup: write agent content: %v", err)
	}

	// --- SIGTERM: the pods must survive the process ---
	shutdown(a1)
	for _, role := range []string{"sandbox", "sidecar", "agent"} {
		if st := be.State("sp1", role); st != fakepod.StateRunning {
			t.Fatalf("after shutdown, sp1/%s = %v, want running — the upgrade path destroyed a spawn", role, st)
		}
	}
	if hasStopOp(be.Ops()) {
		t.Fatalf("shutdown stopped a pod, ops = %v", be.Ops())
	}

	// --- process #2: re-adopt ---
	fs := &fakeCPStream{}
	a2 := restartedNode(t, be, dataRoot, fs)
	done := make(chan struct{})
	go func() { a2.reconcileManagedPods(ctx); close(done) }()

	waitFor(t, "the managed-pods report", func() bool { return reportFrom(fs) != nil })
	rep := reportFrom(fs)
	if len(rep.GetPods()) != 1 || rep.GetPods()[0].GetSpawnId() != "sp1" || rep.GetPods()[0].GetGeneration() != 1 {
		t.Fatalf("report = %+v, want the one surviving pod sp1 gen 1", rep.GetPods())
	}
	answer(a2, rep.GetRequestId(), &nodev1.ReadoptDecision{
		SpawnId: "sp1", Generation: 1, Verdict: nodev1.ReadoptVerdict_READOPT_VERDICT_ADOPT,
		Adopt: &nodev1.AdoptSpawn{Spec: &nodev1.StartSpawn{
			SpawnId: "sp1", AppRef: app, Model: "m", Generation: 1,
		}},
	})
	<-done

	// The pod was never touched, across the WHOLE cycle.
	if hasStopOp(be.Ops()) {
		t.Fatalf("the restart cycle stopped a pod, ops = %v", be.Ops())
	}
	if st := be.State("sp1", "agent"); st != fakepod.StateRunning {
		t.Fatalf("agent state after re-adopt = %v, want running", st)
	}

	// The rebuilt Spawn: same generation, same mount dirs, and the agent's file is still there (Adopt must
	// never re-Prepare a mount — that would re-seed the dir out from under the live agent).
	sp2, ok := a2.mgr.Store().Get("sp1")
	if !ok {
		t.Fatal("sp1 was not re-adopted into the store")
	}
	if sp2.Generation != 1 {
		t.Fatalf("adopted generation = %d, want 1", sp2.Generation)
	}
	if got := sp2.MountDirs[len(sp2.MountDirs)-1]; got != sp1.MountDirs[len(sp1.MountDirs)-1] {
		t.Fatalf("adopted mount dir = %q, want %q", got, sp1.MountDirs[len(sp1.MountDirs)-1])
	}
	content, err := os.ReadFile(work)
	if err != nil || string(content) != "AGENT WORK" {
		t.Fatalf("agent content after restart = %q, %v; want %q intact", content, err, "AGENT WORK")
	}

	// The CP sees it come back on its own: ACTIVE, no operator action, and the node is serving.
	if got := lastPhase(fs.phasesFor("sp1")); got != nodev1.SpawnPhase_ACTIVE {
		t.Fatalf("last phase for sp1 = %v, want ACTIVE", got)
	}
	a2.mu.Lock()
	pumped := a2.pumps[zeroKey("sp1")] != nil
	sessions := len(a2.sessions)
	a2.mu.Unlock()
	if !pumped || sessions != 1 {
		t.Fatalf("after re-adopt: pump=%v sessions=%d, want a live pump and 1 session", pumped, sessions)
	}
	if !a2.awaitReadopt(ctx) {
		t.Fatal("the serve gate never opened after a completed handshake")
	}
}

// deadCPStream is a CP we cannot reach at all: every send fails. (fakeCPStream, by contrast, accepts the
// report and simply never answers — a different arm, covered by TestReconcileCPSilentReapsNothing.)
type deadCPStream struct{}

func (deadCPStream) Send(*nodev1.NodeMessage) error      { return errors.New("cp unreachable") }
func (deadCPStream) Receive() (*nodev1.CPMessage, error) { return nil, errors.New("cp unreachable") }

// A CP that cannot even be reached at startup must cost us ZERO pods — and must NOT open the serve gate.
func TestRestartCPUnreachableReapsNothingAndKeepsGateShut(t *testing.T) {
	ctx := context.Background()
	be := fakepod.New(fakepod.WithAttachScript(scriptGoose))
	t.Cleanup(be.Close)
	dataRoot := t.TempDir()

	a1 := firstNode(t, be, dataRoot, &fakeCPStream{})
	if _, err := a1.mgr.Create(ctx, "sp1", writeNodeApp(t), "model", "", "", 1); err != nil {
		t.Fatalf("Create: %v", err)
	}
	shutdown(a1)

	a2 := restartedNode(t, be, dataRoot, deadCPStream{})
	a2.reconcileManagedPods(ctx) // returns immediately: the report cannot be sent

	for _, role := range []string{"sandbox", "sidecar", "agent"} {
		if st := be.State("sp1", role); st != fakepod.StateRunning {
			t.Fatalf("an unreachable CP cost us sp1/%s (state %v) — a CP blip became data loss", role, st)
		}
	}
	if hasStopOp(be.Ops()) {
		t.Fatalf("reconcile destroyed a pod without a CP verdict, ops = %v", be.Ops())
	}
	if _, ok := a2.mgr.Store().Get("sp1"); ok {
		t.Fatal("the node self-adopted sp1 without a CP decision")
	}

	// The gate stays SHUT: until the CP answers, it must not be allowed to StartPod a spawn id whose pod we
	// are still holding (the CRI lane would reap the stale same-named sandbox out from under the adoption).
	gateCtx, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
	defer cancel()
	if a2.awaitReadopt(gateCtx) {
		t.Fatal("the serve gate opened with no CP answer — the node would serve StartSpawn over a held pod")
	}
}

// A stale generation is reaped — but the agent's work is CAPTURED first, so a future resume can pick it up.
// The claim under test is the ORDER: capture strictly before stop.
func TestRestartStaleGenerationCapturesBeforeReaping(t *testing.T) {
	ctx := context.Background()
	be := fakepod.New(fakepod.WithAttachScript(scriptGoose))
	t.Cleanup(be.Close)
	dataRoot := t.TempDir()
	deltaOn := func(c *spawnlet.ManagerConfig) { c.DeltaCapture = true }

	a1 := firstNodeCfg(t, be, dataRoot, &fakeCPStream{}, deltaOn)
	if _, err := a1.mgr.Create(ctx, "sp1", writeNodeApp(t), "model", "", "", 1); err != nil {
		t.Fatalf("Create: %v", err)
	}
	shutdown(a1)

	fs := &fakeCPStream{}
	a2 := restartedNodeCfg(t, be, dataRoot, fs, deltaOn)
	done := make(chan struct{})
	go func() { a2.reconcileManagedPods(ctx); close(done) }()
	waitFor(t, "the managed-pods report", func() bool { return reportFrom(fs) != nil })
	answer(a2, reportFrom(fs).GetRequestId(), &nodev1.ReadoptDecision{
		SpawnId: "sp1", Generation: 1, Verdict: nodev1.ReadoptVerdict_READOPT_VERDICT_REAP,
		Reason: "generation 1 superseded by 2",
	})
	<-done

	ops := be.Ops()
	capture, stop := opIndex(ops, "capture:sp1"), opIndex(ops, "stop:sp1")
	if capture < 0 {
		t.Fatalf("reap did not capture the agent's work first, ops = %v", ops)
	}
	if stop < 0 {
		t.Fatalf("a REAP verdict left the pod running, ops = %v", ops)
	}
	if capture > stop {
		t.Fatalf("capture ran AFTER stop (capture=%d stop=%d) — the delta is of a dead container, ops = %v",
			capture, stop, ops)
	}
	if len(be.CapturedRefs()) == 0 {
		t.Fatal("capture-before-reap produced no delta ref")
	}
	if _, ok := a2.mgr.Store().Get("sp1"); ok {
		t.Fatal("a reaped spawn is still in the store")
	}
}

// A rebuild that fails AFTER Manager.Adopt succeeded (fault-injected ACP re-dial) must: unwind the store
// entry and the pump, capture the agent's work, and only THEN destroy the pod. Nothing half-adopted.
func TestRestartRebuildFailureCapturesThenDestroys(t *testing.T) {
	ctx := context.Background()
	be := fakepod.New(fakepod.WithAttachScript(scriptGoose))
	t.Cleanup(be.Close)
	dataRoot := t.TempDir()
	deltaOn := func(c *spawnlet.ManagerConfig) { c.DeltaCapture = true }

	a1 := firstNodeCfg(t, be, dataRoot, &fakeCPStream{}, deltaOn)
	if _, err := a1.mgr.Create(ctx, "sp1", writeNodeApp(t), "model", "", "", 1); err != nil {
		t.Fatalf("Create: %v", err)
	}
	shutdown(a1)

	// The fault: the agent is there, but the node cannot re-dial its ACP.
	be.FailOn(fakepod.OpAttach, errors.New("attach boom"))

	fs := &fakeCPStream{}
	a2 := restartedNodeCfg(t, be, dataRoot, fs, deltaOn)
	app := writeNodeApp(t)
	done := make(chan struct{})
	go func() { a2.reconcileManagedPods(ctx); close(done) }()
	waitFor(t, "the managed-pods report", func() bool { return reportFrom(fs) != nil })
	answer(a2, reportFrom(fs).GetRequestId(), &nodev1.ReadoptDecision{
		SpawnId: "sp1", Generation: 1, Verdict: nodev1.ReadoptVerdict_READOPT_VERDICT_ADOPT,
		Adopt: &nodev1.AdoptSpawn{Spec: &nodev1.StartSpawn{
			SpawnId: "sp1", AppRef: app, Model: "m", Generation: 1,
		}},
	})
	<-done

	ops := be.Ops()
	capture, stop := opIndex(ops, "capture:sp1"), opIndex(ops, "stop:sp1")
	if capture < 0 || stop < 0 || capture > stop {
		t.Fatalf("a failed rebuild must capture-THEN-destroy (capture=%d stop=%d), ops = %v", capture, stop, ops)
	}
	if st := be.State("sp1", "agent"); st == fakepod.StateRunning {
		t.Fatal("a failed rebuild left the pod running (it must capture-before-reap)")
	}
	// Nothing half-adopted: no store entry, no pump, no session, no capacity slot leaked.
	if _, ok := a2.mgr.Store().Get("sp1"); ok {
		t.Fatal("a failed rebuild left a half-adopted spawn in the store")
	}
	a2.mu.Lock()
	pumps, sessions, active := len(a2.pumps), len(a2.sessions), a2.active
	a2.mu.Unlock()
	if pumps != 0 || sessions != 0 || active != 0 {
		t.Fatalf("after a failed rebuild: pumps=%d sessions=%d active=%d, want 0/0/0", pumps, sessions, active)
	}
}
