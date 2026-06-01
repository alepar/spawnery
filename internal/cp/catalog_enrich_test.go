package cp

import (
	"context"
	"testing"

	"connectrpc.com/connect"

	cpv1 "spawnery/gen/cp/v1"
	"spawnery/internal/cp/auth"
)

func TestGetAppReturnsManifest(t *testing.T) {
	s, _, _ := newTestServer(t)
	registerApp(t, s, "alice", "alice/app") // helper from moderation_test.go (same package)
	resp, err := s.GetApp(auth.WithOwner(context.Background(), "alice"), connect.NewRequest(&cpv1.GetAppRequest{Id: "alice/app"}))
	if err != nil {
		t.Fatal(err)
	}
	if resp.Msg.Manifest == nil {
		t.Fatal("expected a manifest for a registered app")
	}
	if resp.Msg.Manifest.Id != "alice/app" || resp.Msg.Manifest.Title != "T" {
		t.Fatalf("manifest = %+v", resp.Msg.Manifest)
	}
	if len(resp.Msg.Manifest.Mounts) != 1 || resp.Msg.Manifest.Mounts[0].Name != "main" {
		t.Fatalf("manifest mounts = %+v", resp.Msg.Manifest.Mounts)
	}
}

func TestGetAppSeedAppNilManifest(t *testing.T) {
	s, _, _ := newTestServer(t)
	resp, err := s.GetApp(auth.WithOwner(context.Background(), "alice"), connect.NewRequest(&cpv1.GetAppRequest{Id: "secret-app"}))
	if err != nil {
		t.Fatal(err)
	}
	if resp.Msg.Manifest != nil {
		t.Fatalf("seed app should have nil manifest, got %+v", resp.Msg.Manifest)
	}
	if resp.Msg.App.Id != "secret-app" {
		t.Fatalf("summary missing: %+v", resp.Msg.App)
	}
}
