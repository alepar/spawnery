package node

// readopt_test.go: the node side of the spawnlet-restart re-adoption handshake (SE3 §4.2/§4.3). A
// "restart" is hermetic here: a second Manager + attacher over the SAME fakepod backend, whose container
// state outlives them both. The properties under test, in order of how much they matter:
//
//  1. a CP that never answers destroys NOTHING (a CP blip must never become data loss);
//  2. an ADOPT verdict rebuilds the spawn, re-dials its ACP and reports it Active — pod untouched;
//  3. a REAP verdict capture-before-reaps;
//  4. a rebuild failure falls back to capture-before-reap, leaving nothing half-adopted;
//  5. another node's pod is neither adopted nor reaped.

import (
	"context"
	"errors"
	"testing"
	"time"

	nodev1 "spawnery/gen/node/v1"
	"spawnery/internal/runtime/fakepod"
	"spawnery/internal/spawnlet"
)

// reportFrom returns the ManagedPodsReport the attacher sent to the CP (nil until it does).
func reportFrom(fs *fakeCPStream) *nodev1.ManagedPodsReport {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	for _, m := range fs.sent {
		if r := m.GetManagedPods(); r != nil {
			return r
		}
	}
	return nil
}

// restartedNodeCfg builds the "second process": a fresh Manager + attacher over the same backend and data
// root, with a short readopt timeout so the CP-unreachable arm does not take a minute. opt lets a caller
// tweak the ManagerConfig (e.g. DeltaCapture) before the Manager is built.
func restartedNodeCfg(t *testing.T, be *fakepod.Backend, dataRoot string, fs cpStream, opt func(*spawnlet.ManagerConfig)) *attacher {
	t.Helper()
	cfg := spawnlet.ManagerConfig{AgentImage: "a", SidecarImage: "s", DataRoot: dataRoot, NodeID: "n1"}
	if opt != nil {
		opt(&cfg)
	}
	mgr := spawnlet.NewManagerWithBackend(be, noopApplier{}, cfg)
	a := newAttacher(mgr, fs)
	a.cfg.NodeID = "n1"
	a.sx = &fakeSessionExec{}
	a.readopt = newReadoptState()
	a.readopt.timeout = 300 * time.Millisecond
	return a
}

// restartedNode is restartedNodeCfg with the default ManagerConfig (no tweaks).
func restartedNode(t *testing.T, be *fakepod.Backend, dataRoot string, fs cpStream) *attacher {
	t.Helper()
	return restartedNodeCfg(t, be, dataRoot, fs, nil)
}

// answer drives the CP's reply into the attacher's receive path (what runOnce's loop does in production).
func answer(a *attacher, reqID string, decs ...*nodev1.ReadoptDecision) {
	a.handle(context.Background(), &nodev1.CPMessage{Msg: &nodev1.CPMessage_ReadoptDecisions{
		ReadoptDecisions: &nodev1.ManagedPodsDecisions{RequestId: reqID, Decisions: decs},
	}})
}

// THE acceptance criterion: a CP that never answers must cost us ZERO pods.
func TestReconcileCPSilentReapsNothing(t *testing.T) {
	ctx := context.Background()
	be := fakepod.New(fakepod.WithAttachScript(scriptGoose))
	t.Cleanup(be.Close)
	dataRoot := t.TempDir()

	first := restartedNode(t, be, dataRoot, &fakeCPStream{})
	if _, err := first.mgr.Create(ctx, "sp1", writeNodeApp(t), "model", "", "", 1); err != nil {
		t.Fatalf("Create: %v", err)
	}

	a := restartedNode(t, be, dataRoot, &fakeCPStream{}) // the CP never answers
	a.reconcileManagedPods(ctx)

	for _, role := range []string{"sandbox", "sidecar", "agent"} {
		if st := be.State("sp1", role); st != fakepod.StateRunning {
			t.Fatalf("a silent CP cost us sp1/%s (state %v) — a CP blip became data loss", role, st)
		}
	}
	for _, op := range be.Ops() {
		if op == "stop:sp1" {
			t.Fatalf("reconcile stopped a pod without a CP verdict, ops = %v", be.Ops())
		}
	}
	// ...and the node serves anyway: the gate is open, so a StartSpawn is not deadlocked forever.
	if !a.awaitReadopt(ctx) {
		t.Fatal("the serve gate never opened after the readopt timeout")
	}
}

