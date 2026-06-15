package journal

import (
	"context"
	"testing"
)

func TestGenerationHoldPreservesHeldGenerationAcrossRevokeSuperseded(t *testing.T) {
	ctx := context.Background()
	admin := newFakeAdmin()
	g, err := NewGenerationKeyManager(GenerationKeyConfig{Admin: admin, S3Endpoint: "127.0.0.1:3900"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := g.Mint(ctx, "sp-source", 8); err != nil {
		t.Fatalf("mint gen8: %v", err)
	}
	if _, err := g.Mint(ctx, "sp-source", 9); err != nil {
		t.Fatalf("mint gen9: %v", err)
	}
	hold := g.HoldGeneration("sp-source", 8, "fork ts-1")
	if err := g.RevokeSuperseded(ctx, "sp-source", 9); err != nil {
		t.Fatalf("RevokeSuperseded: %v", err)
	}
	if g.lookup("sp-source", 8) == "" {
		t.Fatal("held gen8 key was revoked")
	}
	hold.Release()
	if err := g.RevokeSuperseded(ctx, "sp-source", 9); err != nil {
		t.Fatalf("RevokeSuperseded after release: %v", err)
	}
	if g.lookup("sp-source", 8) != "" {
		t.Fatal("released gen8 key was not revoked")
	}
}

func TestGenerationHoldReleaseIsIdempotent(t *testing.T) {
	admin := newFakeAdmin()
	g, err := NewGenerationKeyManager(GenerationKeyConfig{Admin: admin, S3Endpoint: "127.0.0.1:3900"})
	if err != nil {
		t.Fatal(err)
	}
	h := g.HoldGeneration("sp-source", 3, "fork ts-2")
	h.Release()
	h.Release()
	if got := g.holdCount("sp-source", 3); got != 0 {
		t.Fatalf("hold count=%d want 0", got)
	}
}
