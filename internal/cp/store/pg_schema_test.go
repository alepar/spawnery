//go:build pgtest

// Postgres schema-soundness test. NOT run by default (build tag `pgtest`) — there is no CI Postgres
// yet (see DAO design §9). Run manually against a throwaway Postgres:
//
//	CP_PG_DSN='postgres://user:pass@localhost:5432/spawnery_test?sslmode=disable' \
//	  go test -tags pgtest ./internal/cp/store/ -run TestPostgresSchemaSoundness -v
//
// Requires the pgx stdlib driver; this file imports it so the build tag pulls it in only here.
package store

import (
	"context"
	"os"
	"testing"

	_ "github.com/jackc/pgx/v5/stdlib"
)

func TestPostgresSchemaSoundness(t *testing.T) {
	dsn := os.Getenv("CP_PG_DSN")
	if dsn == "" {
		t.Skip("set CP_PG_DSN to run the Postgres schema-soundness test")
	}
	st, err := Open(context.Background(), Config{Driver: "postgres", DSN: dsn})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()

	if err := st.Apps().Upsert(ctx, App{ID: "a", Visibility: "public", Listed: true, CreatedAt: 1}); err != nil {
		t.Fatal(err)
	}
	if err := st.Apps().UpsertVersion(ctx,
		AppVersion{AppID: "a", Version: "1", Ref: "r", Tier: TierReviewed, CreatedAt: 2}, nil); err != nil {
		t.Fatal(err)
	}
	v, err := st.Apps().GetVersion(ctx, "a", "1")
	if err != nil || v.Tier != TierReviewed {
		t.Fatalf("tier round-trip: v=%+v err=%v", v, err)
	}
	if err := st.Owners().Upsert(ctx, Owner{ID: "o", Email: "e1", CreatedAt: 1}); err != nil {
		t.Fatal(err)
	}
	if err := st.Owners().Upsert(ctx, Owner{ID: "o", Email: "e2", CreatedAt: 1}); err != nil {
		t.Fatal(err)
	}
	if o, _ := st.Owners().Get(ctx, "o"); o.Email != "e2" {
		t.Fatalf("upsert did not update: %+v", o)
	}
	if err := st.Spawns().Create(ctx, Spawn{
		ID: "sp-name", OwnerID: "o", Name: "Wiki", AppID: "a", AppVersion: "1", AppRef: "r",
		Model: "m", Status: Starting, CreatedAt: 1, LastUsedAt: 1,
	}, nil); err != nil {
		t.Fatalf("create spawn with name: %v", err)
	}
	if got, err := st.Spawns().Get(ctx, "sp-name"); err != nil || got.Name != "Wiki" {
		t.Fatalf("pg name round-trip: got=%q err=%v want %q", got.Name, err, "Wiki")
	}
	bs := st.(*bunStore)
	if _, err := bs.db.NewRaw(
		"INSERT INTO spawns (id, owner_id, app_id, app_version, app_ref, pinned, model, status, recovered, created_at, last_used_at) " +
			"VALUES ('x','o','a','1','r', false, 'm', 'bogus', false, 1, 1)").Exec(ctx); err == nil {
		t.Fatal("status CHECK must reject 'bogus'")
	}
}
