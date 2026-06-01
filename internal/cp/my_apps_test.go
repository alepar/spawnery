package cp

import (
	"context"
	"testing"

	"connectrpc.com/connect"

	cpv1 "spawnery/gen/cp/v1"
	"spawnery/internal/cp/auth"
)

func TestListMyApps(t *testing.T) {
	s, _, _ := newTestServer(t)
	alice := auth.WithOwner(context.Background(), "alice")
	registerApp(t, s, "alice", "alice/one")
	registerApp(t, s, "alice", "alice/two")
	registerApp(t, s, "bob", "bob/app")
	if _, err := s.SetAppListing(alice, connect.NewRequest(&cpv1.SetAppListingRequest{AppId: "alice/two", Listed: false})); err != nil {
		t.Fatal(err)
	}
	resp, err := s.ListMyApps(alice, connect.NewRequest(&cpv1.ListMyAppsRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, a := range resp.Msg.Apps {
		got[a.Id] = a.Listed
	}
	if len(got) != 2 {
		t.Fatalf("want alice's 2 apps, got %d (%v)", len(got), got)
	}
	if _, ok := got["bob/app"]; ok {
		t.Fatal("bob's app leaked into alice's ListMyApps")
	}
	if v, ok := got["alice/one"]; !ok || !v {
		t.Fatalf("alice/one should be listed: %v", got)
	}
	if v, ok := got["alice/two"]; !ok || v {
		t.Fatalf("alice/two should be present and unlisted: %v", got)
	}
}

func TestListMyAppsUnauthenticated(t *testing.T) {
	s, _, _ := newTestServer(t)
	if _, err := s.ListMyApps(context.Background(), connect.NewRequest(&cpv1.ListMyAppsRequest{})); connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Fatalf("want Unauthenticated, got %v", err)
	}
}

func TestListAppsCarriesListed(t *testing.T) {
	s, _, _ := newTestServer(t)
	registerApp(t, s, "alice", "alice/pub")
	resp, err := s.ListApps(auth.WithOwner(context.Background(), "alice"), connect.NewRequest(&cpv1.ListAppsRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range resp.Msg.Apps {
		if !a.Listed {
			t.Fatalf("public ListApps result %q should be listed=true", a.Id)
		}
	}
}
