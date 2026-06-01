package cp

import (
	"context"
	"testing"

	"connectrpc.com/connect"

	cpv1 "spawnery/gen/cp/v1"
	"spawnery/internal/cp/auth"
)

func regReq() *cpv1.RegisterAppVersionRequest {
	return &cpv1.RegisterAppVersionRequest{
		Manifest: &cpv1.AppManifest{
			ApiVersion: "spawnery/v1", Id: "alice/wiki", Title: "Alice Wiki",
			Description: "notes", Tags: []string{"notes"}, Visibility: "open",
			Mounts: []*cpv1.ManifestMount{{Name: "main", Path: "data", Seed: "seed"}},
		},
		Version: "1.0.0", Ref: "alice/wiki@sha1",
	}
}

func TestRegisterAppVersionNewApp(t *testing.T) {
	s, _, _ := newTestServer(t)
	ctx := auth.WithOwner(context.Background(), "alice")
	resp, err := s.RegisterAppVersion(ctx, connect.NewRequest(regReq()))
	if err != nil {
		t.Fatal(err)
	}
	if resp.Msg.AppId != "alice/wiki" || resp.Msg.Tier != cpv1.TrustTier_TRUST_TIER_UNVERIFIED {
		t.Fatalf("resp = %+v", resp.Msg)
	}
	got, err := s.GetApp(ctx, connect.NewRequest(&cpv1.GetAppRequest{Id: "alice/wiki"}))
	if err != nil || got.Msg.App.DisplayName != "Alice Wiki" || got.Msg.Versions[0].Tier != cpv1.TrustTier_TRUST_TIER_UNVERIFIED {
		t.Fatalf("getapp = %+v err=%v", got.Msg, err)
	}
}

func TestRegisterAppVersionCreatorGuard(t *testing.T) {
	s, _, _ := newTestServer(t)
	alice := auth.WithOwner(context.Background(), "alice")
	if _, err := s.RegisterAppVersion(alice, connect.NewRequest(regReq())); err != nil {
		t.Fatal(err)
	}
	r2 := regReq()
	r2.Version = "1.1.0"
	r2.Ref = "alice/wiki@sha2"
	if _, err := s.RegisterAppVersion(alice, connect.NewRequest(r2)); err != nil {
		t.Fatalf("same-owner new version rejected: %v", err)
	}
	mallory := auth.WithOwner(context.Background(), "mallory")
	_, err := s.RegisterAppVersion(mallory, connect.NewRequest(regReq()))
	if connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Fatalf("want PermissionDenied, got %v", err)
	}
}

func TestRegisterAppVersionRejections(t *testing.T) {
	s, _, _ := newTestServer(t)
	alice := auth.WithOwner(context.Background(), "alice")
	bad := regReq()
	bad.Manifest.ApiVersion = "nope"
	if _, err := s.RegisterAppVersion(alice, connect.NewRequest(bad)); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("want InvalidArgument, got %v", err)
	}
	if _, err := s.RegisterAppVersion(context.Background(), connect.NewRequest(regReq())); connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Fatalf("want Unauthenticated, got %v", err)
	}
}
