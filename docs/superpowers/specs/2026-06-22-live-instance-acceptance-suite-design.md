# Live-Instance Acceptance Suite — Design

**Status:** draft
**Date:** 2026-06-22
**Tags:** testing, e2e, acceptance, playwright, spawnctl, ci

## Problem

Every test we have today is *self-contained*: the Go e2e lanes (`internal/cp/e2e_test.go`,
`internal/spawnlet/e2e_test.go`, …) and the Playwright suite (`web/e2e/`, via
`global-setup.ts`) **build the binaries and launch their own CP + node + stub agent**. They are
hermetic by design — deterministic stub agent, egress off, `dev-token` auth. Nothing exercises an
*already-running, external* instance end to end.

We want a suite we can point at a live instance by URL (a dev box such as
`https://blacky.dayton:5173`, later a staging instance) and have it run real user scenarios
black-box, covering functionality from **both the web UI and the `spawnctl` CLI** — spawn create,
sessions, suspend/resume, forks, marketplace, profiles/secrets, tenancy, and (in a later, gated
phase) GitHub mounts/pushes.

This is a different test *category* from the hermetic suites — closer to acceptance /
synthetic-monitoring testing. That difference drives the design: real (non-deterministic) agents
instead of the stub, shared mutable state on a long-lived instance, namespacing + cleanup,
prod-safety guardrails, and pluggable auth (dev-token on dev, OAuth on staging).

## Scope decisions (locked with the user)

- **Targets:** dev + staging from the start; prod read-only *later*. The destructive-op guardrail
  ships now and gates mutating scenarios behind a non-prod allowlist.
- **Agent:** live instances run a **real LLM agent** (OpenRouter, costs tokens). Assertions are
  **side-effect / structural only** — never on agent prose. Agent-exercising scenarios are tagged
  and kept lean.
- **GitHub:** no dedicated test GitHub resources exist yet. GitHub coverage is its **own phase**
  with an explicit "provision test org + OAuth app + bot account" prerequisite.
- **Out of scope** (stays in the host-gated lanes / `E2E_TEST_RUNSC.md`): egress floor, runsc/gVisor
  sandbox, cgroup resource limits. A black-box URL suite cannot inspect iptables/pods on a remote
  host, so those tests do not belong here.

## Research basis

Two research streams informed this design (deep-research workflow + codebase exploration; see
session of 2026-06-22). Load-bearing verified findings:

- **Unify** the web UI and the CP API (Connect-JSON is plain HTTP, so Playwright's
  `APIRequestContext` drives it) under **one Playwright/TS runner**; share auth/fixtures/reporting.
  The CLI surface was the one under-evidenced area — driving a Go CLI from a TS runner as a
  subprocess is the established but lightly-sourced pattern we adopt.
- **No BDD/Gherkin** for an engineer-authored suite — plain code with good structure. Express
  "same scenario, two front-ends" by **sharing the driver/step layer, not whole scenarios**.
- **Non-deterministic agent:** assert on observable side effects + tool-call/structural facts.
  *LLM-as-judge-as-primary was explicitly refuted (0-3 in verification)* — keep it out, or
  supplementary at most.
- **Live env:** staging by default; dedicated per-worker test accounts (never shared across
  workers); namespace every artifact; prod-safety guardrail; assert cross-tenant non-leakage.
- **OAuth:** Playwright setup-project logs in once, saves `storageState`, reused thereafter.
- **GitHub side effects:** throwaway repos, serial mutations ≥1s apart, honor
  `retry-after`/`x-ratelimit-reset`, clean up.

## Architecture

A **standalone TS/Playwright package** at repo root: `acceptance/`. Separate from `web/e2e/`
(which self-provisions a stub stack). This package provisions *nothing* — it takes a target URL and
drives it black-box.

### Components

- **`TargetConfig`** — `{ webOrigin, cpEndpoint, env: 'dev'|'staging'|'prod', authMode, owner
  credentials }`, loaded from env / `.env.<target>` files (one per target; gitignored for secrets).
  `cpEndpoint` may equal `webOrigin` when a gateway proxies `/cp.v1.SpawnService/*` (as Vite does
  in dev).
- **API oracle (`apiDriver`)** — a thin Connect-JSON client following the pattern in
  `web/src/api/connect.ts`. This is the **surface-agnostic source of truth**: actions happen on the
  surface under test; assertions read back through the API (`ListSpawns`, `GetSpawn`, `GetApp`, …).
- **Surface drivers** implementing a common `SpawnDriver` interface:
  - `webDriver` — Playwright page objects against the React SPA.
  - `cliDriver` — `spawnctl` subprocess wrapper (parses exit code / stdout / JSON).
- **Fixtures** — per-worker owner identity, run-namespacing, teardown sweeper, prod-safety guard,
  trace/artifact capture, RPC `x-acc-run-id` stamping.

### Driver model

