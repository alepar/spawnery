package store

import (
	"context"
	"testing"
)

// seed inserts an owner + app + reviewed version "1.0.0" with one declared mount "main".
func seedAppAndOwner(t *testing.T, st Store) {
	t.Helper()
	ctx := context.Background()
	if err := st.Owners().Upsert(ctx, Owner{ID: "alice", CreatedAt: 1}); err != nil {
		t.Fatal(err)
	}
	if err := st.Apps().Upsert(ctx, App{ID: "spawnery/secret", CreatedAt: 1}); err != nil {
		t.Fatal(err)
	}
	if err := st.Apps().UpsertVersion(ctx,
		AppVersion{AppID: "spawnery/secret", Version: "1.0.0", Ref: "ref1", Reviewed: true, CreatedAt: 1},
		[]MountDecl{{AppID: "spawnery/secret", Version: "1.0.0", Name: "main", Required: true}}); err != nil {
		t.Fatal(err)
	}
}

func newSpawn(id string) Spawn {
	return Spawn{
		ID: id, OwnerID: "alice", AppID: "spawnery/secret", AppVersion: "1.0.0", AppRef: "ref1",
		Model: "deepseek", Status: Starting, CreatedAt: 5, LastUsedAt: 5,
	}
}

func TestSpawnCreateAndReads(t *testing.T) {
	st := NewTestStore(t)
	seedAppAndOwner(t, st)
	ctx := context.Background()
	mounts := []Mount{{Name: "main", BackendURI: "scratch"}}

	inTx(t, st, func(tx Store) error { return tx.Spawns().Create(ctx, newSpawn("sp1"), mounts) })

	s, err := st.Spawns().Get(ctx, "sp1")
	if err != nil || s.Status != Starting || s.AppRef != "ref1" {
		t.Fatalf("s=%+v err=%v", s, err)
	}
	c, ok, err := st.Spawns().LiveContainer(ctx, "sp1")
	if err != nil || !ok || c.Generation != 1 || c.Phase != PhaseStarting {
		t.Fatalf("live container c=%+v ok=%v err=%v", c, ok, err)
	}
	ms, err := st.Spawns().GetMounts(ctx, "sp1")
	if err != nil || len(ms) != 1 || ms[0].BackendURI != "scratch" || ms[0].SpawnID != "sp1" {
		t.Fatalf("mounts=%+v err=%v", ms, err)
	}
	list, err := st.Spawns().ListByOwner(ctx, "alice")
	if err != nil || len(list) != 1 {
		t.Fatalf("list=%+v err=%v", list, err)
	}
}

func TestSpawnCreateRejectsUnknownVersionAndMount(t *testing.T) {
	st := NewTestStore(t)
	seedAppAndOwner(t, st)
	ctx := context.Background()
	bad := newSpawn("sp2")
	bad.AppVersion = "9.9.9"
	if err := st.WithTx(ctx, func(tx Store) error {
		return tx.Spawns().Create(ctx, bad, []Mount{{Name: "main", BackendURI: "scratch"}})
	}); err == nil {
		t.Fatal("expected error for unknown app_version")
	}
	if err := st.WithTx(ctx, func(tx Store) error {
		return tx.Spawns().Create(ctx, newSpawn("sp3"), []Mount{{Name: "bogus", BackendURI: "scratch"}})
	}); err == nil {
		t.Fatal("expected error for undeclared mount name")
	}
}
