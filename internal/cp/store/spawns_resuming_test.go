package store

import (
	"context"
	"testing"
)

// setResuming is a test helper that force-sets a spawn's status to Resuming without going through
// the claim/CAS path. Valid only for tests that need to reach Resuming as a precondition.
func setResuming(t *testing.T, st Store, id string) {
	t.Helper()
	db := st.(*bunStore).db
	res, err := db.NewUpdate().Model((*Spawn)(nil)).
		Set("status = ?", Resuming).
		Set("status_seq = status_seq + 1").
		Where("id = ?", id).
		Where("status IN (?)", Starting, Suspended). // accept Starting (from ClaimStarting) or Suspended
		Exec(context.Background())
	if err != nil {
		t.Fatalf("setResuming %s: %v", id, err)
	}
	if n, _ := res.RowsAffected(); n != 1 {
		t.Fatalf("setResuming %s: 0 rows affected (status not Starting or Suspended?)", id)
	}
}

// TestSetActiveFromResuming verifies SetActive accepts Resuming (sp-u53.7.5 from-set extension).
func TestSetActiveFromResuming(t *testing.T) {
	st := NewTestStore(t)
	seedAppAndOwner(t, st)
	ctx := context.Background()
	inTx(t, st, func(tx Store) error { return tx.Spawns().Create(ctx, newSpawn("sp1"), nil) })
	inTx(t, st, func(tx Store) error { return tx.Spawns().SetActive(ctx, "sp1", "n1", 1) })
	inTx(t, st, func(tx Store) error { return tx.Spawns().SetSuspending(ctx, "sp1", 1) })
	inTx(t, st, func(tx Store) error { return tx.Spawns().SetSuspended(ctx, "sp1", 1) })

	// Resume: ClaimStarting bumps to gen 2, Starting.
	var gen int64
	inTx(t, st, func(tx Store) error {
		g, err := tx.Spawns().ClaimStarting(ctx, "sp1", []Status{Suspended})
		gen = g
		return err
	})
	if gen != 2 {
		t.Fatalf("gen=%d want 2", gen)
	}

	// Simulate suspendLocked's TransitionClaimed Starting→Resuming.
	setResuming(t, st, "sp1")
	if sp, _ := st.Spawns().Get(ctx, "sp1"); sp.Status != Resuming {
		t.Fatalf("pre-condition: status=%v want Resuming", sp.Status)
	}

	// SetActive must succeed from Resuming.
	if err := st.Spawns().SetActive(ctx, "sp1", "n2", gen); err != nil {
		t.Fatalf("SetActive from Resuming: %v", err)
	}
	sp, _ := st.Spawns().Get(ctx, "sp1")
	if sp.Status != Active {
		t.Fatalf("status=%v want Active after SetActive from Resuming", sp.Status)
	}
	c, ok, _ := st.Spawns().LiveContainer(ctx, "sp1")
	if !ok || c.NodeID != "n2" || c.Generation != gen {
		t.Fatalf("live container=%+v ok=%v want n2 gen %d", c, ok, gen)
	}
}

// TestSetActiveFromStartingUnaffected verifies the existing Starting path still works after
// the from-set extension.
func TestSetActiveFromStartingUnaffected(t *testing.T) {
	st := NewTestStore(t)
	seedAppAndOwner(t, st)
	ctx := context.Background()
	inTx(t, st, func(tx Store) error { return tx.Spawns().Create(ctx, newSpawn("sp1"), nil) })
	if err := st.Spawns().SetActive(ctx, "sp1", "n1", 1); err != nil {
		t.Fatalf("SetActive from Starting: %v", err)
	}
	if sp, _ := st.Spawns().Get(ctx, "sp1"); sp.Status != Active {
		t.Fatalf("status=%v want Active", sp.Status)
	}
}

// TestRevertSuspendedFromResuming verifies RevertSuspended accepts Resuming (sp-u53.7.5).
func TestRevertSuspendedFromResuming(t *testing.T) {
	st := NewTestStore(t)
	seedAppAndOwner(t, st)
	ctx := context.Background()
	inTx(t, st, func(tx Store) error { return tx.Spawns().Create(ctx, newSpawn("sp1"), nil) })
	inTx(t, st, func(tx Store) error { return tx.Spawns().SetActive(ctx, "sp1", "n1", 1) })
	inTx(t, st, func(tx Store) error { return tx.Spawns().SetSuspending(ctx, "sp1", 1) })
	inTx(t, st, func(tx Store) error { return tx.Spawns().SetSuspended(ctx, "sp1", 1) })

	var gen int64
	inTx(t, st, func(tx Store) error {
		g, err := tx.Spawns().ClaimStarting(ctx, "sp1", []Status{Suspended})
		gen = g
		return err
	})
	setResuming(t, st, "sp1")

	// RevertSuspended from Resuming (migration failure after Starting→Resuming transition).
	if err := st.Spawns().RevertSuspended(ctx, "sp1", gen); err != nil {
		t.Fatalf("RevertSuspended from Resuming: %v", err)
	}
	sp, _ := st.Spawns().Get(ctx, "sp1")
	if sp.Status != Suspended || sp.SuspendedAt == nil {
		t.Fatalf("status=%v suspendedAt=%v want Suspended", sp.Status, sp.SuspendedAt)
	}
	if _, ok, _ := st.Spawns().LiveContainer(ctx, "sp1"); ok {
		t.Fatal("reverted spawn must have no live container")
	}
}

