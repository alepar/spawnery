package cp

import (
	"context"
	"testing"

	"spawnery/internal/cp/store"
)

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
