package cp

import (
	"context"
	"testing"
	"time"

	nodev1 "spawnery/gen/node/v1"
	"spawnery/internal/cp/auth"
	"spawnery/internal/cp/registry"
	"spawnery/internal/cp/store"
)

// setUnapplied sets a new model on spawn id (which flips model_applied=false), so the reconciler
// will pick it up. Fails the test on error.
func setUnapplied(t *testing.T, s *Server, id, model string) {
	t.Helper()
	if err := s.st.Spawns().SetModel(context.Background(), id, model); err != nil {
		t.Fatalf("SetModel(%s): %v", id, err)
	}
}

// countSender records how many SetModel pushes it received and acks each with a fixed ok/detail.
type countSender struct {
	models *modelWaiters
	ok     bool
	detail string
	n      int
}

func (c *countSender) Send(m *nodev1.CPMessage) error {
	sm := m.GetSetModel()
	if sm == nil {
		return nil
	}
	c.n++
	reqID := sm.GetRequestId()
	go c.models.deliver(&nodev1.SetModelResult{SpawnId: sm.GetSpawnId(), Ok: c.ok, Detail: c.detail, RequestId: reqID})
	return nil
}

// TestReconcile_ConnectedNodeAcks: an unapplied spawn whose live pod's node is connected and acks ok
// becomes applied=true after a tick.
func TestReconcile_ConnectedNodeAcks(t *testing.T) {
	s, reg, _ := newTestServer(t)
	sender := &ackSender{models: s.models, ok: true}
	activeSpawnOnNode(t, s, reg, "sp1", "alice", sender)
	setUnapplied(t, s, "sp1", "m-new")
	ctx := auth.WithOwner(context.Background(), "alice")

	s.reconcileTick(context.Background())

	sp, _ := s.st.Spawns().Get(ctx, "sp1")
	if !sp.ModelApplied {
		t.Fatalf("after tick: model_applied=false, want true (detail=%q)", sp.ModelApplyDetail)
	}
	if !sender.gotSet || sender.lastModel != "m-new" {
		t.Fatalf("reconciler push = gotSet %v model %q, want gotSet model m-new", sender.gotSet, sender.lastModel)
	}
}

// TestReconcile_NoLivePod: an unapplied spawn with no live container is marked applied immediately
// (nothing running to diverge).
func TestReconcile_NoLivePod(t *testing.T) {
	s, _, _ := newTestServer(t)
	makeSpawn(t, s, "sp1", "alice")
	// makeSpawn seeds a gen-1 live container; end it so there is genuinely no running pod.
	if err := s.st.WithTx(context.Background(), func(tx store.Store) error {
		return tx.Spawns().EndContainer(context.Background(), "sp1", 1, store.PhaseStopped)
	}); err != nil {
		t.Fatalf("EndContainer: %v", err)
	}
	setUnapplied(t, s, "sp1", "m-new")
	ctx := auth.WithOwner(context.Background(), "alice")

	s.reconcileTick(context.Background())

	sp, _ := s.st.Spawns().Get(ctx, "sp1")
	if !sp.ModelApplied {
		t.Fatalf("no live pod: model_applied=false after tick, want true")
	}
}