// ADOPT: the spawn is rebuilt, its ACP re-dialled, and reported ACTIVE — with the pod never touched.
func TestReconcileAdoptsRunningPod(t *testing.T) {
	ctx := context.Background()
	be := fakepod.New(fakepod.WithAttachScript(scriptGoose))
	t.Cleanup(be.Close)
	dataRoot := t.TempDir()
	app := writeNodeApp(t)

	first := restartedNode(t, be, dataRoot, &fakeCPStream{})
	if _, err := first.mgr.Create(ctx, "sp1", app, "model", "", "", 1); err != nil {
		t.Fatalf("Create: %v", err)
	}

	fs := &fakeCPStream{}
	a := restartedNode(t, be, dataRoot, fs)
	done := make(chan struct{})
	go func() { a.reconcileManagedPods(ctx); close(done) }()

	waitFor(t, "the managed-pods report", func() bool { return reportFrom(fs) != nil })
	rep := reportFrom(fs)
	if len(rep.GetPods()) != 1 || rep.GetPods()[0].GetSpawnId() != "sp1" || rep.GetPods()[0].GetGeneration() != 1 {
		t.Fatalf("report = %+v, want sp1 gen 1", rep.GetPods())
	}
	answer(a, rep.GetRequestId(), &nodev1.ReadoptDecision{
		SpawnId: "sp1", Generation: 1, Verdict: nodev1.ReadoptVerdict_READOPT_VERDICT_ADOPT,
		Adopt: &nodev1.AdoptSpawn{Spec: &nodev1.StartSpawn{
			SpawnId: "sp1", AppRef: app, Model: "model", Generation: 1,
		}},
	})
	<-done

	if st := be.State("sp1", "agent"); st != fakepod.StateRunning {
		t.Fatalf("adopted agent state = %v, want running", st)
	}
	if inv := a.mgr.RunningInventory(); len(inv) != 1 || inv[0].SpawnID != "sp1" {
		t.Fatalf("RunningInventory after adopt = %+v", inv)
	}
	a.mu.Lock()
	pumped := a.pumps[zeroKey("sp1")] != nil
	sessions := len(a.sessions)
	active := a.active
	a.mu.Unlock()
	if !pumped || sessions != 1 || active != 1 {
		t.Fatalf("after adopt: pump=%v sessions=%d active=%d, want a live pump, 1 session, 1 active slot",
			pumped, sessions, active)
	}
	if got := lastPhase(fs.phasesFor("sp1")); got != nodev1.SpawnPhase_ACTIVE {
		t.Fatalf("last phase for sp1 = %v, want ACTIVE (the CP must see it come back on its own)", got)
	}
}

// REAP: capture-before-reap, then the pod is gone.
func TestReconcileReapsOnReapVerdict(t *testing.T) {
	ctx := context.Background()
	be := fakepod.New(fakepod.WithAttachScript(scriptGoose))
	t.Cleanup(be.Close)
	dataRoot := t.TempDir()

	first := restartedNodeCfg(t, be, dataRoot, &fakeCPStream{}, func(c *spawnlet.ManagerConfig) { c.DeltaCapture = true })
	if _, err := first.mgr.Create(ctx, "sp1", writeNodeApp(t), "model", "", "", 1); err != nil {
		t.Fatalf("Create: %v", err)
	}

	fs := &fakeCPStream{}
	a := restartedNodeCfg(t, be, dataRoot, fs, func(c *spawnlet.ManagerConfig) { c.DeltaCapture = true })
	done := make(chan struct{})
	go func() { a.reconcileManagedPods(ctx); close(done) }()
	waitFor(t, "the managed-pods report", func() bool { return reportFrom(fs) != nil })
	answer(a, reportFrom(fs).GetRequestId(), &nodev1.ReadoptDecision{
		SpawnId: "sp1", Generation: 1, Verdict: nodev1.ReadoptVerdict_READOPT_VERDICT_REAP,
		Reason: "generation 1 superseded by 2",
	})
	<-done

	if st := be.State("sp1", "agent"); st == fakepod.StateRunning {
		t.Fatal("a REAP verdict left the pod running")
	}
	if _, ok := a.mgr.Store().Get("sp1"); ok {
		t.Fatal("a reaped spawn is in the store")
	}
}

