package store

import (
	"context"
	"errors"
	"testing"
)

// RevertSuspended rolls a starting episode back to suspended (the migration defined-failure path),
// gen-fenced, ending the failed target container — so the spawn is exactly as it was before the
// migrate attempt and no live container remains.
func TestRevertSuspendedRollsBackStarting(t *testing.T) {
	st := NewTestStore(t)
	seedAppAndOwner(t, st)
	ctx := context.Background()
	inTx(t, st, func(tx Store) error { return tx.Spawns().Create(ctx, newSpawn("sp1"), nil) })

	// active -> suspended (the source suspend) -> starting (the migrate's resume claim, gen 2).
	inTx(t, st, func(tx Store) error { return tx.Spawns().SetActive(ctx, "sp1", "n1", 1) })
	inTx(t, st, func(tx Store) error { return tx.Spawns().SetSuspending(ctx, "sp1", 1) })
	inTx(t, st, func(tx Store) error { return tx.Spawns().SetSuspended(ctx, "sp1", 1) })
	var gen int64
	inTx(t, st, func(tx Store) error {
		g, err := tx.Spawns().ClaimStarting(ctx, "sp1", []Status{Suspended})
		gen = g
		return err
	})
	if gen != 2 {
		t.Fatalf("claimed gen=%d want 2", gen)
	}

	// A wrong-gen revert is fenced.
	if err := st.WithTx(ctx, func(tx Store) error { return tx.Spawns().RevertSuspended(ctx, "sp1", 99) }); !errors.Is(err, ErrConflict) {
		t.Fatalf("RevertSuspended wrong gen: want ErrConflict, got %v", err)
	}

	// The real revert: starting (gen 2) -> suspended, container ended.
	inTx(t, st, func(tx Store) error { return tx.Spawns().RevertSuspended(ctx, "sp1", 2) })
	sp, _ := st.Spawns().Get(ctx, "sp1")
	if sp.Status != Suspended || sp.SuspendedAt == nil {
		t.Fatalf("status=%v suspendedAt=%v, want suspended", sp.Status, sp.SuspendedAt)
	}
	if _, ok, _ := st.Spawns().LiveContainer(ctx, "sp1"); ok {
		t.Fatal("reverted spawn must have no live container")
	}

	// RevertSuspended only applies from starting — an active spawn is rejected.
	inTx(t, st, func(tx Store) error {
		_, err := tx.Spawns().ClaimStarting(ctx, "sp1", []Status{Suspended})
		return err
	})
	inTx(t, st, func(tx Store) error { return tx.Spawns().SetActive(ctx, "sp1", "n1", 3) })
	if err := st.WithTx(ctx, func(tx Store) error { return tx.Spawns().RevertSuspended(ctx, "sp1", 3) }); !errors.Is(err, ErrConflict) {
		t.Fatalf("RevertSuspended from active: want ErrConflict, got %v", err)
	}
}
