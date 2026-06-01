package cp

import (
	"context"
	"testing"

	"connectrpc.com/connect"

	cpv1 "spawnery/gen/cp/v1"
	"spawnery/internal/cp/auth"
)

// registerApp registers an app owned by `owner`. Shared helper (also used by the slice-4 enrich test).
func registerApp(t *testing.T, s *Server, owner, id string) {
	t.Helper()
	ctx := auth.WithOwner(context.Background(), owner)
	_, err := s.RegisterAppVersion(ctx, connect.NewRequest(&cpv1.RegisterAppVersionRequest{
		Manifest: &cpv1.AppManifest{ApiVersion: "spawnery/v1", Id: id, Title: "T", Visibility: "open",
			Mounts: []*cpv1.ManifestMount{{Name: "main", Path: "data", Seed: "seed"}}},
		Version: "1.0.0", Ref: id + "@sha",
	}))
	if err != nil {
		t.Fatalf("register: %v", err)
	}
}

func TestSetAppListingTakedownRelist(t *testing.T) {
	s, _, _ := newTestServer(t)
	alice := auth.WithOwner(context.Background(), "alice")
	registerApp(t, s, "alice", "alice/app")
	if _, err := s.SetAppListing(alice, connect.NewRequest(&cpv1.SetAppListingRequest{AppId: "alice/app", Listed: false})); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetApp(alice, connect.NewRequest(&cpv1.GetAppRequest{Id: "alice/app"})); connect.CodeOf(err) != connect.CodeNotFound {
		t.Fatalf("unlisted GetApp want NotFound, got %v", err)
	}
	if _, err := s.SetAppListing(alice, connect.NewRequest(&cpv1.SetAppListingRequest{AppId: "alice/app", Listed: true})); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetApp(alice, connect.NewRequest(&cpv1.GetAppRequest{Id: "alice/app"})); err != nil {
		t.Fatalf("relisted GetApp should succeed: %v", err)
	}
}

func TestSetAppListingGuards(t *testing.T) {
	s, _, _ := newTestServer(t)
	registerApp(t, s, "alice", "alice/app")
	mallory := auth.WithOwner(context.Background(), "mallory")
	if _, err := s.SetAppListing(mallory, connect.NewRequest(&cpv1.SetAppListingRequest{AppId: "alice/app", Listed: false})); connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Fatalf("non-creator want PermissionDenied, got %v", err)
	}
	alice := auth.WithOwner(context.Background(), "alice")
	if _, err := s.SetAppListing(alice, connect.NewRequest(&cpv1.SetAppListingRequest{AppId: "nope", Listed: false})); connect.CodeOf(err) != connect.CodeNotFound {
		t.Fatalf("missing app want NotFound, got %v", err)
	}
	if _, err := s.SetAppListing(context.Background(), connect.NewRequest(&cpv1.SetAppListingRequest{AppId: "alice/app", Listed: false})); connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Fatalf("unauth want Unauthenticated, got %v", err)
	}
}
