# Tenancy scenarios

Two-owner non-leakage (api + cli surfaces) and per-owner quota (api surface, known-cap targets
only) — Phase 6 of the acceptance suite. No agent involvement (spec scope table row 6: surfaces =
api / cli, agent cost = none).

These are black-box, dual-owner scenarios: the suite provisions nothing, so the target must
already have the identities below configured. If a required var is missing, the specs **fail
loudly** naming it — they never silently skip (the quota cap is the one deliberate exception, see
below).

## Required env vars

- `ACC_APP_ID` — a real, spawnable app id on the target (required; no seed app is provisioned by
  this suite).
- `ACC_MODEL` — optional model id passed to `CreateSpawn`; omit to use the target/app default.
- `ACC_TENANCY_A` / `ACC_TENANCY_B` — two `token=owner` dev-token pairs (same shape as
  `ACC_IDENTITY_POOL`), reserved for the tenancy test and NOT members of the parallel
  `ACC_IDENTITY_POOL` (so no worker contends for them). If unset, each falls back to the first
  two entries of `ACC_IDENTITY_POOL` respectively — fine for correctness (the non-leakage
  assertions check presence/absence of specific spawn ids, never exact counts) but dedicated
  identities are cleaner since they avoid any chance of interference from parallel workers.
- `ACC_QUOTA_TOKEN` + `ACC_QUOTA_OWNER` + `ACC_QUOTA_CAP=N` — a **dedicated** owner with a small,
  known non-zero per-owner cap, and that cap's value. The quota test sweeps this owner's spawns
  clean before running (see below), so it must not be shared with anything else running
  concurrently against the target.

All identities' tokens must be present in the target's `CP_DEV_TOKENS` — this phase runs in
dev-token auth mode only (per the design, OAuth multi-owner needs a live multi-user IdP that
tenancy does not require).

## Quota test is config-gated, not always-on

`internal/cp/server.go`'s `checkSpawnQuota` treats `maxSpawnsPerOwner <= 0` as unlimited, and a
black-box suite can neither set nor reliably discover a target's actual cap. So `quota.spec.ts`
runs ONLY when `ACC_QUOTA_CAP` parses to a positive integer and `ACC_QUOTA_TOKEN` is set;
otherwise it is skipped via `test.skip(...)` naming the reason. This is the one legitimate
skip in this directory — a deliberate test-mode gate, not a down-dependency skip.

## Dedicated owner, swept clean

The quota test starts by listing and deleting everything the quota owner has, then asserts the
list is empty before creating exactly `ACC_QUOTA_CAP` spawns — a deterministic count requires
starting from zero. This is only safe because `ACC_QUOTA_OWNER` is dedicated to this test; do not
point it at an owner used by anything else.

## Cleanup

Neither spec is covered by the worker `teardownSweeper` (it sweeps the current worker's assigned
identity, not the tenancy/quota identities) — both specs delete their own spawns in a `finally`
block, best-effort, on success or failure.
