package node

import (
	"context"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	nodev1 "spawnery/gen/node/v1"
	"spawnery/internal/spawnlet"
	"spawnery/internal/storage/journal"
)

func lastForkSameNodeComplete(f *fakeCPStream) *nodev1.ForkSameNodeComplete {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := len(f.sent) - 1; i >= 0; i-- {
		if fc := f.sent[i].GetForkSameNodeComplete(); fc != nil {
			return fc
		}
	}
	return nil
}

func lastUnpauseIfPausedComplete(f *fakeCPStream) *nodev1.UnpauseIfPausedComplete {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := len(f.sent) - 1; i >= 0; i-- {
		if fc := f.sent[i].GetUnpauseIfPausedComplete(); fc != nil {
			return fc
		}
	}
	return nil
}

func lastForkTurnBoundaryComplete(f *fakeCPStream) *nodev1.ForkTurnBoundaryComplete {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := len(f.sent) - 1; i >= 0; i-- {
		if fc := f.sent[i].GetForkTurnBoundaryComplete(); fc != nil {
			return fc
		}
	}
	return nil
}

func newForkNodeManager(t *testing.T, be *scriptedPodBackend) *spawnlet.Manager {
	t.Helper()
	mgr := spawnlet.NewManagerWithBackend(be, noopApplier{}, spawnlet.ManagerConfig{
		NodeID: "node-test", AgentImage: "a", SidecarImage: "s", DataRoot: t.TempDir(),
		DeltaCapture: true,
	})
	mgr.SetJournal(&fakeNodeJournal{finalID: "manifest-abc"}, t.TempDir())
	return mgr
}

func putForkNodeSource(t *testing.T, mgr *spawnlet.Manager, id string, gen uint64) {
	t.Helper()
	mgr.Store().Put(&spawnlet.Spawn{
		ID: id, Generation: gen, AgentID: "ag-source", SidecarID: "sc-source",
		BaseImageDigest: "agent@sha256:base", LaunchImageRef: "agent:base",
		JournalMounts: []journal.Mount{{Name: "main", HostDir: t.TempDir(), Class: journal.NodeLocal}},
	})
}

type forkStatusDoer struct {
	mu   sync.Mutex
	idle chan struct{}
	reqs []*http.Request
}

func (d *forkStatusDoer) Do(req *http.Request) (*http.Response, error) {
	d.mu.Lock()
	d.reqs = append(d.reqs, req)
	d.mu.Unlock()
	busy := true
	select {
	case <-d.idle:
		busy = false
	default:
	}
	body := `{"busy":true,"active_requests":1}`
	if !busy {
		body = `{"busy":false,"active_requests":0}`
	}
	return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}, nil
}

func (d *forkStatusDoer) lastReq() *http.Request {
	d.mu.Lock()
	defer d.mu.Unlock()
	if len(d.reqs) == 0 {
		return nil
	}
	return d.reqs[len(d.reqs)-1]
}

func idleForkStatusDoer() *forkStatusDoer {
	idle := make(chan struct{})
	close(idle)
	return &forkStatusDoer{idle: idle}
}

func setForkNodeControl(t *testing.T, mgr *spawnlet.Manager, id string) {
	t.Helper()
	sp, ok := mgr.Store().Get(id)
	if !ok {
		t.Fatalf("missing source %s in manager store", id)
	}
	sp.ControlURL = "http://10.0.0.5:8081/control/model"
	sp.ControlToken = "tok-fork"
}

func TestForkSameNodeStaleGenerationDropped(t *testing.T) {
	be := &scriptedPodBackend{script: scriptGoose}
	mgr := newForkNodeManager(t, be)
	putForkNodeSource(t, mgr, "sp-source", 9)
	fs := &fakeCPStream{}
	a := newAttacher(mgr, fs)

	a.handle(context.Background(), &nodev1.CPMessage{Msg: &nodev1.CPMessage_ForkSameNode{ForkSameNode: &nodev1.ForkSameNode{
		SourceSpawnId: "sp-source", ForkSpawnId: "sp-fork", SourceGeneration: 8, TargetGeneration: 1, TransferSetId: "ts-1",
	}}})

	if fc := lastForkSameNodeComplete(fs); fc != nil {
		t.Fatalf("stale ForkSameNode must not emit completion, got %+v", fc)
	}
}

