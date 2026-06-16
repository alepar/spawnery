package cp

import (
	"context"
	"strings"
	"testing"

	"spawnery/internal/cp/store"
)

func TestSeedWritesCatalogMetadata(t *testing.T) {
	st := store.NewTestStore(t)
	ctx := context.Background()
	apps := []AppSeed{{
		ID: "spawnery/wiki", Ref: "examples/wiki", Version: "1.0.0",
		DisplayName: "Wiki & Research Companion", Summary: "capture, connect, recall",
		Tags: []string{"notes", "research"},
	}}
	if err := Seed(ctx, st, map[string]string{"t": "alice"}, apps); err != nil {
		t.Fatal(err)
	}
	got, err := st.Apps().Get(ctx, "spawnery/wiki")
	if err != nil || got.DisplayName != "Wiki & Research Companion" || got.Summary != "capture, connect, recall" {
		t.Fatalf("app = %+v err=%v", got, err)
	}
	if got.Tags != "notes,research" || got.Visibility != "public" || !got.Listed {
		t.Fatalf("catalog meta = %+v", got)
	}
	v, err := st.Apps().LatestReviewed(ctx, "spawnery/wiki")
	if err != nil || v.Tier != store.TierReviewed {
		t.Fatalf("version = %+v err=%v (want reviewed)", v, err)
	}
}

func TestSeed(t *testing.T) {
	st := store.NewTestStore(t)
	ctx := context.Background()
	tokens := map[string]string{"dev-token": "dev", "alice-token": "alice"}
	apps := []AppSeed{{ID: "secret-app", Ref: "examples/secret-app", Version: "1.0.0", Mounts: []string{"main"}}}
	if err := Seed(ctx, st, tokens, apps); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Owners().Get(ctx, "dev"); err != nil {
		t.Fatalf("owner dev not seeded: %v", err)
	}
	if _, err := st.Owners().Get(ctx, "alice"); err != nil {
		t.Fatalf("owner alice not seeded: %v", err)
	}
	v, err := st.Apps().LatestReviewed(ctx, "secret-app")
	if err != nil || v.Ref != "examples/secret-app" {
		t.Fatalf("app not seeded: v=%+v err=%v", v, err)
	}
	m, err := st.Apps().DeclaredMounts(ctx, "secret-app", "1.0.0")
	if err != nil || len(m) != 1 || m[0].Name != "main" {
		t.Fatalf("mounts=%+v err=%v", m, err)
	}
	if err := Seed(ctx, st, tokens, apps); err != nil { // idempotent
		t.Fatalf("re-seed: %v", err)
	}
}

func TestSeedFailsClosedWhenDeclaredMountManifestMissing(t *testing.T) {
	st := store.NewTestStore(t)
	t.Chdir(t.TempDir())
	err := Seed(context.Background(), st, nil, []AppSeed{{
		ID: "secret-app", Ref: "examples/secret-app", Version: "1.0.0", Mounts: []string{"main"},
	}})
	if err == nil || !strings.Contains(err.Error(), "parse seed manifest") {
		t.Fatalf("Seed err=%v, want parse seed manifest failure", err)
	}
}
