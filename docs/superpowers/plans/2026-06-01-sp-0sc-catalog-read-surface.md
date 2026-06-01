# E5 Slice 1 — Catalog Read Surface Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Build the read side of the Spawnery marketplace catalog — browse/search + app-detail API over a CP-side catalog data model, seeded with the demo app lineup.

**Architecture:** Pure CP (no node, no GitHub, no E3), following the merged `sp-pc4` shape: contracts (`cp.proto` → buf codegen) → store (new catalog columns + a `Reviewed bool`→`Tier` enum migration + catalog read queries) → CP handlers (`catalog.go`, overriding the embedded `UnimplementedSpawnServiceHandler`) → seed (the 4-app E11 lineup). Hermetic `:memory:` store tests + `newTestServer` handler tests, `-race` clean.

**Tech Stack:** Go, ConnectRPC, buf (`make gen`), uptrace/bun over modernc.org/sqlite + pgx, goose migrations (sqlite + pg dialect trees).

**Source spec:** `docs/superpowers/specs/2026-06-01-e5-catalog-read-surface-slice1.md`

**Conventions (read before starting):**
- Commits use `git commit --no-verify` (the `.beads` export hook dirties commits otherwise).
- Codegen tools (`buf`, `protoc-gen-go`, `protoc-gen-connect-go`) live at `$(go env GOPATH)/bin` — ensure it's on `PATH` (`export PATH="$PATH:$(go env GOPATH)/bin"`). If a tool is missing, install the pinned version (`buf@v1.45.0`, `protoc-gen-go@v1.34.2`, `protoc-gen-connect-go@v1.16.2`) — **never stub around a missing tool.**
- Bead: `sp-0sc` (already claimed).
- Branch off `master`: `git checkout -b sp-0sc-catalog-read`.

---

## File Structure

| File | Responsibility | Action |
|------|----------------|--------|
| `proto/cp/v1/cp.proto` | `ListApps`/`GetApp` RPCs, `TrustTier` enum, `AppSummary`/`AppVersionSummary`, req/resp | Modify |
| `gen/cp/v1/*` | Generated Go (via `make gen`) | Regenerated |
| `internal/cp/store/types.go` | `App` catalog fields, `Tier` type, `AppVersion.Tier` | Modify |
| `internal/cp/store/migrations/sqlite/0002_catalog.sql` | sqlite migration | Create |
| `internal/cp/store/migrations/pg/0002_catalog.sql` | pg migration | Create |
| `internal/cp/store/apps.go` | Upsert/UpsertVersion/LatestReviewed for new cols; `Catalog`/`AppDetail` | Modify |
| `internal/cp/store/store.go` | `AppRepo` interface: add `Catalog`/`AppDetail`; `CatalogEntry`/`CatalogFilter` | Modify |
| `internal/cp/store/catalog_test.go` | Store catalog read tests | Create |
| `internal/cp/store/owners_apps_test.go` | Update `Reviewed`→`Tier` literals | Modify |
| `internal/cp/store/spawns_create_test.go`, `pg_schema_test.go` | Update `Reviewed`→`Tier` literals | Modify |
| `internal/cp/catalog.go` | `ListApps`/`GetApp` handlers + `tierToProto` | Create |
| `internal/cp/catalog_test.go` | Handler tests | Create |
| `internal/cp/seed.go` | `AppSeed` catalog fields; `Seed` writes catalog cols + `Tier` | Modify |
| `internal/cp/seed_test.go` | Seed catalog-metadata test | Modify |
| `cmd/cp/main.go` | Seed the 4-app lineup | Modify |

---

## Task 1: Contracts — catalog API in `cp.proto`

**Files:**
- Modify: `proto/cp/v1/cp.proto`
- Regenerated: `gen/cp/v1/cp.pb.go`, `gen/cp/v1/cpv1connect/cp.connect.go`

- [ ] **Step 1: Add the two RPCs to `service SpawnService`**

In `proto/cp/v1/cp.proto`, inside `service SpawnService { ... }`, add after the `ListSpawns` line:

```proto
  rpc ListApps(ListAppsRequest) returns (ListAppsResponse);
  rpc GetApp(GetAppRequest) returns (GetAppResponse);
```