func TestForkSameNodeEmitsCompletion(t *testing.T) {
	be := &scriptedPodBackend{script: scriptGoose}
	mgr := newForkNodeManager(t, be)
	putForkNodeSource(t, mgr, "sp-source", 9)
	fs := &fakeCPStream{}
	a := newAttacher(mgr, fs)

	a.handle(context.Background(), &nodev1.CPMessage{Msg: &nodev1.CPMessage_ForkSameNode{ForkSameNode: &nodev1.ForkSameNode{
		SourceSpawnId: "sp-source", ForkSpawnId: "sp-fork", SourceGeneration: 9, TargetGeneration: 1, TransferSetId: "ts-1",
	}}})

	waitFor(t, "ForkSameNodeComplete", func() bool { return lastForkSameNodeComplete(fs) != nil })
	fc := lastForkSameNodeComplete(fs)
	if fc.GetError() != "" {
		t.Fatalf("ForkSameNodeComplete error = %q", fc.GetError())
	}
	if fc.GetSourceSpawnId() != "sp-source" || fc.GetForkSpawnId() != "sp-fork" || fc.GetTransferSetId() != "ts-1" {
		t.Fatalf("ForkSameNodeComplete ids = %+v", fc)
	}
	if len(fc.GetMounts()) != 1 || fc.GetMounts()[0].GetName() != "main" {
		t.Fatalf("mounts = %+v", fc.GetMounts())
	}
	if len(fc.GetRootfsArtifacts()) != 1 || fc.GetRootfsArtifacts()[0].GetGeneration() != 1 {
		t.Fatalf("rootfs artifacts = %+v", fc.GetRootfsArtifacts())
	}
}

func TestForkSameNodeFailureCompletionDoesNotMarkForkActive(t *testing.T) {
	be := &scriptedPodBackend{script: scriptGoose}
	mgr := newForkNodeManager(t, be)
	fs := &fakeCPStream{}
	a := newAttacher(mgr, fs)

	a.handle(context.Background(), &nodev1.CPMessage{Msg: &nodev1.CPMessage_ForkSameNode{ForkSameNode: &nodev1.ForkSameNode{
		SourceSpawnId: "missing-source", ForkSpawnId: "sp-fork", SourceGeneration: 9, TargetGeneration: 1, TransferSetId: "ts-1",
	}}})

	waitFor(t, "ForkSameNodeComplete error", func() bool {
		fc := lastForkSameNodeComplete(fs)
		return fc != nil && fc.GetError() != ""
	})
	if hasPhase(fs.phasesFor("sp-fork"), nodev1.SpawnPhase_ACTIVE) {
		t.Fatal("failed fork materialization must not report fork ACTIVE")
	}
}

func TestCancelForkSameNodeCancelsRunningFork(t *testing.T) {
	be := &scriptedPodBackend{script: scriptGoose}
	finalStarted := make(chan struct{})
	finalBlock := make(chan struct{})
	mgr := spawnlet.NewManagerWithBackend(be, noopApplier{}, spawnlet.ManagerConfig{
		NodeID: "node-test", AgentImage: "a", SidecarImage: "s", DataRoot: t.TempDir(),
		DeltaCapture: true,
	})
	mgr.SetJournal(&fakeNodeJournal{
		finalID:      "manifest-abc",
		finalStarted: finalStarted,
		finalBlock:   finalBlock,
	}, t.TempDir())
	putForkNodeSource(t, mgr, "sp-source", 9)
	fs := &fakeCPStream{}
	a := newAttacher(mgr, fs)

	a.handle(context.Background(), &nodev1.CPMessage{Msg: &nodev1.CPMessage_ForkSameNode{ForkSameNode: &nodev1.ForkSameNode{
		SourceSpawnId: "sp-source", ForkSpawnId: "sp-fork", SourceGeneration: 9, TargetGeneration: 1, TransferSetId: "ts-1",
	}}})
	select {
	case <-finalStarted:
	case <-time.After(time.Second):
		t.Fatal("fork did not reach final snapshot")
	}

	a.handle(context.Background(), &nodev1.CPMessage{Msg: &nodev1.CPMessage_CancelForkSameNode{CancelForkSameNode: &nodev1.CancelForkSameNode{
		SourceSpawnId: "sp-source", ForkSpawnId: "sp-fork", TransferSetId: "ts-1",
	}}})

	waitFor(t, "ForkSameNodeComplete cancellation", func() bool {
		fc := lastForkSameNodeComplete(fs)
		return fc != nil && strings.Contains(fc.GetError(), "canceled")
	})
	fc := lastForkSameNodeComplete(fs)
	if len(fc.GetMounts()) != 0 || len(fc.GetRootfsArtifacts()) != 0 {
		t.Fatalf("canceled fork completion must not include seed pins: %+v", fc)
	}
	close(finalBlock)
}

func TestUnpauseIfPausedEmitsCompletion(t *testing.T) {
	be := &scriptedPodBackend{script: scriptGoose}
	mgr := newForkNodeManager(t, be)
	putForkNodeSource(t, mgr, "sp-source", 9)
	fs := &fakeCPStream{}
	a := newAttacher(mgr, fs)

	a.handle(context.Background(), &nodev1.CPMessage{Msg: &nodev1.CPMessage_UnpauseIfPaused{UnpauseIfPaused: &nodev1.UnpauseIfPaused{
		SpawnId: "sp-source", Generation: 9,
	}}})

	waitFor(t, "UnpauseIfPausedComplete", func() bool { return lastUnpauseIfPausedComplete(fs) != nil })
	got := lastUnpauseIfPausedComplete(fs)
	if got.GetSpawnId() != "sp-source" || got.GetGeneration() != 9 || got.GetError() != "" {
		t.Fatalf("UnpauseIfPausedComplete = %+v", got)
	}
}

