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
	inTx(t, st, func(tx Store) error { return tx.Spawns().SetActive(ctx, "b", 1) }) // active
	inTx(t, st, func(tx Store) error { return tx.Spawns().Create(ctx, newSpawn("c"), nil) })
	inTx(t, st, func(tx Store) error { return tx.Spawns().SetActive(ctx, "c", 1) })
	inTx(t, st, func(tx Store) error { return tx.Spawns().SetSuspending(ctx, "c", 1) })
	inTx(t, st, func(tx Store) error { return tx.Spawns().SetSuspended(ctx, "c", 1) }) // suspended

	n, err := st.Spawns().MarkBootUnreachable(ctx)
	if err != nil || n != 2 {
		t.Fatalf("MarkBootUnreachable n=%d err=%v want 2 (a,b)", n, err)
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
}
