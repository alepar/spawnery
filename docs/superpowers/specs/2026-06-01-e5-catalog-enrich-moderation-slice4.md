# E5 Slice 4 — Catalog Detail Enrichment + Listing Moderation (Design)

**Bead:** `sp-7hl` (E5) — slice 4
**Status:** Draft v1 — build proceeding (autonomous marketplace track; no blocking decision)
**Date:** 2026-06-01
**Builds on:** slice 1 (`f28eff6`), slice 2 (`8be3550`), slice 3 (`b9ec823`)

## 0. Context

The catalog read surface (slice 1) returns app + version summaries; registration (slice 2) stores
the full submitted manifest as a protojson blob on `app_versions.manifest`. This slice exposes that
richer manifest in the **detail view** and adds **listing moderation** (creator takedown / relist).
Pure-CP, hermetic. No store schema change.

## 1. Scope

**In:**
1. **`GetApp` enrichment** — `GetAppResponse` gains the latest version's parsed `AppManifest` (reuses
   the slice-2 message) so a detail page can show persona/model/tools/agents.
2. **`SetAppListing`** — creator-guarded RPC to set `apps.listed` (takedown → `false`, relist →
   `true`). Unlisted apps drop out of `ListApps`/`GetApp` (slice-1 filters already exclude them).

**Out:** admin/Spawnery-staff moderation (only the creator moderates their own app here) · flag/report
flow · per-version manifest in the response (only the latest is surfaced) · surfacing manifest in the
`ListApps` summary (detail-only).

## 2. Contracts (`proto/cp/v1/cp.proto`)

- `GetAppResponse` gains `AppManifest manifest = 3;` — the latest version's manifest (nil if the app
  has no version or no stored manifest, e.g. seed apps registered directly).
- New RPC + messages:
```proto
rpc SetAppListing(SetAppListingRequest) returns (SetAppListingResponse);

message SetAppListingRequest  { string app_id = 1; bool listed = 2; }
message SetAppListingResponse {}
```

## 3. Store (`internal/cp/store`)

- Add `SetListed(ctx, appID string, listed bool) error` to `AppRepo`: `UPDATE apps SET listed = ?
  WHERE id = ?`; `ErrNotFound` if no row affected. (No schema change — `listed` exists.)
- `AppDetail` already returns `[]AppVersion` carrying `.Manifest`; the handler parses the latest.

## 4. CP handlers

- **`GetApp`** (`catalog.go`): after building `versions`, if `len(versions) > 0` and
  `versions[0].Manifest != ""`, `protojson.Unmarshal` it into a `cpv1.AppManifest` and set
  `resp.Manifest`. A parse error is non-fatal — log and leave `Manifest` nil (the summary/versions
  still return). Seed apps (no stored manifest) → nil manifest, which is fine.
- **`SetAppListing`** (new, `internal/cp/moderation.go`): `auth` owner required; creator-ownership
  guard via `s.st.Apps().Creator(appID)` (`ErrNotFound` → `NotFound`; `creator != owner` →
  `PermissionDenied`); then `s.st.Apps().SetListed(appID, req.Msg.Listed)`. Mirrors the slice-2
  `RegisterAppVersion` ownership pattern.

## 5. Testing (hermetic, `newTestServer`)

- Register an app (slice-2 path) → `GetApp` returns a non-nil `Manifest` with the expected
  `Id`/`Title`/mounts. A seed app (`secret-app`) → nil `Manifest` (no stored blob), summary still ok.
- `SetAppListing(listed=false)` by the creator → the app disappears from `ListApps` and `GetApp` →
  `NotFound`; `SetAppListing(listed=true)` restores it.
- `SetAppListing` by a non-creator → `PermissionDenied`; on a missing app → `NotFound`;
  unauthenticated → `Unauthenticated`.

## 6. Decision log

| # | Decision | Choice |
|---|---|---|
| S4.1 | Enrichment shape | `GetAppResponse.manifest` = latest version's parsed `AppManifest` (reuse slice-2 message) |
| S4.2 | Missing/seed manifest | nil manifest, non-fatal (summary + versions still returned) |
| S4.3 | Moderation authority | creator-only (same `Creator` guard as registration); staff/admin deferred |
| S4.4 | Takedown mechanism | `apps.listed=false` → excluded by existing slice-1 filters; relist restores |
| S4.5 | Schema | none (`listed` column already exists) |