func TestUnpauseIfPausedMissingSourceEmitsErrorCompletion(t *testing.T) {
	be := &scriptedPodBackend{script: scriptGoose}
	mgr := newForkNodeManager(t, be)
	fs := &fakeCPStream{}
	a := newAttacher(mgr, fs)

	a.handle(context.Background(), &nodev1.CPMessage{Msg: &nodev1.CPMessage_UnpauseIfPaused{UnpauseIfPaused: &nodev1.UnpauseIfPaused{
		SpawnId: "missing-source", Generation: 9,
	}}})

	waitFor(t, "UnpauseIfPausedComplete error", func() bool {
		got := lastUnpauseIfPausedComplete(fs)
		return got != nil && got.GetError() != ""
	})
	got := lastUnpauseIfPausedComplete(fs)
	if got.GetSpawnId() != "missing-source" || got.GetGeneration() != 9 {
		t.Fatalf("UnpauseIfPausedComplete ids = %+v", got)
	}
	if !strings.Contains(got.GetError(), "missing-source") {
		t.Fatalf("UnpauseIfPausedComplete error = %q, want missing spawn id", got.GetError())
	}
}

func TestForkTurnBoundaryWaitsForACPPumpIdle(t *testing.T) {
	be := &scriptedPodBackend{script: scriptGoose}
	mgr := newForkNodeManager(t, be)
	putForkNodeSource(t, mgr, "sp-source", 9)
	fs := &fakeCPStream{}
	a := newAttacher(mgr, fs)
	p := newPump(io.Discard, strings.NewReader(""))
	p.mu.Lock()
	p.busy = true
	p.inflightPromptID = 1
	p.mu.Unlock()
	a.pumps[zeroKey("sp-source")] = p

	a.handle(context.Background(), &nodev1.CPMessage{Msg: &nodev1.CPMessage_ForkTurnBoundary{ForkTurnBoundary: &nodev1.ForkTurnBoundary{
		SourceSpawnId: "sp-source", SourceGeneration: 9, TransferSetId: "ts-1",
	}}})
	time.Sleep(20 * time.Millisecond)
	if got := lastForkTurnBoundaryComplete(fs); got != nil {
		t.Fatalf("turn-boundary completed while pump was busy: %+v", got)
	}

	p.mu.Lock()
	p.busy = false
	p.inflightPromptID = 0
	p.mu.Unlock()
	waitFor(t, "ForkTurnBoundaryComplete", func() bool { return lastForkTurnBoundaryComplete(fs) != nil })
	got := lastForkTurnBoundaryComplete(fs)
	if got.GetError() != "" || got.GetSourceSpawnId() != "sp-source" || got.GetTransferSetId() != "ts-1" {
		t.Fatalf("ForkTurnBoundaryComplete = %+v", got)
	}
}

func TestForkTurnBoundaryWaitsForStartingMoshSession(t *testing.T) {
	be := &scriptedPodBackend{script: scriptGoose}
	mgr := newForkNodeManager(t, be)
	putForkNodeSource(t, mgr, "sp-source", 9)
	sx := &fakeSessionExec{moshGate: make(chan struct{}), moshReached: make(chan struct{})}
	fs := &fakeCPStream{}
	a := newAttacher(mgr, fs)
	setForkNodeControl(t, mgr, "sp-source")
	a.ctrlHTTP = idleForkStatusDoer()
	a.sx = sx
	reg := newSessionRegistry("sp-source")
	reg.register(&sessionEntry{id: SessionZeroID, state: nodev1.SessionState_SESSION_STATE_ACTIVE, pinned: true, runnable: "goose-acp"})
	a.sessions["sp-source"] = reg
	a.pumps[zeroKey("sp-source")] = newPump(io.Discard, strings.NewReader(""))

	a.handle(context.Background(), &nodev1.CPMessage{Msg: &nodev1.CPMessage_CreateSession{CreateSession: &nodev1.CreateSession{
		SpawnId: "sp-source", Transport: nodev1.SessionTransport_SESSION_TRANSPORT_MOSH, Runnable: "shell",
	}}})
	select {
	case <-sx.moshReached:
	case <-time.After(time.Second):
		t.Fatal("mosh launch did not reach gate")
	}

	a.handle(context.Background(), &nodev1.CPMessage{Msg: &nodev1.CPMessage_ForkTurnBoundary{ForkTurnBoundary: &nodev1.ForkTurnBoundary{
		SourceSpawnId: "sp-source", SourceGeneration: 9, TransferSetId: "ts-1",
	}}})
	time.Sleep(20 * time.Millisecond)
	if got := lastForkTurnBoundaryComplete(fs); got != nil {
		t.Fatalf("turn-boundary completed while mosh session was STARTING: %+v", got)
	}

	close(sx.moshGate)
	waitFor(t, "ForkTurnBoundaryComplete after mosh relay appears", func() bool { return lastForkTurnBoundaryComplete(fs) != nil })
	got := lastForkTurnBoundaryComplete(fs)
	if got.GetError() != "" {
		t.Fatalf("ForkTurnBoundaryComplete error = %q", got.GetError())
	}
}