// An ADOPT whose rebuild cannot complete falls back to capture-before-reap — and leaves nothing behind.
func TestReconcileRebuildFailureFallsBackToReap(t *testing.T) {
	ctx := context.Background()
	be := fakepod.New(fakepod.WithAttachScript(scriptGoose))
	t.Cleanup(be.Close)
	dataRoot := t.TempDir()

	first := restartedNode(t, be, dataRoot, &fakeCPStream{})
	if _, err := first.mgr.Create(ctx, "sp1", writeNodeApp(t), "model", "", "", 1); err != nil {
		t.Fatalf("Create: %v", err)
	}

	fs := &fakeCPStream{}
	a := restartedNode(t, be, dataRoot, fs)
	done := make(chan struct{})
	go func() { a.reconcileManagedPods(ctx); close(done) }()
	waitFor(t, "the managed-pods report", func() bool { return reportFrom(fs) != nil })
	// An adopt spec whose app ref does not exist: the manifest cannot be parsed => rebuild failure.
	answer(a, reportFrom(fs).GetRequestId(), &nodev1.ReadoptDecision{
		SpawnId: "sp1", Generation: 1, Verdict: nodev1.ReadoptVerdict_READOPT_VERDICT_ADOPT,
		Adopt: &nodev1.AdoptSpawn{Spec: &nodev1.StartSpawn{
			SpawnId: "sp1", AppRef: "/nonexistent/app", Model: "model", Generation: 1,
		}},
	})
	<-done

	if st := be.State("sp1", "agent"); st == fakepod.StateRunning {
		t.Fatal("a failed rebuild left the pod running (it must capture-before-reap)")
	}
	if _, ok := a.mgr.Store().Get("sp1"); ok {
		t.Fatal("a failed rebuild left a half-adopted spawn in the store")
	}
	a.mu.Lock()
	pumps := len(a.pumps)
	a.mu.Unlock()
	if pumps != 0 {
		t.Fatalf("a failed rebuild left %d pump(s) registered", pumps)
	}
}

// adoptPod re-registers the spawn's GitHub link(s) with the proactive refresher from the CP's
// re-delivered mount table (sp-2tx8.3.5 D5) — the refresher's state is in-memory and died with the
// previous process, so without this an adopted spawn's GetToken returns ErrGitHubNotLinked.
func TestAdoptPodReRegistersGitHubLink(t *testing.T) {
	ctx := context.Background()
	be := fakepod.New(fakepod.WithAttachScript(scriptGoose))
	t.Cleanup(be.Close)
	dataRoot := t.TempDir()
	app := writeNodeApp(t)

	first := restartedNode(t, be, dataRoot, &fakeCPStream{})
	if _, err := first.mgr.Create(ctx, "sp1", app, "model", "", "", 1); err != nil {
		t.Fatalf("Create: %v", err)
	}

	fs := &fakeCPStream{}
	a := restartedNode(t, be, dataRoot, fs)
	a.githubRefresh = newGitHubRefresher(freshFakeMintClient("tok", time.Now().Add(2*time.Hour).Unix()))

	pods, err := a.mgr.UntrackedPods(ctx)
	if err != nil || len(pods) != 1 {
		t.Fatalf("UntrackedPods = %+v, %v", pods, err)
	}

	// Before adopt: no link registered (as if the process just started).
	if _, _, err := a.githubRefresh.GetToken(ctx, "sp1", 300, false); err == nil {
		t.Fatal("setup: expected ErrGitHubNotLinked before adopt")
	}

	if err := a.adoptPod(ctx, pods[0], &nodev1.AdoptSpawn{Spec: &nodev1.StartSpawn{
		SpawnId: "sp1", AppRef: app, Model: "model", Generation: 1,
		Mounts: []*nodev1.MountBinding{
			{Name: "main", RepositoryId: "owner/repo", GithubMintRef: &nodev1.GitHubMintRef{SecretId: "gh:owner"}},
		},
	}}); err != nil {
		t.Fatalf("adoptPod: %v", err)
	}

	if _, _, err := a.githubRefresh.GetToken(ctx, "sp1", 300, false); err != nil {
		t.Fatalf("GetToken after adopt: %v (the link was not re-registered)", err)
	}
}

