# Acceptance Suite — How to Run

Black-box **acceptance / synthetic-monitoring** suite that points at an **already-running**
spawnery instance by URL and drives it through the **web UI** and **`spawnctl`**, cross-checked via
the CP API. It provisions **nothing** in the system under test.

- Lives in [`acceptance/`](acceptance/) — a standalone Playwright/TS package, separate from
  `web/e2e/` (which self-provisions a hermetic stub stack).
- Design rationale: [`docs/superpowers/specs/2026-06-22-live-instance-acceptance-suite-design.md`](docs/superpowers/specs/2026-06-22-live-instance-acceptance-suite-design.md).
- Detailed reference (guardrail, cost ledger, per-phase preconditions):
  [`acceptance/README.md`](acceptance/README.md).

## TL;DR

```bash
# 1. build the spawnctl the cliDriver shells out to (must match the target's CP version)
distrobox enter --root dev-spawnery -- bash -lc 'cd <repo> && make bin/spawnctl'   # or: go build -o bin/spawnctl ./cmd/spawnctl

# 2. configure a target
cd acceptance
npm ci
cp .env.example .env.dev        # edit: ACC_WEB_ORIGIN / ACC_CP_ENDPOINT / ACC_AUTH_MODE / ACC_IDENTITY_POOL / ACC_NONPROD_HOSTS
set -a; . .env.dev; set +a

# 3. run
npm test              # hermetic unit tests (vitest) — no target needed, safe anywhere
npm run typecheck
npm run lint
npm run test:accept   # Playwright scenarios against the live target (ACC_* above)
```

`npm test` is hermetic (230 unit tests over the harness/drivers/oracle/auth). `npm run test:accept`
talks to the real instance and is governed by the prod-safety guardrail below.

## The one deployment constraint: run co-located (or tunneled)

`spawnctl` speaks **cleartext h2c to the CP** unless the endpoint is `https://` (then it does TLS —
T1/`schemeTransport`), and `spawnctl exec`/`attach` dial the **node directly** (`ACC_NODE_ADDR`),
bypassing the CP. For headless real-OAuth, the target must run **`AS_FAKE_GITHUB`** (reachable +
multi-user — T2), whose redirect must be reachable from where the suite runs. Net: **run the suite
co-located with the target, or over an SSH tunnel / port-forward** — not fully arms-length yet.

## Auth modes (`ACC_AUTH_MODE`)

| Mode | Use when | `ACC_IDENTITY_POOL` entries are… |
|---|---|---|
| `dev-token` | target started **non-prod** (`CP_DEV_TOKENS` active) | `token=owner` static bearers (matches `CP_DEV_TOKENS`) |
| `oauth-pop` | exercising the **real** login/PoP/refresh path | `login_hint=owner` selectors for a reachable multi-user `AS_FAKE_GITHUB` |

One distinct owner **per Playwright worker** — never shared (cross-tenant safety). See
`.env.example` for the full var set and defaults.

## Prod safety

Every test is **mutating by default**; only `@readonly`-tagged tests may run against prod.
"Is prod" is derived from the **actual `ACC_WEB_ORIGIN` host** vs `ACC_NONPROD_HOSTS`
(`localhost`/`127.0.0.1` always non-prod) — not a self-declared label. An untagged test against a
host not on the allowlist hard-fails before running.

## CI

`.github/workflows/acceptance.yml` runs the **hermetic** gates (lint / typecheck / vitest) on every
trigger; the **live** Playwright step is gated behind the `ACCEPTANCE_ENABLED` repo variable, so a
scheduled run no-ops against an unprovisioned target instead of going red.

## Status (epic `sp-tq0t`)

- **Implemented:** Phase 0 (harness) + Phases 1–6 (lifecycle, sessions, suspend/resume+fork,
  marketplace, profiles/secrets, tenancy) + OAuth-PoP auth + `spawnctl` TLS (T1) + fake-GitHub
  multi-user (T2). Scenarios are unit-tested hermetically; **not yet run end-to-end against a live
  target** — do a first smoke run against a dev instance.
- **Deferred (open beads):** `sp-tq0t.10` (provision a real GitHub test org / OAuth app / bot) and
  **Phase 7** `sp-tq0t.11` (GitHub link / mount / clone / push) — gated on that external prereq.
  Note: real-GitHub push needs a **real** GitHub App target, which is incompatible with
  `AS_FAKE_GITHUB` on the same instance.
- **Follow-up:** wire the optional `ACC_*` agent/injection vars into the CI live step before enabling
  the `@agent` phases; add a cross-worker (not per-worker) cost kill-switch.