- [ ] **Step 2: Append the catalog messages + enum at the end of the file**

Append to `proto/cp/v1/cp.proto`:

```proto
// Marketplace trust tier of an App version (E5 §5). Runtime restrictions scale with it.
enum TrustTier {
  TRUST_TIER_UNSPECIFIED = 0;
  TRUST_TIER_UNVERIFIED  = 1; // published, structural checks only / scan declined
  TRUST_TIER_SCANNED     = 2; // passed the automated scanner (E8 §5)
  TRUST_TIER_REVIEWED    = 3; // human-reviewed
}

message AppSummary {
  string id             = 1; // creator/app handle
  string display_name   = 2;
  string summary        = 3;
  repeated string tags  = 4;
  string latest_version = 5; // newest version; "" if none
  TrustTier latest_tier = 6;
}

message AppVersionSummary {
  string version    = 1;
  string ref        = 2;
  TrustTier tier    = 3;
  int64 created_at  = 4;
}

message ListAppsRequest  { string query = 1; } // empty = browse all
message ListAppsResponse { repeated AppSummary apps = 1; }

message GetAppRequest  { string id = 1; }
message GetAppResponse {
  AppSummary app = 1;                      // app metadata + latest tier
  repeated AppVersionSummary versions = 2; // newest first
}
```

- [ ] **Step 3: Regenerate**

Run: `export PATH="$PATH:$(go env GOPATH)/bin" && make gen`
Expected: regenerates `gen/cp/v1/*`; no errors. If `make gen` fails on a missing tool, install the pinned version (see Conventions) and re-run — do not work around it.

- [ ] **Step 4: Verify the generated symbols exist and the tree builds**

Run: `go build ./... && grep -c "ListApps\|GetApp\|AppSummary\|TrustTier_TRUST_TIER_REVIEWED" gen/cp/v1/cp.pb.go gen/cp/v1/cpv1connect/cp.connect.go`
Expected: builds clean; the grep finds the new symbols in both files. (The embedded `UnimplementedSpawnServiceHandler` now carries default `ListApps`/`GetApp` — that is expected; we override them in Task 4.)

- [ ] **Step 5: Commit**

```bash
git add proto/cp/v1/cp.proto gen/cp/v1
git commit --no-verify -m "feat(cp): ListApps/GetApp catalog contracts + TrustTier enum (sp-0sc)"
```

---

## Task 2: Store data model + trust-tier migration

**Files:**
- Modify: `internal/cp/store/types.go`
- Create: `internal/cp/store/migrations/sqlite/0002_catalog.sql`, `internal/cp/store/migrations/pg/0002_catalog.sql`
- Modify: `internal/cp/store/apps.go`
- Modify (compile-fix literals): `internal/cp/store/owners_apps_test.go`, `internal/cp/store/spawns_create_test.go`, `internal/cp/store/pg_schema_test.go`, `internal/cp/seed.go`
- Test: `internal/cp/store/catalog_test.go` (migration/tier portion)

> **This task is consistency-critical (a schema migration with backfill + an index that references the dropped column). Use a capable model.**

- [ ] **Step 1: Write the failing test for the tier model + migration backfill**

Create `internal/cp/store/catalog_test.go`:

```go
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
	// reviewed (older) + unverified (newer): LatestReviewed must return the reviewed one.
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
```

- [ ] **Step 2: Run it to confirm it fails to compile**

Run: `go test ./internal/cp/store/ -run TestTierRoundTrip 2>&1 | head`
Expected: compile failure — `App` has no field `Visibility`/`Listed`, `AppVersion` has no `Tier`, no `TierReviewed`/`TierUnverified`.

- [ ] **Step 3: Update `types.go` — `App` catalog fields, `Tier`, `AppVersion.Tier`**

In `internal/cp/store/types.go`, replace the `App` and `AppVersion` structs and add the `Tier` type:

```go
type Tier string // marketplace trust tier (E5 §5)
const (
	TierUnverified Tier = "unverified"
	TierScanned    Tier = "scanned"
	TierReviewed   Tier = "reviewed"
)

type App struct {
	bun.BaseModel `bun:"table:apps,alias:a"`
	ID            string `bun:"id,pk"`
	DisplayName   string `bun:"display_name"`
	Summary       string `bun:"summary,notnull"`
	Tags          string `bun:"tags,notnull"`       // comma-separated (demo)
	Visibility    string `bun:"visibility,notnull"` // "public" | "private"
	Listed        bool   `bun:"listed,notnull"`
	CreatedAt     int64  `bun:"created_at,notnull"`
}

type AppVersion struct {
	bun.BaseModel `bun:"table:app_versions,alias:av"`
	AppID         string `bun:"app_id,pk"`
	Version       string `bun:"version,pk"`
	Ref           string `bun:"ref,notnull"`
	Tier          Tier   `bun:"tier,notnull"`
	CreatedAt     int64  `bun:"created_at,notnull"`
}
```

- [ ] **Step 4: Write the sqlite migration**

Create `internal/cp/store/migrations/sqlite/0002_catalog.sql`:

```sql
-- +goose Up
ALTER TABLE apps ADD COLUMN summary    TEXT    NOT NULL DEFAULT '';
ALTER TABLE apps ADD COLUMN tags       TEXT    NOT NULL DEFAULT '';
ALTER TABLE apps ADD COLUMN visibility TEXT    NOT NULL DEFAULT 'public' CHECK (visibility IN ('public','private'));
ALTER TABLE apps ADD COLUMN listed     INTEGER NOT NULL DEFAULT 1;

-- index references the column we're about to drop; drop it first.
DROP INDEX idx_app_versions_reviewed;
ALTER TABLE app_versions ADD COLUMN tier TEXT NOT NULL DEFAULT 'unverified' CHECK (tier IN ('unverified','scanned','reviewed'));
UPDATE app_versions SET tier = 'reviewed' WHERE reviewed = 1;
ALTER TABLE app_versions DROP COLUMN reviewed;
CREATE INDEX idx_app_versions_tier ON app_versions(app_id, tier, created_at DESC);

-- +goose Down
DROP INDEX idx_app_versions_tier;
ALTER TABLE app_versions ADD COLUMN reviewed INTEGER NOT NULL DEFAULT 0;
UPDATE app_versions SET reviewed = 1 WHERE tier = 'reviewed';
ALTER TABLE app_versions DROP COLUMN tier;
CREATE INDEX idx_app_versions_reviewed ON app_versions(app_id, reviewed, created_at DESC);
ALTER TABLE apps DROP COLUMN listed;
ALTER TABLE apps DROP COLUMN visibility;
ALTER TABLE apps DROP COLUMN tags;
ALTER TABLE apps DROP COLUMN summary;
```

- [ ] **Step 5: Write the pg migration**

Create `internal/cp/store/migrations/pg/0002_catalog.sql`:

```sql
-- +goose Up
ALTER TABLE apps ADD COLUMN summary    TEXT    NOT NULL DEFAULT '';
ALTER TABLE apps ADD COLUMN tags       TEXT    NOT NULL DEFAULT '';
ALTER TABLE apps ADD COLUMN visibility TEXT    NOT NULL DEFAULT 'public' CHECK (visibility IN ('public','private'));
ALTER TABLE apps ADD COLUMN listed     BOOLEAN NOT NULL DEFAULT TRUE;

DROP INDEX idx_app_versions_reviewed;
ALTER TABLE app_versions ADD COLUMN tier TEXT NOT NULL DEFAULT 'unverified' CHECK (tier IN ('unverified','scanned','reviewed'));
UPDATE app_versions SET tier = 'reviewed' WHERE reviewed = TRUE;
ALTER TABLE app_versions DROP COLUMN reviewed;
CREATE INDEX idx_app_versions_tier ON app_versions(app_id, tier, created_at DESC);

-- +goose Down
DROP INDEX idx_app_versions_tier;
ALTER TABLE app_versions ADD COLUMN reviewed BOOLEAN NOT NULL DEFAULT FALSE;
UPDATE app_versions SET reviewed = TRUE WHERE tier = 'reviewed';
ALTER TABLE app_versions DROP COLUMN tier;
CREATE INDEX idx_app_versions_reviewed ON app_versions(app_id, reviewed, created_at DESC);
ALTER TABLE apps DROP COLUMN listed;
ALTER TABLE apps DROP COLUMN visibility;
ALTER TABLE apps DROP COLUMN tags;
ALTER TABLE apps DROP COLUMN summary;
```