```ts
interface SpawnDriver {
  name: 'web' | 'cli'
  createSpawn(ctx, opts): Promise<SpawnId>
  rename(ctx, id, name): Promise<void>
  setModel(ctx, id, model): Promise<void>
  suspend(ctx, id): Promise<void>
  resume(ctx, id): Promise<void>
  fork(ctx, id, opts): Promise<SpawnId>
  stop(ctx, id): Promise<void>
  delete(ctx, id): Promise<void>
}
```

Dual-surface scenarios loop over `[webDriver, cliDriver]`; assertions go through the `apiDriver`
oracle so they are written once. Surface-specific scenarios are plain tests (multi-tab reload →
web only; `exec` / `shell` → cli only). **No BDD layer** — plain TS.

```ts
const drivers = [webDriver, cliDriver]
for (const d of drivers) {
  test(`create spawn · ${d.name}`, async (ctx) => {
    const id = await d.createSpawn(ctx, { app: 'secret-app' })
    await d.waitActive(ctx, id)
    expect(await api.getSpawn(id)).toMatchObject({ status: 'ACTIVE' })
  })
}
```

### Auth & multi-env

Pluggable `AuthStrategy`:

- **dev-token** — header injection; for dev / `VITE_AUTH_ENABLED=0` boxes.
- **OAuth** — Playwright setup-project logs in once, saves `storageState` (reused by `webDriver`);
  `cliDriver` uses `spawnctl login` → stored creds.

Same scenarios, swapped strategy selected by `TargetConfig.authMode`.

### Assertion strategy (real agent)

- **Act on the surface, assert through the oracle + external side effects.** Lifecycle →
  `GetSpawn().status`. Agent did work → inspect the side effect (a marker file via `spawnctl exec`;
  a git ref via the GitHub API), never the prose.
- Agent-exercising scenarios are **tagged `@agent`**, use short canned prompts, and assert
  structurally ("a non-empty response streamed", "file X exists"). LLM-judge stays out.

### Isolation, namespacing, safety

- **Per-worker owner**, never shared across workers (cross-tenant safety). All artifacts namespaced
  `acc-<runId>-<worker>-…`.
- **Teardown sweeper** deletes everything matching the run namespace, even on failure.
- **Prod-safety guardrail**: a fixture that hard-fails any `@mutating` test unless `env` is on the
  non-prod allowlist. The `@readonly`-tagged subset is the only thing that may run against prod.
- **Tenancy scenario**: two owners; assert A sees A's spawns and *not* B's; quota →
  `ResourceExhausted` at the N+1th spawn.

## Scope & phasing

| Phase | Coverage | Surfaces | Agent cost |
|---|---|---|---|
| **0** | Harness foundation: `TargetConfig`, drivers, oracle, auth strategies, namespacing, sweeper, prod-safety guardrail, Playwright config (retries/trace/reporters), CI skeleton | — | none |
| **1** | Lifecycle: create / list / rename / setmodel / stop / delete | web + cli | none (no prompts) |
| **2** | Sessions: open / prompt / stream, multi-tab reload (web), `exec` exit-code (cli) | web + cli | lean `@agent` |
| **3** | Suspend/resume + fork (marker-file survives) — needs Garage on target | web + cli | none |
| **4** | Marketplace: browse / detail / search, register / version / listing / my-apps, spawn-from-market | web + cli | none |
| **5** | Profiles, catalog entries, secrets (CRUD + attach + observe via `exec`) | cli-primary | none |
| **6** | Tenancy non-leakage + quota | api / cli | none |
| **7** | GitHub: link / mount / clone / push / rotation / unlink — **gated on provisioning prereq** | web + cli | `@agent` + GitHub pacing |

Phase 0 + 1 is the MVP that proves the framework end to end (driver model, oracle, namespacing,
guardrail, CI) on the cheapest scenarios.

## CI cadence & artifacts

- **Scheduled** (cron, synthetic-monitoring style) against dev/staging **+ on-demand** — *not* a
  per-PR gate (too slow/costly with a real agent). Playwright HTML report + trace/video/HAR on
  failure.
- **Log correlation** (lightweight): stamp a per-test `x-acc-run-id` into RPC metadata + the
  artifact namespace; on failure, dump the matching CP telemetry slice alongside the Playwright
  trace. Full server-log piping deferred.

## Risks / open questions

- **Real-agent flake**: side-effect polling with generous timeouts; bounded retries (2) for *infra*
  flake only; `@agent` failures do **not** auto-retry (would mask real regressions).
- **Garage dependency** for Phase 3 on the target — if a target lacks it, those tests fail loudly
  (project convention), not skip.
- **`spawnctl` binary**: the suite needs a built `spawnctl` matching the target's API version; the
  harness builds it or takes a path.
- **GitHub provisioning** (Phase 7 prerequisite): a dedicated throwaway org + Spawnery OAuth app +
  bot account must exist before GitHub scenarios can run.

## Post-Implementation Notes

*As this design is implemented and iterated on — bug fixes, adjustments, anything that diverged from
the assumptions above — append a dated note here, whether or not a formal debugging skill was used.*
