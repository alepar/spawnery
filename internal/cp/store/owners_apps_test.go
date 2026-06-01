package store

import (
	"context"
	"errors"
	"testing"
)

func TestOwnerUpsertGet(t *testing.T) {
	st := NewTestStore(t)
	ctx := context.Background()
	if err := st.Owners().Upsert(ctx, Owner{ID: "alice", Email: "a@x", CreatedAt: 100}); err != nil {
		t.Fatal(err)
	}
	if err := st.Owners().Upsert(ctx, Owner{ID: "alice", Email: "a2@x", CreatedAt: 100}); err != nil {
		t.Fatal(err) // upsert again -> updates, no error
	}
	o, err := st.Owners().Get(ctx, "alice")
	if err != nil || o.Email != "a2@x" {
		t.Fatalf("o=%+v err=%v", o, err)
	}
	if _, err := st.Owners().Get(ctx, "nobody"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}

func TestAppVersionsAndDeclaredMounts(t *testing.T) {
	st := NewTestStore(t)
	ctx := context.Background()
	if err := st.Apps().Upsert(ctx, App{ID: "spawnery/secret", DisplayName: "Secret", Visibility: "public", Listed: true, CreatedAt: 1}); err != nil {
		t.Fatal(err)
	}
	if err := st.Apps().UpsertVersion(ctx,
		AppVersion{AppID: "spawnery/secret", Version: "1.0.0", Ref: "ref1", Tier: TierReviewed, CreatedAt: 10},
		[]MountDecl{{AppID: "spawnery/secret", Version: "1.0.0", Name: "main", Required: true}}); err != nil {
		t.Fatal(err)
	}
	if err := st.Apps().UpsertVersion(ctx,
		AppVersion{AppID: "spawnery/secret", Version: "1.1.0", Ref: "ref2", Tier: TierUnverified, CreatedAt: 20}, nil); err != nil {
		t.Fatal(err)
	}
	lr, err := st.Apps().LatestReviewed(ctx, "spawnery/secret")
	if err != nil || lr.Version != "1.0.0" || lr.Ref != "ref1" {
		t.Fatalf("latest reviewed = %+v err=%v (want 1.0.0/ref1)", lr, err)
	}
	mounts, err := st.Apps().DeclaredMounts(ctx, "spawnery/secret", "1.0.0")
	if err != nil || len(mounts) != 1 || mounts[0].Name != "main" {
		t.Fatalf("mounts=%+v err=%v", mounts, err)
	}
	if err := st.Apps().Upsert(ctx, App{ID: "noreview", Visibility: "public", Listed: true, CreatedAt: 1}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Apps().LatestReviewed(ctx, "noreview"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}
