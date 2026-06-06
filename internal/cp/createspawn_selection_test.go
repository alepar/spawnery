package cp

import (
	"context"
	"testing"

	"connectrpc.com/connect"

	cpv1 "spawnery/gen/cp/v1"
	"spawnery/internal/cp/auth"
)

// seedCatalogGoose records an image whose binary set is {goose} (offers goose-acp + goose-tui).
func seedCatalogGoose(t *testing.T, s *Server, image string) {
	t.Helper()
	s.upsertAgentCatalog(context.Background(), []string{image}, []string{"goose"})
}

func TestCreateSpawnRejectsTmuxMode(t *testing.T) {
	s, _, _ := newTestServer(t)
	seedCatalogGoose(t, s, "img:1")
	ctx := auth.WithOwner(context.Background(), "alice")

	_, err := s.CreateSpawn(ctx, connect.NewRequest(&cpv1.CreateSpawnRequest{
		AppId: "secret-app", Model: "m", Image: "img:1", RunnableId: "goose-tui",
	}))
	if connect.CodeOf(err) != connect.CodeUnimplemented {
		t.Fatalf("want CodeUnimplemented for tmux mode, got %v (err=%v)", connect.CodeOf(err), err)
	}
}

func TestCreateSpawnRejectsUnknownImage(t *testing.T) {
	s, _, _ := newTestServer(t)
	ctx := auth.WithOwner(context.Background(), "alice")

	_, err := s.CreateSpawn(ctx, connect.NewRequest(&cpv1.CreateSpawnRequest{
		AppId: "secret-app", Model: "m", Image: "ghost:9", RunnableId: "goose-acp",
	}))
	if connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("want CodeInvalidArgument for unknown image, got %v (err=%v)", connect.CodeOf(err), err)
	}
}

func TestCreateSpawnRejectsUnknownRunnable(t *testing.T) {
	s, _, _ := newTestServer(t)
	seedCatalogGoose(t, s, "img:1")
	ctx := auth.WithOwner(context.Background(), "alice")

	_, err := s.CreateSpawn(ctx, connect.NewRequest(&cpv1.CreateSpawnRequest{
		AppId: "secret-app", Model: "m", Image: "img:1", RunnableId: "bogus",
	}))
	if connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("want CodeInvalidArgument for unknown runnable, got %v (err=%v)", connect.CodeOf(err), err)
	}
}

func TestCreateSpawnRequiresImageWithRunnable(t *testing.T) {
	s, _, _ := newTestServer(t)
	ctx := auth.WithOwner(context.Background(), "alice")

	_, err := s.CreateSpawn(ctx, connect.NewRequest(&cpv1.CreateSpawnRequest{
		AppId: "secret-app", Model: "m", RunnableId: "goose-acp", // no image
	}))
	if connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("want CodeInvalidArgument when runnable set without image, got %v", connect.CodeOf(err))
	}
}

func TestCreateSpawnRequiresRunnableWithImage(t *testing.T) {
	s, _, _ := newTestServer(t)
	seedCatalogGoose(t, s, "img:1")
	ctx := auth.WithOwner(context.Background(), "alice")

	_, err := s.CreateSpawn(ctx, connect.NewRequest(&cpv1.CreateSpawnRequest{
		AppId: "secret-app", Model: "m", Image: "img:1", // no runnable_id
	}))
	if connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("want CodeInvalidArgument when image set without runnable, got %v", connect.CodeOf(err))
	}
}

func TestCreateSpawnPersistsAcpSelection(t *testing.T) {
	s, _, _ := newTestServer(t)
	seedCatalogGoose(t, s, "img:1")
	ctx := auth.WithOwner(context.Background(), "alice")

	// No node is registered; CreateSpawn inserts synchronously and returns before async provision,
	// so the persisted selection is observable regardless of provisioning outcome.
	resp, err := s.CreateSpawn(ctx, connect.NewRequest(&cpv1.CreateSpawnRequest{
		AppId: "secret-app", Model: "m", Image: "img:1", RunnableId: "goose-acp",
	}))
	if err != nil {
		t.Fatalf("CreateSpawn: %v", err)
	}
	got, err := s.st.Spawns().Get(context.Background(), resp.Msg.SpawnId)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Image != "img:1" || got.RunnableID != "goose-acp" || got.Mode != "acp" {
		t.Fatalf("selection not persisted: image=%q runnable=%q mode=%q", got.Image, got.RunnableID, got.Mode)
	}
}

func TestCreateSpawnLegacyNoSelection(t *testing.T) {
	s, _, _ := newTestServer(t)
	ctx := auth.WithOwner(context.Background(), "alice")

	resp, err := s.CreateSpawn(ctx, connect.NewRequest(&cpv1.CreateSpawnRequest{
		AppId: "secret-app", Model: "m", // no image/runnable
	}))
	if err != nil {
		t.Fatalf("CreateSpawn (legacy): %v", err)
	}
	got, err := s.st.Spawns().Get(context.Background(), resp.Msg.SpawnId)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Image != "" || got.RunnableID != "" || got.Mode != "" {
		t.Fatalf("legacy spawn should have empty selection: %+v", got)
	}
}