- [ ] **Step 6: Update `apps.go` for the new columns**

In `internal/cp/store/apps.go`, update `Upsert`, `UpsertVersion`, and `LatestReviewed`:

```go
func (r *appRepo) Upsert(ctx context.Context, a App) error {
	_, err := r.db.NewInsert().Model(&a).
		On("CONFLICT (id) DO UPDATE").
		Set("display_name = EXCLUDED.display_name").
		Set("summary = EXCLUDED.summary").
		Set("tags = EXCLUDED.tags").
		Set("visibility = EXCLUDED.visibility").
		Set("listed = EXCLUDED.listed").
		Exec(ctx)
	return err
}
```

```go
func (r *appRepo) UpsertVersion(ctx context.Context, v AppVersion, mounts []MountDecl) error {
	if _, err := r.db.NewInsert().Model(&v).
		On("CONFLICT (app_id, version) DO UPDATE").
		Set("ref = EXCLUDED.ref").Set("tier = EXCLUDED.tier").
		Exec(ctx); err != nil {
		return err
	}
	for i := range mounts {
		if _, err := r.db.NewInsert().Model(&mounts[i]).
			On("CONFLICT (app_id, version, name) DO UPDATE").
			Set("required = EXCLUDED.required").
			Exec(ctx); err != nil {
			return err
		}
	}
	return nil
}
```

```go
func (r *appRepo) LatestReviewed(ctx context.Context, appID string) (AppVersion, error) {
	var v AppVersion
	err := r.db.NewSelect().Model(&v).
		Where("app_id = ? AND tier = ?", appID, TierReviewed).
		Order("created_at DESC").Limit(1).Scan(ctx)
	if errors.Is(err, sql.ErrNoRows) {
		return AppVersion{}, ErrNotFound
	}
	return v, err
}
```

- [ ] **Step 7: Fix the broken literals in existing tests + `seed.go`**

In `internal/cp/store/owners_apps_test.go`: change the two `AppVersion{... Reviewed: true ...}` / `Reviewed: false` literals to `Tier: TierReviewed` / `Tier: TierUnverified`, and the `App{ID: "spawnery/secret", DisplayName: "Secret", CreatedAt: 1}` / `App{ID: "noreview", CreatedAt: 1}` literals add `Visibility: "public", Listed: true`.

In `internal/cp/store/spawns_create_test.go`: the `AppVersion{... Reviewed: true ...}` → `Tier: TierReviewed`; the `App{ID: "spawnery/secret", CreatedAt: 1}` → add `Visibility: "public", Listed: true`.

In `internal/cp/store/pg_schema_test.go`: `App{ID: "a", CreatedAt: 1}` → add `Visibility: "public", Listed: true`; `AppVersion{... Reviewed: true ...}` → `Tier: TierReviewed`.

In `internal/cp/seed.go`: the `store.AppVersion{... Reviewed: true ...}` literal → `Tier: store.TierReviewed`, and the `store.App{ID: a.ID, DisplayName: a.ID, CreatedAt: now}` → add `Visibility: "public", Listed: true` (keep the single secret-app; the full lineup is Task 5).

- [ ] **Step 8: Run the store tests + the whole-tree build**

Run: `go build ./... && go test ./internal/cp/store/ -run 'TestTierRoundTrip|TestAppVersionsAndDeclaredMounts|TestOwner'`
Expected: PASS. (`go build ./...` confirms `seed.go` + server compile against the new types.)

- [ ] **Step 9: Commit**

```bash
git add internal/cp/store cmd internal/cp/seed.go
git commit --no-verify -m "feat(store): App catalog columns + AppVersion trust-tier migration (sp-0sc)"
```

---

## Task 3: Store catalog read queries — `Catalog` + `AppDetail`

**Files:**
- Modify: `internal/cp/store/store.go` (interface + `CatalogEntry`/`CatalogFilter`)
- Modify: `internal/cp/store/apps.go` (implementations)
- Test: `internal/cp/store/catalog_test.go`

- [ ] **Step 1: Write the failing tests**

Append to `internal/cp/store/catalog_test.go`:

