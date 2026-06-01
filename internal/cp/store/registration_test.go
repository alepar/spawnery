package store

import (
	"context"
	"errors"
	"testing"
)

func TestCreatorStickyAndManifestRoundTrip(t *testing.T) {
	st := NewTestStore(t)
	ctx := context.Background()
	if _, err := st.Apps().Creator(ctx, "creator/app"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Creator on missing app = %v (want ErrNotFound)", err)
	}
	if err := st.Apps().Upsert(ctx, App{ID: "creator/app", DisplayName: "App", Visibility: "public", Listed: true, CreatorID: "alice", CreatedAt: 1}); err != nil {
		t.Fatal(err)
	}
	if err := st.Apps().Upsert(ctx, App{ID: "creator/app", DisplayName: "App2", Visibility: "public", Listed: true, CreatorID: "mallory", CreatedAt: 2}); err != nil {
		t.Fatal(err)
	}
	creator, err := st.Apps().Creator(ctx, "creator/app")
	if err != nil || creator != "alice" {
		t.Fatalf("creator = %q err=%v (want alice, sticky)", creator, err)
	}
	if err := st.Apps().UpsertVersion(ctx,
		AppVersion{AppID: "creator/app", Version: "1.0.0", Ref: "creator/app@sha", Tier: TierUnverified, Manifest: `{"id":"creator/app"}`, CreatedAt: 3},
		[]MountDecl{{AppID: "creator/app", Version: "1.0.0", Name: "main", Path: "data", Seed: "seed", Required: true}}); err != nil {
		t.Fatal(err)
	}
	v, err := st.Apps().GetVersion(ctx, "creator/app", "1.0.0")
	if err != nil || v.Manifest != `{"id":"creator/app"}` || v.Tier != TierUnverified {
		t.Fatalf("version = %+v err=%v", v, err)
	}
	mounts, err := st.Apps().DeclaredMounts(ctx, "creator/app", "1.0.0")
	if err != nil || len(mounts) != 1 || mounts[0].Path != "data" || mounts[0].Seed != "seed" {
		t.Fatalf("mounts = %+v err=%v", mounts, err)
	}
}
