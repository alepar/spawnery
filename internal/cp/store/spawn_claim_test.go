package store

import (
	"context"
	"errors"
	"testing"
)

// spawnSeq reads the current status_seq for a spawn (panics on error — test helper).
func spawnSeq(t *testing.T, st Store, id string) int64 {
	t.Helper()
	s, err := st.Spawns().Get(context.Background(), id)
	if err != nil {
		t.Fatalf("spawnSeq Get(%s): %v", id, err)
	}
	return s.StatusSeq
}

// spawnRow reads the full Spawn row (panics on error — test helper).
func spawnRow(t *testing.T, st Store, id string) Spawn {
	t.Helper()
	s, err := st.Spawns().Get(context.Background(), id)
	if err != nil {
		t.Fatalf("spawnRow Get(%s): %v", id, err)
	}
	return s
}

// TestAcquireHappyPath: Acquire on a free spawn with a matching status_seq sets claim columns
// and returns newSeq = expectedSeq+1.
func TestAcquireHappyPath(t *testing.T) {
	st := NewTestStore(t)
	seedAppAndOwner(t, st)
	ctx := context.Background()

	inTx(t, st, func(tx Store) error { return tx.Spawns().Create(ctx, newSpawn("sp1"), nil) })
	// status_seq = 0 after Create (no guardStatus called yet)
	seq := spawnSeq(t, st, "sp1")
	if seq != 0 {
		t.Fatalf("expected status_seq=0 after create, got %d", seq)
	}

	newSeq, err := st.Spawns().Acquire(ctx, "sp1", "driver-1", "lease-abc", 100, 200, seq)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if newSeq != seq+1 {
		t.Fatalf("newSeq=%d want %d", newSeq, seq+1)
	}

	s := spawnRow(t, st, "sp1")
	if s.StatusSeq != newSeq {
		t.Fatalf("status_seq=%d want %d", s.StatusSeq, newSeq)
	}
	if s.ClaimHolder == nil || *s.ClaimHolder != "driver-1" {
		t.Fatalf("claim_holder=%v want driver-1", s.ClaimHolder)
	}
	if s.ClaimLeaseID == nil || *s.ClaimLeaseID != "lease-abc" {
		t.Fatalf("claim_lease_id=%v want lease-abc", s.ClaimLeaseID)
	}
	if s.ClaimDeadline == nil || *s.ClaimDeadline != 200 {
		t.Fatalf("claim_deadline=%v want 200", s.ClaimDeadline)
	}
}

// TestAcquireStaleSeqConflict: Acquire with a stale status_seq returns ErrConflict.
func TestAcquireStaleSeqConflict(t *testing.T) {
	st := NewTestStore(t)
	seedAppAndOwner(t, st)
	ctx := context.Background()

	inTx(t, st, func(tx Store) error { return tx.Spawns().Create(ctx, newSpawn("sp1"), nil) })
	inTx(t, st, func(tx Store) error { return tx.Spawns().SetActive(ctx, "sp1", "n", 1) })
	// status_seq is now 1 (bumped by SetActive via guardStatus)
	seq := spawnSeq(t, st, "sp1")

	// use stale seq (seq-1)
	_, err := st.Spawns().Acquire(ctx, "sp1", "driver-1", "lease-abc", 100, 200, seq-1)
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("stale-seq Acquire: want ErrConflict, got %v", err)
	}
}

// TestAcquireActivityBumpInvalidatesReaper: mimics the idle-reaper TOCTOU: read seq, Touch bumps it,
// Acquire on the old seq → ErrConflict.
func TestAcquireActivityBumpInvalidatesReaper(t *testing.T) {
	st := NewTestStore(t)
	seedAppAndOwner(t, st)
	ctx := context.Background()

	inTx(t, st, func(tx Store) error { return tx.Spawns().Create(ctx, newSpawn("sp1"), nil) })
	inTx(t, st, func(tx Store) error { return tx.Spawns().SetActive(ctx, "sp1", "n", 1) })

	// reaper reads the seq
	seenSeq := spawnSeq(t, st, "sp1")

	// activity arrives: Touch bumps status_seq
	if err := st.Spawns().Touch(ctx, "sp1", 999); err != nil {
		t.Fatalf("Touch: %v", err)
	}
	freshSeq := spawnSeq(t, st, "sp1")
	if freshSeq != seenSeq+1 {
		t.Fatalf("Touch must bump status_seq: seenSeq=%d freshSeq=%d", seenSeq, freshSeq)
	}

	// reaper tries to Acquire with the old seq → conflict
	_, err := st.Spawns().Acquire(ctx, "sp1", "reaper", "lease-r", 1000, 2000, seenSeq)
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("stale-after-Touch Acquire: want ErrConflict, got %v", err)
	}
}

