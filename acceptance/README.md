# acceptance/

Standalone black-box acceptance suite. Points at an **already-running** spawnery instance by
URL (dev/staging) and drives it through the web UI and `spawnctl`, cross-checked via the CP API.
It provisions **nothing** in the system under test — see the design doc for the full rationale:
`docs/superpowers/specs/2026-06-22-live-instance-acceptance-suite-design.md`.

This package is separate from `web/e2e/`, which self-provisions a hermetic stub stack; this one
is closer to synthetic monitoring / acceptance testing against shared, mutable state.

## Setup

```bash
cd acceptance
npm ci
cp .env.example .env.dev   # fill in your target's values
```

`.env.*` files are gitignored (except `.env.example`); load one into your shell before running
(`set -a; . .env.dev; set +a`) or via your CI secrets.

## Running

```bash
npm run lint
npm run typecheck
npm test            # pure-logic unit tests (vitest) — hermetic, no target required
npm run test:accept # Playwright scenarios — needs a live target (ACC_* env above)
```

`npm test` is hermetic and safe to run anywhere. `npm run test:accept` talks to a real instance
and is gated by the prod-safety guardrail below.

## Prod-safety guardrail

Every test is **mutating by default** unless explicitly tagged `@readonly`. The guardrail derives
"is this target prod" from the **actual `ACC_WEB_ORIGIN` host**, checked against
`ACC_NONPROD_HOSTS` (plus `localhost`/`127.0.0.1`, always non-prod) — not from a
self-declared label. An untagged or `@mutating` test against a host that isn't on the allowlist
hard-fails before it runs. Only `@readonly` tests may run against prod.

## Namespacing & cleanup

All artifacts created by this suite are named `acc-<runId>-w<workerIndex>-...`. Cleanup is
two-layer: an in-process teardown sweeper removes the current run's namespace even on test
failure, and a pre-run sweep removes stale `acc-*` artifacts older than `ACC_STALE_TTL_MS` (this
catches runs whose process was killed/OOMed/timed out before its own teardown ran). There is no
server-side reaper yet — see the epic's follow-up beads.

## Cost ceiling

`@agent` scenarios (added in later phases) run against a real LLM and cost real tokens. A
per-run token budget + wall-clock cap (`ACC_TOKEN_BUDGET` / `ACC_WALLCLOCK_MS`) is enforced by
`fixtures/budget.ts`'s `CostLedger`, with a kill-switch that aborts the run when exceeded.
`@agent` failures never auto-retry (masks regressions and multiplies cost).

**Caveat — the ledger is per-worker, not run-global.** The `ledger` fixture is worker-scoped
(`harness/test.ts`), so `ACC_TOKEN_BUDGET`/`ACC_WALLCLOCK_MS` are enforced per Playwright worker
process, not as one shared run-wide total — a run with N workers can burn up to N× the budget
before any single worker's check() trips. A true global kill-switch needs cross-worker shared
state (a file/IPC ledger) Playwright workers don't have out of the box; tracked as a follow-up
hardening bead.

## Phase 2 — sessions (`@agent` prompt/transcript, exec)

`tests/sessions/` adds the first `@agent` scenarios: a prompt sent through the web session view,
asserted structurally against the RENDERED transcript (test ids, turn roles — **never** agent
prose, since a real LLM's wording isn't a stable assertion target), transcript persistence across
a full page reload, and the agent's real side effect (a file it was prompted to write) read back
**fresh** via `spawnctl exec`. A sibling `exec-exitcode.spec.ts` proves plain exit-code
propagation + stdout capture with no LLM involved.

Preconditions beyond the base `.env.*` vars (see `.env.example`):
- `ACC_AGENT_APP_ID` — a real coding-agent app registered on the target.
- `ACC_AGENT_MODEL` — a pinned, cheap model (attributable cost, never "whatever the app
  defaults to").
- `ACC_NODE_ADDR` — the node's terminal endpoint reachable from wherever the suite runs. `exec`
  (like `attach`/`shell`) dials the node **directly**, bypassing the CP entirely.
- Missing `ACC_AGENT_APP_ID`/`ACC_AGENT_MODEL` makes the `@agent` scenario (and the
  `exec-exitcode` spec, which reuses the agent app as its guaranteed-present spawn source) fail
  loudly with a precondition error — never a silent skip.
- `@agent` describe-blocks set `retries: 0` — see Cost ceiling above.

## Node-admin scenarios (`@noderestart`)

`tests/lifecycle/node-restart.spec.ts` proves the SE3 guarantee: `systemctl restart spawnery-node` (the
documented upgrade path) must **not** destroy the node's running spawns — the spawn returns to ACTIVE on its
own, its files and its in-flight processes survive, and in-agent git-over-HTTPS still works.

It needs `ACC_NODE_RESTART_CMD` (a host shell command that restarts the target's spawnlet — see
`.env.example`); unset, it fails loudly rather than skipping. Because a node restart disturbs **every** spawn
on the box, the scenario is tagged `@noderestart` and must run in its **own serial pass**:
`scripts/e2e-vm/run.sh` does that automatically (pass 1 `--grep-invert @noderestart`, pass 2
`-g @noderestart --workers=1`). Against a target whose node you cannot restart, exclude it with
`--grep-invert @noderestart`.

## Ownership & SLO

Owner: the spawnery team (see CODEOWNERS once GH scenarios land). This suite runs on a schedule
(cron), not as a PR gate — a red run should be triaged within one business day; if it stays red
longer than that, treat it as broken (fix or quarantine the scenario), not as ambient noise.
Triage path: check the Playwright HTML report + uploaded (redacted) trace/video/HAR from the CI
run artifact, reproduce locally against the same target with `npm run test:accept -- -g <name>`.

## Version pinning

The built `spawnctl`/oracle types are pinned to `ACC_TARGET_REF` (config-declared — the CP has no
live `/version` endpoint yet, see follow-up beads) so a Connect contract-skew failure surfaces as
a distinct `VersionSkewError`, not a false regression.
