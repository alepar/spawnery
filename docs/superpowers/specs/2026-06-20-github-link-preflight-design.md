# GitHub-Link Preflight at CreateSpawn

**Date:** 2026-06-20
**Status:** draft
**Epic:** sp-m859 (MVP gaps) — adjacent to the github-mount work (sp-n7iy)

## Problem

Creating a spawn whose app declares a `github:` mount when the creator hasn't **linked** their GitHub
account fails **deep in provisioning** — the node's at-provision mint calls the AS, which returns
`CodeNotFound "github link not found"` (`internal/authsvc/github_mint.go:83`). By then a spawn record
exists and the user sees an opaque error. (Login ≠ link: the user can be authenticated yet have no
`gh:<accountID>` link.) We want a **server-enforced preflight** at `CreateSpawn` that rejects early,
before persisting the spawn, with an actionable message — for every client (web and `spawnctl`).

## Main challenge

The CP cannot answer "is this account linked?" today: it has **no AS client** (the AS only pushes to
the CP via the fanout; the CP never calls the AS), and its `githubLinkIndex` tracks per-spawn secret
delivery, not per-account link state. The link source of truth is the AS
(`GitHubLinks().Get("gh:"+account)` + the `relink_required` flag). So this adds a **new CP→AS
direction**, mirroring the existing AS↔CP shared-secret RPC auth.

## Design (decisions)

### 1. AS endpoint — `POST /internal/github/link-status` (plain HTTP, shared-secret auth)
The AS serves plain HTTP (`internal/authsvc/handler.go` mux), so add a plain handler rather than a
Connect RPC (no proto churn). Auth mirrors the AS→CP fanout: require header
`X-Spawnery-AS-Secret: <AS_CP_RPC_SECRET>` (same shared secret already configured as
`CP_AS_RPC_SECRET` on the CP); 401 on mismatch; the route is only registered when the secret is set
(internal, server-to-server only — not CORS/SPA-exposed).
- **Request:** `{ "account_id": "<accountID>" }` (JSON).
- **Handler:** `secretID = "gh:"+account_id`; `link, err := GitHubLinks().Get(ctx, secretID)`. Map to:
  `{ "status": "active" }` (found, `relink_required == 0`), `{ "status": "relink_required" }` (found,
  flagged), or `{ "status": "none" }` (`ErrNotFound`). 200 on all three; 401 bad secret; 500 on a
  real store error.

### 2. CP→AS client — gated on `CP_AS_URL`
New CP config `CP_AS_URL` (the AS base URL; in dev the `cp-github` recipe sets
`CP_AS_URL=http://127.0.0.1:8090`). A small `asLinkChecker` (own file, e.g.
`internal/cp/github_link_preflight.go`) holds the base URL + the `CP_AS_RPC_SECRET` (already read at
`cmd/spawnery_cp/main.go:248`) + an `*http.Client`; `LinkStatus(ctx, accountID) (status, error)` POSTs
to `CP_AS_URL/internal/github/link-status` with the secret header. **When `CP_AS_URL` is unset the
checker is nil and the preflight is skipped** (back-compat: non-github dev lanes and hermetic tests
without an AS are unaffected).

### 3. `CreateSpawn` preflight (server.go ~953)
Early in `CreateSpawn`, **after** resolving the app's declared mounts and the owner but **before**
persisting the spawn record: if any declared mount has a `github:` backend URI **and** the link
checker is configured, call `LinkStatus(owner)` and branch:
- `none` → `connect.NewError(connect.CodeFailedPrecondition, …)`: *"GitHub account not linked — link
  your GitHub account in Settings before creating a spawn that uses a github mount."*
- `relink_required` → `CodeFailedPrecondition`: *"GitHub link needs re-authorization — re-link GitHub
  in Settings."*
- checker error (AS unreachable / non-200) → `connect.CodeUnavailable`: *"couldn't verify your GitHub
  link (auth service unavailable) — try again."* (**fail-closed**: don't create a doomed spawn; the
  distinct code lets clients tell "not linked" from "couldn't check".)
- `active` → proceed.

The owner is the authenticated creator's account id (the same value the CP derives `gh:<owner>` from
for the mount credential). Tests use an interface seam so the hermetic suite needs no real AS.

### 4. Wiring
`cp-github` recipe: add `CP_AS_URL=http://{{addr_as}}` (CP_AS_RPC_SECRET already `dev-as-cp-secret`).

## Files touched

- `internal/authsvc/handler.go` (+ a small handler, e.g. `github_link_status.go`) — the
  `/internal/github/link-status` route + shared-secret check, registered when `AS_CP_RPC_SECRET` is set.
- `internal/cp/github_link_preflight.go` (new) — the `asLinkChecker` (interface + http impl) and the
  status enum.
- `internal/cp/server.go` — construct the checker from config; call the preflight in `CreateSpawn`.
- `cmd/spawnery_cp/main.go` — read `CP_AS_URL` into the server config.
- `Justfile` — `cp-github` sets `CP_AS_URL`.
- Tests: AS handler (active/relink/none/bad-secret); CP `CreateSpawn` preflight via a fake checker
  (none→FailedPrecondition, relink→FailedPrecondition, error→Unavailable, active→proceeds; non-github
  app + nil checker → unaffected).

## Testing

Hermetic unit tests (above) — no real AS/network. Manual: with the dev stack, create a `github`-mount
spawn while unlinked → immediate `FailedPrecondition` (no spawn record created); link GitHub → create
succeeds.

## Post-Implementation Notes

*As this design is implemented and iterated on — bug fixes, adjustments, anything that diverged from
the assumptions above — append a dated note here, whether or not a formal debugging skill was used.*
