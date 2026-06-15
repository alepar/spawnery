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

func TestTransitionForkingRecoveredClearsClaimAtomically(t *testing.T) {
	st := NewTestStore(t)
	seedAppAndOwner(t, st)
	ctx := context.Background()

	inTx(t, st, func(tx Store) error { return tx.Spawns().Create(ctx, newSpawn("sp-recover-claim"), nil) })
	inTx(t, st, func(tx Store) error { return tx.Spawns().SetActive(ctx, "sp-recover-claim", "node-a", 1) })
	inTx(t, st, func(tx Store) error { return tx.Spawns().SetForking(ctx, "sp-recover-claim", 1, 100) })

	seq := spawnSeq(t, st, "sp-recover-claim")
	gen := liveGen(t, st, "sp-recover-claim")
	recoverySeq, err := st.Spawns().AcquireForkingRecovery(ctx, "sp-recover-claim", "recovery", "lease-recovery", 200, 300, seq)
	if err != nil {
		t.Fatalf("AcquireForkingRecovery: %v", err)
	}
	newSeq, err := st.Spawns().TransitionForkingRecovered(ctx, "sp-recover-claim", "lease-recovery", recoverySeq, gen)
	if err != nil {
		t.Fatalf("TransitionForkingRecovered: %v", err)
	}

	sp := spawnRow(t, st, "sp-recover-claim")
	if sp.Status != Active || sp.ClaimHolder != nil || sp.ClaimLeaseID != nil || sp.ClaimDeadline != nil {
		t.Fatalf("recovered spawn = status:%v holder:%v lease:%v deadline:%v, want active with no claim", sp.Status, sp.ClaimHolder, sp.ClaimLeaseID, sp.ClaimDeadline)
	}
	if _, err := st.Spawns().Acquire(ctx, "sp-recover-claim", "competitor", "lease-competitor", 400, 500, newSeq); err != nil {
		t.Fatalf("recovered source must be immediately claimable: %v", err)
	}
}

func TestMarkDeletedClaimedHidesRowClearsMetadataAndEndsContainer(t *testing.T) {
	st := NewTestStore(t)
	seedAppAndOwner(t, st)
	ctx := context.Background()
	nowNS := int64(500_000_000_000)
	claimDeadlineNS := int64(530_000_000_000)
	deletedAtUnix := int64(500)

	inTx(t, st, func(tx Store) error { return tx.Spawns().Create(ctx, newSpawn("sp-delete"), nil) })
	inTx(t, st, func(tx Store) error { return tx.Spawns().SetActive(ctx, "sp-delete", "node-a", 1) })
	inTx(t, st, func(tx Store) error { return tx.Spawns().SetForking(ctx, "sp-delete", 1, 250) })
	seq := spawnSeq(t, st, "sp-delete")
	gen := liveGen(t, st, "sp-delete")
	claimedSeq, err := st.Spawns().Acquire(ctx, "sp-delete", "cleanup", "lease-delete", nowNS, claimDeadlineNS, seq)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	claimed := rawSpawnRow(t, st, "sp-delete")
	if claimed.ClaimDeadline == nil || *claimed.ClaimDeadline != claimDeadlineNS {
		t.Fatalf("claim_deadline=%v want nanosecond deadline %d", claimed.ClaimDeadline, claimDeadlineNS)
	}

	newSeq, err := st.Spawns().MarkDeletedClaimed(ctx, "sp-delete", "lease-delete", claimedSeq, gen, deletedAtUnix)
	if err != nil {
		t.Fatalf("MarkDeletedClaimed: %v", err)
	}
	if newSeq != claimedSeq+1 {
		t.Fatalf("newSeq=%d want %d", newSeq, claimedSeq+1)
	}
	if _, err := st.Spawns().Get(ctx, "sp-delete"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get after MarkDeletedClaimed: want ErrNotFound, got %v", err)
	}

	sp := rawSpawnRow(t, st, "sp-delete")
	if sp.Status != Deleted {
		t.Fatalf("status=%v want Deleted", sp.Status)
	}
	if sp.DeletedAt == nil || *sp.DeletedAt != deletedAtUnix {
		t.Fatalf("deleted_at=%v want Unix seconds %d", sp.DeletedAt, deletedAtUnix)
	}
	if sp.ForkCaptureDeadline != nil {
		t.Fatalf("fork_capture_deadline=%v want nil", sp.ForkCaptureDeadline)
	}
	if sp.ClaimHolder != nil || sp.ClaimLeaseID != nil || sp.ClaimDeadline != nil {
		t.Fatalf("claim metadata not cleared: holder=%v lease=%v deadline=%v", sp.ClaimHolder, sp.ClaimLeaseID, sp.ClaimDeadline)
	}
	if _, ok, err := st.Spawns().LiveContainer(ctx, "sp-delete"); err != nil || ok {
		t.Fatalf("LiveContainer after delete: ok=%v err=%v, want no live container", ok, err)
	}
	c, ok, err := st.Spawns().LatestContainer(ctx, "sp-delete")
	if err != nil || !ok {
		t.Fatalf("LatestContainer: ok=%v err=%v", ok, err)
	}
	if c.Phase != PhaseLost || c.EndedAt == nil || *c.EndedAt != deletedAtUnix {
		t.Fatalf("latest container=%+v want lost ended_at=%d", c, deletedAtUnix)
	}
}

