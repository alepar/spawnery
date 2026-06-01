# E5 Slice 1 — Catalog Read Surface (Design)

**Bead:** `sp-0sc` (under E5 `sp-7hl`)
**Status:** Draft v1 — pending user review
**Date:** 2026-06-01
**Parent design:** [E5 Packaging & Catalog](2026-05-28-spawnery-e5-packaging-catalog-design.md) · [Demo MVP Scope](2026-05-28-spawnery-demo-mvp-scope.md) · [E11 App Ideas](2026-05-29-spawnery-e11-app-ideas.md)

This is the **first buildable marketplace slice**: the read side of the catalog. It is **pure
CP** — no node, no GitHub, no E3. It establishes the catalog data model + the browse/detail API +
the demo seed lineup, so the web client has a marketplace to render. Publishing (registration,
manifest validation), version polling, and the scanner are **later slices** (§7).

It mirrors the shape that worked for `sp-pc4`: **contracts → store → CP wiring → seed**.

---

## 1. Scope

**In:**
1. **Contracts** — `ListApps` (browse/search) + `GetApp` (detail) RPCs on `SpawnService` in
   `cp.proto`, with `AppSummary` / `AppVersionSummary` messages and a `TrustTier` enum (E0 §5b).
2. **Store** — extend `App` with catalog metadata; migrate `AppVersion.Reviewed bool` → a 3-value
   `Tier`; add a `Catalog` browse/search query + a `GetApp` detail read; keep `LatestReviewed`
   semantics (now tier-based).
3. **CP** — `catalog.go` handlers mapping store → proto (incl. tier), overriding the embedded
   `UnimplementedSpawnServiceHandler` defaults.
4. **Seed** — replace the single demo app with the **4-app demo lineup** (E11 §5) carrying real
   catalog metadata, all at tier `reviewed`.

**Out (later slices, do not build):** registration / `RegisterApp` · `spawneryapp.yml` JSON-Schema
validation · GitHub fetch + the `github.com`-only SSRF guard · version polling /
`latestKnownTag/Sha` refresh · the E8 §5 scanner (→ `scanned` tier) · human-review queue (→
`reviewed`) · ratings/stars/flags UX · private-app visibility · pagination/faceted search.

---

## 2. Data model changes

### 2.1 `App` — add per-app catalog metadata

```go
type App struct {
	bun.BaseModel `bun:"table:apps,alias:a"`
	ID            string `bun:"id,pk"`           // creator/app handle, e.g. "spawnery/wiki"
	DisplayName   string `bun:"display_name"`
	Summary       string `bun:"summary"`          // NEW: one-line catalog blurb
	Tags          string `bun:"tags"`             // NEW: comma-separated, demo-simple (normalized table = post-MVP)
	Visibility    string `bun:"visibility,notnull"` // NEW: "public" (only value in the demo; "private" = post-MVP)
	Listed        bool   `bun:"listed,notnull"`   // NEW: catalog-visible (false = hidden/taken-down)
	CreatedAt     int64  `bun:"created_at,notnull"`
}
```

Deferred to their owning slices (NOT added now — YAGNI for the read surface): `repo_url`,
`latest_known_tag`, `latest_known_sha` (registration/poll), `stars`, `flags` (ratings/E8).

### 2.2 `AppVersion` — `Reviewed bool` → `Tier`

```go
type Tier string
const (
	TierUnverified Tier = "unverified" // published, structural checks only / scan declined
	TierScanned    Tier = "scanned"    // passed the E8 §5 automated scanner
	TierReviewed   Tier = "reviewed"   // human-reviewed
)

type AppVersion struct {
	bun.BaseModel `bun:"table:app_versions,alias:av"`
	AppID         string `bun:"app_id,pk"`
	Version       string `bun:"version,pk"`
	Ref           string `bun:"ref,notnull"`
	Tier          Tier   `bun:"tier,notnull"` // CHANGED from `Reviewed bool`
	CreatedAt     int64  `bun:"created_at,notnull"`
}
```

### 2.3 Migration `0002` (sqlite + pg trees)

