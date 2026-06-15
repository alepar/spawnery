# Seed Brief: GitHub Credentials + Storage Backend Implementation

**Purpose.** Self-contained handoff for a fresh session to implement the unified GitHub
credentials + storage backend. Everything you need to orient and start is here. Read the spec next.

**Status at handoff (2026-06-15):** design revised and roast-resolved; beads filed and wired; **no
implementation code written yet**. Four empirical spikes gate production and must run first.

---

## 1. Orient (do this first)

- **Branch / worktree:** the revised spec + this brief live on branch `design/github-credentials-storage`
  (worktree `.worktrees/design-github-credentials-storage`). Merge/rebase onto current `master` before
  implementing, or cut impl branches from `master` and treat the spec as the contract.
- **Spec (source of truth):**
  `docs/superpowers/specs/2026-06-14-github-credentials-and-storage-unified-design.md`
  (revised 2026-06-15; read Section 0 Revision Log + Section 15 Decision Log first).
- **Beads:** run `bd prime`, then `bd show sp-v40s` and `bd show sp-u53.1`. Issue history is in the
  Dolt DB; `bd dolt pull` to sync. **`bd` runs only from the main repo dir** (`/var/home/alepar/AleCode/spawnery`),
  not from a worktree.
- **Build/test env:** the host shell lacks the toolchain. Run all builds/tests/codegen/lint **inside
  the `dev-spawnery` distrobox**:
  `distrobox enter --root dev-spawnery -- bash -lc 'cd <repo-or-worktree> && <cmd>'`.
  Unit tests need CGO for `-race`: `CGO_ENABLED=1 go test -race ./...`. Codegen: `make gen` (never
  hand-edit `gen/`). Lint target 0 issues. See CLAUDE.md "Build/test environment".

## 2. What changed and why (the short version)

The v1 design was BLOCKed by `superpowers:roast`. A `deep-research` pass against primary GitHub docs
established the facts; keystone decisions were re-made with the owner. The five resolutions:

1. **No GitHub permission profiles.** User-to-server tokens use *fixed app-level* fine-grained
   permissions (confirmed), not per-link OAuth scopes -- a single App can't express
   `existing-repo` vs `repo-create`. **Ship one App with the union of permissions; "profiles" become a
   Spawnery-enforced mount create-policy.** Least privilege = `repository_id` token scoping.
2. **Hybrid token minting (new AS mint API).** Refresh tokens are strictly single-use (confirmed).
   The node retains the owner-delivered refresh credential for the spawn's life and mints
   `repository_id`-scoped short-lived access tokens via a **new AS API that keeps the App
   `client_secret` AS-side**. Resolves the single-use-refresh loss window + long-running-spawn
   availability + owner-offline suspend-backstop auth. This is a **deliberate relaxation of strict
   CP-blind custody** (fully-CP-blind = backlog `sp-6pqt`).
3. **Bounded exfil.** The agent's token is `repository_id`-scoped to the bound repo; all rendered
   creds live in a **journal-excluded secrets tmpfs** (the Kopia journal captures `.git` + home).
4. **No push rails.** The agent owns real-branch pushes via native git; Spawnery never force-updates a
   user branch. It writes only namespaced backstop machine refs. The impl-design's LWW/safety-ref
   scope is **retired** (see `sp-u53.1.3`).
5. **Journal is the durability path.** Kopia captures `.git`, so committed-but-unpushed work is already
   durable; the GitHub backstop is a **cross-node / human-recovery convenience**, not the sole path.

## 3. CRITICAL: spikes gate production (run before/with impl)