// TestAcquireExpiredClaimTakeover: a second holder can Acquire once nowTS > claim_deadline.
func TestAcquireExpiredClaimTakeover(t *testing.T) {
	st := NewTestStore(t)
	seedAppAndOwner(t, st)
	ctx := context.Background()

	inTx(t, st, func(tx Store) error { return tx.Spawns().Create(ctx, newSpawn("sp1"), nil) })
	seq := spawnSeq(t, st, "sp1")

	// first holder acquires with deadline=100
	newSeq, err := st.Spawns().Acquire(ctx, "sp1", "holder-1", "lease-1", 50, 100, seq)
	if err != nil {
		t.Fatalf("first Acquire: %v", err)
	}

	// second holder at nowTS=50 (claim still active) → ErrConflict
	_, err = st.Spawns().Acquire(ctx, "sp1", "holder-2", "lease-2", 50, 300, newSeq)
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("active claim takeover: want ErrConflict, got %v", err)
	}

	// second holder at nowTS=150 (claim expired) → success
	newSeq2, err := st.Spawns().Acquire(ctx, "sp1", "holder-2", "lease-2", 150, 300, newSeq)
	if err != nil {
		t.Fatalf("expired-claim takeover: %v", err)
	}
	if newSeq2 != newSeq+1 {
		t.Fatalf("newSeq2=%d want %d", newSeq2, newSeq+1)
	}
	s := spawnRow(t, st, "sp1")
	if s.ClaimHolder == nil || *s.ClaimHolder != "holder-2" {
		t.Fatalf("claim_holder=%v want holder-2", s.ClaimHolder)
	}
}

// TestHeartbeatExtendsDeadlineAndLostOnRelease: Heartbeat extends the deadline; after Release the
// claim is gone and Heartbeat returns ErrClaimLost.
func TestHeartbeatExtendsDeadlineAndLostOnRelease(t *testing.T) {
	st := NewTestStore(t)
	seedAppAndOwner(t, st)
	ctx := context.Background()

	inTx(t, st, func(tx Store) error { return tx.Spawns().Create(ctx, newSpawn("sp1"), nil) })
	seq := spawnSeq(t, st, "sp1")

	newSeq, err := st.Spawns().Acquire(ctx, "sp1", "driver-1", "lease-hb", 10, 100, seq)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}

	// Heartbeat extends deadline; must NOT bump status_seq
	if err := st.Spawns().Heartbeat(ctx, "sp1", "lease-hb", 500); err != nil {
		t.Fatalf("Heartbeat: %v", err)
	}
	s := spawnRow(t, st, "sp1")
	if s.ClaimDeadline == nil || *s.ClaimDeadline != 500 {
		t.Fatalf("claim_deadline after Heartbeat=%v want 500", s.ClaimDeadline)
	}
	if s.StatusSeq != newSeq {
		// Heartbeat must NOT bump status_seq
		t.Fatalf("Heartbeat bumped status_seq: was %d now %d", newSeq, s.StatusSeq)
	}

	// Release the claim
	if err := st.Spawns().Release(ctx, "sp1", "lease-hb"); err != nil {
		t.Fatalf("Release: %v", err)
	}

	// Heartbeat after release → ErrClaimLost
	if err := st.Spawns().Heartbeat(ctx, "sp1", "lease-hb", 999); !errors.Is(err, ErrClaimLost) {
		t.Fatalf("Heartbeat after release: want ErrClaimLost, got %v", err)
	}
}

