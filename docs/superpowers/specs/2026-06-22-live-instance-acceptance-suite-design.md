# Live-Instance Acceptance Suite — Design

**Status:** draft (roast-revised r1)
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
prod-safety guardrails, and pluggable auth.

## Scope decisions (locked with the user)

- **Targets:** dev + staging from the start; prod read-only *later*. The destructive-op guardrail
  ships now and gates mutating scenarios behind a non-prod allowlist keyed on the **actual target
  URL** (not a hand-edited label).
- **Auth:** **real OAuth + PoP is in v1 scope** (the user chose to invest here rather than restrict
  to dev-token). Because the SPA's auth binds a short-lived bearer to a **non-extractable device
  key** (`web/src/api/connect.ts` reads the token from `localStorage` and signs a PoP, refreshing
  via `@/auth/refresh`; device keys are `extractable: false`), the naive "log in once, save
  Playwright `storageState`, reuse" pattern does **not** work — a non-extractable `CryptoKey`
  cannot be serialized into `storageState`, and the standalone API-oracle client cannot mint/refresh
  a PoP-gated bearer on its own. **This is the project's single biggest load-bearing unknown and is
  gated behind Spike S1 (below) before any auth-dependent phase is built.** `dev-token` mode is also
  supported for the dev box (and webDriver dev auth **seeds the SPA token store**, it does *not*
  inject an `Authorization` header — the SPA sources its bearer from its own store).
- **Agent:** live instances run a **real LLM agent** (OpenRouter, costs tokens). Assertions are
  **side-effect / structural only** — never on agent prose. Agent-exercising scenarios are tagged
  `@agent`, kept lean, and bounded by the cost/wall-clock NFRs below.
- **Coverage / dual-surface:** we want **full use-case coverage regardless of whether `spawnctl`
  implements the operation**. `spawnctl` currently lacks `rename`/`suspend`/`stop`/`delete` (it has
  `resume` but not `suspend`; top-level commands are create/list/status/set-model/fork/resume/move/
  exec/shell/attach/login/logout/profile/catalog/gh/key — `cmd/spawnctl/main.go`). The `cliDriver`
  therefore implements the **full** `SpawnDriver` interface; the verbs `spawnctl` cannot perform are
  **stubs that FAIL** (never skip), per this project's "fail, don't skip" convention. The dual-surface
  matrix stays complete and the CLI parity gap surfaces as **visible red**, tracked as product debt.
- **GitHub:** no dedicated test GitHub resources exist yet. GitHub coverage is its **own phase**
  with an explicit "provision test org + OAuth app + bot account" prerequisite.
- **Out of scope** (stays in the host-gated lanes / `E2E_TEST_RUNSC.md`): egress floor, runsc/gVisor
  sandbox, cgroup resource limits. A black-box URL suite cannot inspect iptables/pods on a remote
  host, so those tests do not belong here.

## Research basis

Two research streams informed this design (deep-research workflow + codebase exploration; session of
2026-06-22) and an adversarial `superpowers:roast` pass (r1, BLOCK → this revision). Load-bearing
verified findings:

- **Unify** the web UI and the CP API (Connect-JSON is plain HTTP, so Playwright's
  `APIRequestContext` drives **unary** calls) under **one Playwright/TS runner**; share
  fixtures/reporting. `APIRequestContext` is unary-only and buffers full bodies — it **cannot**
  observe bidi-streaming `Session` RPCs or the SPA's WebSocket ACP transport (see Assertion
  strategy).
- **No BDD/Gherkin** — plain code with good structure. Express "same scenario, two front-ends" by
  **sharing the driver/step layer, not whole scenarios**.
- **Non-deterministic agent:** assert on observable side effects + structural facts.
  *LLM-as-judge-as-primary was explicitly refuted* — keep it out.
- **Live env:** dedicated per-worker test identities (never shared across workers); namespace every
  artifact; prod-safety guardrail; assert cross-tenant non-leakage.
- **GitHub side effects:** throwaway repos, serial mutations ≥1s apart, honor
  `retry-after`/`x-ratelimit-reset`, clean up.

