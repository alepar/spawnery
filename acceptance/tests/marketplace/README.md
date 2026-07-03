# Marketplace scenarios

Covers browse/detail/search, register (app-version) + listing + my-apps, and spawn-from-market —
dual-surface (web + `spawnctl`), no agent (Phase 4 = `agent: none` in the epic table: these
scenarios assert a spawned app reaches `ACTIVE`, they never drive the LLM).

## Required precondition: a seed app

`browse.spec.ts` and `spawn-from-market.spec.ts` need a real, **listed**, **spawnable** app
already registered on the target — this suite provisions nothing, so it cannot create that app
itself (design §no self-provisioning). If the app is missing, those specs **fail loudly** naming
the precondition — they do not skip.

- Default id: `spawnery/secret-app` (the repo's `examples/secret-app`).
- Override: set `ACC_SEED_APP_ID` to a different registered app id.
- Seed it once per target with the reference CI client:

  ```bash
  spawnctl -cp <target-cp-url> -token <a-valid-token> \
    -register -app examples/secret-app -version 1.0.0 -ref spawnery/secret-app@<git-sha>
  ```

  `RegisterAppVersion` upserts the app with `Listed:true`, so no separate listing step is needed.

## App id format

`internal/cp/validate.go`'s `validateManifest` requires app ids in exactly **`creator/app`** form
(two lowercase `[a-z0-9._-]+` segments) — a bare namespaced string has no slash and is rejected
with `InvalidArgument`. The mutating specs (`register.spec.ts`, `listing.spec.ts`) therefore build
ids via `marketAppId(ctx, base)` (`market-fixtures.ts`), which returns `acc/<ns(base)>` — a fixed
`acc` creator segment plus the harness's run+worker-namespaced app segment. This is a correction to
the original task plan's grounding facts, which assumed ids were accepted verbatim; see the
`market.ts`/`market-fixtures.ts` header comments for the full note.

## No `DeleteApp` — unlist-based cleanup only

There is no `DeleteApp`/`DeleteAppVersion` RPC, so registered apps/versions can never be removed
via the API. The `market` worker-teardown fixture (`market-fixtures.ts`) best-effort **unlists**
(`SetAppListing(false)`) every `acc/acc-*` app this run's identity owns, after the worker's last
test — this keeps Browse clean for other users, but the underlying rows persist forever. No
scenario in this directory asserts an app row is *deleted*; `listing.spec.ts` only asserts it drops
out of `ListApps`/Browse while remaining visible in `ListMyApps`.

A server-side reaper (or the RPC itself) for `acc/acc-*` app rows is tracked as a follow-up bead
under the sp-tq0t epic — not implemented here.

Spawns, unlike apps, ARE deletable: `spawn-from-market.spec.ts` reuses the Phase-0
`teardownSweeper`, which deletes every spawn in the current run's namespace even on test failure.
No extra cleanup is needed for that spec.

## Tag semantics

- `browse.spec.ts` is entirely `@readonly` — safe to run against prod (it makes zero writes).
- `register.spec.ts`, `listing.spec.ts`, and `spawn-from-market.spec.ts` are `@mutating` — the
  harness's default-deny guardrail hard-fails them off the `ACC_NONPROD_HOSTS` allowlist
  (`localhost`/`127.0.0.1` always count as non-prod).

## CLI parity gap

`spawnctl` only supports the marketplace `register` verb (the no-subcommand `-register` root
action). `browse`/`openDetail`/`listMine`/`setListing` have no CLI surface at all, so
`CliMarketDriver` implements them as **failing stubs** — they throw
`marketplace: spawnctl has no <verb> (product parity gap sp-tq0t)` rather than skipping (this
project's "fail, don't skip" convention). `register.spec.ts` and `spawn-from-market.spec.ts` run
both surfaces where a CLI path exists; the rest run web-only.
