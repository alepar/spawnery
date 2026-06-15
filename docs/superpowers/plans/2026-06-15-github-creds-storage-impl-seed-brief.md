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

**Step-by-step runbooks for all four spikes are in Appendix A** (throwaway-App setup + device-flow
token mint + exact curl calls + how to read the result).

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

---

## Appendix A: Spike runbooks (how to actually run them)

All four need a **throwaway GitHub App** + the owner's GitHub account. Use a disposable App and clean
up created repos afterward. Token-type prefixes are your sanity check: `ghu_` = user-to-server access
(what we need), `ghr_` = user-to-server refresh, `ghp_` = classic PAT (WRONG type -- ignore),
`github_pat_` = fine-grained PAT, `ghs_` = installation token.

### A.0 One-time setup (shared by spikes 1-3)

1. github.com -> Settings -> Developer settings -> **GitHub Apps -> New GitHub App**.
   - Permissions: **Repository -> Administration: Read & write** and **Contents: Read & write**.
   - **Expiring user authorization tokens: enabled** (default for new Apps -- confirm it is on; this is
     what yields refresh tokens and the 8h/6mo lifetimes).
   - **Enable Device Flow: checked** (lets the spike get a user token with no callback server -- easiest).
   - Webhook: uncheck Active. Where can it be installed: only this account.
2. Save. Note the **Client ID**; generate and copy a **Client secret**.
3. **Install** the App on your account: App page -> Install App -> pick **All repositories** (needed for
   spike 2's "all repos" lever) for one run, and **Only select repositories** for the spike-2 contrast run.
4. **Mint a user-to-server token via device flow** (no callback server needed):
   ```bash
   CID=<client_id>
   # 1) request a device + user code
   curl -s -X POST https://github.com/login/device/code \
     -H 'Accept: application/json' -d "client_id=$CID" -d "scope="
   # -> {"device_code":"...","user_code":"XXXX-XXXX","verification_uri":"https://github.com/login/device", "interval":5}
   # 2) open verification_uri in a browser, enter user_code, authorize the App
   # 3) poll for the token (repeat until it stops returning authorization_pending)
   curl -s -X POST https://github.com/login/oauth/access_token \
     -H 'Accept: application/json' -d "client_id=$CID" -d "device_code=<device_code>" \
     -d "grant_type=urn:ietf:params:oauth:grant-type:device_code"
   # -> {"access_token":"ghu_...","expires_in":28800,"refresh_token":"ghr_...","refresh_token_expires_in":15897600,...}
   ```
   Confirm the access token starts with **`ghu_`** -- if it does not, you are not testing a
   user-to-server token and the result is meaningless.
   (Web flow alternative if device flow is undesirable: browse
   `https://github.com/login/oauth/authorize?client_id=$CID`, authorize, grab `?code=` from the
   callback, then `POST /login/oauth/access_token` with `client_id`+`client_secret`+`code`.)

### A.1 (sp-v40s.1, THE gate) Can a `ghu_` token create a personal repo?

```bash
TK=ghu_<token from A.0>
curl -sS -o /tmp/r.json -w '%{http_code}\n' -X POST \
  -H "Authorization: Bearer $TK" -H 'X-GitHub-Api-Version: 2022-11-28' \
  https://api.github.com/user/repos \
  -d '{"name":"spk-del-me","private":true,"auto_init":false}'
cat /tmp/r.json   # look at full_name / message
```
- **201** -> create works with a user-to-server token: keep `Administration:write`, keep
  `create_if_missing` in MVP.
- **403 / 404 / 422 with a "resource not accessible by integration"-style message** -> the docs' `x`
  marker holds: **KILL** -- drop `create_if_missing` (require pre-existing repo) and drop
  `Administration:write` from the App. Record the exact status + message in the bead.
- Clean up: delete the repo from the GitHub UI (the token likely lacks delete rights).

### A.2 (sp-v40s.2) Newly-created-repo coverage + `repository_id` scoping

Two questions, two runs:
1. **All-repositories install** (A.0 step 3 variant A): right after a successful A.1 create, with the
   same `ghu_` token, `GET /repos/<owner>/spk-del-me` and try a `git clone` over HTTPS using the token.
   200/clone-ok -> new repos are auto-covered.
2. **Selected-repositories install** (variant B, the new repo NOT in the selection): repeat -> expect
   **404** until the installation's repo selection is updated to include it. That confirms the lever:
   **require an "all repositories" installation for create-capable links**, or programmatically update
   the selection before clone.
3. **`repository_id` narrowing:** verify *where* GitHub accepts `repository_id` to scope a user token to
   one repo (deep-research cites it on user-access-token generation; confirm the exact request -- it is
   the param the node will use per-mount). Mint a token narrowed to one repo id and confirm it can reach
   that repo and 404s on another the user otherwise has.

### A.3 (sp-v40s.3) Token revocation before 8h

```bash
CID=<client_id>; CS=<client_secret>; TK=ghu_<token>
# revoke a single user token (Basic auth = client_id:client_secret)
curl -sS -o /dev/null -w '%{http_code}\n' -X DELETE \
  -u "$CID:$CS" -H 'Accept: application/vnd.github+json' \
  https://api.github.com/applications/$CID/token \
  -d "{\"access_token\":\"$TK\"}"      # 204 = revoked
# confirm the token is now dead:
curl -sS -o /dev/null -w '%{http_code}\n' -H "Authorization: Bearer $TK" https://api.github.com/user  # expect 401
```
- 204 then 401 -> there IS a pre-expiry kill switch (good for the exfil story). Also note: refreshing
  already invalidates the prior access token (doc-confirmed single-use) -- this DELETE is the
  *explicit, immediate* path. Record which works.

### A.4 (sp-v40s.4) Scaled-AS nonce -- reasoning, not curl

No GitHub call. Decide how the single-use response-wrap nonce is redeemed when the AS runs multiple
instances: (a) shared store (Redis/DB) holding `nonce -> wrapped-tuple` with single-use delete, or
(b) sticky-session pinning redemption to the issuing instance. Pick one, write it into the AS deploy
notes, and ensure leaked-nonce replay is rejected (single-use + short TTL + authenticated redemption).
Only blocks multi-instance AS deployment.