// TestReleaseClaimsColumnsAndBumpsSeq: Release clears claim columns, bumps status_seq, and is
// lease-fenced (a stale leaseID returns ErrClaimLost).
func TestReleaseClaimsColumnsAndBumpsSeq(t *testing.T) {
	st := NewTestStore(t)
	seedAppAndOwner(t, st)
	ctx := context.Background()

	inTx(t, st, func(tx Store) error { return tx.Spawns().Create(ctx, newSpawn("sp1"), nil) })
	seq := spawnSeq(t, st, "sp1")

	newSeq, err := st.Spawns().Acquire(ctx, "sp1", "driver-1", "lease-rel", 10, 200, seq)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}

	// wrong lease → ErrClaimLost
	if err := st.Spawns().Release(ctx, "sp1", "wrong-lease"); !errors.Is(err, ErrClaimLost) {
		t.Fatalf("Release wrong lease: want ErrClaimLost, got %v", err)
	}

	// correct lease → success
	if err := st.Spawns().Release(ctx, "sp1", "lease-rel"); err != nil {
		t.Fatalf("Release: %v", err)
	}

	s := spawnRow(t, st, "sp1")
	if s.ClaimHolder != nil {
		t.Fatalf("claim_holder after Release=%v want nil", s.ClaimHolder)
	}
	if s.ClaimLeaseID != nil {
		t.Fatalf("claim_lease_id after Release=%v want nil", s.ClaimLeaseID)
	}
	if s.ClaimDeadline != nil {
		t.Fatalf("claim_deadline after Release=%v want nil", s.ClaimDeadline)
	}
	if s.StatusSeq != newSeq+1 {
		t.Fatalf("status_seq after Release=%d want %d", s.StatusSeq, newSeq+1)
	}
}

// TestTransitionClaimedHappyPath: Acquire then TransitionClaimed flips status and bumps status_seq.
func TestTransitionClaimedHappyPath(t *testing.T) {
	st := NewTestStore(t)
	seedAppAndOwner(t, st)
	ctx := context.Background()

	inTx(t, st, func(tx Store) error { return tx.Spawns().Create(ctx, newSpawn("sp1"), nil) })
	inTx(t, st, func(tx Store) error { return tx.Spawns().SetActive(ctx, "sp1", "n", 1) })

	seq := spawnSeq(t, st, "sp1")
	gen := liveGen(t, st, "sp1")

	// Acquire the claim
	newSeq, err := st.Spawns().Acquire(ctx, "sp1", "driver-1", "lease-tc", 10, 200, seq)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}

	// TransitionClaimed: Active → Suspending
	finalSeq, err := st.Spawns().TransitionClaimed(ctx, "sp1", "lease-tc", newSeq, gen, Suspending)
	if err != nil {
		t.Fatalf("TransitionClaimed: %v", err)
	}
	if finalSeq != newSeq+1 {
		t.Fatalf("finalSeq=%d want %d", finalSeq, newSeq+1)
	}

	s := spawnRow(t, st, "sp1")
	if s.Status != Suspending {
		t.Fatalf("status=%v want suspending", s.Status)
	}
	if s.StatusSeq != finalSeq {
		t.Fatalf("status_seq=%d want %d", s.StatusSeq, finalSeq)
	}
}

// TestTransitionClaimedFencedByRecreatedGeneration: a recreated generation (via ClaimStarting)
// invalidates TransitionClaimed even when status_seq and leaseID match.
func TestTransitionClaimedFencedByRecreatedGeneration(t *testing.T) {
	st := NewTestStore(t)
	seedAppAndOwner(t, st)
	ctx := context.Background()

	inTx(t, st, func(tx Store) error { return tx.Spawns().Create(ctx, newSpawn("sp1"), nil) })
	inTx(t, st, func(tx Store) error { return tx.Spawns().SetActive(ctx, "sp1", "n", 1) })

	seq := spawnSeq(t, st, "sp1")
	oldGen := liveGen(t, st, "sp1") // gen=1

	// Acquire the claim (our driver holds the lease)
	newSeq, err := st.Spawns().Acquire(ctx, "sp1", "driver-1", "lease-gen", 10, 200, seq)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}

	// A separate actor calls ClaimStarting (simulates a pod recreate), which:
	//   - bumps status to Starting via guardStatus (bumps status_seq)
	//   - ends the old container and inserts a new one at gen=2
	// Note: ClaimStarting does not clear the claim columns — our leaseID is still on the row.
	inTx(t, st, func(tx Store) error {
		_, err := tx.Spawns().ClaimStarting(ctx, "sp1", []Status{Active})
		return err
	})

	// Re-read the current seq (ClaimStarting bumped it again)
	currentSeq := spawnSeq(t, st, "sp1")
	if currentSeq <= newSeq {
		t.Fatalf("ClaimStarting must bump status_seq beyond Acquire's newSeq: newSeq=%d currentSeq=%d", newSeq, currentSeq)
	}

	// TransitionClaimed with current seq + leaseID but OLD generation → ErrConflict
	// (live container is now gen=2; oldGen=1 doesn't match the subquery)
	_, err = st.Spawns().TransitionClaimed(ctx, "sp1", "lease-gen", currentSeq, oldGen, Suspending)
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("old-gen TransitionClaimed: want ErrConflict, got %v", err)
	}
}