- `apps`: `ALTER TABLE ADD COLUMN summary TEXT NOT NULL DEFAULT ''`, `tags TEXT NOT NULL
  DEFAULT ''`, `visibility TEXT NOT NULL DEFAULT 'public'`, `listed INTEGER/BOOLEAN NOT NULL
  DEFAULT 1/true`. CHECK `visibility IN ('public','private')`.
- `app_versions`: add `tier TEXT NOT NULL DEFAULT 'unverified' CHECK (tier IN
  ('unverified','scanned','reviewed'))`; **backfill** `UPDATE app_versions SET tier='reviewed'
  WHERE reviewed = 1/true`; then **drop** `reviewed`. (modernc.org/sqlite supports
  `ALTER TABLE ... DROP COLUMN`; pg trivially.)
- Down migration reverses (re-add `reviewed`, backfill `reviewed = (tier='reviewed')`, drop new
  columns).

---

## 3. Store repo surface

`AppRepo` gains catalog reads; the existing `App`/`AppVersion` reads carry the new columns
automatically (bun struct mapping).

```go
// CatalogEntry is the browse-row projection: an app + its latest listable version's tier/version.
type CatalogEntry struct {
	App           App
	LatestVersion string // newest version row (by created_at); "" if the app has no version
	LatestTier    Tier
}

type CatalogFilter struct {
	Query string // case-insensitive substring over display_name + summary + tags; "" = all
}

// Catalog returns listed, public apps matching the filter, each with its latest version's tier,
// ordered by tier rank (reviewed > scanned > unverified) then display_name ASC.
Catalog(ctx context.Context, f CatalogFilter) ([]CatalogEntry, error)

// AppDetail returns one app + all its versions (newest first). ErrNotFound if absent or unlisted.
AppDetail(ctx context.Context, id string) (App, []AppVersion, error)
```

- **`LatestReviewed`** stays (used by `CreateSpawn`), reimplemented as `WHERE tier = 'reviewed'`.
  `CreateSpawn` is unchanged in this slice — demo seed apps are all `reviewed`, so spawn behavior
  is identical. (Spawning lower tiers arrives with registration in slice 2.)
- `UpsertVersion` sets `tier` (was `reviewed`).
- **Query mechanics:** `Catalog` does the join in SQL where clean, but an N+1 (list listed apps,
  then per-app latest version) is acceptable for the demo's handful of apps and is simpler to test;
  implementer's choice. Search uses `LOWER(col) LIKE '%'||LOWER(?)||'%'` across the three text
  columns. Tier ordering via a CASE rank.

---

## 4. Contracts (`proto/cp/v1/cp.proto`)

Add to `service SpawnService`:

```proto
rpc ListApps(ListAppsRequest) returns (ListAppsResponse);
rpc GetApp(GetAppRequest) returns (GetAppResponse);
```

```proto
enum TrustTier {
  TRUST_TIER_UNSPECIFIED = 0;
  TRUST_TIER_UNVERIFIED  = 1;
  TRUST_TIER_SCANNED     = 2;
  TRUST_TIER_REVIEWED    = 3;
}

message AppSummary {
  string    id            = 1; // creator/app
  string    display_name  = 2;
  string    summary       = 3;
  repeated string tags    = 4;
  string    latest_version = 5;
  TrustTier latest_tier   = 6;
}

message AppVersionSummary {
  string    version    = 1;
  string    ref        = 2;
  TrustTier tier       = 3;
  int64     created_at = 4;
}

message ListAppsRequest  { string query = 1; }              // empty = browse all
message ListAppsResponse { repeated AppSummary apps = 1; }

message GetAppRequest  { string id = 1; }
message GetAppResponse {
  AppSummary app = 1;                          // app-level metadata + latest tier
  repeated AppVersionSummary versions = 2;     // newest first
}
```

`make gen` (buf) regenerates `cpv1` + `cpv1connect`. Codegen tools on PATH from
`$(go env GOPATH)/bin` (per the dependency directive — never stub around a missing tool).

---

## 5. CP wiring (`internal/cp/catalog.go`)

