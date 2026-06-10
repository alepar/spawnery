package cp

import (
	"context"
	"errors"
	"testing"
	"time"

	"connectrpc.com/connect"

	cpv1 "spawnery/gen/cp/v1"
	nodev1 "spawnery/gen/node/v1"
	"spawnery/internal/cp/auth"
	"spawnery/internal/cp/registry"
	"spawnery/internal/cp/store"
)

// ackSender is a fake NodeSender that, on receiving a SetModel, asynchronously delivers a
// SetModelResult into the server's waiter registry (simulating the node's Attach-stream ack).
type ackSender struct {
	models    *modelWaiters
	ok        bool
	detail    string
	drop      bool          // if true, never ack (forces timeout)
	delay     time.Duration // ack delay
	lastGen   uint64
	lastModel string
	lastReqID string
	gotSet    bool
}

func (a *ackSender) Send(m *nodev1.CPMessage) error {
	sm := m.GetSetModel()
	if sm == nil {
		return nil
	}
	a.gotSet = true
	a.lastGen = sm.GetGeneration()
	a.lastModel = sm.GetModel()
	a.lastReqID = sm.GetRequestId()
	if a.drop {
		return nil
	}
	reqID := sm.GetRequestId() // node echoes the request_id of the SetModel it handled
	go func() {
		if a.delay > 0 {
			time.Sleep(a.delay)
		}
		a.models.deliver(&nodev1.SetModelResult{SpawnId: sm.GetSpawnId(), Ok: a.ok, Detail: a.detail, RequestId: reqID})
	}()
	return nil
}

// activeSpawnOnNode seeds an active spawn whose gen-1 live container is hosted by connected node "n1".
func activeSpawnOnNode(t *testing.T, s *Server, reg *registry.Registry, id, owner string, sender *ackSender) {
	t.Helper()
	ctx := context.Background()
	makeSpawn(t, s, id, owner)
	if err := s.st.WithTx(ctx, func(tx store.Store) error { return tx.Spawns().SetActive(ctx, id, "n1", 1) }); err != nil {
		t.Fatalf("SetActive: %v", err)
	}
	reg.Add(&registry.Node{ID: "n1", Sender: sender, Max: 1, Free: 1})
}

func TestSetSpawnModel_Authz(t *testing.T) {
	s, _, _ := newTestServer(t)
	makeSpawn(t, s, "sp1", "alice")
	alice := auth.WithOwner(context.Background(), "alice")

	// no owner -> Unauthenticated
	if _, err := s.SetSpawnModel(context.Background(), connect.NewRequest(&cpv1.SetSpawnModelRequest{SpawnId: "sp1", Model: "x"})); connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Fatalf("no owner: want Unauthenticated, got %v", err)
	}
	// foreign owner -> PermissionDenied
	bob := auth.WithOwner(context.Background(), "bob")
	if _, err := s.SetSpawnModel(bob, connect.NewRequest(&cpv1.SetSpawnModelRequest{SpawnId: "sp1", Model: "x"})); connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Fatalf("foreign: want PermissionDenied, got %v", err)
	}
	// unknown spawn -> NotFound
	if _, err := s.SetSpawnModel(alice, connect.NewRequest(&cpv1.SetSpawnModelRequest{SpawnId: "nope", Model: "x"})); connect.CodeOf(err) != connect.CodeNotFound {
		t.Fatalf("unknown: want NotFound, got %v", err)
	}
	// empty model -> InvalidArgument
	if _, err := s.SetSpawnModel(alice, connect.NewRequest(&cpv1.SetSpawnModelRequest{SpawnId: "sp1", Model: "   "})); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("empty model: want InvalidArgument, got %v", err)
	}
}

