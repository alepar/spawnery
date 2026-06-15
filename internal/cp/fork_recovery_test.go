package cp

import (
	"context"
	"errors"
	"testing"
	"time"

	nodev1 "spawnery/gen/node/v1"
	"spawnery/internal/cp/registry"
	"spawnery/internal/cp/store"
)

type fakeForkPauseController struct {
	paused  map[string]bool
	unpause []string
	err     error
}

func (f *fakeForkPauseController) UnpauseIfPaused(_ context.Context, spawnID string, gen int64) error {
	_ = gen
	f.unpause = append(f.unpause, spawnID)
	if f.err != nil {
		return f.err
	}
	f.paused[spawnID] = false
	return nil
}

func newForkRecoveryTestServer(t *testing.T) (*Server, store.Store) {
	t.Helper()
	s, _, _ := newTestServer(t)
	s.claimTTL = time.Second
	now := time.Unix(0, 200)
	s.now = func() time.Time { return now }
	return s, s.st
}

func seedForkingSource(t *testing.T, st store.Store, id string, claimDeadline int64, captureDeadline int64) {
	t.Helper()
	ctx := context.Background()
	srv := &Server{st: st}
	makeSpawn(t, srv, id, "alice")
	if err := st.WithTx(ctx, func(tx store.Store) error { return tx.Spawns().SetActive(ctx, id, "node-a", 1) }); err != nil {
		t.Fatalf("SetActive %s: %v", id, err)
	}
	c, ok, err := st.Spawns().LiveContainer(ctx, id)
	if err != nil {
		t.Fatalf("LiveContainer %s: %v", id, err)
	}
	if !ok {
		t.Fatalf("LiveContainer %s missing", id)
	}
	if err := st.WithTx(ctx, func(tx store.Store) error { return tx.Spawns().SetForking(ctx, id, c.Generation, captureDeadline) }); err != nil {
		t.Fatalf("SetForking %s: %v", id, err)
	}
	sp, err := st.Spawns().Get(ctx, id)
	if err != nil {
		t.Fatalf("Get %s: %v", id, err)
	}
	if _, err := st.Spawns().Acquire(ctx, id, "driver", "lease-"+id, 10, claimDeadline, sp.StatusSeq); err != nil {
		t.Fatalf("Acquire %s: %v", id, err)
	}
}

func TestRecoverForkingSourcePrePauseIsIdempotent(t *testing.T) {
	s, st := newForkRecoveryTestServer(t)
	ctx := context.Background()
	seedForkingSource(t, st, "sp-pre", 100, 1000)

	pause := &fakeForkPauseController{paused: map[string]bool{"sp-pre": false}}
	if err := s.recoverForkingSources(ctx, pause); err != nil {
		t.Fatalf("recoverForkingSources: %v", err)
	}
	if err := s.recoverForkingSources(ctx, pause); err != nil {
		t.Fatalf("second recoverForkingSources: %v", err)
	}
	sp, err := st.Spawns().Get(ctx, "sp-pre")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if sp.Status != store.Active {
		t.Fatalf("status=%v want Active", sp.Status)
	}
	if sp.ForkCaptureDeadline != nil {
		t.Fatalf("fork_capture_deadline=%v want nil", sp.ForkCaptureDeadline)
	}
}

func TestRecoverForkingSourceMidPauseUnpausesThenCASesActive(t *testing.T) {
	s, st := newForkRecoveryTestServer(t)
	ctx := context.Background()
	seedForkingSource(t, st, "sp-mid", 100, 1000)

	pause := &fakeForkPauseController{paused: map[string]bool{"sp-mid": true}}
	if err := s.recoverForkingSources(ctx, pause); err != nil {
		t.Fatalf("recoverForkingSources: %v", err)
	}
	if pause.paused["sp-mid"] {
		t.Fatal("paused source must be unpaused")
	}
	if len(pause.unpause) != 1 || pause.unpause[0] != "sp-mid" {
		t.Fatalf("unpause calls=%v want [sp-mid]", pause.unpause)
	}
	sp, err := st.Spawns().Get(ctx, "sp-mid")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if sp.Status != store.Active {
		t.Fatalf("status=%v want Active", sp.Status)
	}
}