```go
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

	// Browse all: only listed + public; reviewed-first then display_name.
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

	// Query matches summary/tags case-insensitively.
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
	// unlisted apps are not retrievable via the catalog detail.
	seedCatApp(t, st, "spawnery/hidden", "Hidden", "x", "x", false, "public", TierReviewed, "1.0.0", 5)
	if _, _, err := st.Apps().AppDetail(ctx, "spawnery/hidden"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unlisted want ErrNotFound, got %v", err)
	}
}
```

Add `"errors"` to the `catalog_test.go` imports.

- [ ] **Step 2: Run to confirm failure**

Run: `go test ./internal/cp/store/ -run 'TestCatalog|TestAppDetail' 2>&1 | head`
Expected: compile failure — no `CatalogFilter`, `CatalogEntry`, `Catalog`, or `AppDetail`.

- [ ] **Step 3: Add the types + interface methods**

In `internal/cp/store/store.go`, add above the `AppRepo` interface:

```go
// CatalogEntry is one browse row: an app plus its newest version's tier/version.
type CatalogEntry struct {
	App           App
	LatestVersion string
	LatestTier    Tier
}

// CatalogFilter narrows a catalog browse. Query is a case-insensitive substring over
// display_name + summary + tags; empty Query browses all listed+public apps.
type CatalogFilter struct {
	Query string
}
```

In the `AppRepo` interface, add:

```go
	Catalog(ctx context.Context, f CatalogFilter) ([]CatalogEntry, error)
	AppDetail(ctx context.Context, id string) (App, []AppVersion, error)
```

- [ ] **Step 4: Implement `Catalog` + `AppDetail` in `apps.go`**

Append to `internal/cp/store/apps.go` (the N+1 here is fine for demo-scale and keeps the SQL simple and dialect-portable):

```go
func (r *appRepo) Catalog(ctx context.Context, f CatalogFilter) ([]CatalogEntry, error) {
	var apps []App
	q := r.db.NewSelect().Model(&apps).
		Where("listed = ?", true).Where("visibility = ?", "public")
	if f.Query != "" {
		like := "%" + strings.ToLower(f.Query) + "%"
		q = q.Where("(LOWER(display_name) LIKE ? OR LOWER(summary) LIKE ? OR LOWER(tags) LIKE ?)", like, like, like)
	}
	if err := q.Order("display_name ASC").Scan(ctx); err != nil {
		return nil, err
	}
	out := make([]CatalogEntry, 0, len(apps))
	for _, a := range apps {
		var v AppVersion
		err := r.db.NewSelect().Model(&v).
			Where("app_id = ?", a.ID).Order("created_at DESC").Limit(1).Scan(ctx)
		e := CatalogEntry{App: a}
		if err == nil {
			e.LatestVersion, e.LatestTier = v.Version, v.Tier
		} else if !errors.Is(err, sql.ErrNoRows) {
			return nil, err
		}
		out = append(out, e)
	}
	// reviewed > scanned > unverified > (no version), then display_name (already sorted).
	sort.SliceStable(out, func(i, j int) bool {
		return tierRank(out[i].LatestTier) > tierRank(out[j].LatestTier)
	})
	return out, nil
}

func tierRank(t Tier) int {
	switch t {
	case TierReviewed:
		return 3
	case TierScanned:
		return 2
	case TierUnverified:
		return 1
	default:
		return 0
	}
}

func (r *appRepo) AppDetail(ctx context.Context, id string) (App, []AppVersion, error) {
	var a App
	err := r.db.NewSelect().Model(&a).Where("id = ? AND listed = ?", id, true).Scan(ctx)
	if errors.Is(err, sql.ErrNoRows) {
		return App{}, nil, ErrNotFound
	}
	if err != nil {
		return App{}, nil, err
	}
	var versions []AppVersion
	if err := r.db.NewSelect().Model(&versions).
		Where("app_id = ?", id).Order("created_at DESC").Scan(ctx); err != nil {
		return App{}, nil, err
	}
	return a, versions, nil
}
```

Add `"sort"` and `"strings"` to the `apps.go` imports.

- [ ] **Step 5: Run the tests**

Run: `go test ./internal/cp/store/ -run 'TestCatalog|TestAppDetail'`
Expected: PASS.