func TestSetSpawnModel_HappyPath(t *testing.T) {
	s, reg, _ := newTestServer(t)
	sender := &ackSender{models: s.models, ok: true}
	activeSpawnOnNode(t, s, reg, "sp1", "alice", sender)
	ctx := auth.WithOwner(context.Background(), "alice")

	resp, err := s.SetSpawnModel(ctx, connect.NewRequest(&cpv1.SetSpawnModelRequest{SpawnId: "sp1", Model: "  new-model  "}))
	if err != nil {
		t.Fatalf("SetSpawnModel: %v", err)
	}
	if resp.Msg.Model != "new-model" || !resp.Msg.Applied {
		t.Fatalf("resp = %+v, want model=new-model applied=true", resp.Msg)
	}
	if !sender.gotSet || sender.lastGen != 1 || sender.lastModel != "new-model" {
		t.Fatalf("push = gen %d model %q gotSet %v, want gen 1 model new-model", sender.lastGen, sender.lastModel, sender.gotSet)
	}
	sp, _ := s.st.Spawns().Get(ctx, "sp1")
	if sp.Model != "new-model" || !sp.ModelApplied {
		t.Fatalf("persisted = model %q applied %v, want new-model true", sp.Model, sp.ModelApplied)
	}
}

func TestSetSpawnModel_AckNotOk(t *testing.T) {
	s, reg, _ := newTestServer(t)
	sender := &ackSender{models: s.models, ok: false, detail: "sidecar 500"}
	activeSpawnOnNode(t, s, reg, "sp1", "alice", sender)
	ctx := auth.WithOwner(context.Background(), "alice")

	resp, err := s.SetSpawnModel(ctx, connect.NewRequest(&cpv1.SetSpawnModelRequest{SpawnId: "sp1", Model: "m2"}))
	if err != nil {
		t.Fatalf("SetSpawnModel: %v", err)
	}
	if resp.Msg.Applied {
		t.Fatal("want applied=false on not-ok ack")
	}
	sp, _ := s.st.Spawns().Get(ctx, "sp1")
	if sp.Model != "m2" || sp.ModelApplied {
		t.Fatalf("persisted = model %q applied %v, want m2 false", sp.Model, sp.ModelApplied)
	}
	if sp.ModelApplyDetail == "" {
		t.Fatal("want a failure detail recorded")
	}
}

func TestSetSpawnModel_Timeout(t *testing.T) {
	s, reg, _ := newTestServer(t)
	s.setModelTimeout = 50 * time.Millisecond
	sender := &ackSender{models: s.models, drop: true}
	activeSpawnOnNode(t, s, reg, "sp1", "alice", sender)
	ctx := auth.WithOwner(context.Background(), "alice")

	resp, err := s.SetSpawnModel(ctx, connect.NewRequest(&cpv1.SetSpawnModelRequest{SpawnId: "sp1", Model: "m3"}))
	if err != nil {
		t.Fatalf("SetSpawnModel: %v", err)
	}
	if resp.Msg.Applied {
		t.Fatal("want applied=false on timeout")
	}
	sp, _ := s.st.Spawns().Get(ctx, "sp1")
	if sp.Model != "m3" || sp.ModelApplied || sp.ModelApplyDetail == "" {
		t.Fatalf("persisted = model %q applied %v detail %q, want m3 false <non-empty>", sp.Model, sp.ModelApplied, sp.ModelApplyDetail)
	}
}

func TestSetSpawnModel_NoLivePod(t *testing.T) {
	s, _, _ := newTestServer(t)
	ctx0 := context.Background()
	makeSpawn(t, s, "sp1", "alice")
	if err := s.st.WithTx(ctx0, func(tx store.Store) error { return tx.Spawns().EndContainer(ctx0, "sp1", 1, store.PhaseStopped) }); err != nil {
		t.Fatalf("EndContainer: %v", err)
	}
	ctx := auth.WithOwner(ctx0, "alice")

	resp, err := s.SetSpawnModel(ctx, connect.NewRequest(&cpv1.SetSpawnModelRequest{SpawnId: "sp1", Model: "m4"}))
	if err != nil {
		t.Fatalf("SetSpawnModel: %v", err)
	}
	if !resp.Msg.Applied {
		t.Fatal("no live pod -> want applied=true immediately")
	}
	sp, _ := s.st.Spawns().Get(ctx, "sp1")
	if sp.Model != "m4" || !sp.ModelApplied {
		t.Fatalf("persisted = model %q applied %v, want m4 true", sp.Model, sp.ModelApplied)
	}
}