## Architecture

A **standalone TS/Playwright package** at repo root: `acceptance/`. Separate from `web/e2e/`
(which self-provisions a stub stack). This package **provisions nothing in the system under test**;
it takes a target URL and drives it black-box. (It *does* depend on a pre-provisioned pool of test
identities — see Isolation.)

### Components

- **`TargetConfig`** — `{ webOrigin, cpEndpoint, env, targetHost, authMode, identityPool }`, loaded
  from env / `.env.<target>` (gitignored). `cpEndpoint` may equal `webOrigin` when a gateway proxies
  `/cp.v1.SpawnService/*` (as Vite does in dev). The **prod-safety guardrail derives prod-ness from
  `targetHost`** against a non-prod allowlist — not from a self-declared label.
- **API oracle (`apiDriver`)** — a Connect-JSON client following `web/src/api/connect.ts`. It is the
  surface-agnostic **cross-check**, *not* the sole source of truth: it reads back through the same CP
  the surfaces write to, so a CP read/write path that is consistently-wrong would yield a false
  green. Web scenarios therefore assert **rendered UI state** as the primary check (see below). The
  oracle reads status via **`ListSpawns`** (there is no `GetSpawn` RPC).
- **Surface drivers** implementing a common `SpawnDriver`:
  - `webDriver` — Playwright page objects against the React SPA; **asserts rendered DOM state**.
  - `cliDriver` — `spawnctl` subprocess wrapper; **stubs unsupported verbs as failing**.
- **Auth strategies** — `DevTokenAuth` (seeds the SPA token store; `-token` for cli/oracle) and
  `OAuthPoPAuth` (the Spike-S1 outcome). Selected by `TargetConfig.authMode`.
- **Fixtures** — per-worker identity from the pool, run-namespacing, pre-run stale-namespace sweep,
  teardown sweeper, prod-safety guard, **artifact redaction**, preflight health gate.

### Driver model

```ts
interface SpawnDriver {
  name: 'web' | 'cli'
  createSpawn(ctx, opts): Promise<SpawnId>
  rename(ctx, id, name): Promise<void>    // cliDriver: STUB → fails (no spawnctl rename)
  setModel(ctx, id, model): Promise<void>
  suspend(ctx, id): Promise<void>         // cliDriver: STUB → fails (no spawnctl suspend)
  resume(ctx, id): Promise<void>
  fork(ctx, id, opts): Promise<SpawnId>
  stop(ctx, id): Promise<void>            // cliDriver: STUB → fails (no spawnctl stop)
  delete(ctx, id): Promise<void>          // cliDriver: STUB → fails (no spawnctl delete)
}
```

Dual-surface scenarios loop over `[webDriver, cliDriver]`. Web assertions check rendered DOM;
the `apiDriver` cross-checks. Surface-specific scenarios are plain tests (multi-tab reload → web
only; `exec`/`shell` → cli only).

```ts
const drivers = [webDriver, cliDriver]
for (const d of drivers) {
  test(`create spawn · ${d.name}`, async (ctx) => {
    const id = await d.createSpawn(ctx, { app: 'secret-app' })
    await d.waitActive(ctx, id)                       // web: assert the active dot renders
    expect(await api.listSpawns()).toContainSpawn(id, { status: 'ACTIVE' })  // cross-check
  })
}
```

### Assertion strategy (real agent)

- **Web: assert rendered DOM** (the spawn row, the active dot, the chat transcript) as the primary
  check — that is the thing a UI acceptance suite exists to verify. The `apiDriver` is a complementary
  cross-check, never the sole oracle.
- **CLI: assert exit code + parsed stdout/JSON**, cross-checked via `apiDriver`.
- **Streaming / sessions (Phase 2):** the unary `apiDriver` cannot observe the bidi `Session` stream
  or the WebSocket ACP transport. Assert the **rendered transcript** in the UI (web) and the
  `spawnctl exec`/attached output (cli) instead.
