# E5 Slice 5 — Creator "My Apps" Management View (Design)

**Bead:** `sp-7hl` (E5) — slice 5
**Status:** Draft v1 — autonomous marketplace track (no blocking decision)
**Date:** 2026-06-01
**Builds on:** slices 1–4 (`f28eff6`, `8be3550`, `b9ec823`, `1eace92`)

## 0. Context

Slice 4 added creator takedown (`SetAppListing listed=false`), but unlisted apps drop out of the
public `ListApps`/`GetApp` — so a creator currently can't *see* their taken-down apps to manage or
relist them (they can only blind-`SetAppListing` by id). This slice closes that with a creator-scoped
**`ListMyApps`** (includes unlisted), completing the publish → manage → moderate loop. E5 §3 "listing
UX". Pure-CP, no schema change.

## 1. Scope

**In:**
1. `AppSummary` gains `listed` so the creator can see which of their apps are taken down.
2. `ListMyApps` RPC — the authenticated owner's apps where `creator_id == owner`, **including
   unlisted**, each with its latest version's tier + listed state.
3. Store `ListByCreator` (creator-scoped, no `listed`/`public` filter).
4. Populate `AppSummary.listed` in the existing `ListApps`/`GetApp` mappers too (always `true` there,
   since those filter to listed — but set it for correctness/consistency).

**Out:** pagination · editing app metadata via API (re-register a version instead) · transfer of
ownership · ratings.

## 2. Contracts (`proto/cp/v1/cp.proto`)

- `AppSummary` gains `bool listed = 7;`.
- New RPC + messages:
```proto
rpc ListMyApps(ListMyAppsRequest) returns (ListMyAppsResponse);

message ListMyAppsRequest  {}
message ListMyAppsResponse { repeated AppSummary apps = 1; }
```

## 3. Store

Add to `AppRepo`:
```go
// ListByCreator returns all apps a creator owns (any visibility, listed or not), each with its
// latest version's tier/version. For the creator's management view.
ListByCreator(ctx context.Context, creatorID string) ([]CatalogEntry, error)
```
Mirrors `Catalog` but `Where("creator_id = ?", creatorID)` with **no** listed/public filter; order
`display_name ASC`. Reuses the same per-app latest-version lookup + `CatalogEntry` projection.

## 4. CP handler (`catalog.go`)

- **`ListMyApps`**: `auth` owner required (`Unauthenticated` if none); `s.st.Apps().ListByCreator(owner)`;
  map each `CatalogEntry` → `AppSummary` including `Listed: e.App.Listed`.
- Update the `ListApps` + `GetApp` summary builders to also set `Listed` (from `e.App.Listed` /
  `app.Listed`).

## 5. Testing (hermetic, `newTestServer`)

- Register two apps as alice (slice-2 `registerApp` helper), take one down (`SetAppListing` false);
  `ListMyApps` as alice returns **both**, with the taken-down one `Listed=false` and the other
  `Listed=true`.
- Register an app as bob; alice's `ListMyApps` excludes it.
- `ListMyApps` unauthenticated → `Unauthenticated`.
- `ListApps` results carry `Listed=true` (sanity).

## 6. Decision log

| # | Decision | Choice |
|---|---|---|
| S5.1 | Creator view | `ListMyApps` = apps where `creator_id == owner`, includes unlisted |
| S5.2 | Listed visibility | add `AppSummary.listed`; populate in all three mappers |
| S5.3 | Store | `ListByCreator` (creator-scoped, no listed/public filter); no schema change |
| S5.4 | Ownership key | `apps.creator_id` (set at registration, slice 2) |