- [ ] **Step 6: Full store suite + race**

Run: `go test ./internal/cp/store/ -race`
Expected: PASS (all store tests, race-clean).

- [ ] **Step 7: Commit**

```bash
git add internal/cp/store
git commit --no-verify -m "feat(store): Catalog browse/search + AppDetail reads (sp-0sc)"
```

---

## Task 4: CP handlers — `catalog.go`

**Files:**
- Create: `internal/cp/catalog.go`
- Test: `internal/cp/catalog_test.go`

- [ ] **Step 1: Write the failing handler tests**

Create `internal/cp/catalog_test.go`:

```go
package cp

import (
	"context"
	"testing"

	"connectrpc.com/connect"

	cpv1 "spawnery/gen/cp/v1"
	"spawnery/internal/cp/auth"
	"spawnery/internal/cp/store"
)

// seed two extra catalog apps into the test server's store for browsing.
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
	// seeded "secret-app" (from newTestServer) + "spawnery/wiki".
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
	// search
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
```

> Auth helpers (confirmed in `internal/cp/auth/auth.go`): `auth.WithOwner(ctx, owner)` injects an owner; `auth.OwnerFromContext(ctx)` reads it (already used in `lifecycle.go`/`server_test.go:142`).

- [ ] **Step 2: Run to confirm failure**

