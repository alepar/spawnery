# E5 Slice 3 — Version Selection & Pinning (Design)

**Bead:** `sp-7hl` (E5) — slice 3
**Status:** Draft v1 — build proceeding per "keep going" unless flagged
**Date:** 2026-06-01
**Builds on:** [slice 1](2026-06-01-e5-catalog-read-surface-slice1.md) (`f28eff6`) · [slice 2](2026-06-01-e5-app-registration-slice2.md) (`8be3550`) · [E5 §4 versioning](2026-05-28-spawnery-e5-packaging-catalog-design.md)

## 0. Context

After the API-source-of-truth shift (slice 2), versions arrive via `RegisterAppVersion` and live in
the store. The original GitHub-poll model (CP resolves latest tag→SHA on the hot path) is gone —
**"resolve the latest version" is now a DB query.** This slice surfaces that: let `CreateSpawn`
**select a version** (latest, or an explicit one) and **pin** the spawn so it won't auto-upgrade.

`Spawn` already has the columns (`app_version`, `app_ref`, `pinned`) — **no schema change.**
`CreateSpawn` currently always resolves `LatestReviewed`; this slice makes that selectable.

## 1. Scope

**In:**
1. **Contracts** — add `version` (optional) + `pin` (bool) to `CreateSpawnRequest`.
2. **CP** — `CreateSpawn` resolves the version per request and records `Pinned`.

**Out:** auto-upgrade *re-resolution on resume* (latent — needs lifecycle resume, part 3b/`sp-gd9`) ·
pre-upgrade snapshot (E3/node) · changelog surfacing · spawning non-`reviewed` tiers (gated on the
security floor `sp-rpa`/`sp-eha`) · a `RepinSpawn`/un-pin RPC (post-slice).

## 2. Semantics

`CreateSpawnRequest { ..., string version = 4; bool pin = 5; }`:

- **`version == ""`** → resolve **latest `reviewed`** version (current behavior; `LatestReviewed`).
- **`version == "1.2.0"`** → that exact version; it must exist **and** be tier `reviewed`
  (spawnable). Missing → `InvalidArgument`; exists but not `reviewed` → `FailedPrecondition`
  ("version <v> is tier <t>, not spawnable"). (Spawning unverified/scanned tiers is gated on the
  security floor — out of this slice.)
- **`pin`** → sets `Spawn.Pinned`. `pin=true`: the spawn is fixed to the resolved `app_version`/
  `app_ref` and will **not** auto-upgrade. `pin=false` (default): `Pinned=false` — the spawn is
  "auto", and a future resume (part 3b) would re-resolve to latest. Until resume exists this is a
  recorded intent only.

The resolved `(version, ref)` is recorded on the spawn (`AppVersion`, `AppRef`) exactly as today;
the only additions are the selection input and persisting `Pinned`.

**Demo invariant unchanged:** only `reviewed` versions are spawnable. Seed apps are `reviewed`;
registered third-party apps are `unverified` and remain non-spawnable until the security floor lands.

## 3. CP implementation (`internal/cp/server.go`, `CreateSpawn`)

Replace the fixed `LatestReviewed` resolution with:

```go
appID := req.Msg.AppId
var ver store.AppVersion
if v := req.Msg.Version; v != "" {
	ver, err = s.st.Apps().GetVersion(ctx, appID, v)
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("unknown app version: %s@%s", appID, v))
	}
	if ver.Tier != store.TierReviewed {
		return nil, connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("version %s@%s is tier %q, not spawnable", appID, v, ver.Tier))
	}
} else {
	ver, err = s.st.Apps().LatestReviewed(ctx, appID)
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("unknown app: %s", appID))
	}
}
```

and set `Pinned: req.Msg.Pin` in the `store.Spawn{...}` literal. Everything else (mounts, lock,
WithTx Create, Provision, SetActive, compensation) is unchanged.

## 4. Testing (`internal/cp` via `newTestServer`)

- explicit reviewed version → spawn records that `app_version`/`app_ref`.
- `version=""` → latest reviewed (seed app already covers this; add a 2-reviewed-version case to
  confirm "latest").
- unknown version → `InvalidArgument`.
- a non-reviewed version (register an `unverified` version via the slice-2 path, then request it) →
  `FailedPrecondition`.
- `pin=true` → the persisted spawn has `Pinned=true`; `pin=false` → `Pinned=false`.

## 5. Decision log

| # | Decision | Choice |
|---|---|---|
| S3.1 | Selection input | `CreateSpawnRequest.version` (empty = latest reviewed) + `pin` bool |
| S3.2 | Spawnable tier | `reviewed` only (unverified/scanned gated on security floor) |
| S3.3 | Errors | unknown version → `InvalidArgument`; non-reviewed → `FailedPrecondition` |
| S3.4 | Pin meaning | records `Spawn.Pinned`; auto re-resolution is latent until resume (part 3b) |
| S3.5 | Schema | none — `Spawn.pinned/app_version/app_ref` already exist |