func TestRecoverForkingSourcePostUnpausePreCAS(t *testing.T) {
	s, st := newForkRecoveryTestServer(t)
	ctx := context.Background()
	seedForkingSource(t, st, "sp-post", 100, 1000)

	sp, err := st.Spawns().Get(ctx, "sp-post")
	if err != nil {
		t.Fatalf("Get before staged recovery: %v", err)
	}
	leaseID := "lease-recovery-stage"
	seq, err := st.Spawns().AcquireForkingRecovery(ctx, "sp-post", "recovery", leaseID, 200, 300, sp.StatusSeq)
	if err != nil {
		t.Fatalf("AcquireForkingRecovery staged: %v", err)
	}
	pause := &fakeForkPauseController{paused: map[string]bool{"sp-post": true}}
	if err := pause.UnpauseIfPaused(ctx, "sp-post", liveGenForCPTest(t, st, "sp-post")); err != nil {
		t.Fatalf("staged UnpauseIfPaused: %v", err)
	}
	if seq <= sp.StatusSeq {
		t.Fatalf("staged recovery seq=%d want > %d", seq, sp.StatusSeq)
	}
	staged, err := st.Spawns().Get(ctx, "sp-post")
	if err != nil {
		t.Fatalf("Get after staged recovery crash: %v", err)
	}
	if staged.ClaimLeaseID == nil || *staged.ClaimLeaseID != leaseID {
		t.Fatalf("staged recovery should leave crashed claim in place, got lease=%v", staged.ClaimLeaseID)
	}

	s.now = func() time.Time { return time.Unix(0, 400) }
	if err := s.recoverForkingSources(ctx, pause); err != nil {
		t.Fatalf("recoverForkingSources: %v", err)
	}
	got, err := st.Spawns().Get(ctx, "sp-post")
	if err != nil {
		t.Fatalf("Get after recovery: %v", err)
	}
	if got.Status != store.Active {
		t.Fatalf("status=%v want Active after post-unpause recovery", got.Status)
	}
}

func TestRecoverForkingSourceWedgedCaptureDeadlinePreemptsLiveClaim(t *testing.T) {
	s, st := newForkRecoveryTestServer(t)
	ctx := context.Background()
	seedForkingSource(t, st, "sp-wedge", 1000, 100)

	pause := &fakeForkPauseController{paused: map[string]bool{"sp-wedge": true}}
	if err := s.recoverForkingSources(ctx, pause); err != nil {
		t.Fatalf("recoverForkingSources: %v", err)
	}
	sp, err := st.Spawns().Get(ctx, "sp-wedge")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if sp.Status != store.Active {
		t.Fatalf("status=%v want Active", sp.Status)
	}
	if err := st.Spawns().Heartbeat(ctx, "sp-wedge", "lease-sp-wedge", 2000); !errors.Is(err, store.ErrClaimLost) {
		t.Fatalf("old driver heartbeat: want ErrClaimLost, got %v", err)
	}
}

func TestReconcileTickRecoversForkingSourceThroughNodeUnpause(t *testing.T) {
	s, st := newForkRecoveryTestServer(t)
	s.forkUnpauses = newForkUnpauseWaiters()
	ctx := context.Background()
	seedForkingSource(t, st, "sp-reconcile", 100, 100)
	sender := &capSender{}
	s.reg.Add(&registry.Node{ID: "node-a", Sender: sender, Max: 1, Free: 1})

	done := make(chan struct{})
	go func() {
		for {
			if msg := sender.lastCPMessage(); msg != nil && msg.GetUnpauseIfPaused() != nil {
				cmd := msg.GetUnpauseIfPaused()
				s.deliverUnpauseIfPausedComplete(&nodev1.UnpauseIfPausedComplete{
					SpawnId:    cmd.GetSpawnId(),
					Generation: cmd.GetGeneration(),
				})
				close(done)
				return
			}
			time.Sleep(time.Millisecond)
		}
	}()

	s.reconcileTick(ctx)
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("reconcile did not send UnpauseIfPaused")
	}
	sp, err := st.Spawns().Get(ctx, "sp-reconcile")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if sp.Status != store.Active || sp.ForkCaptureDeadline != nil {
		t.Fatalf("after reconcile = status %s deadline %v, want active nil", sp.Status, sp.ForkCaptureDeadline)
	}
}

func liveGenForCPTest(t *testing.T, st store.Store, id string) int64 {
	t.Helper()
	c, ok, err := st.Spawns().LiveContainer(context.Background(), id)
	if err != nil || !ok {
		t.Fatalf("LiveContainer(%s): ok=%v err=%v", id, ok, err)
	}
	return c.Generation
}