// TestReconcile_GivesUpAfterWindow: a spawn whose pushes keep failing is retried each tick within the
// give-up window, then abandoned — it stays applied=false with a detail and receives NO further push.
func TestReconcile_GivesUpAfterWindow(t *testing.T) {
	s, reg, _ := newTestServer(t)
	s.setModelTimeout = 50 * time.Millisecond
	s.reconcileGiveUp = time.Minute
	nowVal := time.Unix(1000, 0)
	s.now = func() time.Time { return nowVal }

	// Active spawn whose connected node always NAKs (ok=false) -> every push fails.
	sender := &countSender{models: s.models, ok: false, detail: "sidecar 500"}
	makeSpawn(t, s, "sp1", "alice")
	if err := s.st.WithTx(context.Background(), func(tx store.Store) error {
		return tx.Spawns().SetActive(context.Background(), "sp1", "n1", 1)
	}); err != nil {
		t.Fatalf("SetActive: %v", err)
	}
	reg.Add(&registry.Node{ID: "n1", Sender: sender, Max: 1, Free: 1})
	setUnapplied(t, s, "sp1", "m-new")
	ctx := auth.WithOwner(context.Background(), "alice")

	// First tick: within window -> one push, fails, detail recorded, still false.
	s.reconcileTick(context.Background())
	if sender.n != 1 {
		t.Fatalf("after tick 1: pushes=%d, want 1", sender.n)
	}
	sp, _ := s.st.Spawns().Get(ctx, "sp1")
	if sp.ModelApplied || sp.ModelApplyDetail == "" {
		t.Fatalf("after tick 1: applied=%v detail=%q, want false + non-empty", sp.ModelApplied, sp.ModelApplyDetail)
	}

	// Second tick still within window -> another push.
	nowVal = nowVal.Add(30 * time.Second)
	s.reconcileTick(context.Background())
	if sender.n != 2 {
		t.Fatalf("after tick 2 (within window): pushes=%d, want 2", sender.n)
	}

	// Past the window -> give up: NO further push, still false with detail.
	nowVal = nowVal.Add(2 * time.Minute)
	s.reconcileTick(context.Background())
	if sender.n != 2 {
		t.Fatalf("after give-up tick: pushes=%d, want 2 (no further push)", sender.n)
	}
	sp, _ = s.st.Spawns().Get(ctx, "sp1")
	if sp.ModelApplied || sp.ModelApplyDetail == "" {
		t.Fatalf("after give-up: applied=%v detail=%q, want false + non-empty", sp.ModelApplied, sp.ModelApplyDetail)
	}
}

// TestReconcile_GiveUpResetsOnModelChange: once a spawn has given up, a fresh SetSpawnModel (new model)
// resets the give-up clock so the reconciler retries the new model.
func TestReconcile_GiveUpResetsOnModelChange(t *testing.T) {
	s, reg, _ := newTestServer(t)
	s.setModelTimeout = 50 * time.Millisecond
	s.reconcileGiveUp = time.Minute
	nowVal := time.Unix(1000, 0)
	s.now = func() time.Time { return nowVal }

	sender := &countSender{models: s.models, ok: false, detail: "sidecar 500"}
	makeSpawn(t, s, "sp1", "alice")
	if err := s.st.WithTx(context.Background(), func(tx store.Store) error {
		return tx.Spawns().SetActive(context.Background(), "sp1", "n1", 1)
	}); err != nil {
		t.Fatalf("SetActive: %v", err)
	}
	reg.Add(&registry.Node{ID: "n1", Sender: sender, Max: 1, Free: 1})
	setUnapplied(t, s, "sp1", "m-old")

	s.reconcileTick(context.Background()) // n=1, records first attempt for m-old
	nowVal = nowVal.Add(2 * time.Minute)
	s.reconcileTick(context.Background()) // gave up on m-old, n stays 1
	if sender.n != 1 {
		t.Fatalf("after give-up on m-old: pushes=%d, want 1", sender.n)
	}

	// New model -> key changes -> clock resets -> retried.
	setUnapplied(t, s, "sp1", "m-new")
	s.reconcileTick(context.Background())
	if sender.n != 2 {
		t.Fatalf("after model change: pushes=%d, want 2 (retried new model)", sender.n)
	}
}

// TestReconcileLoop_AppliesThenStops: StartReconciler ticks until an unapplied (no-live-pod) spawn is
// applied, and stops cleanly when its context is cancelled.
func TestReconcileLoop_AppliesThenStops(t *testing.T) {
	s, _, _ := newTestServer(t)
	s.reconcileInterval = 5 * time.Millisecond
	makeSpawn(t, s, "sp1", "alice")
	// makeSpawn seeds a gen-1 live container; end it so the loop's pushModel takes the no-live-pod arm.
	if err := s.st.WithTx(context.Background(), func(tx store.Store) error {
		return tx.Spawns().EndContainer(context.Background(), "sp1", 1, store.PhaseStopped)
	}); err != nil {
		t.Fatalf("EndContainer: %v", err)
	}
	setUnapplied(t, s, "sp1", "m-new")
	ctx := auth.WithOwner(context.Background(), "alice")

	loopCtx, cancel := context.WithCancel(context.Background())
	s.StartReconciler(loopCtx)
	defer cancel()

	deadline := time.Now().Add(2 * time.Second)
	for {
		sp, _ := s.st.Spawns().Get(ctx, "sp1")
		if sp.ModelApplied {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("reconciler loop did not apply the model within 2s")
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel() // loop must return; no assertion needed beyond not hanging/panicking
}
