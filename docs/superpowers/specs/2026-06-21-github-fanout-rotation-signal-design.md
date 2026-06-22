# Convert GitHub proactive fanout to a rotation heads-up signal; nodes pull from AS

**Status:** draft · **Date:** 2026-06-21 · Decided via brainstorm (resumed + roast-revised).

Fixes the github-mount spawn provisioning failure AND simplifies GitHub-token delivery by turning
the round-3 proactive **token fanout** (which seals and delivers token bytes per target, requiring
per-target secret templates) into a token-free **"link rotated" heads-up signal**. Each node, on
receipt, invalidates its cached token and pulls a fresh one **directly from the AS** over the
existing node-identity-authed mint path. A light sidecar 401-retry backstops the missed-signal /
in-flight straggler case.

> **Supersedes** the first draft of this spec ("Remove GitHub proactive fanout; rely on
> pull-on-expiry refresh"), which a roast **BLOCKed**: its load-bearing premise — "old access
> tokens stay valid until their own expiry" — is false (GitHub invalidates the predecessor token on
> refresh; our own spike `sp-v40s.3` confirmed it). Pure deletion would reintroduce the very 401
> window the round-3 fanout was built to close. This revision keeps the proactive *invalidation*
> (as a signal) while dropping the token *delivery* (the source of the bug).

## Symptom

github-mount spawn provisioning fails:

```
failed_precondition: github mount "repo": mint access token: unavailable:
github fanout target <spawnID> has no secret templates
```

Chain: `internal/node/attach.go:671` (`mintGitHubMountsAtProvision`) → AS `MintGitHubAccessToken`
→ `FanoutGitHubAccessToken` → `sealForTarget` (`internal/authsvc/github_fanout.go:79`).

## Root cause

The AS mint (`internal/authsvc/github_mint.go:121` and `:221`) triggers a proactive fanout that
**seals the new token for each live target spawn**. Sealing hard-requires per-target secret
templates (`github_fanout.go:79-80`, `return nil, fmt.Errorf("github fanout target %s has no secret
templates", ...)`). The D3 live-dev flow seeds spawns into the CP `githubLinkIndex` **without** a
template (`seedGitHubMintLinks` → `note(spawnID, secretID)`; deliberate, `cp/github_fanout.go:224-227`).
The requesting spawn is itself in the templateless fanout set, the AS-side seal hard-errors (unlike
the **best-effort** CP-side fanout at `cp/github_fanout.go:178-207`), the error surfaces as
`connect.CodeUnavailable` (`github_mint.go:131`), and the whole mint → the whole provision fails.

**Not a config-framework (sp-0sqa) regression:** the github-mint code (sp-ache.4/.10, sp-v40s
round-3) predates the config merge `94b2eeb`; that merge touched no github-mint files.

## The corrected model (what round-3 actually decided)

From the round-3 design (`2026-06-14-github-credentials-and-storage-unified-design.md`, §16):

- **GitHub kills the old token on rotation.** Spike `sp-v40s.3`: "refresh rotation immediately
  invalidates the predecessor access token." Token expiry is an opt-out App setting; we keep
  short-lived expiring tokens deliberately (the whole MITM-proxy containment posture depends on it).
- **One link = one single-active *shared* access token** across all of a user's concurrent spawns
  (Decision 24 / §16.1), because single-use refresh makes independent per-spawn tokens impossible.
- **AS-custodial, node never holds the refresh token** (§16.2), after a round-2 roast BLOCKed the
  node-retains-refresh posture on node-compromise grounds.
- **The per-refresh 401 window was explicitly accepted** (§16.4): "until the fanout reaches a node
  its in-flight agent git operations may 401 … This is the accepted MVP cost; **the git-proxy
  upgrade removes it.**" Reactive-on-401 was called *impossible* only because git runs in the agent
  and **there was no proxy** to intercept the 401.

**That proxy now exists** — `sp-n7iy` / the 2026-06-19 sidecar MITM proxy (`internal/sidecar/githubproxy.go`).
So the reactive path §16.4 named as the future upgrade is available today. This design executes that
planned evolution: replace the proactive token *delivery* (fanout) with a proactive *invalidation
signal* + on-demand pull, backstopped by the reactive proxy retry §16.4 anticipated.