Run: `go test ./internal/cp/ -run 'TestListApps|TestGetApp' 2>&1 | head`
Expected: compile failure — `*Server` has no `ListApps`/`GetApp` (the embedded `Unimplemented` provides them but with `connect.CodeUnimplemented`, and the calls won't match our intended behavior).

- [ ] **Step 3: Implement `catalog.go`**

Create `internal/cp/catalog.go`:

```go
package cp

import (
	"errors"
	"fmt"
	"strings"

	"connectrpc.com/connect"

	cpv1 "spawnery/gen/cp/v1"
	"spawnery/internal/cp/auth"
	"spawnery/internal/cp/store"
)

func tierToProto(t store.Tier) cpv1.TrustTier {
	switch t {
	case store.TierUnverified:
		return cpv1.TrustTier_TRUST_TIER_UNVERIFIED
	case store.TierScanned:
		return cpv1.TrustTier_TRUST_TIER_SCANNED
	case store.TierReviewed:
		return cpv1.TrustTier_TRUST_TIER_REVIEWED
	default:
		return cpv1.TrustTier_TRUST_TIER_UNSPECIFIED
	}
}

func splitTags(csv string) []string {
	if csv == "" {
		return nil
	}
	parts := strings.Split(csv, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// ListApps returns the public, listed catalog (optionally filtered by query). Browsing requires an
// authenticated caller but is NOT owner-scoped — the catalog is public.
func (s *Server) ListApps(ctx context.Context, req *connect.Request[cpv1.ListAppsRequest]) (*connect.Response[cpv1.ListAppsResponse], error) {
	if _, ok := auth.OwnerFromContext(ctx); !ok {
		return nil, connect.NewError(connect.CodeUnauthenticated, fmt.Errorf("no owner"))
	}
	entries, err := s.st.Apps().Catalog(ctx, store.CatalogFilter{Query: req.Msg.Query})
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	out := make([]*cpv1.AppSummary, len(entries))
	for i, e := range entries {
		out[i] = &cpv1.AppSummary{
			Id: e.App.ID, DisplayName: e.App.DisplayName, Summary: e.App.Summary,
			Tags: splitTags(e.App.Tags), LatestVersion: e.LatestVersion, LatestTier: tierToProto(e.LatestTier),
		}
	}
	return connect.NewResponse(&cpv1.ListAppsResponse{Apps: out}), nil
}

// GetApp returns one catalog app's metadata + its versions (newest first).
func (s *Server) GetApp(ctx context.Context, req *connect.Request[cpv1.GetAppRequest]) (*connect.Response[cpv1.GetAppResponse], error) {
	if _, ok := auth.OwnerFromContext(ctx); !ok {
		return nil, connect.NewError(connect.CodeUnauthenticated, fmt.Errorf("no owner"))
	}
	app, versions, err := s.st.Apps().AppDetail(ctx, req.Msg.Id)
	if errors.Is(err, store.ErrNotFound) {
		return nil, connect.NewError(connect.CodeNotFound, err)
	}
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	summary := &cpv1.AppSummary{
		Id: app.ID, DisplayName: app.DisplayName, Summary: app.Summary, Tags: splitTags(app.Tags),
	}
	vout := make([]*cpv1.AppVersionSummary, len(versions))
	for i, v := range versions {
		vout[i] = &cpv1.AppVersionSummary{Version: v.Version, Ref: v.Ref, Tier: tierToProto(v.Tier), CreatedAt: v.CreatedAt}
		if i == 0 {
			summary.LatestVersion, summary.LatestTier = v.Version, tierToProto(v.Tier)
		}
	}
	return connect.NewResponse(&cpv1.GetAppResponse{App: summary, Versions: vout}), nil
}
```

- [ ] **Step 4: Run the handler tests**

Run: `go test ./internal/cp/ -run 'TestListApps|TestGetApp'`
Expected: PASS. (If the auth injector name differed, the subagent already adjusted `authCtx()` in Step 1.)

- [ ] **Step 5: Full CP package + race**

Run: `go test ./internal/cp/ -race`
Expected: PASS, race-clean.

- [ ] **Step 6: Commit**

```bash
git add internal/cp/catalog.go internal/cp/catalog_test.go
git commit --no-verify -m "feat(cp): ListApps/GetApp handlers + tier mapping (sp-0sc)"
```

---

## Task 5: Seed the demo app lineup

**Files:**
- Modify: `internal/cp/seed.go` (extend `AppSeed`; write catalog cols + tier)
- Modify: `internal/cp/seed_test.go`
- Modify: `cmd/cp/main.go` (seed the 4 apps)

- [ ] **Step 1: Write the failing seed test**

In `internal/cp/seed_test.go`, add (keep existing tests):

```go
func TestSeedWritesCatalogMetadata(t *testing.T) {
	st := store.NewTestStore(t)
	ctx := context.Background()
	apps := []AppSeed{{
		ID: "spawnery/wiki", Ref: "examples/wiki", Version: "1.0.0",
		DisplayName: "Wiki & Research Companion", Summary: "capture, connect, recall",
		Tags: []string{"notes", "research"}, Mounts: []string{"main"},
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
```

Ensure `seed_test.go` imports `"spawnery/internal/cp/store"` and `"context"`.

- [ ] **Step 2: Run to confirm failure**

Run: `go test ./internal/cp/ -run TestSeedWritesCatalogMetadata 2>&1 | head`
Expected: compile failure — `AppSeed` has no `DisplayName`/`Summary`/`Tags`.

- [ ] **Step 3: Extend `AppSeed` + `Seed` in `seed.go`**

In `internal/cp/seed.go`, update the struct and the seed loop:

```go
type AppSeed struct {
	ID          string   // public app id (e.g. "spawnery/wiki")
	Ref         string   // definition ref the node mounts
	Version     string   // seeded version
	DisplayName string   // catalog title
	Summary     string   // one-line catalog blurb
	Tags        []string // catalog tags
	Mounts      []string // declared mount names
}
```

In the `for _, a := range apps` loop, replace the `Upsert`/`UpsertVersion` calls:

```go
		display := a.DisplayName
		if display == "" {
			display = a.ID
		}
		if err := st.Apps().Upsert(ctx, store.App{
			ID: a.ID, DisplayName: display, Summary: a.Summary,
			Tags: strings.Join(a.Tags, ","), Visibility: "public", Listed: true, CreatedAt: now,
		}); err != nil {
			return err
		}
		decls := make([]store.MountDecl, len(a.Mounts))
		for i, name := range a.Mounts {
			decls[i] = store.MountDecl{AppID: a.ID, Version: a.Version, Name: name, Required: true}
		}
		if err := st.Apps().UpsertVersion(ctx,
			store.AppVersion{AppID: a.ID, Version: a.Version, Ref: a.Ref, Tier: store.TierReviewed, CreatedAt: now},
			decls); err != nil {
			return err
		}
```

Add `"strings"` to the `seed.go` imports.

- [ ] **Step 4: Run the seed test + full CP package**

Run: `go test ./internal/cp/ -run TestSeed && go test ./internal/cp/`
Expected: PASS (existing `server_test`/`e2e`-tag-excluded tests still pass — their `AppSeed` literals omit the new fields, which zero-value cleanly).

- [ ] **Step 5: Seed the 4-app lineup in `cmd/cp/main.go`**

In `cmd/cp/main.go`, replace the `seedApps := []cp.AppSeed{...}` line with the E11 lineup:

```go
	seedApps := []cp.AppSeed{
		{ID: "spawnery/wiki", Ref: "examples/wiki", Version: "1.0.0",
			DisplayName: "Wiki & Research Companion", Summary: "Capture articles, links, and notes; it extracts, connects, and recalls.",
			Tags: []string{"notes", "research", "second-brain"}, Mounts: []string{"main"}},
		{ID: "spawnery/language", Ref: "examples/language", Version: "1.0.0",
			DisplayName: "Language-Learning Partner", Summary: "Tracks your vocab and mistakes; drills, converses, and adapts.",
			Tags: []string{"language", "tutor", "practice"}, Mounts: []string{"main"}},
		{ID: "spawnery/interview", Ref: "examples/interview", Version: "1.0.0",
			DisplayName: "Interview Coach (System Design)", Summary: "Mock interviews with structured, scored feedback over time.",
			Tags: []string{"interview", "coaching", "system-design"}, Mounts: []string{"main"}},
		{ID: "spawnery/zork", Ref: "examples/secret-app", Version: "1.0.0",
			DisplayName: "Zork", Summary: "The classic adventure — vertical-slice smoke test and toy.",
			Tags: []string{"game", "demo", "smoke-test"}, Mounts: []string{"main"}},
	}
```

> **Confirmed:** `examples/` contains only `secret-app` today. So point **all four** `Ref`s at `examples/secret-app` for now (the catalog lists/details fine regardless of ref; the real spawn definitions are a later slice). Use `Ref: "examples/secret-app"` on every entry and add a `// TODO(sp-7hl): real definition repos per app` comment above the slice. The `DisplayName`/`Summary`/`Tags`/`ID` stay per-app as shown.

- [ ] **Step 6: Build + vet the whole tree**

Run: `go build ./... && go vet ./...`
Expected: clean.

- [ ] **Step 7: Commit**

```bash
git add internal/cp/seed.go internal/cp/seed_test.go cmd/cp/main.go
git commit --no-verify -m "feat(cp): seed the demo app catalog lineup (sp-0sc)"
```

---

## Final Verification (before finishing the branch)

- [ ] `export PATH="$PATH:$(go env GOPATH)/bin" && make gen` — generated code is current (no diff).
- [ ] `go build ./...` — clean.
- [ ] `go build -tags e2e ./...` — e2e still compiles.
- [ ] `go vet ./...` — clean.
- [ ] `go test ./...` — full hermetic suite passes.
- [ ] `go test ./internal/cp/ ./internal/cp/store/ -race` — race-clean.

Then use **superpowers:finishing-a-development-branch** (Option 1: merge to master locally; no remote).

---

## Self-Review Notes

- **Spec coverage:** §2.1 App fields → T2; §2.2 tier → T2; §2.3 migration → T2 (incl. the index-drop ordering + backfill + down); §3 store methods → T2 (`LatestReviewed`) + T3 (`Catalog`/`AppDetail`); §4 contracts → T1; §5 CP handlers + auth-not-owner-scoped → T4; §6 seed lineup → T5; §7 testing → tests in every task. Out-of-scope items (registration, manifest, poll, scanner) are not in any task. ✓
- **Type consistency:** `Tier`/`TierReviewed`/`TierUnverified`/`TierScanned`, `CatalogEntry{App,LatestVersion,LatestTier}`, `CatalogFilter{Query}`, `Catalog`/`AppDetail`, `tierToProto`, `splitTags`, `cpv1.TrustTier_TRUST_TIER_*`, `AppSummary`/`AppVersionSummary` used identically across T1–T5. ✓
- **Compile-per-commit:** T2 fixes all `Reviewed`→`Tier` literal breakage (tests + `seed.go`) in the same commit; T5 only adds fields. ✓