These are **empirical** (a registered GitHub App + the OAuth user-authorization flow -- needs the
owner's hands; cannot be done headless). Do not ship the gated features until they pass.

| Bead | Spike | Gates | Kill criteria |
|---|---|---|---|
| `sp-v40s.1` (P0) | Can a GitHub App **user-to-server token** create a personal repo via `POST /user/repos`? Docs mark it `x` for app user tokens. | `create_if_missing`; whether `Administration:write` is in the App. | If no: **drop create from MVP** (require pre-existing repo), drop `Administration:write`. |
| `sp-v40s.2` (P1) | Is a **newly-created repo** immediately reachable by a freshly minted token, or must installation repo-selection update first? | create-then-clone in one prepare flow. | If neither: gate create accordingly (lever: require "all repositories" installation). |
| `sp-v40s.3` (P1) | Is there an API to **revoke** a user token before 8h expiry? | exfil kill-switch story. | informational. |
| `sp-v40s.4` (P2) | Response-wrap **nonce redemption under a scaled AS**. | multi-instance AS deploy. | resolve before scaling AS; not a single-instance blocker. |

> A classic PAT (`ghp_`) was already shown to create a repo (201) during design, but that is the
> WRONG token type and proves nothing about the user-to-server path. `sp-v40s.1` still needs a real
> App user token.

## 4. Bead map (what to build)

**New impl beads (under `sp-v40s`):**
- `sp-v40s.5` (P0) **AS mint API** -- repository_id-scoped short-lived tokens, client_secret AS-side,
  node-driven, persist-before-confirm idempotency. *Keystone new mechanism.*
- `sp-v40s.6` (P0) **GitHub App: single union-permission registration** (no profiles; confirm
  expiring-tokens enabled).
- `sp-v40s.7` (P1) **Agent-render** -- GH_CONFIG_DIR/helper/hosts.yml into journal-excluded tmpfs;
  repository_id-scoped agent token. (Closes roast #14/#18.)
- `sp-v40s.8` (P0) **Proto owner** -- StartSpawn.secrets routing fields + AS mint RPC. *Serialize all
  proto-touching tasks here (esp. with `sp-7h6.1.8`); run `make gen`.*

**Storage backend (`sp-u53.1`):**
- `sp-u53.1.1` (P0) per-mount backend dispatch.
- `sp-u53.1.2` (P0) GitHub backend Prepare/Finalize -- mint via `sp-v40s.5`, Spawnery-enforced create
  policy, create gated on `sp-v40s.1`+`.2`.
- `sp-u53.1.3` (P0) suspend backstop -- **LWW/non-ff scope RETIRED**; now backstop-only (leaf-hash
  refs, multi-branch best-effort + per-branch SuspendComplete warnings, enumeration/recovery, node
  self-refresh via mint at suspend).
- `sp-u53.1.4` (P1) declare github-token required secret for github: mounts.
- `sp-u53.1.5` (P1) e2e (gitea, static token).

**Secret-delivery floor (`sp-7h6.1.*`) -- production prerequisites (Spec Section 2):**
- `sp-vd5w` first (live bug: cross-node owner-sealed delivery / InFlightAAD).
- `sp-7h6.1.8` replay guards + proto; `sp-7h6.1.9` A4-folded owner-online delivery;
  `sp-7h6.1.4` inject; `sp-7h6.1.11` real node-revocation checker (replace AllowAll no-op);
  `sp-7h6.1.1` data model + CRUD.
- `sp-7h6.1.5` (under `sp-v40s`) raw GitHub token injection.

**Backlog (not MVP):** `sp-6pqt` (configurable/durable AS-side custody), `sp-lqld` (WIP dirty-state
backstop).

## 5. Suggested build order (respect `bd ready`)

1. `sp-vd5w` (live bug) -- unblocks owner-sealed delivery.
2. Secret floor: `sp-7h6.1.{1,8,11}` -> `.4` -> `.9`.
3. Spikes (owner-driven, parallel): `sp-v40s.1` (THE gate) -> `sp-v40s.6` (App registration).
4. Proto: `sp-v40s.8` (serialized with `sp-7h6.1.8`).
5. `sp-v40s.5` (AS mint API) after `.8`+`.6`.
6. Storage: `sp-u53.1.1` -> `sp-u53.1.2` (after mint `.5` + spikes `.1`,`.2`); `sp-u53.1.3` (after
   `.5`); `sp-u53.1.4`; agent-render `sp-v40s.7` (after `.5`+`.8`).
7. e2e `sp-u53.1.5`.

`bd ready` reflects this via the wired deps; trust it over this list if they diverge.

## 6. Execution approach

Per CLAUDE.md ("Implementing Specced bd Tasks"): run as **parallel subagents via a dynamic Workflow**
(the `autonomous-sdd` skill), one git worktree+branch per task, disjoint file sets, serialize
`proto/`-touching tasks (`sp-v40s.8`, `sp-7h6.1.8`). Per-task pipeline: planner (opus) -> implementer
(sonnet, worktree) -> spec-compliance review (opus) -> code-quality review (opus) -> bounded fixes ->
merge integrator (sonnet: merge --no-ff, full gates in `dev-spawnery` distrobox, `bd close`, push +
`bd dolt push`). **Never push a red master.**

## 7. Open items / escalations (not yet resolved)

- The **required github.com egress channel** must be reconciled with the per-pod egress floor (does
  the floor permit the node's mint/clone/push + the agent's repo-scoped push?). Called out in spec
  Section 12; coordinate with the egress-floor design.
- **GC credential path:** backstop-ref GC after spawn deletion has no live spawn to mint through
  (spec Section 10); warning-only today, needs a credential path or accept best-effort.
- Custody is **explicitly relaxed** (AS-mediated minting). If the project later wants strict
  CP-blindness, that is `sp-6pqt` and changes the mint design.

## 8. Housekeeping from the design session (please action)

- **Delete leftover test repo** `alepar/spawnery-spike-delete-me` (created during the repo-create
  proxy test; API delete failed -- no `delete_repo` scope).
- **Revoke the classic PAT** used for that test (it was pasted into the design-session transcript).
- A separate re-roast bead `sp-nrzf.3.13` was filed for the **Profiles customization** spec (different
  effort; depends on all profiles impl beads) -- unrelated to this work, just don't be surprised by it.