func TestForkTurnBoundaryWaitsForStartingACPSession(t *testing.T) {
	be := &scriptedPodBackend{script: scriptGoose}
	mgr := newForkNodeManager(t, be)
	putForkNodeSource(t, mgr, "sp-source", 9)
	sx := &fakeSessionExec{dialGate: make(chan struct{}), dialReached: make(chan struct{})}
	fs := &fakeCPStream{}
	a := newAttacher(mgr, fs)
	a.sx = sx
	reg := newSessionRegistry("sp-source")
	reg.register(&sessionEntry{id: SessionZeroID, state: nodev1.SessionState_SESSION_STATE_ACTIVE, pinned: true, runnable: "goose-acp"})
	a.sessions["sp-source"] = reg
	a.pumps[zeroKey("sp-source")] = newPump(io.Discard, strings.NewReader(""))

	a.handle(context.Background(), &nodev1.CPMessage{Msg: &nodev1.CPMessage_CreateSession{CreateSession: &nodev1.CreateSession{
		SpawnId: "sp-source", Transport: nodev1.SessionTransport_SESSION_TRANSPORT_ACP, Runnable: "goose-acp",
	}}})
	select {
	case <-sx.dialReached:
	case <-time.After(time.Second):
		t.Fatal("acp launch did not reach dial gate")
	}

	a.handle(context.Background(), &nodev1.CPMessage{Msg: &nodev1.CPMessage_ForkTurnBoundary{ForkTurnBoundary: &nodev1.ForkTurnBoundary{
		SourceSpawnId: "sp-source", SourceGeneration: 9, TransferSetId: "ts-1",
	}}})
	time.Sleep(20 * time.Millisecond)
	if got := lastForkTurnBoundaryComplete(fs); got != nil {
		t.Fatalf("turn-boundary completed while acp session was STARTING: %+v", got)
	}

	close(sx.dialGate)
	waitFor(t, "ForkTurnBoundaryComplete after acp pump appears", func() bool { return lastForkTurnBoundaryComplete(fs) != nil })
	got := lastForkTurnBoundaryComplete(fs)
	if got.GetError() != "" {
		t.Fatalf("ForkTurnBoundaryComplete error = %q", got.GetError())
	}
	a.mu.Lock()
	if p := a.pumps[sessionKey{"sp-source", "1"}]; p != nil {
		p.stop()
	}
	a.mu.Unlock()
}

func TestForkTurnBoundaryBlocksPromptUntilForkSameNodeCompletes(t *testing.T) {
	be := &scriptedPodBackend{script: scriptGoose}
	mgr := newForkNodeManager(t, be)
	putForkNodeSource(t, mgr, "sp-source", 9)
	fs := &fakeCPStream{}
	a := newAttacher(mgr, fs)
	p := newPump(io.Discard, strings.NewReader(""))
	a.pumps[zeroKey("sp-source")] = p

	a.handle(context.Background(), &nodev1.CPMessage{Msg: &nodev1.CPMessage_ForkTurnBoundary{ForkTurnBoundary: &nodev1.ForkTurnBoundary{
		SourceSpawnId: "sp-source", SourceGeneration: 9, TransferSetId: "ts-1",
	}}})
	waitFor(t, "ForkTurnBoundaryComplete", func() bool { return lastForkTurnBoundaryComplete(fs) != nil })

	a.fromClient("sp-source", SessionZeroID, "c1", encodeFrame(Frame{Kind: "prompt", Text: "during fork"}))
	p.mu.Lock()
	blockedBusy, blockedInflight, blockedLogLen, blockedQueueLen := p.busy, p.inflightPromptID, len(p.log), len(p.queue)
	p.mu.Unlock()
	if blockedBusy || blockedInflight != 0 || blockedLogLen != 2 || blockedQueueLen != 1 {
		t.Fatalf("prompt during fork barrier must queue without starting: busy=%v inflight=%d logLen=%d queue=%d", blockedBusy, blockedInflight, blockedLogLen, blockedQueueLen)
	}

	a.handle(context.Background(), &nodev1.CPMessage{Msg: &nodev1.CPMessage_ForkSameNode{ForkSameNode: &nodev1.ForkSameNode{
		SourceSpawnId: "sp-source", ForkSpawnId: "sp-fork", SourceGeneration: 9, TargetGeneration: 1, TransferSetId: "ts-1",
	}}})
	waitFor(t, "ForkSameNodeComplete", func() bool { return lastForkSameNodeComplete(fs) != nil })
	if fc := lastForkSameNodeComplete(fs); fc.GetError() != "" {
		t.Fatalf("ForkSameNodeComplete error = %q", fc.GetError())
	}

	a.fromClient("sp-source", SessionZeroID, "c1", encodeFrame(Frame{Kind: "prompt", Text: "after fork"}))
	p.mu.Lock()
	releasedBusy, releasedInflight, releasedLogLen := p.busy, p.inflightPromptID, len(p.log)
	p.mu.Unlock()
	if !releasedBusy || releasedInflight == 0 || releasedLogLen == 0 {
		t.Fatalf("prompt after fork barrier release did not start: busy=%v inflight=%d logLen=%d", releasedBusy, releasedInflight, releasedLogLen)
	}
}

