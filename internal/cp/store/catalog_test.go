package store

import (
	"context"
	"errors"
	"testing"
)

func TestTierRoundTripAndLatestReviewed(t *testing.T) {
	st := NewTestStore(t)
	ctx := context.Background()
	if err := st.Apps().Upsert(ctx, App{ID: "spawnery/x", DisplayName: "X", Visibility: "public", Listed: true, CreatedAt: 1}); err != nil {
		t.Fatal(err)
	}
	if err := st.Apps().UpsertVersion(ctx,
		AppVersion{AppID: "spawnery/x", Version: "1.0.0", Ref: "r1", Tier: TierReviewed, CreatedAt: 10}, nil); err != nil {
		t.Fatal(err)
	}
	if err := st.Apps().UpsertVersion(ctx,
		AppVersion{AppID: "spawnery/x", Version: "1.1.0", Ref: "r2", Tier: TierUnverified, CreatedAt: 20}, nil); err != nil {
		t.Fatal(err)
	}
	got, err := st.Apps().GetVersion(ctx, "spawnery/x", "1.1.0")
	if err != nil || got.Tier != TierUnverified {
		t.Fatalf("GetVersion tier = %q err=%v (want unverified)", got.Tier, err)
	}
	lr, err := st.Apps().LatestReviewed(ctx, "spawnery/x")
	if err != nil || lr.Version != "1.0.0" {
		t.Fatalf("LatestReviewed = %+v err=%v (want 1.0.0)", lr, err)
	}
}

func seedCatApp(t *testing.T, st Store, id, name, summary, tags string, listed bool, vis string, tier Tier, ver string, created int64) {
	t.Helper()
	ctx := context.Background()
	if err := st.Apps().Upsert(ctx, App{ID: id, DisplayName: name, Summary: summary, Tags: tags, Visibility: vis, Listed: listed, CreatedAt: created}); err != nil {
		t.Fatal(err)
	}
	if err := st.Apps().UpsertVersion(ctx, AppVersion{AppID: id, Version: ver, Ref: "ref-" + ver, Tier: tier, CreatedAt: created}, nil); err != nil {
		t.Fatal(err)
	}
}

func TestCatalogBrowseFilterOrder(t *testing.T) {
	st := NewTestStore(t)
	ctx := context.Background()
	seedCatApp(t, st, "spawnery/wiki", "Wiki", "research notes", "notes,research", true, "public", TierReviewed, "1.0.0", 10)
	seedCatApp(t, st, "spawnery/lang", "Language", "language tutor", "language,tutor", true, "public", TierUnverified, "0.9.0", 20)
	seedCatApp(t, st, "spawnery/hidden", "Hidden", "secret", "x", false, "public", TierReviewed, "1.0.0", 30)
	seedCatApp(t, st, "spawnery/priv", "Priv", "private one", "y", true, "private", TierReviewed, "1.0.0", 40)

	all, err := st.Apps().Catalog(ctx, CatalogFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 2 {
		t.Fatalf("want 2 listed+public, got %d (%+v)", len(all), all)
	}
	if all[0].App.ID != "spawnery/wiki" || all[0].LatestTier != TierReviewed || all[0].LatestVersion != "1.0.0" {
		t.Fatalf("first entry = %+v (want wiki/reviewed/1.0.0 first)", all[0])
	}
	if all[1].App.ID != "spawnery/lang" {
		t.Fatalf("second = %s (want lang)", all[1].App.ID)
	}

	hit, err := st.Apps().Catalog(ctx, CatalogFilter{Query: "RESEARCH"})
	if err != nil || len(hit) != 1 || hit[0].App.ID != "spawnery/wiki" {
		t.Fatalf("query=research -> %+v err=%v (want wiki only)", hit, err)
	}
	miss, err := st.Apps().Catalog(ctx, CatalogFilter{Query: "nonexistent"})
	if err != nil || len(miss) != 0 {
		t.Fatalf("query=nonexistent -> %+v err=%v (want empty)", miss, err)
	}
}

func TestAppDetail(t *testing.T) {
	st := NewTestStore(t)
	ctx := context.Background()
	seedCatApp(t, st, "spawnery/wiki", "Wiki", "notes", "notes", true, "public", TierReviewed, "1.0.0", 10)
	if err := st.Apps().UpsertVersion(ctx, AppVersion{AppID: "spawnery/wiki", Version: "1.1.0", Ref: "ref2", Tier: TierUnverified, CreatedAt: 20}, nil); err != nil {
		t.Fatal(err)
	}
	app, versions, err := st.Apps().AppDetail(ctx, "spawnery/wiki")
	if err != nil || app.DisplayName != "Wiki" {
		t.Fatalf("app=%+v err=%v", app, err)
	}
	if len(versions) != 2 || versions[0].Version != "1.1.0" || versions[1].Version != "1.0.0" {
		t.Fatalf("versions=%+v (want newest-first 1.1.0,1.0.0)", versions)
	}
	if _, _, err := st.Apps().AppDetail(ctx, "nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
	seedCatApp(t, st, "spawnery/hidden", "Hidden", "x", "x", false, "public", TierReviewed, "1.0.0", 5)
	if _, _, err := st.Apps().AppDetail(ctx, "spawnery/hidden"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unlisted want ErrNotFound, got %v", err)
	}
}