- **Agent side effects:** inspect a **per-run unique marker** (unique content, asserted *fresh* —
  never "file X exists", which passes on a stale marker from a prior run) or a git ref via the
  GitHub API. Never assert on agent prose. `@agent` scenarios pin/record the model (below).
- **Accepted limitation:** structural assertions verify *plumbing, not agent quality* — a genuine
  agent-quality regression can pass green. This is the conscious trade-off (LLM-judge-as-primary was
  refuted); agent-quality measurement is the Eval harness's job, not this suite's.

### Isolation, namespacing, safety

- **Test-identity pool:** the suite requires a **pre-provisioned pool of test owners** (a documented
  prerequisite — under OAuth these are real accounts, tied to Spike S1). Pool size ≥ max Playwright
  workers; a **stable `parallelIndex → identity` map** assigns one owner per worker; owners are
  **never shared across workers**.
- All artifacts namespaced `acc-<runId>-<worker>-…`.
- **Cleanup is two-layer:** (1) in-process **teardown sweeper** deletes the run namespace even on
  test failure; (2) a **pre-run sweep** deletes stale `acc-*` artifacts older than a TTL (covers
  runs whose process was SIGKILLed/OOMed/CI-timed-out, which the in-process sweeper misses). A
  **server-side TTL reaper** is documented as a target dependency for defence in depth.
- **Prod-safety guardrail:** hard-fails any `@mutating` test unless `targetHost` is on the non-prod
  allowlist, and is **default-deny** — an **untagged** test is treated as mutating; only explicit
  `@readonly` tests may run against prod.
- **Artifact redaction:** trace/HAR/video and any persisted auth state are scrubbed of bearer
  tokens, cookies, and keys before upload; treated as sensitive in CI.
- **Tenancy scenario:** two owners; assert A sees A's spawns and *not* B's. **Quota** scenario runs
  only against a target whose per-owner cap is **known and non-zero** (the CP treats `cap ≤ 0` as
  unlimited and a black-box suite can neither set nor reliably discover it); it uses a dedicated
  single-worker owner starting from a swept-clean state.

### Non-functional requirements

- **Cost ceiling:** a per-run **token budget + global wall-clock cap** with a **kill-switch** that
  aborts the run when exceeded (defence against a cron run silently burning the OpenRouter budget).
  `@agent` failures do **not** auto-retry (would mask regressions *and* multiply cost).
- **Model pinning:** record (and where the API allows, pin) the model/provider per run so an
  `@agent` failure is attributable to a spawnery regression vs. model/provider drift.
- **Version compatibility:** the built `spawnctl` and the oracle's generated types are **pinned to
  the target's deployed CP version** (read a version endpoint, or build from the target's ref) to
  avoid Connect contract-skew false failures.
- **Preflight health gate:** before scenarios run, a reachability/health check fails fast with a
  distinct "target/dependency down" signal so an outage is not mistaken for a code regression.

## Spikes (de-risk before building on them)

