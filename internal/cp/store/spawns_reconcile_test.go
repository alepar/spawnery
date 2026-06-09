package store

import (
	"context"
	"errors"
	"testing"
)

func TestReconcileQueries(t *testing.T) {
	st := NewTestStore(t)
	seedAppAndOwner(t, st)
	ctx := context.Background()
	inTx(t, st, func(tx Store) error { return tx.Spawns().Create(ctx, newSpawn("sp1"), nil) })
	inTx(t, st, func(tx Store) error { return tx.Spawns().SetActive(ctx, "sp1", "n", 1) })
	inTx(t, st, func(tx Store) error { return tx.Spawns().Adopt(ctx, "sp1", "nodeA", 1) })

	live, err := st.Spawns().LiveContainersByNode(ctx, "nodeA")
	if err != nil || len(live) != 1 || live[0].SpawnID != "sp1" || live[0].Generation != 1 {
		t.Fatalf("live=%+v err=%v", live, err)
	}
	if l, _ := st.Spawns().LiveContainersByNode(ctx, "nodeB"); len(l) != 0 {
		t.Fatalf("nodeB should have no live containers, got %+v", l)
	}
	inTx(t, st, func(tx Store) error { return tx.Spawns().EndContainer(ctx, "sp1", 1, PhaseLost) })
	if l, _ := st.Spawns().LiveContainersByNode(ctx, "nodeA"); len(l) != 0 {
		t.Fatalf("ended container must not be live, got %+v", l)
	}
}

func TestMarkReachableFlipsUnreachable(t *testing.T) {
	st := NewTestStore(t)
	seedAppAndOwner(t, st)
	ctx := context.Background()
	inTx(t, st, func(tx Store) error { return tx.Spawns().Create(ctx, newSpawn("sp1"), nil) })
	inTx(t, st, func(tx Store) error { return tx.Spawns().SetActive(ctx, "sp1", "n", 1) })
	if _, err := st.Spawns().MarkUnreachable(ctx, []string{"sp1"}); err != nil {
		t.Fatal(err)
	}

	inTx(t, st, func(tx Store) error { return tx.Spawns().MarkReachable(ctx, "sp1", 1) })
	if s, _ := st.Spawns().Get(ctx, "sp1"); s.Status != Active {
		t.Fatalf("status=%v want active", s.Status)
	}
	// the live container is untouched — same gen, still live
	c, ok, _ := st.Spawns().LiveContainer(ctx, "sp1")
	if !ok || c.Generation != 1 {
		t.Fatalf("MarkReachable must keep the live container, ok=%v c=%+v", ok, c)
	}
}

func TestMarkReachableRejectsNonUnreachable(t *testing.T) {
	st := NewTestStore(t)
	seedAppAndOwner(t, st)
	ctx := context.Background()
	// active spawn: flip is rejected, status untouched
	inTx(t, st, func(tx Store) error { return tx.Spawns().Create(ctx, newSpawn("sp1"), nil) })
	inTx(t, st, func(tx Store) error { return tx.Spawns().SetActive(ctx, "sp1", "n", 1) })
	err := st.WithTx(ctx, func(tx Store) error { return tx.Spawns().MarkReachable(ctx, "sp1", 1) })
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("MarkReachable on active: want ErrConflict, got %v", err)
	}
	if s, _ := st.Spawns().Get(ctx, "sp1"); s.Status != Active {
		t.Fatalf("sp1 status=%v want active (untouched)", s.Status)
	}
	// starting spawn: same rejection (no live-gen confusion — gen 1 IS live, status guard rejects)
	inTx(t, st, func(tx Store) error { return tx.Spawns().Create(ctx, newSpawn("sp2"), nil) })
	err = st.WithTx(ctx, func(tx Store) error { return tx.Spawns().MarkReachable(ctx, "sp2", 1) })
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("MarkReachable on starting: want ErrConflict, got %v", err)
	}
	if s, _ := st.Spawns().Get(ctx, "sp2"); s.Status != Starting {
		t.Fatalf("sp2 status=%v want starting (untouched)", s.Status)
	}
}

func TestMarkReachableRejectsStaleGen(t *testing.T) {
	st := NewTestStore(t)
	seedAppAndOwner(t, st)
	ctx := context.Background()
	inTx(t, st, func(tx Store) error { return tx.Spawns().Create(ctx, newSpawn("sp1"), nil) })
	inTx(t, st, func(tx Store) error { return tx.Spawns().SetActive(ctx, "sp1", "n", 1) })
	if _, err := st.Spawns().MarkUnreachable(ctx, []string{"sp1"}); err != nil {
		t.Fatal(err)
	}
	// a stale gen (2) does not match the live container (gen 1) -> ErrConflict, no flip
	err := st.WithTx(ctx, func(tx Store) error { return tx.Spawns().MarkReachable(ctx, "sp1", 2) })
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("MarkReachable stale gen: want ErrConflict, got %v", err)
	}
	if s, _ := st.Spawns().Get(ctx, "sp1"); s.Status != Unreachable {
		t.Fatalf("status=%v want unreachable (untouched)", s.Status)
	}
}

func TestAdoptRejectsStaleGen(t *testing.T) {
	st := NewTestStore(t)
	seedAppAndOwner(t, st)
	ctx := context.Background()
	inTx(t, st, func(tx Store) error { return tx.Spawns().Create(ctx, newSpawn("sp1"), nil) })
	inTx(t, st, func(tx Store) error { return tx.Spawns().SetActive(ctx, "sp1", "nodeA", 1) })

	err := st.WithTx(ctx, func(tx Store) error { return tx.Spawns().Adopt(ctx, "sp1", "nodeB", 2) })
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("Adopt stale gen: want ErrConflict, got %v", err)
	}
	// the live container keeps its original node binding
	c, ok, _ := st.Spawns().LiveContainer(ctx, "sp1")
	if !ok || c.NodeID != "nodeA" {
		t.Fatalf("stale Adopt must not rebind node_id, ok=%v c=%+v", ok, c)
	}
}
