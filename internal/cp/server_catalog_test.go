package cp

import (
	"context"
	"testing"
)

func TestUpsertAgentCatalog(t *testing.T) {
	s, _, _ := newTestServer(t)
	ctx := context.Background()

	// First registration records the image + its binaries.
	s.upsertAgentCatalog(ctx, []string{"ghcr.io/acme/goose:1"}, []string{"goose", "opencode"})
	if _, err := s.st.AgentImages().Get(ctx, "ghcr.io/acme/goose:1"); err != nil {
		t.Fatalf("image not recorded: %v", err)
	}
	bins, err := s.st.AgentImages().Binaries(ctx, "ghcr.io/acme/goose:1")
	if err != nil || len(bins) != 2 || bins[0] != "goose" || bins[1] != "opencode" {
		t.Fatalf("binaries = %v err = %v", bins, err)
	}

	// Reconnect replaces the binary set (idempotent), catalog row persists.
	s.upsertAgentCatalog(ctx, []string{"ghcr.io/acme/goose:1"}, []string{"claude-code"})
	bins, _ = s.st.AgentImages().Binaries(ctx, "ghcr.io/acme/goose:1")
	if len(bins) != 1 || bins[0] != "claude-code" {
		t.Fatalf("reconnect should replace binaries, got %v", bins)
	}

	// Empty image names are skipped (no spurious catalog row).
	s.upsertAgentCatalog(ctx, []string{""}, nil)
	if _, err := s.st.AgentImages().Get(ctx, ""); err == nil {
		t.Fatalf("empty image name should be skipped")
	}
}