// TestListStrandedAndRevert: ListStranded returns spawns in transient status with no/expired claim,
// and a revert via Acquire+TransitionClaimed brings the spawn back to Active.
func TestListStrandedAndRevert(t *testing.T) {
	st := NewTestStore(t)
	seedAppAndOwner(t, st)
	ctx := context.Background()

	// sp-strand: in Suspending with NO claim (stranded — driver died before claiming)
	inTx(t, st, func(tx Store) error { return tx.Spawns().Create(ctx, newSpawn("sp-strand"), nil) })
	inTx(t, st, func(tx Store) error { return tx.Spawns().SetActive(ctx, "sp-strand", "n", 1) })
	inTx(t, st, func(tx Store) error { return tx.Spawns().SetSuspending(ctx, "sp-strand", 1) })
	// claim columns remain NULL — driver never called Acquire

	// sp-exp: in Suspending with an EXPIRED claim (deadline < nowTS)
	inTx(t, st, func(tx Store) error { return tx.Spawns().Create(ctx, newSpawn("sp-exp"), nil) })
	inTx(t, st, func(tx Store) error { return tx.Spawns().SetActive(ctx, "sp-exp", "n", 1) })
	seqExp := spawnSeq(t, st, "sp-exp")
	genExp := liveGen(t, st, "sp-exp")
	newSeqExp, err := st.Spawns().Acquire(ctx, "sp-exp", "d", "lease-exp", 50, 100, seqExp)
	if err != nil {
		t.Fatalf("Acquire sp-exp: %v", err)
	}
	if _, err := st.Spawns().TransitionClaimed(ctx, "sp-exp", "lease-exp", newSeqExp, genExp, Suspending); err != nil {
		t.Fatalf("TransitionClaimed sp-exp: %v", err)
	}
	// claim deadline=100, nowTS=200 → expired

	// sp-live: in Suspending with an ACTIVE claim (deadline=500 > nowTS=200) — must NOT appear
	inTx(t, st, func(tx Store) error { return tx.Spawns().Create(ctx, newSpawn("sp-live"), nil) })
	inTx(t, st, func(tx Store) error { return tx.Spawns().SetActive(ctx, "sp-live", "n", 1) })
	seqLive := spawnSeq(t, st, "sp-live")
	genLive := liveGen(t, st, "sp-live")
	newSeqLive, err := st.Spawns().Acquire(ctx, "sp-live", "d", "lease-live", 100, 500, seqLive)
	if err != nil {
		t.Fatalf("Acquire sp-live: %v", err)
	}
	if _, err := st.Spawns().TransitionClaimed(ctx, "sp-live", "lease-live", newSeqLive, genLive, Suspending); err != nil {
		t.Fatalf("TransitionClaimed sp-live: %v", err)
	}
	// claim deadline=500, nowTS=200 → still active

	// ListStranded at nowTS=200
	stranded, err := st.Spawns().ListStranded(ctx, 200)
	if err != nil {
		t.Fatalf("ListStranded: %v", err)
	}
	ids := make([]string, len(stranded))
	for i, s := range stranded {
		ids[i] = s.ID
	}
	// expect sp-exp and sp-strand (in id ASC order); sp-live must be absent
	found := map[string]bool{}
	for _, id := range ids {
		found[id] = true
	}
	if !found["sp-strand"] {
		t.Fatalf("ListStranded must include sp-strand, got %v", ids)
	}
	if !found["sp-exp"] {
		t.Fatalf("ListStranded must include sp-exp, got %v", ids)
	}
	if found["sp-live"] {
		t.Fatalf("ListStranded must NOT include sp-live (active claim), got %v", ids)
	}

	// Revert sp-strand: re-Acquire (no active claim → succeeds) then TransitionClaimed Suspending→Active
	seqStrand := spawnSeq(t, st, "sp-strand")
	genStrand := liveGen(t, st, "sp-strand")
	recoverySeq, err := st.Spawns().Acquire(ctx, "sp-strand", "recovery", "lease-rec", 200, 300, seqStrand)
	if err != nil {
		t.Fatalf("recovery Acquire sp-strand: %v", err)
	}
	if _, err := st.Spawns().TransitionClaimed(ctx, "sp-strand", "lease-rec", recoverySeq, genStrand, Active); err != nil {
		t.Fatalf("recovery TransitionClaimed sp-strand: %v", err)
	}
	if s := spawnRow(t, st, "sp-strand"); s.Status != Active {
		t.Fatalf("sp-strand status after revert=%v want active", s.Status)
	}
}