Two handlers on `*Server`, overriding the embedded `UnimplementedSpawnServiceHandler` (watch the
silent-`Unimplemented` signature hazard — exact `context.Context, *connect.Request[...]` shapes):

- **`ListApps`** — any authenticated caller may browse (catalog is public-visibility; no owner
  scoping). Calls `st.Apps().Catalog`, maps each `CatalogEntry` → `AppSummary` (split `Tags` CSV;
  `tierToProto`).
- **`GetApp`** — `st.Apps().AppDetail(id)`; `ErrNotFound` → `connect.CodeNotFound`. Maps app +
  versions; `versions` newest-first.
- **`tierToProto(store.Tier) cpv1.TrustTier`** helper (and the inverse is unused this slice).

Auth: reuse the existing auth-context extraction (same as `ListSpawns`); require an authenticated
caller but do **not** owner-scope (browsing is not owner-private). Unauthenticated → `Unauthenticated`.

---

## 6. Seed — the demo lineup (`internal/cp/seed.go`, `cmd/cp/main.go`)

Extend `AppSeed` with catalog metadata and seed the **four E11 apps** (all `public`, `listed`,
tier `reviewed`):

| id | display | tags | mounts |
|----|---------|------|--------|
| `spawnery/wiki` | Wiki & Research Companion | notes, research, second-brain | `main` |
| `spawnery/language` | Language-Learning Partner | language, tutor, practice | `main` |
| `spawnery/interview` | Interview Coach (System Design) | interview, coaching, system-design | `main` |
| `spawnery/zork` | Zork | game, demo, smoke-test | `main` |

```go
type AppSeed struct {
	ID, Ref, Version string
	DisplayName, Summary string   // NEW
	Tags    []string              // NEW (joined to CSV on seed)
	Mounts  []string
}
```

`Seed` writes `App{...catalog fields, Visibility:"public", Listed:true}` and
`AppVersion{...Tier: store.TierReviewed}`.

**Compat:** existing `server_test`/`e2e_test` seed their own stores; `cmd/cp/main.go` owns the
production lineup, so changing it is free. If `e2e_test.go` hard-codes the old `secret-app` id,
keep one lineup entry (or the test's own seed) on that id — verify during implementation, don't
break e2e.

---

## 7. Testing

Hermetic, `:memory:` store (`NewTestStore`) + the `newTestServer` pattern:

- **Store:** `Catalog` returns only listed+public, filters by query (case-insensitive over
  name/summary/tags), orders reviewed-first; `AppDetail` returns versions newest-first and
  `ErrNotFound` for unlisted/absent; tier round-trips through `UpsertVersion`/`GetVersion`;
  `LatestReviewed` still returns the newest `reviewed` version; migration backfill (a pre-existing
  `reviewed=true` row → `tier=reviewed`).
- **CP:** `ListApps` (browse all + a query hit/miss), `GetApp` (hit + NotFound), unauthenticated →
  `Unauthenticated`, tier mapping correct, `-race` clean.

---

## 8. Decision log

| # | Decision | Choice |
|---|---|---|
| S1.1 | Catalog data model | Extend `App` (per-app metadata) + per-version `Tier` on `AppVersion`; no separate catalog table |
| S1.2 | Trust tier | Migrate `Reviewed bool` → `Tier` enum (`unverified`/`scanned`/`reviewed`) now; `LatestReviewed` = `tier='reviewed'`; `CreateSpawn` unchanged |
| S1.3 | Catalog unit | List **apps** (one card each) carrying latest version's tier; `GetApp` returns versions |
| S1.4 | Search | Case-insensitive substring over display_name+summary+tags; visibility public-only; order tier-rank then name; no pagination |
| S1.5 | Tags | Comma-separated TEXT (demo); normalized table = post-MVP |
| S1.6 | Auth | Authenticated callers browse; not owner-scoped |
| S1.7 | Seed | The four E11 demo apps, all `reviewed`/`public`/`listed` |
| S1.8 | Deferred columns | `repo_url`, `latest_known_*`, `stars`, `flags` added with their owning slices, not now |
