package store

import (
	"context"
	"errors"
	"testing"
)

func TestForkingStatusConstant(t *testing.T) {
	if string(Forking) != "forking" {
		t.Fatalf("Forking wire value=%q want %q", Forking, "forking")
	}
	for _, other := range []Status{Starting, Active, Suspending, Suspended, Resuming, Unreachable, Errored, Deleted} {
		if Forking == other {
			t.Fatalf("Forking must be distinct from %q", other)
		}
	}
}

func TestSchemaAcceptsForkingStatusAndDeadline(t *testing.T) {
	st := NewTestStore(t)
	seedAppAndOwner(t, st)
	ctx := context.Background()

	inTx(t, st, func(tx Store) error { return tx.Spawns().Create(ctx, newSpawn("sp-forking"), nil) })
	inTx(t, st, func(tx Store) error { return tx.Spawns().SetActive(ctx, "sp-forking", "node-a", 1) })
	inTx(t, st, func(tx Store) error { return tx.Spawns().SetForking(ctx, "sp-forking", 1, 250) })

	sp := spawnRow(t, st, "sp-forking")
	if sp.Status != Forking {
		t.Fatalf("status=%v want Forking", sp.Status)
	}
	if sp.ForkCaptureDeadline == nil || *sp.ForkCaptureDeadline != 250 {
		t.Fatalf("fork_capture_deadline=%v want 250", sp.ForkCaptureDeadline)
	}
}

func TestListRecoverableForkingIncludesExpiredClaimAndCaptureDeadline(t *testing.T) {
	st := NewTestStore(t)
	seedAppAndOwner(t, st)
	ctx := context.Background()

	makeForking := func(id string, claimDeadline int64, captureDeadline int64) {
		t.Helper()
		inTx(t, st, func(tx Store) error { return tx.Spawns().Create(ctx, newSpawn(id), nil) })
		inTx(t, st, func(tx Store) error { return tx.Spawns().SetActive(ctx, id, "node-a", 1) })
		seq := spawnSeq(t, st, id)
		gen := liveGen(t, st, id)
		newSeq, err := st.Spawns().Acquire(ctx, id, "driver", "lease-"+id, 10, claimDeadline, seq)
		if err != nil {
			t.Fatalf("Acquire %s: %v", id, err)
		}
		if _, err := st.Spawns().TransitionClaimed(ctx, id, "lease-"+id, newSeq, gen, Forking); err != nil {
			t.Fatalf("TransitionClaimed %s to Forking: %v", id, err)
		}
		if err := forceForkCaptureDeadline(ctx, st, id, captureDeadline); err != nil {
			t.Fatalf("forceForkCaptureDeadline %s: %v", id, err)
		}
	}

	makeForking("sp-expired-claim", 100, 1000)
	makeForking("sp-expired-capture", 1000, 100)
	makeForking("sp-live", 1000, 1000)

	rows, err := st.Spawns().ListRecoverableForking(ctx, 200)
	if err != nil {
		t.Fatalf("ListRecoverableForking: %v", err)
	}
	found := map[string]bool{}
	for _, sp := range rows {
		found[sp.ID] = true
	}
	if !found["sp-expired-claim"] {
		t.Fatalf("expired claim forking source missing from recovery set: %v", rows)
	}
	if !found["sp-expired-capture"] {
		t.Fatalf("expired capture deadline source missing from recovery set: %v", rows)
	}
	if found["sp-live"] {
		t.Fatalf("live forking source must not be recoverable: %v", rows)
	}
}

func TestAcquireForkingRecoveryPreemptsExpiredCaptureDeadline(t *testing.T) {
	st := NewTestStore(t)
	seedAppAndOwner(t, st)
	ctx := context.Background()

	inTx(t, st, func(tx Store) error { return tx.Spawns().Create(ctx, newSpawn("sp-wedged"), nil) })
	inTx(t, st, func(tx Store) error { return tx.Spawns().SetActive(ctx, "sp-wedged", "node-a", 1) })
	seq := spawnSeq(t, st, "sp-wedged")
	gen := liveGen(t, st, "sp-wedged")
	newSeq, err := st.Spawns().Acquire(ctx, "sp-wedged", "driver", "lease-driver", 10, 1000, seq)
	if err != nil {
		t.Fatalf("driver Acquire: %v", err)
	}
	if _, err := st.Spawns().TransitionClaimed(ctx, "sp-wedged", "lease-driver", newSeq, gen, Forking); err != nil {
		t.Fatalf("TransitionClaimed to Forking: %v", err)
	}
	if err := forceForkCaptureDeadline(ctx, st, "sp-wedged", 100); err != nil {
		t.Fatalf("force deadline: %v", err)
	}

	currentSeq := spawnSeq(t, st, "sp-wedged")
	recoverySeq, err := st.Spawns().AcquireForkingRecovery(ctx, "sp-wedged", "recovery", "lease-recovery", 200, 300, currentSeq)
	if err != nil {
		t.Fatalf("AcquireForkingRecovery must preempt an expired capture deadline despite a live claim: %v", err)
	}
	if recoverySeq != currentSeq+1 {
		t.Fatalf("recoverySeq=%d want %d", recoverySeq, currentSeq+1)
	}
	if err := st.Spawns().Heartbeat(ctx, "sp-wedged", "lease-driver", 2000); !errors.Is(err, ErrClaimLost) {
		t.Fatalf("old driver heartbeat after capture-deadline preemption: want ErrClaimLost, got %v", err)
	}
}

func TestTransitionForkingRecoveredClearsDeadline(t *testing.T) {
	st := NewTestStore(t)
	seedAppAndOwner(t, st)
	ctx := context.Background()

	inTx(t, st, func(tx Store) error { return tx.Spawns().Create(ctx, newSpawn("sp-recover"), nil) })
	inTx(t, st, func(tx Store) error { return tx.Spawns().SetActive(ctx, "sp-recover", "node-a", 1) })
	inTx(t, st, func(tx Store) error { return tx.Spawns().SetForking(ctx, "sp-recover", 1, 100) })

	seq := spawnSeq(t, st, "sp-recover")
	gen := liveGen(t, st, "sp-recover")
	recoverySeq, err := st.Spawns().AcquireForkingRecovery(ctx, "sp-recover", "recovery", "lease-recovery", 200, 300, seq)
	if err != nil {
		t.Fatalf("AcquireForkingRecovery: %v", err)
	}
	if _, err := st.Spawns().TransitionForkingRecovered(ctx, "sp-recover", "lease-recovery", recoverySeq, gen); err != nil {
		t.Fatalf("TransitionForkingRecovered: %v", err)
	}

	sp := spawnRow(t, st, "sp-recover")
	if sp.Status != Active {
		t.Fatalf("status=%v want Active", sp.Status)
	}
	if sp.ForkCaptureDeadline != nil {
		t.Fatalf("fork_capture_deadline=%v want nil", sp.ForkCaptureDeadline)
	}
}

func forceForkCaptureDeadline(ctx context.Context, st Store, id string, deadline int64) error {
	bs := st.(*bunStore)
	_, err := bs.db.NewUpdate().Model((*Spawn)(nil)).
		Set("fork_capture_deadline = ?", deadline).
		Where("id = ?", id).
		Exec(ctx)
	return err
}
