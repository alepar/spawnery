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
   → AS stores the refresh chain (AS-custodial), mints an access token,
     and the owner-sealed access token reaches the CP catalog as secret gh:<account>   ← S0

CREATE (per spawn):  web create  OR  spawnctl create --app <id> --mount repo=github:owner/repo[,create]
   → CP loads the app manifest (declares a github mount SLOT — T4)
   → CP AUTO-RESOLVES creator's gh:<account> link → mount.credential_secret_id          (T3 / E1)
   → CP validates github-token type; puts the mount binding in the pending intent
   → client (web/spawnctl) signs the intent via pollAndSign (already implemented on both)
   → node renders the sealed github-token into GitHubCredentialsRoot (node-only tmpfs)
   → storage.GitHub.Prepare clones github.com/owner/repo; binds it writable into the agent pod
   → journal snapshots .git; suspend/resume restores the working tree incl. unpushed commits
```

## S0 — load-bearing spike (do first)

The riskiest hop, never run live: **how the AS-custodial token becomes a deliverable `gh:<account>`
secret the node renders at create-time.** Expected mechanism: link → AS mints → **AS→CP fanout** seals
it into the CP catalog → create-time owner-sealed delivery uses it. S0 must confirm:
- whether the CP **holds** a fanned-out owner-sealed token keyed by `gh:<account>` for a spawn created
  *later* (vs only delivering to an already-active spawn), and
- whether initial credential acquisition is **fanout-then-deliver** or **node-JIT-mint**.

The answer determines whether T2 must wire `AS_CP_URL`/`AS_CP_SECRET` (fanout) and/or the node→AS
channel, and how T3 hands the resolved credential to delivery. **Resolve S0 before T2/T3 wiring.**

## Components / tasks

| Task | What | Touches |
|------|------|---------|
| **S0** | Spike: trace + confirm the live credential-delivery mechanism (above) | read-only + a throwaway probe |
| **T1** | Activate `WithGitHubLink` in `cmd/authsvc/main.go` (exchanger/store/client_id/redirect from `.env`), gated on config presence | `cmd/authsvc` |
| **T2** | `dev-github` lane (extend `dev-enforced`): WithGitHubLink active, GitHub App env, `AS_CP_URL`+`AS_CP_SECRET` faithful fanout, relaxed node→AS, garage journaling | `Justfile`, `mprocs-*.yaml` |
| **T3** | E1: CP auto-resolves creator's single `gh:<account>` → `credential_secret_id` for github mounts lacking an explicit credential | `internal/cp` (CreateSpawn / mounts.go) |
| **T4** | App-manifest github mount-slot marker (declare a mount is a github slot; user supplies repo) + bind validation | `internal/manifest`, `proto/`, `internal/cp` |
| **T5** | spawnctl `create --mount name=github:owner/repo[,create]` → set `CreateSpawnRequest.Mounts` | `cmd/spawnctl` |
| **T6** | web create-flow github-mount field (owner/repo + create_if_missing) → pass `mounts` to `createSpawn` | `web/` |
| **T7** | Example app whose manifest declares a journaled github mount slot | `examples/` |
| **T8** | Live verification runbook + checks, from both clients | docs / manual |

Ordering: **S0 → T1 → T2** (link foundation) → **T3, T4** (CP/manifest) → **T5, T6** (clients; depend on
T3/T4) → **T7** → **T8**. T4 is proto-touching (serialize; re-run `make gen`).

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