func TestForkSameNodeFailureReleasesForkTurnBoundaryBarrier(t *testing.T) {
	be := &scriptedPodBackend{script: scriptGoose}
	mgr := newForkNodeManager(t, be)
	fs := &fakeCPStream{}
	a := newAttacher(mgr, fs)
	p := newPump(io.Discard, strings.NewReader(""))
	a.pumps[zeroKey("missing-source")] = p

	a.handle(context.Background(), &nodev1.CPMessage{Msg: &nodev1.CPMessage_ForkTurnBoundary{ForkTurnBoundary: &nodev1.ForkTurnBoundary{
		SourceSpawnId: "missing-source", SourceGeneration: 9, TransferSetId: "ts-1",
	}}})
	waitFor(t, "ForkTurnBoundaryComplete", func() bool { return lastForkTurnBoundaryComplete(fs) != nil })

	a.handle(context.Background(), &nodev1.CPMessage{Msg: &nodev1.CPMessage_ForkSameNode{ForkSameNode: &nodev1.ForkSameNode{
		SourceSpawnId: "missing-source", ForkSpawnId: "sp-fork", SourceGeneration: 9, TargetGeneration: 1, TransferSetId: "ts-1",
	}}})
	waitFor(t, "ForkSameNodeComplete error", func() bool {
		fc := lastForkSameNodeComplete(fs)
		return fc != nil && fc.GetError() != ""
	})

	a.fromClient("missing-source", SessionZeroID, "c1", encodeFrame(Frame{Kind: "prompt", Text: "after failure"}))
	p.mu.Lock()
	releasedBusy, releasedInflight := p.busy, p.inflightPromptID
	p.mu.Unlock()
	if !releasedBusy || releasedInflight == 0 {
		t.Fatalf("prompt after failed fork did not start: busy=%v inflight=%d", releasedBusy, releasedInflight)
	}
}

func TestUnpauseIfPausedReleasesForkTurnBoundaryBarrier(t *testing.T) {
	be := &scriptedPodBackend{script: scriptGoose}
	mgr := newForkNodeManager(t, be)
	putForkNodeSource(t, mgr, "sp-source", 9)
	fs := &fakeCPStream{}
	a := newAttacher(mgr, fs)
	p := newPump(io.Discard, strings.NewReader(""))
	a.pumps[zeroKey("sp-source")] = p

	a.handle(context.Background(), &nodev1.CPMessage{Msg: &nodev1.CPMessage_ForkTurnBoundary{ForkTurnBoundary: &nodev1.ForkTurnBoundary{
		SourceSpawnId: "sp-source", SourceGeneration: 9, TransferSetId: "ts-1",
	}}})
	waitFor(t, "ForkTurnBoundaryComplete", func() bool { return lastForkTurnBoundaryComplete(fs) != nil })

	a.fromClient("sp-source", SessionZeroID, "c1", encodeFrame(Frame{Kind: "prompt", Text: "during fork"}))
	p.mu.Lock()
	blockedBusy, blockedInflight := p.busy, p.inflightPromptID
	p.mu.Unlock()
	if blockedBusy || blockedInflight != 0 {
		t.Fatalf("prompt before unpause release started a turn: busy=%v inflight=%d", blockedBusy, blockedInflight)
	}

	a.handle(context.Background(), &nodev1.CPMessage{Msg: &nodev1.CPMessage_UnpauseIfPaused{UnpauseIfPaused: &nodev1.UnpauseIfPaused{
		SpawnId: "sp-source", Generation: 9,
	}}})
	waitFor(t, "UnpauseIfPausedComplete", func() bool { return lastUnpauseIfPausedComplete(fs) != nil })

	a.fromClient("sp-source", SessionZeroID, "c1", encodeFrame(Frame{Kind: "prompt", Text: "after unpause"}))
	p.mu.Lock()
	releasedBusy, releasedInflight := p.busy, p.inflightPromptID
	p.mu.Unlock()
	if !releasedBusy || releasedInflight == 0 {
		t.Fatalf("prompt after unpause release did not start: busy=%v inflight=%d", releasedBusy, releasedInflight)
	}
}

