package cp

import (
	"context"
	"testing"

	"connectrpc.com/connect"

	cpv1 "spawnery/gen/cp/v1"
	"spawnery/internal/cp/auth"
)

func TestListAgentImages(t *testing.T) {
	s, _, _ := newTestServer(t)
	ctx := auth.WithOwner(context.Background(), "alice")
	s.upsertAgentCatalog(context.Background(), []string{"img:2"}, []string{"claude-code"})
	s.upsertAgentCatalog(context.Background(), []string{"img:1"}, []string{"goose"})

	resp, err := s.ListAgentImages(ctx, connect.NewRequest(&cpv1.ListAgentImagesRequest{}))
	if err != nil {
		t.Fatalf("ListAgentImages: %v", err)
	}
	imgs := resp.Msg.Images
	if len(imgs) != 2 {
		t.Fatalf("want 2 images, got %d", len(imgs))
	}
	// List is sorted by image asc.
	if imgs[0].Image != "img:1" || len(imgs[0].Binaries) != 1 || imgs[0].Binaries[0] != "goose" {
		t.Fatalf("img[0] = %+v", imgs[0])
	}
	if imgs[1].Image != "img:2" || len(imgs[1].Binaries) != 1 || imgs[1].Binaries[0] != "claude-code" {
		t.Fatalf("img[1] = %+v", imgs[1])
	}
	// img:1 has binary "goose" → runnables goose-acp (acp) + goose-tui (tmux), with labels.
	got := imgs[0]
	if got.Image != "img:1" {
		t.Fatalf("img[0].Image = %q", got.Image)
	}
	byID := map[string]*cpv1.RunnableInfo{}
	for _, r := range got.Runnables {
		byID[r.Id] = r
	}
	if byID["goose-acp"] == nil || byID["goose-acp"].Mode != "acp" || byID["goose-acp"].Label == "" {
		t.Fatalf("goose-acp runnable missing/wrong: %+v", got.Runnables)
	}
	if byID["goose-tui"] == nil || byID["goose-tui"].Mode != "tmux" {
		t.Fatalf("goose-tui runnable missing/wrong: %+v", got.Runnables)
	}
}

func TestListAgentImagesRequiresAuth(t *testing.T) {
	s, _, _ := newTestServer(t)
	_, err := s.ListAgentImages(context.Background(), connect.NewRequest(&cpv1.ListAgentImagesRequest{}))
	if connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Fatalf("want Unauthenticated, got %v", connect.CodeOf(err))
	}
}

func TestListAgentImagesEmpty(t *testing.T) {
	s, _, _ := newTestServer(t)
	ctx := auth.WithOwner(context.Background(), "alice")
	resp, err := s.ListAgentImages(ctx, connect.NewRequest(&cpv1.ListAgentImagesRequest{}))
	if err != nil {
		t.Fatalf("ListAgentImages: %v", err)
	}
	if len(resp.Msg.Images) != 0 {
		t.Fatalf("want 0 images, got %d", len(resp.Msg.Images))
	}
}
