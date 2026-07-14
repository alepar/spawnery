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

## Production authorization evidence

The fake-profile VM lane still uses production client custody. The SPA keeps its non-extractable
P-256 session key, while each `spawnctl` worker uses a real device login and its isolated stored
credential pair. Both clients verify the CP-relayed node identity against build/run-stamped public
roots, trust metadata, issuing intermediates, CRLs, and checkpoint state before signing. The SPA's
CRL bundle is build-stamped: CRL rotation or expiry requires a fresh web build; runtime CRL fetch is
deliberately not part of this lane.

Real SPA and stored-login CLI probes substitute the resolved node ID, class/account, or certificate
chain and require client-side refusal with zero `SubmitIntent` calls. Missing intent or node-token
fields are instead rejected by CP as `InvalidArgument` before relay; this suite does not claim a
node `MISSING_INTENT` NACK. Its exact node rejection evidence is `WRONG_AUDIENCE`, `CNF_MISMATCH`,
`BAD_SIG`, `OWNER_MISMATCH`, `CORRESPONDENCE`, `STALE`, `SKEW`, and `REPLAY`.

Session revocation is audited as well as observed at the socket: logout requires a correlated node
close record with reason `node authorization revoked`, and auth-service outage waits for the real
AS-issued 15-minute node token's signed expiry before requiring `node authorization expired`.

## Prod-safety guardrail

Every test is **mutating by default** unless explicitly tagged `@readonly`. The guardrail derives
"is this target prod" from the **actual `ACC_WEB_ORIGIN` host**, checked against
`ACC_NONPROD_HOSTS` (plus `localhost`/`127.0.0.1`, always non-prod) — not from a
self-declared label. An untagged or `@mutating` test against a host that isn't on the allowlist
hard-fails before it runs. Only `@readonly` tests may run against prod.

## Namespacing & cleanup

All artifacts created by this suite are named `acc-<runId>-w<workerIndex>-...`. Cleanup is
three-layer: a per-test owner-visible ID snapshot removes only rows created by that test (including
unnamed web/CLI rows and failed creates), an in-process teardown sweep removes the current run's
namespace, and a pre-run sweep removes stale `acc-*` artifacts older than `ACC_STALE_TTL_MS`.
There is no server-side reaper yet — see the epic's follow-up beads.

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
- `ACC_AGENT_INFERENCE_AVAILABLE` — must be explicit. `1` runs live inference; `0` marks only the
  prompt/transcript scenario fixme with a reason while non-LLM ACP/session coverage remains active.
- `ACC_AGENT_APP_ID` — a real coding-agent app registered on the target.
- `ACC_AGENT_MODEL` — a pinned, cheap model (attributable cost, never "whatever the app
  defaults to").
- `spawnctl exec` uses the authenticated CP relay and the fixture's isolated stored custody.
- A target that omits the capability declaration, or declares `1` without
  `ACC_AGENT_APP_ID`/`ACC_AGENT_MODEL`, fails loudly with a precondition error.
- The non-LLM `exec-exitcode` spec uses `ACC_TEST_APP_ID`, independent of live inference.
- `@agent` describe-blocks set `retries: 0` — see Cost ceiling above.

## Node-admin scenarios (`@noderestart`)

`tests/lifecycle/node-restart.spec.ts` proves the SE3 guarantee: `systemctl restart spawnery-node` (the
documented upgrade path) must **not** destroy the node's running spawns — the spawn returns to ACTIVE on its
own, its files and its in-flight processes survive, and in-agent git-over-HTTPS still works.

It needs `ACC_NODE_RESTART_CMD` (a host shell command that restarts the target's spawnlet — see
`.env.example`); unset, it fails loudly rather than skipping. Because a node restart disturbs **every** spawn
on the box, the scenario is tagged `@noderestart` and must run in its **own serial pass**.
`scripts/e2e-vm/run.sh` does that automatically: restart-only Chromium first, ordinary Chromium
excluding `@noderestart|@skill-staging` second, and the destructive root-artifact project without
dependencies last:

1. pass 1 `--project=chromium -g @noderestart`
2. pass 2 `--project=chromium --grep-invert @noderestart|@skill-staging`
3. pass 3 `--project=destructive-root-artifacts --no-deps`

The destructive project remains dependent on Chromium for ordinary direct Playwright use; only the final
VM pass suppresses that dependency to avoid replaying restart and ordinary coverage after destructive
state changes. Against a target whose node you cannot restart, exclude the scenario with
`--grep-invert @noderestart`.

## Skill-staging measurement (`@skill-staging`)

`tests/customization/skill-staging-s5.spec.ts` is a performance measurement, not ordinary
acceptance coverage. It retains `@mutating` and adds `@skill-staging`; the default VM ordinary
pass explicitly excludes it. Selection is opt-in:

```bash
export ACC_SKILL_SOURCE_REPOS
: "${ACC_SKILL_SOURCE_REPOS:?set the reviewed repository fixture first}"
npm run test:accept -- --project=chromium --workers=1 -g '@skill-staging'
```

The fixture must contain at least `ACC_SKILL_BUNDLE_SIZE` distinct public GitHub
`owner/repo[:subdir]` entries with `SKILL.md`; `ACC_SKILL_STAGING_ITERATIONS` controls the sample
count. The bundle size must be a finite integer of at least 8, and the iteration count a finite
integer of at least 1. Ingest must yield `ACC_SKILL_BUNDLE_SIZE` distinct catalog IDs, so sources
deduplicated by content fail before measurement. Missing or invalid input fails the selected
scenario loudly. See `.env.example` for all variables and `docs/e2e-vm-testing.md` for the
disposable-VM invocation.

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