func TestForkTurnBoundaryReleaseCommandReleasesAcquiredBarrier(t *testing.T) {
	be := &scriptedPodBackend{script: scriptGoose}
	mgr := newForkNodeManager(t, be)
	putForkNodeSource(t, mgr, "sp-source", 9)
	fs := &fakeCPStream{}
	a := newAttacher(mgr, fs)
	p := newPump(io.Discard, strings.NewReader(""))
	a.pumps[zeroKey("sp-source")] = p

	a.handle(context.Background(), &nodev1.CPMessage{Msg: &nodev1.CPMessage_ForkTurnBoundary{ForkTurnBoundary: &nodev1.ForkTurnBoundary{
		SourceSpawnId: "sp-source", SourceGeneration: 9, TransferSetId: "ts-1",
	}}})
	waitFor(t, "ForkTurnBoundaryComplete", func() bool { return lastForkTurnBoundaryComplete(fs) != nil })

	a.fromClient("sp-source", SessionZeroID, "c1", encodeFrame(Frame{Kind: "prompt", Text: "during fork"}))
	p.mu.Lock()
	blockedBusy, blockedInflight := p.busy, p.inflightPromptID
	p.mu.Unlock()
	if blockedBusy || blockedInflight != 0 {
		t.Fatalf("prompt before release started a turn: busy=%v inflight=%d", blockedBusy, blockedInflight)
	}

	a.handle(context.Background(), &nodev1.CPMessage{Msg: &nodev1.CPMessage_ReleaseForkTurnBoundary{ReleaseForkTurnBoundary: &nodev1.ReleaseForkTurnBoundary{
		SourceSpawnId: "sp-source", SourceGeneration: 9, TransferSetId: "ts-1",
	}}})

	a.fromClient("sp-source", SessionZeroID, "c1", encodeFrame(Frame{Kind: "prompt", Text: "after release"}))
	p.mu.Lock()
	releasedBusy, releasedInflight := p.busy, p.inflightPromptID
	p.mu.Unlock()
	if !releasedBusy || releasedInflight == 0 {
		t.Fatalf("prompt after release did not start: busy=%v inflight=%d", releasedBusy, releasedInflight)
	}
}

func TestForkTurnBoundaryReleaseCommandCancelsPendingAcquire(t *testing.T) {
	be := &scriptedPodBackend{script: scriptGoose}
	mgr := newForkNodeManager(t, be)
	putForkNodeSource(t, mgr, "sp-source", 9)
	fs := &fakeCPStream{}
	a := newAttacher(mgr, fs)
	p := newPump(io.Discard, strings.NewReader(""))
	p.mu.Lock()
	p.busy = true
	p.inflightPromptID = 1
	p.mu.Unlock()
	a.pumps[zeroKey("sp-source")] = p

	a.handle(context.Background(), &nodev1.CPMessage{Msg: &nodev1.CPMessage_ForkTurnBoundary{ForkTurnBoundary: &nodev1.ForkTurnBoundary{
		SourceSpawnId: "sp-source", SourceGeneration: 9, TransferSetId: "ts-1",
	}}})
	time.Sleep(20 * time.Millisecond)
	if got := lastForkTurnBoundaryComplete(fs); got != nil {
		t.Fatalf("turn-boundary completed while pump was busy: %+v", got)
	}

	a.handle(context.Background(), &nodev1.CPMessage{Msg: &nodev1.CPMessage_ReleaseForkTurnBoundary{ReleaseForkTurnBoundary: &nodev1.ReleaseForkTurnBoundary{
		SourceSpawnId: "sp-source", SourceGeneration: 9, TransferSetId: "ts-1",
	}}})
	p.mu.Lock()
	p.busy = false
	p.inflightPromptID = 0
	p.mu.Unlock()
	time.Sleep(2 * forkTurnBoundaryPoll)

	a.fromClient("sp-source", SessionZeroID, "c1", encodeFrame(Frame{Kind: "prompt", Text: "after canceled acquire"}))
	p.mu.Lock()
	releasedBusy, releasedInflight := p.busy, p.inflightPromptID
	p.mu.Unlock()
	if !releasedBusy || releasedInflight == 0 {
		t.Fatalf("release must cancel pending acquire; prompt busy=%v inflight=%d", releasedBusy, releasedInflight)
	}
}

