package store

import (
	"context"
	"errors"
	"testing"
)

// Freshly-created spawns start applied=true with an empty detail (the fresh pod runs spawns.model).
func TestModelAppliedDefaultsTrueOnCreate(t *testing.T) {
	st := NewTestStore(t)
	seedAppAndOwner(t, st)
	ctx := context.Background()
	inTx(t, st, func(tx Store) error { return tx.Spawns().Create(ctx, newSpawn("sp1"), nil) })

	s, err := st.Spawns().Get(ctx, "sp1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !s.ModelApplied {
		t.Fatal("fresh spawn: model_applied = false, want true")
	}
	if s.ModelApplyDetail != "" {
		t.Fatalf("fresh spawn: model_apply_detail = %q, want empty", s.ModelApplyDetail)
	}
}

// SetModel writes the new model, flips applied->false, and clears a prior failure detail in one shot.
func TestSetModelMarksUnappliedAndClearsDetail(t *testing.T) {
	st := NewTestStore(t)
	seedAppAndOwner(t, st)
	ctx := context.Background()
	inTx(t, st, func(tx Store) error { return tx.Spawns().Create(ctx, newSpawn("sp1"), nil) })

	// Seed a stale failure detail to prove SetModel clears it.
	if err := st.Spawns().MarkModelApplyFailed(ctx, "sp1", "boom"); err != nil {
		t.Fatalf("MarkModelApplyFailed: %v", err)
	}

	if err := st.Spawns().SetModel(ctx, "sp1", "anthropic/claude"); err != nil {
		t.Fatalf("SetModel: %v", err)
	}
	s, _ := st.Spawns().Get(ctx, "sp1")
	if s.Model != "anthropic/claude" {
		t.Fatalf("model = %q, want anthropic/claude", s.Model)
	}
	if s.ModelApplied {
		t.Fatal("after SetModel: model_applied = true, want false")
	}
	if s.ModelApplyDetail != "" {
		t.Fatalf("after SetModel: detail = %q, want cleared", s.ModelApplyDetail)
	}
}

// SetModel on a missing or deleted spawn returns ErrNotFound (mirrors Rename).
func TestSetModelNotFound(t *testing.T) {
	st := NewTestStore(t)
	seedAppAndOwner(t, st)
	ctx := context.Background()

	if err := st.Spawns().SetModel(ctx, "ghost", "m"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("SetModel missing: want ErrNotFound, got %v", err)
	}

	inTx(t, st, func(tx Store) error { return tx.Spawns().Create(ctx, newSpawn("sp1"), nil) })
	inTx(t, st, func(tx Store) error { return tx.Spawns().MarkDeleted(ctx, "sp1", 9) })
	if err := st.Spawns().SetModel(ctx, "sp1", "m"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("SetModel deleted: want ErrNotFound, got %v", err)
	}
}

// MarkModelApplied flips applied->true and clears detail.
func TestMarkModelApplied(t *testing.T) {
	st := NewTestStore(t)
	seedAppAndOwner(t, st)
	ctx := context.Background()
	inTx(t, st, func(tx Store) error { return tx.Spawns().Create(ctx, newSpawn("sp1"), nil) })

	if err := st.Spawns().SetModel(ctx, "sp1", "m2"); err != nil {
		t.Fatalf("SetModel: %v", err)
	}
	if err := st.Spawns().MarkModelApplyFailed(ctx, "sp1", "boom"); err != nil {
		t.Fatalf("MarkModelApplyFailed: %v", err)
	}
	if err := st.Spawns().MarkModelApplied(ctx, "sp1"); err != nil {
		t.Fatalf("MarkModelApplied: %v", err)
	}
	s, _ := st.Spawns().Get(ctx, "sp1")
	if !s.ModelApplied {
		t.Fatal("after MarkModelApplied: model_applied = false, want true")
	}
	if s.ModelApplyDetail != "" {
		t.Fatalf("after MarkModelApplied: detail = %q, want cleared", s.ModelApplyDetail)
	}
}

// MarkModelApplyFailed keeps applied=false and records the reason.
func TestMarkModelApplyFailed(t *testing.T) {
	st := NewTestStore(t)
	seedAppAndOwner(t, st)
	ctx := context.Background()
	inTx(t, st, func(tx Store) error { return tx.Spawns().Create(ctx, newSpawn("sp1"), nil) })

	if err := st.Spawns().SetModel(ctx, "sp1", "m2"); err != nil {
		t.Fatalf("SetModel: %v", err)
	}
	if err := st.Spawns().MarkModelApplyFailed(ctx, "sp1", "node down"); err != nil {
		t.Fatalf("MarkModelApplyFailed: %v", err)
	}
	s, _ := st.Spawns().Get(ctx, "sp1")
	if s.ModelApplied {
		t.Fatal("after MarkModelApplyFailed: model_applied = true, want false")
	}
	if s.ModelApplyDetail != "node down" {
		t.Fatalf("detail = %q, want \"node down\"", s.ModelApplyDetail)
	}
}

// ListUnappliedModel returns exactly the spawns with model_applied=false (non-deleted).
func TestListUnappliedModel(t *testing.T) {
	st := NewTestStore(t)
	seedAppAndOwner(t, st)
	ctx := context.Background()
	for _, id := range []string{"sp1", "sp2", "sp3"} {
		inTx(t, st, func(tx Store) error { return tx.Spawns().Create(ctx, newSpawn(id), nil) })
	}

	// All fresh -> applied=true -> none unapplied.
	if got, err := st.Spawns().ListUnappliedModel(ctx); err != nil || len(got) != 0 {
		t.Fatalf("initial ListUnappliedModel = %d rows (err=%v), want 0", len(got), err)
	}

	// Flip sp1 and sp2 unapplied.
	if err := st.Spawns().SetModel(ctx, "sp1", "x"); err != nil {
		t.Fatalf("SetModel sp1: %v", err)
	}
	if err := st.Spawns().SetModel(ctx, "sp2", "y"); err != nil {
		t.Fatalf("SetModel sp2: %v", err)
	}
	got, err := st.Spawns().ListUnappliedModel(ctx)
	if err != nil {
		t.Fatalf("ListUnappliedModel: %v", err)
	}
	ids := map[string]bool{}
	for _, s := range got {
		ids[s.ID] = true
	}
	if len(got) != 2 || !ids["sp1"] || !ids["sp2"] {
		t.Fatalf("ListUnappliedModel = %v, want {sp1, sp2}", ids)
	}

	// Re-apply sp1 -> only sp2 remains unapplied.
	if err := st.Spawns().MarkModelApplied(ctx, "sp1"); err != nil {
		t.Fatalf("MarkModelApplied sp1: %v", err)
	}
	got, _ = st.Spawns().ListUnappliedModel(ctx)
	if len(got) != 1 || got[0].ID != "sp2" {
		t.Fatalf("after re-apply: ListUnappliedModel = %v, want [sp2]", got)
	}
}
