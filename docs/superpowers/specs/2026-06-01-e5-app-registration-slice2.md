# E5 Slice 2 — App-Version Registration (API as Source of Truth) (Design)

**Bead:** `sp-7hl` (E5) — slice 2
**Status:** Draft v1 — pending user review (build proceeding per "keep going" unless flagged)
**Date:** 2026-06-01
**Builds on:** [E5 slice 1](2026-06-01-e5-catalog-read-surface-slice1.md) (merged `f28eff6`) · [E5 design](2026-05-28-spawnery-e5-packaging-catalog-design.md) · [E0 §3 manifest](2026-05-26-spawnery-e0-contracts-design.md) · [data-mounts](2026-05-29-data-mounts-design.md)

## 0. The shift (user decision, 2026-06-01)

**The API is the source of truth for registration**, not a CP-fetched `spawneryapp.yml`. CP exposes
`RegisterAppVersion`, which takes the **full app-version manifest as structured proto input** and
persists it. CP never fetches or parses YAML, so there is **no GitHub-fetch / SSRF / repo-auth
surface** in this slice. `spawneryapp.yml` still lives in the definition repo as the artifact a
**GitHub Action (or equivalent CI) reads to call `RegisterAppVersion`** — the YAML→API translation
is client/CI-side. This slice ships a thin `spawnctl register` reference client to prove that path
and give end-to-end coverage.

Freshly registered versions enter at tier **`unverified`** (the scanner, `sp-5a9`, promotes to
`scanned` later). This exercises the multi-tier catalog built in slice 1 — the catalog will now show
unverified third-party apps alongside the seeded `reviewed` first-party lineup.

## 1. Scope

**In:**
1. **Contracts** — `AppManifest` proto message (mirrors E0 §3) + `RegisterAppVersion` RPC.
2. **Store** — persist registration: `apps.creator_id`; `app_versions.manifest` (full submitted
   manifest JSON blob, for fidelity); `app_version_mounts.path` + `.seed`; an `UpsertVersion` that
   takes the new fields. Migration `0003`.
3. **Validation** — structural checks on the submitted manifest (E5 §5), CP-side, pure function.
4. **CP** — `RegisterAppVersion` handler: auth (owner = creator), validate, creator-ownership
   guard, `WithTx` upsert of App (catalog fields from manifest) + AppVersion (`unverified` + blob) +
   declared mounts.
5. **`spawnctl register`** — reference CI client: parse a local `spawneryapp.yml` (full schema) →
   `AppManifest` proto → call `RegisterAppVersion`. Doubles as the e2e.

**Out (later slices):** GitHub fetch / webhook / polling (slice 3: version resolution) · the scanner
(`sp-5a9`, E8) · human review queue → `reviewed` · private-app visibility + review · permission
escalation guards · auto-upgrade / pinning · icon storage · ratings/flags.

## 2. Contracts (`proto/cp/v1/cp.proto`)

```proto
rpc RegisterAppVersion(RegisterAppVersionRequest) returns (RegisterAppVersionResponse);
```

```proto
// AppManifest is the structured app-version manifest — the registration source of truth
// (mirrors the spawneryapp.yml schema, E0 §3). CI maps YAML -> this and calls RegisterAppVersion.
message AppManifest {
  string api_version          = 1;  // "spawnery/v1"
  string id                   = 2;  // "creator/app"
  string title                = 3;
  string description          = 4;
  repeated string tags        = 5;
  string visibility           = 6;  // "open" | "private" (slice 2: only "open" accepted)
  ManifestAgents agents       = 7;
  repeated string tools       = 8;
  string persona              = 9;  // repo-relative path (stored, not fetched)
  repeated string skills      = 10; // repo-relative globs/paths
  ManifestModel model         = 11;
  string runtime_base_version = 12; // e.g. ">=1.0"
  repeated ManifestMount mounts = 13;
}
message ManifestAgents {
  repeated string support      = 1; // ["any"] or explicit agent ids
  repeated string exclude      = 2;
  repeated string requires_acp = 3; // e.g. ["prompt","tools"]
}
message ManifestModel {
  bool tool_use            = 1;
  int64 min_context_tokens = 2;
  bool vision              = 3;
  string recommended_default = 4;
}
message ManifestMount { string name = 1; string path = 2; string seed = 3; }

message RegisterAppVersionRequest {
  AppManifest manifest = 1;
  string version       = 2; // semver tag, e.g. "1.2.0"
  string ref           = 3; // immutable ref the node mounts, e.g. "creator/app@<sha>"
}
message RegisterAppVersionResponse {
  string app_id   = 1;
  string version  = 2;
  TrustTier tier  = 3; // always UNVERIFIED on fresh register
}
```

