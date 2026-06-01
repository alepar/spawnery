package store

import (
	"context"
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