// TestSetSpawnModel_NoConnectedNode: a live pod exists but its node is not currently connected
// (not in the registry) -> can't push -> applied=false with a detail; reconciler retries later.
func TestSetSpawnModel_NoConnectedNode(t *testing.T) {
	s, _, _ := newTestServer(t)
	ctx0 := context.Background()
	makeSpawn(t, s, "sp1", "alice")
	if err := s.st.WithTx(ctx0, func(tx store.Store) error { return tx.Spawns().SetActive(ctx0, "sp1", "n1", 1) }); err != nil {
		t.Fatalf("SetActive: %v", err)
	}
	// note: node "n1" is intentionally NOT registered.
	ctx := auth.WithOwner(ctx0, "alice")

	resp, err := s.SetSpawnModel(ctx, connect.NewRequest(&cpv1.SetSpawnModelRequest{SpawnId: "sp1", Model: "m5"}))
	if err != nil {
		t.Fatalf("SetSpawnModel: %v", err)
	}
	if resp.Msg.Applied {
		t.Fatal("no connected node -> want applied=false")
	}
	sp, _ := s.st.Spawns().Get(ctx, "sp1")
	if sp.Model != "m5" || sp.ModelApplied || sp.ModelApplyDetail == "" {
		t.Fatalf("persisted = model %q applied %v detail %q, want m5 false <non-empty>", sp.Model, sp.ModelApplied, sp.ModelApplyDetail)
	}
}

// liveErrStore wraps a Store so LiveContainer returns a forced error while every other repo method
// passes through to the real store — lets us exercise the DB-error branch of pushModel.
type liveErrStore struct {
	store.Store
	err error
}

func (s liveErrStore) Spawns() store.SpawnRepo {
	return liveErrSpawns{SpawnRepo: s.Store.Spawns(), err: s.err}
}

type liveErrSpawns struct {
	store.SpawnRepo
	err error
}

func (r liveErrSpawns) LiveContainer(context.Context, string) (store.Container, bool, error) {
	return store.Container{}, false, r.err
}

// TestSetSpawnModel_LiveContainerDBError: a real DB error from LiveContainer must NOT be conflated with
// "no live pod". The handler must report applied=false and record a failure detail (so the reconciler,
// which scans model_applied=false, retries) — it must NOT MarkModelApplied / report success.
func TestSetSpawnModel_LiveContainerDBError(t *testing.T) {
	s, _, _ := newTestServer(t)
	ctx0 := context.Background()
	makeSpawn(t, s, "sp1", "alice")
	if err := s.st.WithTx(ctx0, func(tx store.Store) error { return tx.Spawns().SetActive(ctx0, "sp1", "n1", 1) }); err != nil {
		t.Fatalf("SetActive: %v", err)
	}
	// Wrap the store so LiveContainer fails; Get/SetModel/MarkModelApplyFailed still pass through.
	s.st = liveErrStore{Store: s.st, err: errors.New("boom: db unavailable")}
	ctx := auth.WithOwner(ctx0, "alice")

	resp, err := s.SetSpawnModel(ctx, connect.NewRequest(&cpv1.SetSpawnModelRequest{SpawnId: "sp1", Model: "m6"}))
	if err != nil {
		t.Fatalf("SetSpawnModel: %v", err)
	}
	if resp.Msg.Applied {
		t.Fatal("DB error on live-pod check -> want applied=false (must not falsely mark applied)")
	}
	sp, _ := s.st.Spawns().Get(ctx, "sp1")
	if sp.Model != "m6" || sp.ModelApplied || sp.ModelApplyDetail == "" {
		t.Fatalf("persisted = model %q applied %v detail %q, want m6 false <non-empty>", sp.Model, sp.ModelApplied, sp.ModelApplyDetail)
	}
}

