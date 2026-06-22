package store

import (
	"context"
	"errors"
	"strings"
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

	inTx(t, st, func(tx Store) error { return tx.Spawns().SetActive(ctx, "sp1", "n", 1) })
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

func TestLatestContainerReturnsHighestGenerationWhenSuspended(t *testing.T) {
	st := NewTestStore(t)
	seedAppAndOwner(t, st)
	ctx := context.Background()
	inTx(t, st, func(tx Store) error { return tx.Spawns().Create(ctx, newSpawn("sp1"), nil) })
	inTx(t, st, func(tx Store) error { return tx.Spawns().SetActive(ctx, "sp1", "source", 1) })
	inTx(t, st, func(tx Store) error { return tx.Spawns().SetSuspending(ctx, "sp1", 1) })
	inTx(t, st, func(tx Store) error { return tx.Spawns().SetSuspended(ctx, "sp1", 1) })
	var newGen int64
	inTx(t, st, func(tx Store) error {
		var err error
		newGen, err = tx.Spawns().ClaimStarting(ctx, "sp1", []Status{Suspended})
		return err
	})
	if err := st.Spawns().SetActive(ctx, "sp1", "target", newGen); err != nil {
		t.Fatal(err)
	}
	inTx(t, st, func(tx Store) error { return tx.Spawns().SetSuspending(ctx, "sp1", newGen) })
	inTx(t, st, func(tx Store) error { return tx.Spawns().SetSuspended(ctx, "sp1", newGen) })

	got, ok, err := st.Spawns().LatestContainer(ctx, "sp1")
	if err != nil || !ok {
		t.Fatalf("LatestContainer ok=%v err=%v", ok, err)
	}
	if got.Generation != 2 || got.NodeID != "target" {
		t.Fatalf("LatestContainer = %+v, want target gen 2", got)
	}
}

func TestGuardedTransitionsReject(t *testing.T) {
	st := NewTestStore(t)
	seedAppAndOwner(t, st)
	ctx := context.Background()
	inTx(t, st, func(tx Store) error { return tx.Spawns().Create(ctx, newSpawn("sp1"), nil) })
	inTx(t, st, func(tx Store) error { return tx.Spawns().SetActive(ctx, "sp1", "n", 1) })

	err := st.WithTx(ctx, func(tx Store) error { return tx.Spawns().SetActive(ctx, "sp1", "n", 1) })
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
	inTx(t, st, func(tx Store) error { return tx.Spawns().SetError(ctx, "sp1", "", "") })
	if s, _ := st.Spawns().Get(ctx, "sp1"); s.Status != Errored {
		t.Fatalf("status=%v want error", s.Status)
	}
	if _, ok, _ := st.Spawns().LiveContainer(ctx, "sp1"); ok {
		t.Fatal("SetError must end the live container")
	}
}

// TestSetErrorPersistsAndTruncatesDetail verifies SetError stores the step+detail and truncates
// oversized details to ≤8 KiB (sp-m859.3).
func TestSetErrorPersistsAndTruncatesDetail(t *testing.T) {
	st := NewTestStore(t)
	seedAppAndOwner(t, st)
	ctx := context.Background()

	// Build a detail string longer than maxErrorDetailBytes (8192).
	long := make([]byte, 9000)
	for i := range long {
		long[i] = 'x'
	}
	longDetail := string(long)

	inTx(t, st, func(tx Store) error { return tx.Spawns().Create(ctx, newSpawn("sp1"), nil) })
	inTx(t, st, func(tx Store) error { return tx.Spawns().SetError(ctx, "sp1", "create-pod", longDetail) })

	sp, err := st.Spawns().Get(ctx, "sp1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if sp.Status != Errored {
		t.Fatalf("status=%v want Errored", sp.Status)
	}
	if sp.ErrorStep != "create-pod" {
		t.Fatalf("error_step=%q want create-pod", sp.ErrorStep)
	}
	if len(sp.ErrorDetail) > maxErrorDetailBytes {
		t.Fatalf("error_detail len=%d want ≤%d", len(sp.ErrorDetail), maxErrorDetailBytes)
	}
	if !strings.HasPrefix(longDetail, sp.ErrorDetail) {
		t.Fatalf("error_detail must be a prefix of the original")
	}

	// ("","") leaves both fields empty but spawn is still Errored.
	inTx(t, st, func(tx Store) error { return tx.Spawns().Create(ctx, newSpawn("sp2"), nil) })
	inTx(t, st, func(tx Store) error { return tx.Spawns().SetError(ctx, "sp2", "", "") })
	sp2, _ := st.Spawns().Get(ctx, "sp2")
	if sp2.Status != Errored {
		t.Fatalf("sp2 status=%v want Errored", sp2.Status)
	}
	if sp2.ErrorStep != "" || sp2.ErrorDetail != "" {
		t.Fatalf("sp2 error fields non-empty: step=%q detail=%q", sp2.ErrorStep, sp2.ErrorDetail)
	}
}

func TestSetSuspendedRejectsStaleGen(t *testing.T) {
	st := NewTestStore(t)
	seedAppAndOwner(t, st)
	ctx := context.Background()
	inTx(t, st, func(tx Store) error { return tx.Spawns().Create(ctx, newSpawn("sp1"), nil) })
	inTx(t, st, func(tx Store) error { return tx.Spawns().SetActive(ctx, "sp1", "n", 1) })
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
	inTx(t, st, func(tx Store) error { return tx.Spawns().SetActive(ctx, "sp4", "n", 1) })
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

// A spawn can be deleted while still 'starting' (killed before it ever went active): MarkDeleted must
// soft-delete it and end its (gen-1) live container, same as deleting an active spawn.
func TestMarkDeletedFromStarting(t *testing.T) {
	st := NewTestStore(t)
	seedAppAndOwner(t, st)
	ctx := context.Background()
	inTx(t, st, func(tx Store) error { return tx.Spawns().Create(ctx, newSpawn("sp5"), nil) }) // starting, never active
	if err := st.WithTx(ctx, func(tx Store) error { return tx.Spawns().MarkDeleted(ctx, "sp5", 99) }); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Spawns().Get(ctx, "sp5"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("deleted-from-starting spawn must be ErrNotFound on Get, got %v", err)
	}
	if _, ok, _ := st.Spawns().LiveContainer(ctx, "sp5"); ok {
		t.Fatal("MarkDeleted from starting must end the live container")
	}
}

func TestMarkUnreachableKeepsLiveContainerAndFilters(t *testing.T) {
	st := NewTestStore(t)
	seedAppAndOwner(t, st)
	ctx := context.Background()
	inTx(t, st, func(tx Store) error { return tx.Spawns().Create(ctx, newSpawn("sp1"), nil) })
	inTx(t, st, func(tx Store) error { return tx.Spawns().SetActive(ctx, "sp1", "n", 1) })
	inTx(t, st, func(tx Store) error { return tx.Spawns().Adopt(ctx, "sp1", "nodeA", 1) })

	n, err := st.Spawns().MarkUnreachable(ctx, []string{"sp1"})
	if err != nil || n != 1 {
		t.Fatalf("MarkUnreachable n=%d err=%v want 1", n, err)
	}
	if s, _ := st.Spawns().Get(ctx, "sp1"); s.Status != Unreachable {
		t.Fatalf("status=%v want unreachable", s.Status)
	}
	// live container is KEPT (the adopt arm needs it) — still live, still on nodeA
	c, ok, _ := st.Spawns().LiveContainer(ctx, "sp1")
	if !ok || c.Generation != 1 {
		t.Fatalf("unreachable must keep live container, ok=%v c=%+v", ok, c)
	}
	if l, _ := st.Spawns().LiveContainersByNode(ctx, "nodeA"); len(l) != 1 {
		t.Fatalf("unreachable container must still appear in node inventory, got %+v", l)
	}
	// idempotent: re-marking an already-unreachable spawn flips nothing (status filter excludes it)
	if n2, _ := st.Spawns().MarkUnreachable(ctx, []string{"sp1"}); n2 != 0 {
		t.Fatalf("re-mark unreachable n=%d want 0", n2)
	}
	// a suspended spawn is NOT eligible for unreachable (filter is status IN starting,active)
	inTx(t, st, func(tx Store) error { return tx.Spawns().Create(ctx, newSpawn("sp2"), nil) })
	inTx(t, st, func(tx Store) error { return tx.Spawns().SetActive(ctx, "sp2", "n", 1) })
	inTx(t, st, func(tx Store) error { return tx.Spawns().SetSuspending(ctx, "sp2", 1) })
	inTx(t, st, func(tx Store) error { return tx.Spawns().SetSuspended(ctx, "sp2", 1) })
	if n3, _ := st.Spawns().MarkUnreachable(ctx, []string{"sp2"}); n3 != 0 {
		t.Fatalf("suspended spawn must not be marked unreachable, n=%d", n3)
	}
	if s, _ := st.Spawns().Get(ctx, "sp2"); s.Status != Suspended {
		t.Fatalf("sp2 status=%v want suspended", s.Status)
	}
}

func TestRecreateFromUnreachableFencesOldGen(t *testing.T) {
	st := NewTestStore(t)
	seedAppAndOwner(t, st)
	ctx := context.Background()
	inTx(t, st, func(tx Store) error { return tx.Spawns().Create(ctx, newSpawn("sp1"), nil) })
	inTx(t, st, func(tx Store) error { return tx.Spawns().SetActive(ctx, "sp1", "n", 1) })
	if _, err := st.Spawns().MarkUnreachable(ctx, []string{"sp1"}); err != nil {
		t.Fatal(err)
	}
	// Recreate: ClaimStarting from unreachable -> new gen 2; old gen-1 container ended (fenced)
	var g int64
	inTx(t, st, func(tx Store) error {
		ng, err := tx.Spawns().ClaimStarting(ctx, "sp1", []Status{Unreachable})
		g = ng
		return err
	})
	if g != 2 {
		t.Fatalf("recreate newGen=%d want 2", g)
	}
	if lc := liveGen(t, st, "sp1"); lc != 2 {
		t.Fatalf("live gen=%d want 2 (old gen-1 must be fenced/ended)", lc)
	}
	if s, _ := st.Spawns().Get(ctx, "sp1"); s.Status != Starting {
		t.Fatalf("status=%v want starting", s.Status)
	}
}

func TestSetActiveRecordsNodeID(t *testing.T) {
	st := NewTestStore(t)
	seedAppAndOwner(t, st)
	ctx := context.Background()
	inTx(t, st, func(tx Store) error { return tx.Spawns().Create(ctx, newSpawn("sp1"), nil) })
	inTx(t, st, func(tx Store) error { return tx.Spawns().SetActive(ctx, "sp1", "nodeA", 1) })

	c, ok, err := st.Spawns().LiveContainer(ctx, "sp1")
	if err != nil || !ok || c.NodeID != "nodeA" {
		t.Fatalf("SetActive must record node_id: c=%+v ok=%v err=%v", c, ok, err)
	}
	live, _ := st.Spawns().LiveContainersByNode(ctx, "nodeA")
	if len(live) != 1 || live[0].SpawnID != "sp1" {
		t.Fatalf("LiveContainersByNode(nodeA)=%+v want [sp1]", live)
	}
}

func TestTouchMarkRecoveredAndMountMarker(t *testing.T) {
	st := NewTestStore(t)
	seedAppAndOwner(t, st)
	ctx := context.Background()
	inTx(t, st, func(tx Store) error {
		return tx.Spawns().Create(ctx, newSpawn("sp1"), []Mount{{Name: "main", BackendURI: "managed:r"}})
	})
	if err := st.Spawns().Touch(ctx, "sp1", 777); err != nil {
		t.Fatal(err)
	}
	if s, _ := st.Spawns().Get(ctx, "sp1"); s.LastUsedAt != 777 {
		t.Fatalf("Touch: last_used_at=%d want 777", s.LastUsedAt)
	}
	if err := st.Spawns().MarkRecovered(ctx, "sp1"); err != nil {
		t.Fatal(err)
	}
	if s, _ := st.Spawns().Get(ctx, "sp1"); !s.Recovered {
		t.Fatal("MarkRecovered should set recovered=true")
	}
	if err := st.Spawns().SetMountMarker(ctx, "sp1", "main", "spawnery-suspend/sp1/1"); err != nil {
		t.Fatal(err)
	}
	ms, _ := st.Spawns().GetMounts(ctx, "sp1")
	if len(ms) != 1 || ms[0].PersistMarker != "spawnery-suspend/sp1/1" {
		t.Fatalf("SetMountMarker: mounts=%+v", ms)
	}
}