- **S1 — Headless OAuth + PoP auth (BLOCKING for all OAuth phases).**
  *Question:* Can an automated suite obtain and *refresh* a PoP-bound bearer for a test owner against
  an OAuth/auth-enabled instance, given the device key is non-extractable and `storageState` cannot
  carry it?
  *Cheapest test:* attempt, in order, until one works: (a) drive the full OAuth+device-enrollment
  ceremony in a persistent Playwright context and keep the context alive for the run (no
  `storageState` round-trip); (b) reproduce the PoP-refresh signing in Node against a test-owned key;
  (c) add a **test-only bearer-mint endpoint / long-lived test token** on dev/staging instances.
  *Kill criteria:* if none of (a)/(b)/(c) yields a refreshable bearer for both the browser and the
  standalone oracle within the spike budget, OAuth phases are descoped to dev-token-only targets and
  real-OAuth coverage is re-planned as a product change to the auth service.

  **RESULT (2026-06-22, sp-tq0t.1 — GREEN, kill criteria NOT triggered).** Real-OAuth coverage is
  buildable headlessly. Findings (code-grounded):
  - The auth-relevant key is the **session key** (ECDSA P-256, non-extractable, IndexedDB
    `spawnery-auth`); the X25519 device-set keys are owner-sealed-secrets and irrelevant here. The
    refresh **PoP** is bespoke but fully specified (`web/src/auth/pop.ts`: domain
    `spawnery/refresh-pop/v1` ‖ sha256(refresh_token) ‖ be64(ts) ‖ nonce, ECDSA-P256 P1363) and
    **already reproduced in Go** by `spawnctl` (`cmd/spawnctl/authstate.go`) — so it is portable to a
    Node oracle. Access-token TTL = 15 min; `/refresh` is gated on the non-extractable key + an
    HttpOnly refresh cookie.
  - **The binding point is open:** `GET /oauth/authorize?session_pubkey=<b64 SPKI>` binds a refresh
    family to a caller-supplied key. The Auth Service supports **`AS_FAKE_GITHUB=1`** (an in-process
    fake IdP the team **already uses** for its auth e2e suite — `web/playwright.auth.config.ts` +
    `web/e2e/global-setup-auth.ts`), which runs the *real* auth/token/PoP code paths but stubs the
    GitHub login page.
  - **Chosen approach — (b) for the oracle + (a) for the browser, both against an `AS_FAKE_GITHUB`
    target:** the Node `apiDriver` generates its own P-256 session key, drives `/oauth/authorize` →
    `/oauth/callback` over HTTP (fake IdP auto-redirects), captures the access token + refresh cookie,
    and refreshes autonomously with the reproduced PoP — **fully headless, no browser, no
    github.com**. The `webDriver` uses a **persistent Playwright context** (no `storageState`
    round-trip; the non-extractable key lives in the context's IndexedDB for the run), reusing the
    existing auth e2e setup as ~90% reference.
  - **(c) dev-token** (`CP_DEV_TOKENS`, default in any non-`prod` CP) remains the zero-cost path for
    scenarios that only need a CP bearer and don't exercise the AS/PoP path. It is silently disabled
    at `auth.mode=prod`.
  - **The only RED path is automating *real* github.com** (2FA/bot-detection/ToS) — universally
    avoided; the `AS_FAKE_GITHUB` design already encodes that decision.
  - **Carried constraint → Phase 7 / GH prereq (sp-tq0t.10/.11):** `AS_FAKE_GITHUB` gives auth but
    **cannot mint a *real* GitHub token** for repo mount/push. Phase-7 real-GitHub scenarios therefore
    need a target wired to a **real** GitHub OAuth app + test org (the provisioning prereq), which is
    incompatible with `AS_FAKE_GITHUB` on the *same* instance. Resolve in the GH-prereq task: either a
    dedicated real-GitHub target for Phase 7, or a fake-link path for non-push GitHub coverage.

## Scope & phasing

| Phase | Coverage | Surfaces | Agent cost |
|---|---|---|---|
| **0** | Harness foundation: `TargetConfig` + URL-based guardrail, drivers, oracle (`ListSpawns`), **auth strategies + Spike S1**, identity-pool mapping, namespacing, pre-run + teardown sweeps, artifact redaction, version-pinning, preflight health gate, cost/kill-switch, Playwright config, CI skeleton | — | none |
| **1** | Lifecycle: create / list / rename / setmodel / stop / delete (cli stubs fail for rename/stop/delete) | web + cli | none |
| **2** | Sessions: open / prompt / rendered-transcript assertion, multi-tab reload (web), `exec` exit-code (cli) | web + cli | lean `@agent` |
| **3** | Suspend/resume + fork (per-run marker survives) — needs Garage on target; cli suspend stub fails | web + cli | none |
| **4** | Marketplace: browse / detail / search, register / version / listing / my-apps, spawn-from-market (required seed apps are a documented target precondition) | web + cli | none |
| **5** | Profiles, catalog entries, secrets (CRUD + attach + observe via `exec`) | cli-primary | none |
| **6** | Tenancy non-leakage + quota (quota only on a known-cap target) | api / cli | none |
| **7** | GitHub: link / mount / clone / push / rotation / unlink — **gated on provisioning prereq** | web + cli | `@agent` + GitHub pacing |

Phase 0 (incl. Spike S1) + Phase 1 is the MVP that proves the framework end to end.

## CI cadence & artifacts

- **Scheduled** (cron, synthetic-monitoring style) against dev/staging **+ on-demand** — *not* a
  per-PR gate (too slow/costly with a real agent). Playwright HTML report + redacted trace/video/HAR
  on failure.
- **Ownership:** the suite has a **named owner, a pass-rate SLO, and a triage path** so a
  non-PR-gating cron job does not rot into ignored-red.
- **Log correlation** (client-side only): stamp a per-test `x-acc-run-id` into the artifact namespace
  and RPC metadata. Server-side telemetry correlation requires server access the black-box suite does
  not have, so it is **descoped** (an optional, separately-provisioned capability).

## Risks / open questions

- ~~**Spike S1 is the gating risk** — if headless OAuth+PoP proves infeasible, OAuth phases descope to
  dev-token targets.~~ **RESOLVED 2026-06-22 (GREEN)** — headless OAuth+PoP is buildable against an
  `AS_FAKE_GITHUB` target (oracle reproduces the PoP in Node; browser uses a persistent context). See
  the S1 Result block under Spikes. New carried constraint: Phase-7 *real*-GitHub needs a real-GitHub
  target, incompatible with `AS_FAKE_GITHUB` on the same instance (tracked on sp-tq0t.10).
- **Real-agent flake / cost**: side-effect polling with generous timeouts under the wall-clock cap;
  bounded retries (2) for *infra* flake only; `@agent` failures do not auto-retry.
- **Garage dependency** for Phase 3 on the target — if a target lacks it, those tests fail loudly.
- **`spawnctl` parity debt**: rename/suspend/stop/delete are missing; the cli arm fails on those rows
  by design (visible product debt; optionally tracked as follow-up beads issues).

## Post-Implementation Notes

*As this design is implemented and iterated on — bug fixes, adjustments, anything that diverged from
the assumptions above — append a dated note here, whether or not a formal debugging skill was used.*

- **2026-06-22 (Spike S1 resolved — GREEN):** investigated the live auth path (session key = ECDSA
  P-256 non-extractable; bespoke refresh PoP already reproduced in Go by `spawnctl`;
  `/oauth/authorize?session_pubkey=…` binds a caller key; `AS_FAKE_GITHUB` runs real auth code with a
  stubbed IdP and is already used by the auth e2e suite). Verdict: headless OAuth+PoP buildable —
  oracle reproduces PoP in Node + drives the fake-IdP code flow over HTTP; browser uses a persistent
  Playwright context. dev-token (`CP_DEV_TOKENS`, non-prod default) stays the zero-cost CP-only path.
  Only real-github.com automation is RED. Carried constraint logged for Phase 7 (real-GitHub vs
  `AS_FAKE_GITHUB`). Spike bead sp-tq0t.1 closed; OAuthPoP task sp-tq0t.3 updated with the approach.
- **2026-06-22 (roast r1):** `superpowers:roast` returned BLOCK. Inflated count (same-family panel
  confirmed 80/83) but several distinct findings verified against code and folded in: auth model
  (non-extractable-key PoP) breaks `storageState` reuse and dev-token header-injection → added
  Spike S1 + token-store seeding; `spawnctl` lacks rename/suspend/stop/delete → full-interface
  `cliDriver` with failing stubs (user's "full coverage, let it fail" call); `GetSpawn` RPC does not
  exist → oracle uses `ListSpawns`; unary `APIRequestContext` can't read streams → rendered-transcript
  assertions; oracle not independent → DOM-primary web assertions; quota cap-0 default → quota gated
  to known-cap targets; in-process sweeper leaks on process-kill → pre-run sweep + reaper dependency;
  prod guardrail label-trust + fail-open → URL-allowlist + default-deny; added cost ceiling/kill-switch,
  model pinning, version pinning, preflight health gate, artifact redaction, identity-pool sizing,
  owner/SLO; descoped server-side log correlation.