// TestModelWaiters_StaleAckDropped: an ack whose request_id has no registered waiter (or matches a
// different waiter) is safely discarded — no panic, no misattribution to another push's waiter.
func TestModelWaiters_StaleAckDropped(t *testing.T) {
	w := newModelWaiters()

	// No waiters at all: an unknown ack must be a no-op (no panic).
	w.deliver(&nodev1.SetModelResult{SpawnId: "sp1", Ok: true, RequestId: "ghost"})

	// A waiter for req-A must not receive an ack addressed to req-B.
	ch := w.register("req-A")
	w.deliver(&nodev1.SetModelResult{SpawnId: "sp1", Ok: false, RequestId: "req-B"})
	select {
	case <-ch:
		t.Fatal("req-A waiter received an ack meant for req-B")
	default:
	}

	// Its own ack is delivered.
	w.deliver(&nodev1.SetModelResult{SpawnId: "sp1", Ok: true, RequestId: "req-A"})
	select {
	case r := <-ch:
		if !r.GetOk() {
			t.Fatal("want ok ack for req-A")
		}
	default:
		t.Fatal("req-A waiter did not receive its own ack")
	}
}

// progSender records each push's request_id (via sent) and never auto-acks, letting the test drive
// ack delivery and timing precisely.
type progSender struct {
	models *modelWaiters
	sent   chan string
}

func (p *progSender) Send(m *nodev1.CPMessage) error {
	if sm := m.GetSetModel(); sm != nil {
		p.sent <- sm.GetRequestId()
	}
	return nil
}

// TestSetSpawnModel_SequentialStaleAckNotMisrouted is the exact bug request_id keying closes: two
// SEQUENTIAL SetSpawnModel calls on the same spawn where the first TIMES OUT, and its late (failure)
// ack arrives while the second push is waiting. With spawn_id-only keying the stale failure ack would
// land in the second push's waiter and falsely fail it; with request_id keying it is dropped and the
// second push gets its own (success) ack.
func TestSetSpawnModel_SequentialStaleAckNotMisrouted(t *testing.T) {
	s, reg, _ := newTestServer(t)
	s.setModelTimeout = 80 * time.Millisecond
	sender := &progSender{models: s.models, sent: make(chan string, 4)}
	ctx0 := context.Background()
	makeSpawn(t, s, "sp1", "alice")
	if err := s.st.WithTx(ctx0, func(tx store.Store) error { return tx.Spawns().SetActive(ctx0, "sp1", "n1", 1) }); err != nil {
		t.Fatalf("SetActive: %v", err)
	}
	reg.Add(&registry.Node{ID: "n1", Sender: sender, Max: 1, Free: 1})
	ctx := auth.WithOwner(ctx0, "alice")

	// First push: capture its request_id, then let it time out (no ack delivered).
	done1 := make(chan bool, 1)
	go func() {
		resp, _ := s.SetSpawnModel(ctx, connect.NewRequest(&cpv1.SetSpawnModelRequest{SpawnId: "sp1", Model: "m-first"}))
		done1 <- resp.Msg.Applied
	}()
	reqID1 := <-sender.sent
	if applied1 := <-done1; applied1 { // blocks until the first push's timeout fires; lock released
		t.Fatal("first push should time out -> applied=false")
	}

	// Second push: capture its request_id, then inject the STALE failure ack from the first push while
	// the second is waiting, followed by the second's own success ack.
	done2 := make(chan bool, 1)
	go func() {
		resp, _ := s.SetSpawnModel(ctx, connect.NewRequest(&cpv1.SetSpawnModelRequest{SpawnId: "sp1", Model: "m-second"}))
		done2 <- resp.Msg.Applied
	}()
	reqID2 := <-sender.sent
	if reqID1 == reqID2 {
		t.Fatal("expected distinct per-push request_ids")
	}
	s.models.deliver(&nodev1.SetModelResult{SpawnId: "sp1", Ok: false, Detail: "stale failure from prior push", RequestId: reqID1})
	s.models.deliver(&nodev1.SetModelResult{SpawnId: "sp1", Ok: true, RequestId: reqID2})

	if applied2 := <-done2; !applied2 {
		t.Fatal("second push got its own success ack -> want applied=true (stale ack must not corrupt it)")
	}
	sp, _ := s.st.Spawns().Get(ctx, "sp1")
	if sp.Model != "m-second" || !sp.ModelApplied {
		t.Fatalf("persisted = model %q applied %v, want m-second true", sp.Model, sp.ModelApplied)
	}
}
