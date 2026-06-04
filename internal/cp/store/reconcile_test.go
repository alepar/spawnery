package store

import (
	"context"
	"testing"
)

func TestMarkBootUnreachable(t *testing.T) {
	st := NewTestStore(t)
	seedAppAndOwner(t, st)
	ctx := context.Background()
	inTx(t, st, func(tx Store) error { return tx.Spawns().Create(ctx, newSpawn("a"), nil) }) // starting
	inTx(t, st, func(tx Store) error { return tx.Spawns().Create(ctx, newSpawn("b"), nil) })
	inTx(t, st, func(tx Store) error { return tx.Spawns().SetActive(ctx, "b", "n", 1) }) // active
	inTx(t, st, func(tx Store) error { return tx.Spawns().Create(ctx, newSpawn("c"), nil) })
	inTx(t, st, func(tx Store) error { return tx.Spawns().SetActive(ctx, "c", "n", 1) })
	inTx(t, st, func(tx Store) error { return tx.Spawns().SetSuspending(ctx, "c", 1) })
	inTx(t, st, func(tx Store) error { return tx.Spawns().SetSuspended(ctx, "c", 1) }) // suspended
	inTx(t, st, func(tx Store) error { return tx.Spawns().Create(ctx, newSpawn("d"), nil) })
	inTx(t, st, func(tx Store) error { return tx.Spawns().SetActive(ctx, "d", "n", 1) })
	inTx(t, st, func(tx Store) error { return tx.Spawns().SetSuspending(ctx, "d", 1) }) // suspending (CP crashed mid-suspend)

	n, err := st.Spawns().MarkBootUnreachable(ctx)
	if err != nil || n != 3 {
		t.Fatalf("MarkBootUnreachable n=%d err=%v want 3 (a,b,d)", n, err)
	}
	if s, _ := st.Spawns().Get(ctx, "a"); s.Status != Unreachable {
		t.Fatalf("a status=%v want unreachable", s.Status)
	}
	if s, _ := st.Spawns().Get(ctx, "b"); s.Status != Unreachable {
		t.Fatalf("b status=%v want unreachable", s.Status)
	}
	if s, _ := st.Spawns().Get(ctx, "c"); s.Status != Suspended {
		t.Fatalf("c status=%v want suspended (untouched)", s.Status)
	}
	if s, _ := st.Spawns().Get(ctx, "d"); s.Status != Unreachable {
		t.Fatalf("d status=%v want unreachable (suspending must be swept)", s.Status)
	}
}
