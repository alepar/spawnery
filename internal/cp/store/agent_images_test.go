package store

import (
	"context"
	"errors"
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

func TestAgentImageList(t *testing.T) {
	st := NewTestStore(t)
	ctx := context.Background()
	inTx(t, st, func(tx Store) error {
		if err := tx.AgentImages().Upsert(ctx, AgentImage{Image: "b:1", CreatedAt: 1}, []string{"goose"}); err != nil {
			return err
		}
		return tx.AgentImages().Upsert(ctx, AgentImage{Image: "a:1", CreatedAt: 1}, []string{"claude-code"})
	})

	imgs, err := st.AgentImages().List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	// List returns images sorted ascending by image.
	if len(imgs) != 2 || imgs[0].Image != "a:1" || imgs[1].Image != "b:1" {
		t.Fatalf("imgs = %+v", imgs)
	}
}

func TestAgentImageGetNotFound(t *testing.T) {
	st := NewTestStore(t)
	_, err := st.AgentImages().Get(context.Background(), "missing:1")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}

func TestAgentImageUpsertPreservesCreatedAt(t *testing.T) {
	st := NewTestStore(t)
	ctx := context.Background()
	inTx(t, st, func(tx Store) error {
		return tx.AgentImages().Upsert(ctx, AgentImage{Image: "img:1", CreatedAt: 1}, []string{"goose"})
	})
	inTx(t, st, func(tx Store) error {
		return tx.AgentImages().Upsert(ctx, AgentImage{Image: "img:1", CreatedAt: 999}, []string{"goose"})
	})

	img, err := st.AgentImages().Get(ctx, "img:1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if img.CreatedAt != 1 {
		t.Fatalf("created_at should be preserved on conflict, got %d", img.CreatedAt)
	}
}

func TestAgentImageUpsertEmptyBinaries(t *testing.T) {
	st := NewTestStore(t)
	ctx := context.Background()
	// An image with zero binaries is valid: the image row exists, with no binary rows.
	inTx(t, st, func(tx Store) error {
		return tx.AgentImages().Upsert(ctx, AgentImage{Image: "img:1", CreatedAt: 1}, nil)
	})

	if _, err := st.AgentImages().Get(ctx, "img:1"); err != nil {
		t.Fatalf("Get: %v", err)
	}
	bins, err := st.AgentImages().Binaries(ctx, "img:1")
	if err != nil {
		t.Fatalf("Binaries: %v", err)
	}
	if len(bins) != 0 {
		t.Fatalf("want no binaries, got %v", bins)
	}
}
