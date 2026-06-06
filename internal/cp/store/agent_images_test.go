package store

import (
	"context"
	"testing"
)

func TestAgentImagesWiring(t *testing.T) {
	st := NewTestStore(t) // opens + applies all migrations, incl. 0005
	if st.AgentImages() == nil {
		t.Fatal("AgentImages() returned nil")
	}
}

func TestAgentImageUpsertAndGet(t *testing.T) {
	st := NewTestStore(t)
	ctx := context.Background()
	inTx(t, st, func(tx Store) error {
		return tx.AgentImages().Upsert(ctx, AgentImage{Image: "ghcr.io/acme/goose:1", CreatedAt: 100}, []string{"goose", "opencode"})
	})

	img, err := st.AgentImages().Get(ctx, "ghcr.io/acme/goose:1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if img.Image != "ghcr.io/acme/goose:1" || img.CreatedAt != 100 {
		t.Fatalf("img = %+v", img)
	}

	bins, err := st.AgentImages().Binaries(ctx, "ghcr.io/acme/goose:1")
	if err != nil {
		t.Fatalf("Binaries: %v", err)
	}
	// Binaries returns sorted ascending.
	if len(bins) != 2 || bins[0] != "goose" || bins[1] != "opencode" {
		t.Fatalf("bins = %v", bins)
	}
}

func TestAgentImageUpsertReplacesBinaries(t *testing.T) {
	st := NewTestStore(t)
	ctx := context.Background()
	inTx(t, st, func(tx Store) error {
		return tx.AgentImages().Upsert(ctx, AgentImage{Image: "img:1", CreatedAt: 1}, []string{"goose", "opencode"})
	})
	inTx(t, st, func(tx Store) error {
		return tx.AgentImages().Upsert(ctx, AgentImage{Image: "img:1", CreatedAt: 2}, []string{"claude-code"})
	})

	bins, err := st.AgentImages().Binaries(ctx, "img:1")
	if err != nil {
		t.Fatalf("Binaries: %v", err)
	}
	if len(bins) != 1 || bins[0] != "claude-code" {
		t.Fatalf("want only claude-code, got %v", bins)
	}
}
