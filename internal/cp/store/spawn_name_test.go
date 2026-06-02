package store

import (
	"context"
	"errors"
	"testing"
)

// The name column round-trips, and the store intentionally does NOT enforce name uniqueness
// (manual rename allows duplicates; dedup is an app-side concern). Two non-deleted spawns of the
// same owner may share a name.
func TestSpawnNamePersistsAndAllowsDuplicates(t *testing.T) {
	st := NewTestStore(t)
	seedAppAndOwner(t, st)
	ctx := context.Background()

	sp1 := newSpawn("sp1")
	sp1.Name = "Wiki"
	inTx(t, st, func(tx Store) error { return tx.Spawns().Create(ctx, sp1, nil) })

	got, err := st.Spawns().Get(ctx, "sp1")
	if err != nil || got.Name != "Wiki" {
		t.Fatalf("name round-trip: got=%q err=%v want %q", got.Name, err, "Wiki")
	}

	// Same owner, same name — allowed (no DB uniqueness constraint).
	sp2 := newSpawn("sp2")
	sp2.Name = "Wiki"
	if err := st.WithTx(ctx, func(tx Store) error { return tx.Spawns().Create(ctx, sp2, nil) }); err != nil {
		t.Fatalf("duplicate name must be allowed by the store, got %v", err)
	}
}

func TestSpawnRename(t *testing.T) {
	st := NewTestStore(t)
	seedAppAndOwner(t, st)
	ctx := context.Background()

	sp := newSpawn("sp1")
	sp.Name = "before"
	inTx(t, st, func(tx Store) error { return tx.Spawns().Create(ctx, sp, nil) })

	if err := st.Spawns().Rename(ctx, "sp1", "after"); err != nil {
		t.Fatalf("Rename: %v", err)
	}
	got, err := st.Spawns().Get(ctx, "sp1")
	if err != nil {
		t.Fatalf("Get after Rename: %v", err)
	}
	if got.Name != "after" {
		t.Fatalf("after rename name=%q want %q", got.Name, "after")
	}

	// Unknown spawn -> ErrNotFound.
	if err := st.Spawns().Rename(ctx, "nope", "x"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("rename unknown: want ErrNotFound, got %v", err)
	}

	// Deleted spawn -> ErrNotFound (status<>deleted guard).
	if err := st.WithTx(ctx, func(tx Store) error { return tx.Spawns().MarkDeleted(ctx, "sp1", 99) }); err != nil {
		t.Fatal(err)
	}
	if err := st.Spawns().Rename(ctx, "sp1", "y"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("rename deleted: want ErrNotFound, got %v", err)
	}
}