## 3. Store changes

### 3.1 Schema (migration `0003_registration.sql`, sqlite + pg)

- `apps`: `ADD COLUMN creator_id TEXT NOT NULL DEFAULT ''` (the owner who registered; first-party
  seed apps get `'spawnery'`). Used to authorize new-version pushes (only the creator).
- `app_versions`: `ADD COLUMN manifest TEXT NOT NULL DEFAULT ''` (full submitted manifest as JSON;
  fidelity store — richer catalog surfacing is a later read-only change).
- `app_version_mounts`: `ADD COLUMN path TEXT NOT NULL DEFAULT ''`, `ADD COLUMN seed TEXT NOT NULL
  DEFAULT ''`.

### 3.2 Types (`types.go`)

- `App`: add `CreatorID string` (`bun:"creator_id,notnull"`).
- `AppVersion`: add `Manifest string` (`bun:"manifest,notnull"`) — JSON blob.
- `MountDecl`: add `Path string`, `Seed string` (both `notnull`).

### 3.3 Repo

- `Upsert` (App): also set `creator_id` on insert; on conflict, do NOT overwrite `creator_id`
  (ownership is sticky — set once by the first registrant/seed).
- `UpsertVersion`: persist `manifest`; `UpsertVersion` already takes `mounts` — extend `MountDecl`
  upsert to set `path`/`seed`.