## Design: heads-up signal, not payload

1. **AS rotates a link** (near the ~8h expiry, triggered by some node's mint). The AS already
   caches the new shared token (`github_mint.go` write-ahead→commit) and holds the per-secretID lock,
   so it single-flights (`github_mint.go:77-79`). No change to *when/how* it rotates.
2. **AS emits a token-free signal to the CP:** `{ link_ref, version, delivery_id,
   access_expires_at_unix }` — **no token bytes, no per-target sealing, no templates.**
3. **CP relays the signal** to the nodes currently hosting a spawn on that link (reusing the
   fanout's hosting-node knowledge — the templateless `has()` index). The CP relays only non-secret
   metadata and stays blind (strictly *more* blind than the old sealed-bytes relay).
4. **Each node lazy-invalidates** its cached token for the link and advances its
   `(version, delivery_id)` pointer from the signal. It does **not** eagerly re-pull. The next
   actual git/API request through the sidecar proxy finds no valid cache and pulls a fresh token
   **directly from the AS** (`MintGitHubAccessToken`, node-identity authZ). The AS returns its
   already-rotated cached token — **no force flag needed**, because the rotation already happened
   AS-side.

### Why this resolves the roast blockers

| Roast finding | Resolution |
|---|---|
| Premise "old tokens stay valid" is false (blocker) | Embraced, not denied — the signal *is* the proactive invalidation GitHub's rotation-kills-old-token behavior demands. |
| Phase-2 force-refresh can't re-rotate the AS (major/blocker) | Not needed. Rotation happens AS-side first; a node re-pull returns the already-rotated token. The `force_refresh` proto field and AS cache-bypass are dropped entirely. |
| Deleting the fanout strands version advancement; the AS version-mismatch gate (`github_mint.go:118-146`) loses its only feeder (blocker) | The signal carries `version`/`delivery_id`; the node advances its pointer from it. `noteGitHubRefresh` (`internal/cp/secrets.go:300,403`) real-version semantics are preserved — verify the gate still matches on signal-advanced pointers during impl. |
| Reintroduces the 401 window (blocker, vs pure deletion) | Closed for connected nodes by the signal; bounded for stragglers by the 401-retry backstop + §16.4 reconnect/resume re-sync. |
| Custody / CP-blindness | Improved — CP relays non-secret metadata, never sealed token bytes. |

## Phase 1 — fanout-of-token → signal-of-rotation

**Proto-touching** (`make gen` required).

### Proto / gen
- `proto/cp/v1/cp.proto`: replace the `FanoutGitHubSealedAccessToken` RPC (carries sealed bytes) and
  the `GetGitHubLinkTargets` RPC (returns targets *with* `secret_templates`) with a single token-free
  `SignalGitHubTokenRotated` (or fold into an existing notify) carrying `{ link_ref, version,
  delivery_id, access_expires_at_unix }`. Remove the `GitHubLinkTarget.secret_templates` field and the
  `GitHubSealedAccessTokenDelivery` message. `make gen`; never hand-edit `gen/`.
- **Wire-contract test:** update `internal/wirecheck/github_secrets_test.go` (asserts
  `GitHubLinkTarget.secret_templates` field #6, ~`:296-306`) — it will fail after `make gen` if not
  updated in the same change.

### AS side
- `internal/authsvc/github_mint.go`: the two fanout calls (`:121`, `:221`) become a **signal emit**
  (link_ref/version/delivery_id/expiry), not a seal-and-deliver. Remove the
  `CodeUnavailable`-on-seal-failure path (`:131`).
- Delete the sealing path in `internal/authsvc/github_fanout.go` — `sealForTarget`,
  `verifyTargetNode`, the template requirement. Whether the file survives as a thin signal-notifier
  or is deleted in favor of a CP-notify call is an implementation choice; the **sealing + templates**
  must go.
- `internal/authsvc/service.go`: the `GitHubAccessTokenFanout` notifier surface (`:107-126`,
  `:193-195`, field `:47`) is retyped to a token-free rotation-signal notifier (or removed if the
  emit goes through an existing CP-notify seam).
- `cmd/authsvc/main.go:293`: update the wiring accordingly.

### CP side
- `internal/cp/github_fanout.go`: remove `GetGitHubLinkTargets` (`:272`), replace
  `FanoutGitHubSealedAccessToken` (`:384`) with the token-free signal relay, and delete the
  **template machinery** (`secretTemplates` field on `githubLinkRecord` `:24`; template args to
  `note`/`noteNodeSecrets`/`noteCPSecrets`). **KEEP** `authorizeGitHubMint` (`:111`),
  `seedGitHubMintLinks` (`:228`), `prepareGitHubMintProvision` (`:242`), and the hosting-node
  `has()` index — the index is both the mint authorization AND the signal's recipient set.
- **Complete the inventory** (roast-flagged omissions): the now-orphaned CP helpers
  `githubLinkTargets()` and lowercase `fanoutGitHubSealedAccessToken()`; the out-of-file
  `noteNodeSecrets`/`noteCPSecrets` callers (`internal/cp/secrets.go:233`, `internal/cp/server.go:1426`,
  `internal/cp/lifecycle.go`); the §16.4 reconnect-resync references that rode the sealed-delivery
  path (the resync now re-pulls instead of replaying a sealed blob — keep the resync, repoint it).
- `cmd/spawnery_cp/main.go`: update handler registration for the replaced/removed RPCs.
- `internal/cp/auth/service_scoped_test.go`: update references to the changed RPCs.

### Node side
- `internal/node/secrets.go` (signal receiver): on the rotation signal, **invalidate** the cached
  token for the link and advance the `(version, delivery_id)` pointer; do **not** eager-pull.
- `internal/node/github_refresh.go`: reconcile the now-stale fanout coupling — `refreshInFlightGrace`
  ("wait for the sealed fanout to arrive"), the succeed/attempt counters, and any logic that
  discards a minted token pending a sealed fanout (`~:50`, `:374-440`). The refresher keeps minting
  on its own expiry schedule; it no longer waits for a sealed push.

### Tests
- Delete/rewrite `internal/authsvc/github_fanout_test.go` and `internal/cp/github_fanout_test.go` to
  the signal contract.
- `internal/authsvc/github_mint_test.go`: retarget the fanout-notifier setup to the signal notifier.
- `internal/node/github_refresh_e2e_test.go`: replace the `capturedFanout` (`:57-84`) sealed-delivery
  assertions with signal-emit + node-invalidate-and-repull assertions.
- `internal/cp/github_mint_resolve_test.go`: drop `GetGitHubLinkTargets` mocks; keep
  `authorizeGitHubMint` / `prepareGitHubMintProvision` coverage.

## Phase 2 — light sidecar 401-retry backstop

For missed-signal stragglers (node disconnected during the relay) and ops in flight at the instant
of rotation. **No AS `force_refresh`** — recovery is node-cache-local.

- `internal/sidecar/githubproxy.go`: today `DoFunc` (`:104-141`) is an `OnRequest` short-circuit
  that **never sees the upstream response**. Change it to let a token-bearing request reach GitHub
  and inspect the response: on a **401**, invalidate the proxy/`githubControl` cache
  (`internal/sidecar/githubcontrol.go:78`, the 5-min short-circuit) and re-pull, then retry the
  request **once**. If the retry also 401s, return as-is (this is the grant-revoked kill-switch case
  — the refresh chain is dead, a re-pull can't help, and breaking is intended per §16.5).
- `internal/node/github_refresh.go`: the re-pull must actually clear the node cache so `GetToken`
  doesn't serve the stale token, and must not be defeated by the `minMintInterval=10s` floor
  (`~:407-413`, which returns the stale cached token with `ErrGitHubMintRateLimited`). Since the
  signal already invalidated the cache in the common path, the floor interaction only matters for
  the pure-401 straggler; ensure invalidate-then-mint pierces it.
- Bound: one forced re-pull + one retry per 401; the floor + the once-retry prevent a storm.

## Verify / gates

- `make gen` first; then build + full hermetic `go test ./...` (`-race`, in the `dev-spawnery`
  distrobox). Grep for orphaned symbols: `sealForTarget`, `FanoutGitHubSealedAccessToken`,
  `GetGitHubLinkTargets`, `secretTemplates`, `GitHubLinkTarget` (the deleted message),
  `noteNodeSecrets`/`noteCPSecrets` template args, `githubLinkTargets`.
- `just lint` → 0 issues.
- **Single-spawn smoke** (the reported bug): a github-mount spawn provisions and a `git`/`gh` op
  works through the sidecar proxy. Must pass — this is the failure being fixed.
- **Concurrent-sibling rotation smoke** (the case the old design's premise missed): two spawns on
  the same link; force a rotation; confirm the non-rotating sibling receives the signal, invalidates,
  re-pulls the new token on its next op, and does **not** serve a dead token.
- **Straggler backstop:** drop the signal to one node (simulate disconnect during relay); confirm its
  next op 401s once, the proxy re-pulls, and the retry succeeds.

## Implementation sequencing (keep master green)

The proto edit removes RPCs/messages that AS+CP code references, so the proto change and all its
referencing deletions/rewrites must land **together** to avoid a red master (roast-flagged: the
prior "task 1 = proto, task 2 = AS" order left master red at the task-1 merge). Suggested:

1. **Phase 1 as one tightly-coupled change** (proto + gen + AS signal-emit + CP signal-relay +
   node signal-receive/invalidate + wirecheck + all tests), merged `--no-ff` with full gates green.
   The CP/AS/node/proto coupling is too tight to parallelize without a red intermediate.
2. **Phase 2 (sidecar 401-retry)** as a separable follow-on change — different package
   (`internal/sidecar` + a node `GetToken` invalidation tweak), sequenced after Phase 1.

## Related epics

- sp-ache — .4 CP auto-resolve creator link → mount credential; .10 node mint-at-provision.
- sp-v40s — round-3 AS-custodial refresh + CP fanout (§16; the fanout being converted; §16.4 named
  the git-proxy upgrade this realizes).
- sp-n7iy — sidecar MITM proxy / node credential server (the reactive path now relied on).
- sp-u53.1 / sp-dl62 — github storage backend + integration.

## Post-Implementation Notes

**2026-06-22 — Changes vs. original design (epic sp-v40s.22, implemented; branch `feat/sp-v40s.22-fanout-signal`).**
Implemented as specified across both phases; both per-task reviews (spec + quality) and a final
whole-epic review passed, gates green (`make gen`, `go test -race ./...`, golangci-lint 0).
Concrete deltas the implementation settled that the spec left open:
- **Two RPCs collapsed into one.** With no sealing the AS no longer needs per-target data, so
  `GetGitHubLinkTargets` + `FanoutGitHubSealedAccessToken` were replaced by a single
  `SignalGitHubTokenRotated(secret_id, version, delivery_id, access_expires_at_unix)` (AS→CP).
- **New CP→node carrier.** Added a `GitHubTokenRotatedSignal` message + `github_token_rotated`
  variant (tag 21) on the `proto/node/v1` `CPMessage` oneof; the CP relays it per hosting spawn
  with live generation over the existing Attach substrate.
- **`internal/authsvc/github_fanout.go` survived** as a thin token-free notifier (kept the
  CP-client seam) rather than being deleted.
- **Version-gate feeder unchanged in code:** the node `Invalidate` advances the same
  `(version, delivery_id)` fields with the same values the sealed delivery used, so the AS gate
  (`github_mint.go`) matched without modification.
- **Phase 2 force path needed a new `force_refresh` bool** on `proto/sidecar/v1` `GetTokenRequest`
  — a large `min_remaining` alone is defeated by the node's 10s `minMintInterval` floor, so the
  forced re-pull clears `st.token`/`st.lastMintAt` to pierce both the sidecar 5-min cache and the
  node floor. No AS `force_refresh` (the AS already holds the rotated token).
- **Bounded straggler self-heal (by design):** after a Phase 2 force re-pull a straggler caches the
  new token but keeps its old `(version, delivery_id)`; its next proactive refresh re-presents the
  stale tuple and takes the AS stale-older + re-signal path (at most one redundant mint) until a
  signal lands and advances the pointer. Self-healing on reconnect; intended, not a defect.

*As this design is implemented and iterated on — bug fixes, adjustments, anything that diverged
from the assumptions above — append a dated note here, whether or not a formal debugging skill was
used.*
