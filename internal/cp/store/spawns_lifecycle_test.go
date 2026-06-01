package store

import (
	"context"
	"errors"
	"testing"
)

func liveGen(t *testing.T, st Store, id string) int64 {
	t.Helper()
	c, ok, err := st.Spawns().LiveContainer(context.Background(), id)
	if err != nil || !ok {
		t.Fatalf("no live container for %s: ok=%v err=%v", id, ok, err)
	}
	return c.Generation
}

func TestHappyLifecycle(t *testing.T) {
	st := NewTestStore(t)
	seedAppAndOwner(t, st)
	ctx := context.Background()
	inTx(t, st, func(tx Store) error { return tx.Spawns().Create(ctx, newSpawn("sp1"), nil) })

	inTx(t, st, func(tx Store) error { return tx.Spawns().SetActive(ctx, "sp1", 1) })
	if s, _ := st.Spawns().Get(ctx, "sp1"); s.Status != Active {
		t.Fatalf("status=%v want active", s.Status)
	}
	inTx(t, st, func(tx Store) error { return tx.Spawns().SetSuspending(ctx, "sp1", 1) })
	inTx(t, st, func(tx Store) error { return tx.Spawns().SetSuspended(ctx, "sp1", 1) })
	if s, _ := st.Spawns().Get(ctx, "sp1"); s.Status != Suspended || s.SuspendedAt == nil {
		t.Fatalf("status=%v suspendedAt=%v", s.Status, s.SuspendedAt)
	}
	if _, ok, _ := st.Spawns().LiveContainer(ctx, "sp1"); ok {
		t.Fatal("suspended spawn must have no live container")
	}

	var newGen int64
	inTx(t, st, func(tx Store) error {
		g, err := tx.Spawns().ClaimStarting(ctx, "sp1", []Status{Suspended})
		newGen = g
		return err
	})
	if newGen != 2 {
		t.Fatalf("newGen=%d want 2", newGen)
	}
	if g := liveGen(t, st, "sp1"); g != 2 {
		t.Fatalf("live gen=%d want 2", g)
	}
}

func TestGuardedTransitionsReject(t *testing.T) {
	st := NewTestStore(t)
	seedAppAndOwner(t, st)
	ctx := context.Background()
	inTx(t, st, func(tx Store) error { return tx.Spawns().Create(ctx, newSpawn("sp1"), nil) })
	inTx(t, st, func(tx Store) error { return tx.Spawns().SetActive(ctx, "sp1", 1) })

	err := st.WithTx(ctx, func(tx Store) error { return tx.Spawns().SetActive(ctx, "sp1", 1) })
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("SetActive on active: want ErrConflict, got %v", err)
	}
	err = st.WithTx(ctx, func(tx Store) error {
		_, e := tx.Spawns().ClaimStarting(ctx, "sp1", []Status{Suspended})
		return e
	})
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("ClaimStarting from active: want ErrConflict, got %v", err)
	}
	err = st.WithTx(ctx, func(tx Store) error { return tx.Spawns().SetSuspending(ctx, "sp1", 999) })
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("SetSuspending wrong gen: want ErrConflict, got %v", err)
	}
}

// The DB-enforced single-live invariant: a second live container insert for one spawn must fail.
// A loud backstop bug, NOT ErrConflict — correct code never trips it (ClaimStarting ends old first).
func TestSingleLiveContainerInvariant(t *testing.T) {
	st := NewTestStore(t)
	seedAppAndOwner(t, st)
	ctx := context.Background()
	inTx(t, st, func(tx Store) error { return tx.Spawns().Create(ctx, newSpawn("sp1"), nil) })

	bs := st.(*bunStore)
	dup := Container{SpawnID: "sp1", Generation: 2, NodeID: "n", Phase: PhaseStarting, StartedAt: 1}
	_, err := bs.db.NewInsert().Model(&dup).Exec(ctx)
	if err == nil {
		t.Fatal("second live container insert must fail the partial unique index")
	}
}

func TestErrorEndsLiveContainer(t *testing.T) {
	st := NewTestStore(t)
	seedAppAndOwner(t, st)
	ctx := context.Background()
	inTx(t, st, func(tx Store) error { return tx.Spawns().Create(ctx, newSpawn("sp1"), nil) })
	inTx(t, st, func(tx Store) error { return tx.Spawns().SetError(ctx, "sp1") })
	if s, _ := st.Spawns().Get(ctx, "sp1"); s.Status != Errored {
		t.Fatalf("status=%v want error", s.Status)
	}
	if _, ok, _ := st.Spawns().LiveContainer(ctx, "sp1"); ok {
		t.Fatal("SetError must end the live container")
	}
}

func TestSetSuspendedRejectsStaleGen(t *testing.T) {
	st := NewTestStore(t)
	seedAppAndOwner(t, st)
	ctx := context.Background()
	inTx(t, st, func(tx Store) error { return tx.Spawns().Create(ctx, newSpawn("sp1"), nil) })
	inTx(t, st, func(tx Store) error { return tx.Spawns().SetActive(ctx, "sp1", 1) })
	inTx(t, st, func(tx Store) error { return tx.Spawns().SetSuspending(ctx, "sp1", 1) })
	// a stale gen (2) does not match the live container (gen 1) -> ErrConflict, no suspend
	err := st.WithTx(ctx, func(tx Store) error { return tx.Spawns().SetSuspended(ctx, "sp1", 2) })
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("SetSuspended stale gen: want ErrConflict, got %v", err)
	}
	// the correct gen still works
	inTx(t, st, func(tx Store) error { return tx.Spawns().SetSuspended(ctx, "sp1", 1) })
	if s, _ := st.Spawns().Get(ctx, "sp1"); s.Status != Suspended {
		t.Fatalf("status=%v want suspended", s.Status)
	}
}

func TestSpawnGetFiltersDeleted(t *testing.T) {
	st := NewTestStore(t)
	seedAppAndOwner(t, st)
	ctx := context.Background()
	inTx(t, st, func(tx Store) error { return tx.Spawns().Create(ctx, newSpawn("sp4"), nil) })
	inTx(t, st, func(tx Store) error { return tx.Spawns().SetActive(ctx, "sp4", 1) })
	if err := st.WithTx(ctx, func(tx Store) error { return tx.Spawns().MarkDeleted(ctx, "sp4", 99) }); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Spawns().Get(ctx, "sp4"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("deleted spawn must be ErrNotFound on Get, got %v", err)
	}
	if _, ok, _ := st.Spawns().LiveContainer(ctx, "sp4"); ok {
		t.Fatal("MarkDeleted must end the live container")
	}
}