// A mount with no mint ref has nothing to re-register; adoptPod must not fail or fabricate a link.
func TestAdoptPodNoMintRefNotesNothing(t *testing.T) {
	ctx := context.Background()
	be := fakepod.New(fakepod.WithAttachScript(scriptGoose))
	t.Cleanup(be.Close)
	dataRoot := t.TempDir()
	app := writeNodeApp(t)

	first := restartedNode(t, be, dataRoot, &fakeCPStream{})
	if _, err := first.mgr.Create(ctx, "sp1", app, "model", "", "", 1); err != nil {
		t.Fatalf("Create: %v", err)
	}

	fs := &fakeCPStream{}
	a := restartedNode(t, be, dataRoot, fs)
	a.githubRefresh = newGitHubRefresher(freshFakeMintClient("tok", time.Now().Add(2*time.Hour).Unix()))

	pods, err := a.mgr.UntrackedPods(ctx)
	if err != nil || len(pods) != 1 {
		t.Fatalf("UntrackedPods = %+v, %v", pods, err)
	}

	if err := a.adoptPod(ctx, pods[0], &nodev1.AdoptSpawn{Spec: &nodev1.StartSpawn{
		SpawnId: "sp1", AppRef: app, Model: "model", Generation: 1,
		Mounts: []*nodev1.MountBinding{{Name: "main"}}, // no GithubMintRef
	}}); err != nil {
		t.Fatalf("adoptPod: %v", err)
	}

	if _, _, err := a.githubRefresh.GetToken(ctx, "sp1", 300, false); !errors.Is(err, ErrGitHubNotLinked) {
		t.Fatalf("GetToken after adopt with no mint ref: err = %v, want ErrGitHubNotLinked", err)
	}
}

// DEFER (and an unknown/absent decision) leaves the pod running, unsupervised, for the next attempt.
func TestReconcileDeferLeavesPodRunning(t *testing.T) {
	ctx := context.Background()
	be := fakepod.New(fakepod.WithAttachScript(scriptGoose))
	t.Cleanup(be.Close)
	dataRoot := t.TempDir()

	first := restartedNode(t, be, dataRoot, &fakeCPStream{})
	if _, err := first.mgr.Create(ctx, "sp1", writeNodeApp(t), "model", "", "", 1); err != nil {
		t.Fatalf("Create: %v", err)
	}

	fs := &fakeCPStream{}
	a := restartedNode(t, be, dataRoot, fs)
	done := make(chan struct{})
	go func() { a.reconcileManagedPods(ctx); close(done) }()
	waitFor(t, "the managed-pods report", func() bool { return reportFrom(fs) != nil })
	answer(a, reportFrom(fs).GetRequestId(), &nodev1.ReadoptDecision{
		SpawnId: "sp1", Generation: 1, Verdict: nodev1.ReadoptVerdict_READOPT_VERDICT_DEFER,
		Reason: "spawn is mid-transition",
	})
	<-done

	if st := be.State("sp1", "agent"); st != fakepod.StateRunning {
		t.Fatalf("DEFER stopped the pod (state %v) — DEFER means LEAVE IT RUNNING", st)
	}
	if _, ok := a.mgr.Store().Get("sp1"); ok {
		t.Fatal("DEFER adopted the spawn")
	}
}
