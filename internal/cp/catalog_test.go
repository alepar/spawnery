package cp

import (
	"context"
	"testing"

	"connectrpc.com/connect"

	cpv1 "spawnery/gen/cp/v1"
	"spawnery/internal/cp/auth"
	"spawnery/internal/cp/store"
)

func seedCatalog(t *testing.T, s *Server) {
	t.Helper()
	ctx := context.Background()
	st := s.st
	if err := st.Apps().Upsert(ctx, store.App{ID: "spawnery/wiki", DisplayName: "Wiki", Summary: "research notes", Tags: "notes,research", Visibility: "public", Listed: true, CreatedAt: 5}); err != nil {
		t.Fatal(err)
	}
	if err := st.Apps().UpsertVersion(ctx, store.AppVersion{AppID: "spawnery/wiki", Version: "1.0.0", Ref: "r1", Tier: store.TierReviewed, CreatedAt: 5}, nil); err != nil {
		t.Fatal(err)
	}
}

func authCtx() context.Context {
	return auth.WithOwner(context.Background(), "alice")
}

func TestListAppsBrowseAndSearch(t *testing.T) {
	s, _, _ := newTestServer(t)
	seedCatalog(t, s)
	resp, err := s.ListApps(authCtx(), connect.NewRequest(&cpv1.ListAppsRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Msg.Apps) < 2 {
		t.Fatalf("want >=2 apps, got %d", len(resp.Msg.Apps))
	}
	var wiki *cpv1.AppSummary
	for _, a := range resp.Msg.Apps {
		if a.Id == "spawnery/wiki" {
			wiki = a
		}
	}
	if wiki == nil || wiki.LatestTier != cpv1.TrustTier_TRUST_TIER_REVIEWED || wiki.LatestVersion != "1.0.0" {
		t.Fatalf("wiki summary = %+v", wiki)
	}
	if len(wiki.Tags) != 2 {
		t.Fatalf("wiki tags = %v (want 2)", wiki.Tags)
	}
	hit, err := s.ListApps(authCtx(), connect.NewRequest(&cpv1.ListAppsRequest{Query: "research"}))
	if err != nil || len(hit.Msg.Apps) != 1 || hit.Msg.Apps[0].Id != "spawnery/wiki" {
		t.Fatalf("search research -> %+v err=%v", hit.Msg.Apps, err)
	}
}

func TestListAppsUnauthenticated(t *testing.T) {
	s, _, _ := newTestServer(t)
	_, err := s.ListApps(context.Background(), connect.NewRequest(&cpv1.ListAppsRequest{}))
	if connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Fatalf("want Unauthenticated, got %v", err)
	}
}

func TestGetApp(t *testing.T) {
	s, _, _ := newTestServer(t)
	seedCatalog(t, s)
	resp, err := s.GetApp(authCtx(), connect.NewRequest(&cpv1.GetAppRequest{Id: "spawnery/wiki"}))
	if err != nil {
		t.Fatal(err)
	}
	if resp.Msg.App.Id != "spawnery/wiki" || len(resp.Msg.Versions) != 1 {
		t.Fatalf("detail = %+v", resp.Msg)
	}
	if resp.Msg.Versions[0].Tier != cpv1.TrustTier_TRUST_TIER_REVIEWED {
		t.Fatalf("version tier = %v", resp.Msg.Versions[0].Tier)
	}
	_, err = s.GetApp(authCtx(), connect.NewRequest(&cpv1.GetAppRequest{Id: "nope"}))
	if connect.CodeOf(err) != connect.CodeNotFound {
		t.Fatalf("want NotFound, got %v", err)
	}
}