- New `AppRepo` method: `Creator(ctx, appID) (string, error)` → returns `apps.creator_id`
  (`ErrNotFound` if the app doesn't exist) so the handler can authorize before writing.

## 4. Validation — `internal/manifest` (pure, reusable)

Add `func ValidateManifest(m *cpv1.AppManifest, version, ref string) error` (or a small
`manifest.Validate(...)` taking a neutral struct — implementer's call, but it must be a pure
function the handler calls; return a single error with a clear message). Checks (E5 §5 structural
tier — no repo-content access, so seed-content sanity is out):

- `api_version == "spawnery/v1"` (else reject; never silently coerce — E5 §6).
- `id` non-empty and matches `creator/app` shape: exactly one `/`, each side non-empty, chars in
  `[a-z0-9._-]` (lowercase). 
- `title` non-empty.
- `version` non-empty; basic semver shape `MAJOR.MINOR.PATCH` (digits + dots; pre-release suffix
  allowed) — reject obviously bad tags.
- `ref` non-empty.
- `visibility` in `{"open","private"}`; **`private` → reject** with "private apps are post-MVP"
  (slice 2 is open-only).
- `mounts`: each `name` non-empty + unique within the manifest; each `path` non-empty; `seed` may be
  empty (storage-less apps: `mounts: []` is valid).
- Unknown/extra proto fields can't occur (proto is strict by shape); semantic unknowns are N/A here.

On failure the handler returns `connect.CodeInvalidArgument` with the message.

## 5. CP handler — `RegisterAppVersion` (`internal/cp/registration.go`)

1. `owner, ok := auth.OwnerFromContext(ctx)`; `!ok → Unauthenticated`.
2. `manifest.Validate(req.Msg.Manifest, req.Msg.Version, req.Msg.Ref)`; err → `InvalidArgument`.
3. **Creator-ownership guard:** `creator, err := st.Apps().Creator(ctx, id)`:
   - `ErrNotFound` → new app; `owner` becomes the creator.
   - found and `creator != owner` → `PermissionDenied` ("not the app's creator").
   - found and `creator == owner` → ok (publishing a new version).
4. Map manifest → store rows and `WithTx`:
   - `App{ID: id, DisplayName: title, Summary: description, Tags: csv(tags), Visibility: "public",
     Listed: true, CreatorID: ownerOrExisting, CreatedAt: now}` (visibility "open" maps to the
     store's `"public"`; `Listed: true` — open apps list immediately as unverified).
   - `AppVersion{AppID: id, Version: version, Ref: ref, Tier: TierUnverified, Manifest: json(manifest),
     CreatedAt: now}`.
   - `[]MountDecl{ {AppID,Version,Name,Path,Seed,Required:true} ... }`.
5. Return `{app_id: id, version, tier: UNVERIFIED}`.

Re-registering the same `(id, version)` upserts (idempotent) — a creator can correct a manifest for
an unscanned version; semver immutability enforcement is a slice-3/policy concern, not slice 2.

**Seed:** unchanged behavior, but `Seed` now sets `CreatorID: "spawnery"` on first-party apps (they
stay `reviewed`). Registration is the third-party `unverified` path; the two coexist.

## 6. `spawnctl register` (reference CI client)

Extend `internal/manifest`'s `Manifest`/`Parse` to the full E0 §3 schema (currently only `id` +
`storage.mounts`). Add a `register` mode to `cmd/spawnctl`:

`spawnctl -cp <addr> -token <t> register -app <dir> -version <v> -ref <ref>`:
parse `<dir>/spawneryapp.yml` → build `cpv1.AppManifest` → `RegisterAppVersion`. Print the response
(app_id, version, tier). This is the concrete "GitHub Action or equivalent" and the e2e for the
slice. (The existing spawn modes are untouched.)

## 7. Testing

- **manifest.Validate** (table test): valid manifest passes; each rejection (bad api_version, bad
  id shape, empty title, bad semver, empty ref, `private`, dup mount name, empty mount path) fails
  with a recognizable error.
- **Store:** `creator_id` sticky on re-upsert; `manifest` blob + mount `path`/`seed` round-trip;
  `Creator` returns the id / `ErrNotFound`.
- **CP handler** (`newTestServer`): register a new app → returns `unverified`, then `ListApps`/
  `GetApp` show it (tier unverified, below the seeded reviewed apps in order); re-register a new
  version by the same owner → ok; register an existing app id as a *different* owner →
  `PermissionDenied`; invalid manifest → `InvalidArgument`; unauthenticated → `Unauthenticated`.
- **spawnctl** path: unit-test the YAML→proto mapping (parse `examples/secret-app/spawneryapp.yml`,
  assert the built `AppManifest`); full CLI wiring covered by manual/e2e, not a unit gate.

## 8. Decision log

| # | Decision | Choice |
|---|---|---|
| S2.1 | Registration ingress | **API is source of truth**; structured `AppManifest` proto via `RegisterAppVersion`. CP does not fetch/parse YAML. |
| S2.2 | `spawneryapp.yml` role | Stays in the repo; CI (or `spawnctl register`) maps it to the API. |
| S2.3 | Fresh-register tier | `unverified` (scanner promotes later). |
| S2.4 | Manifest persistence | Structured catalog/version/mount columns the system uses + full manifest **JSON blob** for fidelity. |
| S2.5 | Creator ownership | `apps.creator_id` (sticky); only the creator publishes new versions (`PermissionDenied` otherwise). |
| S2.6 | Private apps | **Rejected** in slice 2 (open-only); private + review is post-slice. |
| S2.7 | Idempotency | Re-register `(id,version)` upserts; semver immutability is a later policy concern. |
| S2.8 | Reference client | `spawnctl register` parses local YAML → API; proves the CI path + e2e. |
