package store

import (
	"context"
	"testing"
)

func TestReconcileQueries(t *testing.T) {
	st := NewTestStore(t)
	seedAppAndOwner(t, st)
	ctx := context.Background()
	inTx(t, st, func(tx Store) error { return tx.Spawns().Create(ctx, newSpawn("sp1"), nil) })
	inTx(t, st, func(tx Store) error { return tx.Spawns().SetActive(ctx, "sp1", 1) })
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