func TestMarkDeletedClaimedRollsBackWhenEndingLiveContainerFails(t *testing.T) {
	st := NewTestStore(t)
	seedAppAndOwner(t, st)
	ctx := context.Background()

	inTx(t, st, func(tx Store) error { return tx.Spawns().Create(ctx, newSpawn("sp-rollback"), nil) })
	inTx(t, st, func(tx Store) error { return tx.Spawns().SetActive(ctx, "sp-rollback", "node-a", 1) })
	seq := spawnSeq(t, st, "sp-rollback")
	gen := liveGen(t, st, "sp-rollback")
	claimedSeq, err := st.Spawns().Acquire(ctx, "sp-rollback", "cleanup", "lease-rollback", 10, 100, seq)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	bs := st.(*bunStore)
	if _, err := bs.db.NewRaw(`
CREATE TRIGGER fail_end_live_container
BEFORE UPDATE OF ended_at ON spawn_containers
WHEN NEW.ended_at = 777
BEGIN
  SELECT RAISE(ABORT, 'injected end live failure');
END;
`).Exec(ctx); err != nil {
		t.Fatalf("create trigger: %v", err)
	}

	_, err = st.Spawns().MarkDeletedClaimed(ctx, "sp-rollback", "lease-rollback", claimedSeq, gen, 777)
	if err == nil {
		t.Fatal("MarkDeletedClaimed must return injected end-live-container failure")
	}
	sp, err := st.Spawns().Get(ctx, "sp-rollback")
	if err != nil {
		t.Fatalf("Get after failed MarkDeletedClaimed: %v", err)
	}
	if sp.Status == Deleted {
		t.Fatalf("failed MarkDeletedClaimed must roll back deleted status: %+v", sp)
	}
	if _, ok, err := st.Spawns().LiveContainer(ctx, "sp-rollback"); err != nil || !ok {
		t.Fatalf("LiveContainer after failed MarkDeletedClaimed: ok=%v err=%v, want live", ok, err)
	}
}

func TestMarkDeletedClaimedFencesLeaseSeqAndGeneration(t *testing.T) {
	for _, tc := range []struct {
		name      string
		leaseID   string
		seqDelta  int64
		genDelta  int64
		wantError error
	}{
		{name: "lease", leaseID: "wrong-lease", wantError: ErrConflict},
		{name: "seq", seqDelta: -1, wantError: ErrConflict},
		{name: "generation", genDelta: 1, wantError: ErrConflict},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := NewTestStore(t)
			seedAppAndOwner(t, st)
			ctx := context.Background()

			id := "sp-fence-" + tc.name
			inTx(t, st, func(tx Store) error { return tx.Spawns().Create(ctx, newSpawn(id), nil) })
			inTx(t, st, func(tx Store) error { return tx.Spawns().SetActive(ctx, id, "node-a", 1) })
			seq := spawnSeq(t, st, id)
			gen := liveGen(t, st, id)
			claimedSeq, err := st.Spawns().Acquire(ctx, id, "cleanup", "lease-delete", 10, 100, seq)
			if err != nil {
				t.Fatalf("Acquire: %v", err)
			}

			leaseID := "lease-delete"
			if tc.leaseID != "" {
				leaseID = tc.leaseID
			}
			_, err = st.Spawns().MarkDeletedClaimed(ctx, id, leaseID, claimedSeq+tc.seqDelta, gen+tc.genDelta, 500)
			if !errors.Is(err, tc.wantError) {
				t.Fatalf("MarkDeletedClaimed fenced %s: want %v, got %v", tc.name, tc.wantError, err)
			}
			sp, err := st.Spawns().Get(ctx, id)
			if err != nil {
				t.Fatalf("Get after failed MarkDeletedClaimed: %v", err)
			}
			if sp.Status == Deleted {
				t.Fatalf("failed fenced delete must not mark row deleted: %+v", sp)
			}
			if _, ok, err := st.Spawns().LiveContainer(ctx, id); err != nil || !ok {
				t.Fatalf("LiveContainer after failed fenced delete: ok=%v err=%v, want live", ok, err)
			}
		})
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

func rawSpawnRow(t *testing.T, st Store, id string) Spawn {
	t.Helper()
	bs := st.(*bunStore)
	var sp Spawn
	if err := bs.db.NewSelect().Model(&sp).Where("id = ?", id).Scan(context.Background()); err != nil {
		t.Fatalf("raw Spawn(%s): %v", id, err)
	}
	return sp
}