// TestRevertSuspendedFromStartingUnaffected verifies the Starting path still works.
func TestRevertSuspendedFromStartingUnaffected(t *testing.T) {
	st := NewTestStore(t)
	seedAppAndOwner(t, st)
	ctx := context.Background()
	inTx(t, st, func(tx Store) error { return tx.Spawns().Create(ctx, newSpawn("sp1"), nil) })
	inTx(t, st, func(tx Store) error { return tx.Spawns().SetActive(ctx, "sp1", "n1", 1) })
	inTx(t, st, func(tx Store) error { return tx.Spawns().SetSuspending(ctx, "sp1", 1) })
	inTx(t, st, func(tx Store) error { return tx.Spawns().SetSuspended(ctx, "sp1", 1) })
	var gen int64
	inTx(t, st, func(tx Store) error {
		g, err := tx.Spawns().ClaimStarting(ctx, "sp1", []Status{Suspended})
		gen = g
		return err
	})
	inTx(t, st, func(tx Store) error { return tx.Spawns().RevertSuspended(ctx, "sp1", gen) })
	if sp, _ := st.Spawns().Get(ctx, "sp1"); sp.Status != Suspended {
		t.Fatalf("status=%v want Suspended", sp.Status)
	}
}

// TestSetErrorFromResuming verifies SetError accepts Resuming (sp-u53.7.5 plain-resume failure path).
func TestSetErrorFromResuming(t *testing.T) {
	st := NewTestStore(t)
	seedAppAndOwner(t, st)
	ctx := context.Background()
	inTx(t, st, func(tx Store) error { return tx.Spawns().Create(ctx, newSpawn("sp1"), nil) })
	inTx(t, st, func(tx Store) error { return tx.Spawns().SetActive(ctx, "sp1", "n1", 1) })
	inTx(t, st, func(tx Store) error { return tx.Spawns().SetSuspending(ctx, "sp1", 1) })
	inTx(t, st, func(tx Store) error { return tx.Spawns().SetSuspended(ctx, "sp1", 1) })
	inTx(t, st, func(tx Store) error {
		_, err := tx.Spawns().ClaimStarting(ctx, "sp1", []Status{Suspended})
		return err
	})
	setResuming(t, st, "sp1")

	if err := st.Spawns().SetError(ctx, "sp1"); err != nil {
		t.Fatalf("SetError from Resuming: %v", err)
	}
	if sp, _ := st.Spawns().Get(ctx, "sp1"); sp.Status != Errored {
		t.Fatalf("status=%v want Errored", sp.Status)
	}
	if _, ok, _ := st.Spawns().LiveContainer(ctx, "sp1"); ok {
		t.Fatal("SetError from Resuming must end the live container")
	}
}

// TestListStrandedIncludesResuming verifies that ListStranded includes spawns in Resuming status
// (sp-u53.7.5: Resuming added to transientStatuses).
func TestListStrandedIncludesResuming(t *testing.T) {
	st := NewTestStore(t)
	seedAppAndOwner(t, st)
	ctx := context.Background()

	// sp-suspending: stranded Suspending (existing).
	inTx(t, st, func(tx Store) error { return tx.Spawns().Create(ctx, newSpawn("sp-suspending"), nil) })
	inTx(t, st, func(tx Store) error { return tx.Spawns().SetActive(ctx, "sp-suspending", "n", 1) })
	inTx(t, st, func(tx Store) error { return tx.Spawns().SetSuspending(ctx, "sp-suspending", 1) })
	// no claim → stranded

	// sp-resuming: stranded Resuming (new).
	inTx(t, st, func(tx Store) error { return tx.Spawns().Create(ctx, newSpawn("sp-resuming"), nil) })
	// Starting state → set to Resuming directly (driver would have called ClaimStarting + TransitionClaimed)
	setResuming(t, st, "sp-resuming")
	// no claim → stranded

	// sp-active: Active with no claim — NOT transient, must not appear.
	inTx(t, st, func(tx Store) error { return tx.Spawns().Create(ctx, newSpawn("sp-active"), nil) })
	inTx(t, st, func(tx Store) error { return tx.Spawns().SetActive(ctx, "sp-active", "n", 1) })

	stranded, err := st.Spawns().ListStranded(ctx, 9999)
	if err != nil {
		t.Fatalf("ListStranded: %v", err)
	}
	found := map[string]bool{}
	for _, s := range stranded {
		found[s.ID] = true
	}
	if !found["sp-suspending"] {
		t.Errorf("ListStranded must include sp-suspending, got ids=%v", strandedIDs(stranded))
	}
	if !found["sp-resuming"] {
		t.Errorf("ListStranded must include sp-resuming, got ids=%v", strandedIDs(stranded))
	}
	if found["sp-active"] {
		t.Errorf("ListStranded must NOT include sp-active, got ids=%v", strandedIDs(stranded))
	}
}

func strandedIDs(spawns []Spawn) []string {
	ids := make([]string, len(spawns))
	for i, s := range spawns {
		ids[i] = s.ID
	}
	return ids
}

// TestResumingStatusConstant verifies the Resuming constant has the expected wire value.
func TestResumingStatusConstant(t *testing.T) {
	if string(Resuming) != "resuming" {
		t.Fatalf("Resuming wire value=%q want \"resuming\"", Resuming)
	}
	// Must be distinct from all other statuses.
	others := []Status{Starting, Active, Suspending, Suspended, Unreachable, Errored, Deleted}
	for _, other := range others {
		if Resuming == other {
			t.Fatalf("Resuming must be distinct from %q", other)
		}
	}
}
