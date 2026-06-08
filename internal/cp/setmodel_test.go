package cp

import (
	"context"
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
	if a.drop {
		return nil
	}
	go func() {
		if a.delay > 0 {
			time.Sleep(a.delay)
		}
		a.models.deliver(&nodev1.SetModelResult{SpawnId: sm.GetSpawnId(), Ok: a.ok, Detail: a.detail})
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