func TestForkTurnBoundaryRejectsNewACPSessionDuringBarrier(t *testing.T) {
	be := &scriptedPodBackend{script: scriptGoose}
	mgr := newForkNodeManager(t, be)
	putForkNodeSource(t, mgr, "sp-source", 9)
	sx := &fakeSessionExec{}
	fs := &fakeCPStream{}
	a := newAttacher(mgr, fs)
	a.sx = sx
	reg := newSessionRegistry("sp-source")
	reg.register(&sessionEntry{id: SessionZeroID, state: nodev1.SessionState_SESSION_STATE_ACTIVE, pinned: true, runnable: "goose-acp"})
	a.sessions["sp-source"] = reg
	a.pumps[zeroKey("sp-source")] = newPump(io.Discard, strings.NewReader(""))

	a.handle(context.Background(), &nodev1.CPMessage{Msg: &nodev1.CPMessage_ForkTurnBoundary{ForkTurnBoundary: &nodev1.ForkTurnBoundary{
		SourceSpawnId: "sp-source", SourceGeneration: 9, TransferSetId: "ts-1",
	}}})
	waitFor(t, "ForkTurnBoundaryComplete", func() bool { return lastForkTurnBoundaryComplete(fs) != nil })

	a.handle(context.Background(), &nodev1.CPMessage{Msg: &nodev1.CPMessage_CreateSession{CreateSession: &nodev1.CreateSession{
		SpawnId: "sp-source", Generation: 9, Transport: nodev1.SessionTransport_SESSION_TRANSPORT_ACP, Runnable: "goose-acp",
	}}})
	time.Sleep(20 * time.Millisecond)

	sx.mu.Lock()
	acpLaunches, dials := len(sx.acpLaunched), sx.dials
	sx.mu.Unlock()
	if acpLaunches != 0 || dials != 0 {
		t.Fatalf("acp session during fork barrier must not launch or dial, launches=%d dials=%d", acpLaunches, dials)
	}
	a.mu.Lock()
	p := a.pumps[sessionKey{"sp-source", "1"}]
	a.mu.Unlock()
	if p != nil {
		t.Fatal("acp session during fork barrier must not register a Pump")
	}
	if got := len(reg.snapshot()); got != 1 {
		t.Fatalf("rejected acp session must not register a session, roster=%d", got)
	}
	if st := lastSessionStatus(fs); st == nil || st.State != nodev1.SessionState_SESSION_STATE_ERROR {
		t.Fatalf("want ERROR SessionStatus for fork barrier rejection, got %+v", st)
	}
}

func TestForkTurnBoundaryCompletesForTmuxRelaySource(t *testing.T) {
	be := &scriptedPodBackend{script: scriptGoose}
	mgr := newForkNodeManager(t, be)
	putForkNodeSource(t, mgr, "sp-source", 9)
	setForkNodeControl(t, mgr, "sp-source")
	fs := &fakeCPStream{}
	a := newAttacher(mgr, fs)
	a.ctrlHTTP = idleForkStatusDoer()
	a.tmuxRelays[zeroKey("sp-source")] = newTmuxRelay([]string{"true"}, func(string, []byte) error { return nil })

	a.handle(context.Background(), &nodev1.CPMessage{Msg: &nodev1.CPMessage_ForkTurnBoundary{ForkTurnBoundary: &nodev1.ForkTurnBoundary{
		SourceSpawnId: "sp-source", SourceGeneration: 9, TransferSetId: "ts-1",
	}}})

	waitFor(t, "ForkTurnBoundaryComplete", func() bool { return lastForkTurnBoundaryComplete(fs) != nil })
	got := lastForkTurnBoundaryComplete(fs)
	if got.GetError() != "" {
		t.Fatalf("ForkTurnBoundaryComplete error = %q", got.GetError())
	}
}

func TestForkTurnBoundaryWaitsForTmuxSidecarInflight(t *testing.T) {
	be := &scriptedPodBackend{script: scriptGoose}
	mgr := newForkNodeManager(t, be)
	putForkNodeSource(t, mgr, "sp-source", 9)
	setForkNodeControl(t, mgr, "sp-source")
	fs := &fakeCPStream{}
	a := newAttacher(mgr, fs)
	idle := make(chan struct{})
	doer := &forkStatusDoer{idle: idle}
	a.ctrlHTTP = doer
	a.tmuxRelays[zeroKey("sp-source")] = newTmuxRelay([]string{"true"}, func(string, []byte) error { return nil })

	a.handle(context.Background(), &nodev1.CPMessage{Msg: &nodev1.CPMessage_ForkTurnBoundary{ForkTurnBoundary: &nodev1.ForkTurnBoundary{
		SourceSpawnId: "sp-source", SourceGeneration: 9, TransferSetId: "ts-1",
	}}})
	time.Sleep(2 * forkTurnBoundaryPoll)
	if got := lastForkTurnBoundaryComplete(fs); got != nil {
		t.Fatalf("turn-boundary completed while sidecar reported an active request: %+v", got)
	}

	close(idle)
	waitFor(t, "ForkTurnBoundaryComplete after sidecar idle", func() bool { return lastForkTurnBoundaryComplete(fs) != nil })
	got := lastForkTurnBoundaryComplete(fs)
	if got.GetError() != "" {
		t.Fatalf("ForkTurnBoundaryComplete error = %q", got.GetError())
	}
	last := doer.lastReq()
	if last == nil {
		t.Fatal("turn-boundary must query sidecar control status")
	}
	if got := last.URL.String(); got != "http://10.0.0.5:8081/control/status" {
		t.Fatalf("sidecar status URL = %s", got)
	}
	if got := last.Header.Get("Authorization"); got != "Bearer tok-fork" {
		t.Fatalf("sidecar status auth = %q, want bearer token", got)
	}
}

