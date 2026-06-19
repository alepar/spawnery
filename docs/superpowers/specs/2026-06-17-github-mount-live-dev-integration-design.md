# GitHub Mount — Live Dev Integration (end-user path, both clients)

**Status:** draft · **Date:** 2026-06-17 · **Epic:** sp-dl62 follow-on (E1/A5 residuals + dev mint-chain wiring)

Builds on (read these first):
- `2026-06-14-github-credentials-and-storage-unified-design.md` (§16 AS-custodial — authoritative)
- `2026-06-17-owner-github-link-flow-design.md` (owner link flow; redeem returns metadata-only)
- `2026-06-17-github-egress-floor-reconciliation-design.md`

## Problem

Epic `sp-dl62` shipped the github mount **backend + dispatch + AS-custodial credential path + owner
link-flow handlers**, all unit/e2e-proven in pieces. But the **end-user, end-to-end path has never
run live**: a user cannot today `just dev`, link their GitHub through the real App, create a spawn of
an app that declares a github mount, and watch it clone + persist. Three concrete gaps block it:

1. **AS link handlers are dormant** — `cmd/authsvc/main.go` never calls `WithGitHubLink`, so
   `/github/link/{start,callback,redeem,list,revoke}` exist as methods but are not activated.
2. **No dev lane wires the mint chain** — even `dev-enforced` omits `AS_CP_URL` (AS→CP fanout) and the
   node↔AS mint channel, so `nodeGitHubMint()` returns nil and no token ever mints/delivers.
3. **No mount→credential resolution + no client mount input** — a github mount needs a
   `credential_secret_id` and a repo, but the CP does not resolve the creator's link, and neither
   spawnctl nor the web create flow lets the user declare a github mount binding (both already *sign*
   pending-intent mounts via `pollAndSign`; they just never *set* one, so mounts default to scratch).

## Goal

Run `just dev` and, from **both web and spawnctl**: link real GitHub (App), create a spawn of an app
that declares a **github mount slot**, supply `owner/repo`, and have the spawn **clone** it under the
user's identity, let the agent **do git ops**, and **survive suspend/resume** — using the real
**AS-custodial minted token** (not a static PAT).

## Key decisions (collaboratively chosen)

- **D1 — Binding model: generic slot, user picks repo.** The app manifest declares a github mount
  *slot* (name/path/durability + "this is a github mount"); the **user supplies `owner/repo`** and uses
  their own linked identity at create. One reusable app works for any user+repo. (Not: app-pinned repo.)
- **D2 — Credential resolution: auto-resolve from the creator's link (E1).** The CP looks up the spawn
  owner's single `gh:<account>` link and injects it as the mount's `credential_secret_id`. User input
  is just `owner/repo` (+ `create_if_missing`). (Not: explicit `credential_secret_id` from the client.)
- **D3 — Dev mint-auth posture: dev-simplified node→AS.** The node→AS **refresh/mint client** uses a
  relaxed/insecure auth in dev; the secure mTLS node-identity leg (containment invariant d) is already
  proven by the green e2e (`TestGitHubE2E_Rotation`, `TestGitHubE2E_RequiresNodeIdentity`). The
  **AS→CP fanout** leg is wired *faithfully* (it is on the credential-delivery path, not just refresh).

## End-to-end flow

```
LINK (once):  web Settings→GitHub  OR  spawnctl gh link (device/loopback)
   → AS /github/link/start→callback→redeem        (T1: activate WithGitHubLink)
   → AS stores the refresh chain + initial access token (AS-custodial); redeem is metadata-only.
     NO token is pushed to the CP. (Link existence is all the CP needs — see S0 outcome.)

CREATE (per spawn):  web create  OR  spawnctl create --app <id> --mount repo=github:owner/repo[,create]
   → CP loads the app manifest (declares a github mount SLOT — T4)
   → CP derives gh:<owner>, expresses the mount credential as a node-JIT-mint LINK-REF descriptor,
     routes gh: past the owner-sealed catalog gate, seeds the github-link index entry        (T3)
   → client (web/spawnctl) signs the intent via pollAndSign (already implemented on both)
   → node, for the github mount, calls MintGitHubAccessToken(link-ref) and renders the token into
     GitHubCredentialsRoot BEFORE Prepare — token arrives via the authenticated mint response,
     NEVER via the CP; then Note the link for the existing proactive refresher                (Tb)
   → storage.GitHub.Prepare clones github.com/owner/repo; binds it writable into the agent pod
   → journal snapshots .git; suspend/resume restores the working tree incl. unpushed commits
```

## S0 outcome (2026-06-18) — the assumed mechanism does not exist

The spike **disproved** the expected "link → AS mint → AS→CP fanout → CP owner-sealed catalog →
create-time delivery" path. Findings (file:line in bead sp-ache.1):
- Redeem persists link+access+refresh **AS-side only**, metadata-only to the client, **no CP fanout**.
- `gh:<account>` is an **AS-side namespace**; there is **no CP catalog row**, and the CP's
  `githubLinkIndex` is **in-memory/transient/metadata-only**, populated only by *active-spawn*
  deliveries. Fanout targets only spawns **already in the index** → cannot bootstrap a new spawn.
