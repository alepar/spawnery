# Dev-Env Ergonomics: Persisted State + Multi-Tenant Dev Node

**Date:** 2026-06-20
**Status:** draft
**Goal:** stop having to match the dev node's startup params to the randomized AS account ID on every fresh stack, and stop re-linking GitHub every run.

## Problem

A `github`-mount spawn's owner is the AS **account ID**, which `oauth.go:72` mints as
`uuid.NewString()` on first registration (stable *within* an `authsvc.db`, keyed on the GitHub `sub`,
but new on every fresh/wiped DB). Self-hosted placement requires a match on that id in **two** gates:
- CP placement (`registry.go:142`): a `self-hosted` node is single-tenant — `n.Owner == spawnOwner`;
- node intent-verify (`cmd/spawnlet/main.go:482`): the spawn token's `account_id` must equal `NODE_OWNER`.

So every fresh dev stack (wiped `authsvc.db` → new random account ID) forces re-minting the dev node's
owner to that id — and because dev state is scattered (`.dev-ca`, `.spawns/authsvc.db`, `cp.db`,
garage creds) and routinely torn down, the GitHub OAuth **link** is lost too, forcing a re-link
(browser) every run. The T8 shakedown hit this repeatedly.

## Goal & non-goals

- **Goal:** the dev node's startup params never reference the random account ID; dev state (incl. the
  GitHub link) persists across `just dev` runs by default; a one-command reset.
- **Non-goal:** changing the production identity model (the account ID stays `uuid.NewString()` — its
  opacity is deliberate). This is a dev-ergonomics change only.

## Design

Three independent parts.

### A. `.envs/dev/` — one gitignored dev-state folder
Consolidate all repo-persisted dev artifacts under `.envs/dev/` so persistence is the default and reset
is `rm -rf .envs/dev`:

| Was | Now |
|---|---|
| `.dev-ca/` (CA + node identities) | `.envs/dev/dev-ca/` |
| `.spawns/` (data root incl. `authsvc.db`) | `.envs/dev/data/` |
| `cp.db` (CP store, cwd default) | `.envs/dev/cp.db` |
| `deploy/garage/dev-creds.env` | `.envs/dev/garage-creds.env` |

`.gitignore` gains `/.envs/`. Because `authsvc.db` now persists, the AS account *and* the
`gh:<accountID>` GitHub link survive across runs → **no re-OAuth, no re-link**. Garage's *data* already
lives in Docker volumes (out of the repo); only its creds file moves. Existing `.dev-ca`/`.spawns`/`cp.db`
are abandoned in place (no migration — a fresh `.envs/dev/` is created on next `just dev`; the old
artifacts can be deleted).

Justfile wiring: the `devca` and `data_root` vars repoint under `.envs/dev/`; `cp-github` sets
`CP_STORE_DSN=file:{{repo}}/.envs/dev/cp.db?…`; the garage recipe + its bootstrap write/read the creds at
`.envs/dev/garage-creds.env`, and the node recipes source that path.

### B. Cloud-class dev node (reuse built-in multi-tenancy)
A `cloud` node is **multi-tenant** by construction — `eligibleForOwner` returns true for any owner
(`registry.go:143-146`) — so it needs no account-ID match at all. The dev node switches to cloud:

- **`gen-dev-ca` mints a cloud node identity.** `spawnery-ca dev` is extended to also emit a **cloud
  intermediate** (`cloud-intermediate.pem`/`-key.pem`, `inter := root.NewIntermediate(ClassCloud)`) and a
  **cloud node identity** (`.envs/dev/dev-ca/node-cloud/`, `IssueNode("node-1", "spawnery-system",
  ClassCloud, …)`). The existing self-hosted node identity stays (for `dev-enforced`). Both chain to the
  same dev `root.pem`, so the CP — which validates to `CP_NODE_ROOT_CA` and reads class+owner from the
  cert SAN — accepts the cloud node as `class=cloud` with no CP change.
- **`node-github` (now the `just dev` node) runs cloud:** `NODE_CLASS=cloud`, `NODE_OWNER` **unset**
  (so the node-side owner check at `main.go:482` is skipped), `NODE_ID_DIR=…/dev-ca/node-cloud`.

Result: any logged-in user's spawns place on the dev node regardless of their account ID, and the node's
startup never references it.

### C. Egress-floor force-off override (default off)
Cloud class forces the egress floor (`egressEnforced() = NodeClass=="cloud" || EgressEnforce`,
`manager.go:382`), but the rootless dev node can't run `iptables` (the only firewall backends shell out
to it). Today's dev already runs **floor-off** (self-hosted + `EGRESS_ENFORCE=false`), so we keep that
posture with one explicit, default-off override:

- New config `ManagerConfig.EgressFloorForceOff` (bool, default `false`) + env `EGRESS_FLOOR_FORCE_OFF`
  read in `cmd/spawnlet/main.go`.
- `egressEnforced()` returns `false` when it's set, **before** the class/enforce logic:
  ```go
  func (m *Manager) egressEnforced() bool {
      if m.cfg.EgressFloorForceOff {
          return false // DEV-ONLY override; MUST NOT be set in production
      }
      return m.cfg.NodeClass == "cloud" || m.cfg.EgressEnforce
  }
  ```
- Default off ⇒ **"cloud always enforces" still holds in prod**; only the explicit dev opt-out forces it
  off. The `node-github` recipe sets `EGRESS_FLOOR_FORCE_OFF=1`. The flag is loud and commented as
  dangerous/dev-only, mirroring `AS_DEV_RELAX_NODE_AUTH`.

This is **not** a regression: dev runs floor-off today; this just preserves that under cloud class.

## Files touched

- `.gitignore` — add `/.envs/`.
- `Justfile` — `devca`/`data_root` vars → `.envs/dev/{dev-ca,data}`; `cp-github` `CP_STORE_DSN`;
  garage recipe + node recipes → `.envs/dev/garage-creds.env`; `gen-dev-ca` emits the cloud identity;
  `node-github` → `NODE_CLASS=cloud` + `NODE_OWNER` unset + `NODE_ID_DIR=node-cloud` +
  `EGRESS_FLOOR_FORCE_OFF=1`.
- `cmd/spawnery-ca/main.go` — `dev` subcommand also emits the cloud intermediate + cloud node identity.
- `internal/spawnlet/manager.go` — `EgressFloorForceOff` field + `egressEnforced()` override.
- `cmd/spawnlet/main.go` — read `EGRESS_FLOOR_FORCE_OFF`.
- `deploy/garage/*` (bootstrap) — write the creds env at `.envs/dev/garage-creds.env`.

## Testing

- **Hermetic unit:** `egressEnforced()` returns false when `EgressFloorForceOff` is set even with
  `NodeClass=="cloud"`; returns the existing result when off. (Add to the manager egress tests.)
- **pki:** `spawnery-ca dev` produces a verifiable cloud node identity (chains to root, SAN
  class=cloud, accountID `spawnery-system`).
- **Manual / lane:** fresh box → `just dev` → link GitHub once → create a `github`-mount spawn under the
  logged-in account → it places on the cloud dev node with **no** account-ID minting; `rm -rf .envs/dev`
  resets; a second `just dev` run reuses the persisted account + link (no re-link). Confirm the floor is
  off (no iptables errors) and the MITM proxy still reaches GitHub.

## Post-Implementation Notes

*As this design is implemented and iterated on — bug fixes, adjustments, anything that diverged from the
assumptions above — append a dated note here, whether or not a formal debugging skill was used.*
