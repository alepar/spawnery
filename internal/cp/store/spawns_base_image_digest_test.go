package store

// spawns_base_image_digest_test.go: tests for SetBaseImageDigest (sp-ei4.1.10, spec §4).
// Mirrors spawns_model_test.go in structure (same patterns, same store harness).

import (
	"context"
	"errors"
	"testing"
)

// F1: SetBaseImageDigest writes the digest and it round-trips via Get; default is "".
func TestBaseImageDigestDefaultAndSet(t *testing.T) {
	st := NewTestStore(t)
	seedAppAndOwner(t, st)
	ctx := context.Background()
	inTx(t, st, func(tx Store) error { return tx.Spawns().Create(ctx, newSpawn("sp1"), nil) })

	// Default must be empty string (NOT NULL default '' from the migration).
	s, err := st.Spawns().Get(ctx, "sp1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if s.BaseImageDigest != "" {
		t.Fatalf("fresh spawn: base_image_digest = %q, want empty", s.BaseImageDigest)
	}

	// SetBaseImageDigest writes the value.
	const digest = "spawnery/agent@sha256:cafebabe"
	if err := st.Spawns().SetBaseImageDigest(ctx, "sp1", digest); err != nil {
		t.Fatalf("SetBaseImageDigest: %v", err)
	}
	s, err = st.Spawns().Get(ctx, "sp1")
	if err != nil {
		t.Fatalf("Get after set: %v", err)
	}
	if s.BaseImageDigest != digest {
		t.Fatalf("base_image_digest = %q, want %q", s.BaseImageDigest, digest)
	}
}

// F2: SetBaseImageDigest on a missing or deleted spawn returns ErrNotFound.
func TestBaseImageDigestNotFound(t *testing.T) {
	st := NewTestStore(t)
	seedAppAndOwner(t, st)
	ctx := context.Background()

	// Missing spawn → ErrNotFound.
	if err := st.Spawns().SetBaseImageDigest(ctx, "ghost", "d"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("SetBaseImageDigest(missing): want ErrNotFound, got %v", err)
	}

	// Deleted spawn → ErrNotFound.
	inTx(t, st, func(tx Store) error { return tx.Spawns().Create(ctx, newSpawn("sp1"), nil) })
	inTx(t, st, func(tx Store) error { return tx.Spawns().MarkDeleted(ctx, "sp1", 9) })
	if err := st.Spawns().SetBaseImageDigest(ctx, "sp1", "d"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("SetBaseImageDigest(deleted): want ErrNotFound, got %v", err)
	}
}