func TestForkTurnBoundaryWaitsForTmuxOutputDrainBeforeSidecarStatus(t *testing.T) {
	be := &scriptedPodBackend{script: scriptGoose}
	mgr := newForkNodeManager(t, be)
	putForkNodeSource(t, mgr, "sp-source", 9)
	setForkNodeControl(t, mgr, "sp-source")
	fs := &fakeCPStream{}
	a := newAttacher(mgr, fs)
	doer := idleForkStatusDoer()
	a.ctrlHTTP = doer

	outputStarted := make(chan struct{})
	outputRelease := make(chan struct{})
	var outputOnce sync.Once
	relay := newTmuxRelay([]string{"/bin/sh", "-c", "printf fork-output; sleep 30"}, func(string, []byte) error {
		outputOnce.Do(func() { close(outputStarted) })
		<-outputRelease
		return nil
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	defer func() {
		select {
		case <-outputRelease:
		default:
			close(outputRelease)
		}
		relay.stop()
	}()
	if err := relay.attach(ctx, "client-1"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-outputStarted:
	case <-time.After(time.Second):
		t.Fatal("tmux relay output did not reach send gate")
	}
	a.tmuxRelays[zeroKey("sp-source")] = relay

	a.handle(ctx, &nodev1.CPMessage{Msg: &nodev1.CPMessage_ForkTurnBoundary{ForkTurnBoundary: &nodev1.ForkTurnBoundary{
		SourceSpawnId: "sp-source", SourceGeneration: 9, TransferSetId: "ts-1",
	}}})
	time.Sleep(2 * forkTurnBoundaryPoll)
	if req := doer.lastReq(); req != nil {
		t.Fatalf("sidecar status sampled before tmux output drained: %s", req.URL.String())
	}

	close(outputRelease)
	waitFor(t, "sidecar status sampled after tmux output drained", func() bool { return doer.lastReq() != nil })
	waitFor(t, "ForkTurnBoundaryComplete after tmux output drained", func() bool { return lastForkTurnBoundaryComplete(fs) != nil })
	if got := lastForkTurnBoundaryComplete(fs); got.GetError() != "" {
		t.Fatalf("ForkTurnBoundaryComplete error = %q", got.GetError())
	}
}

func TestForkTurnBoundaryBlocksTmuxInputWhileWaitingForSidecarIdle(t *testing.T) {
	be := &scriptedPodBackend{script: scriptGoose}
	mgr := newForkNodeManager(t, be)
	putForkNodeSource(t, mgr, "sp-source", 9)
	setForkNodeControl(t, mgr, "sp-source")
	fs := &fakeCPStream{}
	a := newAttacher(mgr, fs)
	idle := make(chan struct{})
	doer := &forkStatusDoer{idle: idle}
	a.ctrlHTTP = doer

	relay := newTmuxRelay([]string{"true"}, func(string, []byte) error { return nil })
	readEnd, writeEnd, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer readEnd.Close()
	relay.mu.Lock()
	relay.clients["client-1"] = &tmuxClient{ptmx: writeEnd}
	relay.mu.Unlock()
	a.tmuxRelays[zeroKey("sp-source")] = relay

	a.handle(context.Background(), &nodev1.CPMessage{Msg: &nodev1.CPMessage_ForkTurnBoundary{ForkTurnBoundary: &nodev1.ForkTurnBoundary{
		SourceSpawnId: "sp-source", SourceGeneration: 9, TransferSetId: "ts-1",
	}}})
	waitFor(t, "sidecar status sampled", func() bool { return doer.lastReq() != nil })

	relay.fromClient("client-1", append([]byte{tmuxOpInput}, []byte("new prompt\n")...))
	time.Sleep(2 * forkTurnBoundaryPoll)
	_ = writeEnd.Close()
	written, err := io.ReadAll(readEnd)
	if err != nil {
		t.Fatal(err)
	}
	if len(written) != 0 {
		t.Fatalf("tmux input passed through while fork waited for sidecar idle: %q", written)
	}

	close(idle)
	waitFor(t, "ForkTurnBoundaryComplete after sidecar idle", func() bool { return lastForkTurnBoundaryComplete(fs) != nil })
	got := lastForkTurnBoundaryComplete(fs)
	if got.GetError() != "" {
		t.Fatalf("ForkTurnBoundaryComplete error = %q", got.GetError())
	}
}

func TestForkTurnBoundaryFailsClosedWithoutObservableSession(t *testing.T) {
	be := &scriptedPodBackend{script: scriptGoose}
	mgr := newForkNodeManager(t, be)
	putForkNodeSource(t, mgr, "sp-source", 9)
	fs := &fakeCPStream{}
	a := newAttacher(mgr, fs)

	a.handle(context.Background(), &nodev1.CPMessage{Msg: &nodev1.CPMessage_ForkTurnBoundary{ForkTurnBoundary: &nodev1.ForkTurnBoundary{
		SourceSpawnId: "sp-source", SourceGeneration: 9, TransferSetId: "ts-1",
	}}})

	waitFor(t, "ForkTurnBoundaryComplete error", func() bool {
		got := lastForkTurnBoundaryComplete(fs)
		return got != nil && got.GetError() != ""
	})
}