- `credential_secret_id = gh:<account>` therefore **fails CreateSpawn preflight today**.
- Node initial acquisition is **render-of-a-delivered-secret**; node→AS mint is **refresh-only** and
  double-gated on a prior delivery + a CP index entry.

**Conclusion:** the **create-time initial token delivery** ("spawn-start initial token delivery
(runtime)" — explicitly deferred in the owner-link-flow spec) is **unbuilt**. This slice builds it,
via the design below.

## Create-time initial token delivery — Approach 2 (node-JIT-mint at create)

Chosen over a CP-proxied mint+seal because it reuses the *entire* existing, e2e-proven node→AS mint
path, keeps the token off the CP entirely, and matches D3.

1. **Mount link-ref descriptor (proto, minimal):** a github mount binding carries a **github
   link-ref** (`secret_id=gh:<owner>`, mint-at-provision) instead of pointing at an owner-sealed
   catalog secret.
2. **CP resolution + seeding (T3):** at `CreateSpawn` for a github mount with no explicit credential,
   the CP derives `gh:<owner-account>` from the spawn owner, sets the link-ref descriptor, **routes
   `gh:` ids past the owner-sealed-secret catalog gate** (they have no catalog row by design), and at
   provision **seeds the github-link index entry** so `authorizeGitHubMint` passes. No token touches
   the CP.
3. **AS initial-mint (small):** `MintGitHubAccessToken` accepts an **initial** link-ref (`secret_id`
   only; resolves the link's current version/delivery_id when not supplied), keeping node-identity
   authZ + the CP-index check + dedup.
4. **Node mint-at-provision (Tb):** for a github mount carrying a link-ref, the node calls
   `MintGitHubAccessToken` → renders the token into `GitHubCredentialsRoot` **before** `Prepare`
   clones, then `Note`s the link for the existing proactive refresher.
5. **Failure path:** owner has no link → mint returns `relink_required` → spawn errors with a clear
   "link your GitHub first" message.

**Containment:** (a) refresh AS-only ✓ (only the access token is minted); (c) CP relays only sealed
bytes ✓ — the token never transits the CP at all; (d) node→AS node-identity authZ ✓ (relaxed in the
dev lane per D3, e2e proves the secure leg); (e) installation-selection is the only scope guarantee ✓.

## Components / tasks

| Task | What | Touches |
|------|------|---------|
| **S0** | ✅ DONE — spike disproved the assumed mechanism; design pivoted to Approach 2 (node-JIT-mint) | findings in sp-ache.1 |
| **T1** | Activate `WithGitHubLink` in `cmd/authsvc/main.go` (from `.env`, gated on config presence) **+ AS initial-mint**: `MintGitHubAccessToken` accepts an initial link-ref (secret_id only → resolve current version/delivery_id) | `cmd/authsvc`, `internal/authsvc` |
| **Tb** | **Node mint-at-provision** for github mounts + the mount **link-ref descriptor proto**: node mints via `MintGitHubAccessToken` and renders into `GitHubCredentialsRoot` before `Prepare`, then `Note`s the link | `proto/`, `internal/node`, `internal/spawnlet` |
| **T3** | CP: for a github mount w/o explicit credential, derive `gh:<owner>`, set the link-ref descriptor, **route `gh:` past the owner-sealed catalog gate**, and **seed the github-link index** at provision | `internal/cp` (CreateSpawn / mounts.go / github_fanout index) |
| **T2** | `dev-github` lane (extend `dev-enforced`): WithGitHubLink active, GitHub App env, **relaxed node→AS** mint channel (D3), garage journaling. AS→CP fanout refresh-only/optional for the demo | `Justfile`, `mprocs-*.yaml` |
| **T4** | App-manifest github mount-slot marker (declare a mount is a github slot; user supplies repo) + bind validation | `internal/manifest`, `proto/`, `internal/cp` |
| **T5** | spawnctl `create --mount name=github:owner/repo[,create]` → set `CreateSpawnRequest.Mounts` | `cmd/spawnctl` |
| **T6** | web create-flow github-mount field (owner/repo + create_if_missing) → pass `mounts` to `createSpawn` | `web/` |
| **T7** | Example app whose manifest declares a journaled github mount slot | `examples/` |
| **T8** | Live verification runbook + checks, from both clients | docs / manual |

Ordering: **S0 ✅ → T1, Tb, T4** (proto-touchers Tb/T4 serialize; re-run `make gen`) → **T3** (CP
resolution; depends on Tb's descriptor + T4) → **T2** (dev lane) → **T5, T6** (clients) → **T7** →
**T8**. `gh:`-id routing (T3) and node mint-at-provision (Tb) are the load-bearing new code.

## Acceptance (definition of done)

From **both web and spawnctl**, end to end in `just dev-github`:
1. **Link** real GitHub (App) succeeds and a `gh:<account>` credential exists.
2. **Create** a spawn of the example app, supplying `owner/repo`.
3. Inside the spawn: the repo is **cloned** (`.git` + files present), **writable** (agent `git status`,
   write a file, `git commit`), and the **commit survives suspend→resume** (journal restore).
4. Containment for the node→AS mint leg is covered by the existing green e2e (not re-proven live).

## Prerequisites / spikes

- **S1 (web link redirect):** the web link needs the App to have the dev callback registered
  (`AS_GITHUB_REDIRECT_URI`, e.g. `http://localhost:<as>/github/link/callback`). The owner adds it in
  App settings; otherwise the **spawnctl device/loopback** `gh link` avoids the redirect entirely
  (prove the spawnctl link path first if App settings are not touched).
- **S2 (dev delivery):** confirm owner-sealed secret delivery + journaling actually function in the
  `dev-github` lane (node unseal under relaxed dev auth; garage up for journaled mounts).

## Out of scope (deferred)

Node proactive-refresh wiring in dev (8h expiry irrelevant to a demo); faithful node→AS mTLS (e2e
covers it); app-manifest-pinned repo + multi-link per account; multi-node; production deployment.

## Containment invariants (unchanged; must still hold)

(a) refresh token AS-only; (b) node cred provider in node-only `GitHubCredentialsRoot`, never the
agent bind mount; (c) CP relays only sealed bytes; (d) node→AS authZ node-identity-bound *(relaxed in
the dev lane per D3 — e2e is the proof)*; (e) installation-selection is the only scope guarantee,
`repository_id` is audit-only. The dev relaxation of (d) is a **dev-lane-only** posture and MUST NOT
leak into the enforced/prod path.

## Post-Implementation Notes

*As this design is implemented and iterated on — bug fixes, adjustments, anything that diverged from
the assumptions above — append a dated note here, whether or not a formal debugging skill was used.*

### 2026-06-19 — T8 live verification (the `dev-github` lane had never booted)

Ran the full path live for the first time. The github mount works **end-to-end from both clients**:
spawnctl (clone ✓, agent commit ✓, suspend→`spawnctl resume`→commit survives ✓, `create_if_missing` ✓)
and web (created a spawn with a github-mount field → repo created/cloned into the mount ✓).

The shakedown surfaced a long tail of lane-wiring + cross-component gaps that the build-tagged e2e
(mTLS, in-harness keys, no userns-remap, no vite proxy) never exercised. Fixes landed on this branch:

- **CP↔AS↔node identity:** CP now validates AS sessions (`CP_AS_SESSION_PUBKEYS`) + lazily creates the
  owner row (`Server.ensureOwner`); `spawnery-ca node` re-mints the dev node identity under the real
  accountID; `CP_DEV_OWNER` pins the dev-token owner. (CP-owner == node-owner == AS-account == `gh:` link.)
- **Intent-token chain:** `CP_DEV_AS_KEY` + `NODE_AS_PUBKEYS` wire the session/aud=node token signing
  ↔ verification (was unset everywhere → `TOKEN_INVALID: unknown key_id`).
- **AS serves h2c** (cmd/authsvc) so the dev-relaxed node→AS gRPC mint client (HTTP/2 cleartext) connects.
- **Node cred key:** strip the `.stage` staging suffix so the at-provision render (`repo`) and the
  userns-remap Prepare lookup (`repo.stage`) agree.
- **Empty-repo init:** a no-`seed` mount no longer seeds the whole app dir; empty repos get a valid
  `--allow-empty` initial commit gated on `info.Empty`; example app gained the `repo/` mountpoint dir.
- **Clients:** `spawnctl --mount` comma-split (`DisableSliceFlagSeparator`); a new **`spawnctl resume`**
  (no in-place resume existed); pinned login port (`SPAWNCTL_LOGIN_PORT`) for remote/tunnel; dropped the
  bogus `appRef` client gate in both spawnctl and web (id≠ref for seeded apps → intent never submitted).
- **Web lane:** `web-github` recipe (`VITE_AUTH_ENABLED=1` so the SPA signs intents); AS login callback
  via the vite proxy origin (`AS_GITHUB_REDIRECT_URI` override) to fix the flow-cookie host mismatch;
  vite `/ca` → `/ca/` proxy rule (was swallowing the SPA `/callback` route).

**Filed follow-ups (beads under epic sp-m859 "MVP gaps"):** `spawnctl exec` non-TTY mode; github mount
agent **push** (Approach 2 renders node-only creds, so the agent can't push — only the node-side clone);
**web sessions blank** under enforced node + intent flow (terminal WS bind sends no session-open intent →
`MISSING_INTENT` NACK — the first *prod*-affecting gap); plus a P2 empty-repo re-mount churn (empty commit
per mount of a content-less repo). `create_if_missing` requires the App installed with Contents (and works).
