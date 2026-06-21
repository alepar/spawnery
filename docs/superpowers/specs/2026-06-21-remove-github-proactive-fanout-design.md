# Remove GitHub proactive fanout; rely on pull-on-expiry refresh

**Status:** draft · **Date:** 2026-06-21 · **Decided via brainstorm** (resumed from prior session).

Fixes the github-mount spawn provisioning failure AND simplifies GitHub-token delivery to a
single pull-based path. Removes the round-3 AS-custodial proactive fanout (sp-v40s §16) as
redundant with the already-working pull-on-expiry refresh, and adds sidecar 401-detect-and-retry
robustness for the mid-spawn revoke case the fanout never handled well.

## Symptom

github-mount spawn provisioning fails:

```
failed_precondition: github mount "repo": mint access token: unavailable:
github fanout target <spawnID> has no secret templates
```

Chain: `internal/node/attach.go:671` (`mintGitHubMountsAtProvision`) → AS `MintGitHubAccessToken`
→ `FanoutGitHubAccessToken` → `sealForTarget` (`internal/authsvc/github_fanout.go:79`).

## Root cause

The AS mint (`internal/authsvc/github_mint.go:121` and `:221`) triggers a **proactive fanout** to
every live spawn sharing the secret. The D3 live-dev flow seeds spawns into the CP
`githubLinkIndex` **without** a template (`seedGitHubMintLinks` → `note(spawnID, secretID)`;
deliberate — see comment at `internal/cp/github_fanout.go:224-227`: at-provision JIT mint only
needs `has(secretID, spawnID)`; proactive-fanout-which-needs-templates is "out of scope for the
live-dev demo (D3)").

The AS-side fanout **hard-errors** on a templateless target (`github_fanout.go:79-80`,
`return nil, fmt.Errorf(...)` — NOT best-effort), and that error is surfaced to the node as
`connect.CodeUnavailable` (`github_mint.go:131`), failing the whole mint → failing provision. The
requesting spawn is itself in the fanout set, seeded templateless.

**The inconsistency that makes it a bug:** the CP-side fanout (`cp/github_fanout.go:178-207`) **is**
best-effort ("a per-spawn problem must not deny the requesting node its token"); the AS-side is not.

**Not a config-framework (sp-0sqa) regression:** the github-mint code (sp-ache.4/.10, sp-v40s
round-3) predates the config merge `94b2eeb`; that merge touched no github-mint files; all
github-lane config vars are aliased + delivered correctly.

## Why the fanout is redundant (verified)

The "pull-only" model the deletion converges on is **already implemented**:

- **Pull-on-expiry already works:** the sidecar checks the token's remaining lifetime on every
  request and calls `GetToken` under a 5-min buffer (`internal/sidecar/githubcontrol.go:78`,
  `minRemainingSeconds = 300`) → node refresher mints only if stale, with a per-spawn floor
  (`internal/node/github_refresh.go:374-440`) → AS mint.
- **AS already caches + single-flights:** a per-secretID lock is taken BEFORE fetching the link
  (`github_mint.go:77-79`) so concurrent stale pulls trigger ONE rotation; the mint returns the
  cached token if expiry > now+10min (`github_mint.go:148`, `githubMintRefreshLead = 10m`);
  write-ahead then commit.
- **Old access tokens stay valid until their own expiry** — the AS rotates the refresh chain, not
  the live access token out from under other spawns — so no proactive push is needed; spawns
  refresh lazily on their own expiry.
- **The token never transits the CP** (direct AS↔node). Initial provision mints direct + renders to
  node storage (does NOT use fanout) — only the *refresh* path is affected by this change.
- Removing the fanout **directly fixes the bug** (the mint no longer fans out, so it never hits the
  templateless target). The single-active-token property is preserved by the AS cache + lock; only
  the proactive PUSH is removed.

## Scope decision (owner-confirmed 2026-06-21)

- **Phase 1 + Phase 2 together.** Delete the proactive fanout AND add sidecar
  401-detect-and-retry robustness in this implementation.
- **Delete sp-v40s round-3 fanout: confirmed.** It is recent, twice-roasted design, but redundant
  with the already-working pull path + the sidecar proxy. Remove it (not the band-aid).

---

## Phase 1 — remove the proactive fanout

Mostly deletion. **This is a proto-touching change** — the CP fanout RPCs are defined in
`proto/cp/v1/cp.proto` and generated into `gen/`, so a `make gen` is required.

### AS side

- `internal/authsvc/github_mint.go`: drop the two fanout calls (`:121`, `:221`, both guarded by
  `s.githubTokenFanout != nil`). The mint just returns the cached/fresh token to the requester;
  other spawns refresh lazily on their own expiry. Remove the `CodeUnavailable`-on-fanout-error
  path (`:131`) that this introduced.
- Delete `internal/authsvc/github_fanout.go` (entire) — `cpGitHubAccessTokenFanout`,
  `GitHubFanoutCP` interface, `NewCPGitHubAccessTokenFanout`, `FanoutGitHubAccessToken`,
  `sealForTarget`, `verifyTargetNode`.
- `internal/authsvc/service.go`: remove the `githubTokenFanout` field (`:47`), the
  `GitHubAccessTokenFanout` struct (`:107-116`), `GitHubAccessTokenFanoutNotifier` interface
  (`:118-120`), `GitHubAccessTokenFanoutFunc` (`:122-126`), and `WithGitHubAccessTokenFanout`
  option (`:193-195`).
- `cmd/authsvc/main.go:293`: remove the `WithGitHubAccessTokenFanout(NewCPGitHubAccessTokenFanout(...))`
  wiring.

### CP side

- `internal/cp/github_fanout.go`: remove the `GetGitHubLinkTargets` handler (`:272`), the
  `FanoutGitHubSealedAccessToken` handler (`:384`), and the **template machinery**
  (`noteNodeSecrets`/`noteCPSecrets` template arguments + the `secretTemplates` field on
  `githubLinkRecord` at `:24`).
- **KEEP** `authorizeGitHubMint` (`:111`), `seedGitHubMintLinks` (`:228`),
  `prepareGitHubMintProvision` (`:242`). The templateless `has()` index is the **authorization**
  that the requesting node legitimately hosts the linked spawn; only sealing/templates/push go.
  `githubLinkIndex` (`:18-29`) shrinks to a has-set (spawnID under secretID) — drop the
  `secretTemplates` field and template params from `note`/`noteNodeSecrets`/`noteCPSecrets`.
- `cmd/spawnery_cp/main.go`: remove registration of the two deleted RPC handlers.
- `internal/cp/auth/service_scoped_test.go`: drop references to the removed RPCs.

### Proto / gen

- `proto/cp/v1/cp.proto`: remove the `FanoutGitHubSealedAccessToken` and `GetGitHubLinkTargets`
  RPC definitions and the now-orphaned request/response messages (incl. the `secret_templates`
  field carrier). Run `make gen`; never hand-edit `gen/`.

### Tests

- Delete `internal/authsvc/github_fanout_test.go` and `internal/cp/github_fanout_test.go`.
- `internal/authsvc/github_mint_test.go`: remove the `WithGitHubAccessTokenFanout` /
  `GitHubAccessTokenFanoutFunc` setup (≈7 references) and any fanout-call assertions.
- `internal/node/github_refresh_e2e_test.go`: remove the `capturedFanout` struct + assertions
  (`:57-84`) and the `WithGitHubAccessTokenFanout` wiring.
- `internal/cp/github_mint_resolve_test.go`: audit/remove `GetGitHubLinkTargets` mocks; keep the
  `authorizeGitHubMint` / `prepareGitHubMintProvision` coverage.

---

## Phase 2 — sidecar 401-detect-and-retry

`internal/sidecar/githubproxy.go` `DoFunc` (`:104-141`) does NO inspection of GitHub responses —
refresh is purely expiry-timed, so a token **revoked** (or dying inside the 5-min window) sends one
failing request to GitHub before the next expiry check.

Add: on a **401 from GitHub** for a token-bearing request, force a fresh mint (`GetToken` with a
force flag / a large `minRemaining` so it bypasses the node cache) and retry the request **once**.
This covers the revoke / early-invalidation case the fanout never handled well anyway.

- Retry at most once (no loops); if the retry also 401s, return the response as-is.
- Only force-refresh for requests that actually carried a swapped token (don't refresh on a 401 to
  an unauthenticated request).
- Thread the force flag through the node `GetToken` path (`internal/node/github_refresh.go`) so it
  bypasses the staleness short-circuit; respect the existing per-spawn floor enough to avoid a
  revoked-token request storm hammering the AS (a single forced mint per 401, the once-retry
  bound, and the floor together bound it).

---

## Verify / gates

- Build + full hermetic `go test ./...` (with `-race`, in the `dev-spawnery` distrobox) after
  deletion; `make gen` first. Grep for any remaining references to removed symbols:
  `FanoutGitHubAccessToken`, `FanoutGitHubSealedAccessToken`, `GetGitHubLinkTargets`,
  `sealForTarget`, `githubTokenFanout`, `WithGitHubAccessTokenFanout`,
  `GitHubAccessTokenFanoutNotifier`, `GitHubAccessTokenFanoutFunc`, `secretTemplates`,
  `NewCPGitHubAccessTokenFanout`, `cpGitHubAccessTokenFanout`, `GitHubFanoutCP`.
- `just lint` → 0 issues.
- Smoke the dev-github lane (`just dev` or cp-github + node-github + authsvc-github): a
  github-mount spawn provisions, and a `git`/`gh` op works through the sidecar proxy (token minted
  direct from AS, no fanout).
- Phase 2: revoke/expire a token mid-spawn and confirm the proxy transparently refreshes (one 401,
  forced mint, retry succeeds).

## Implementation sequencing (multi-agent)

This touches `proto/` + `gen/`, so the proto-edit task must be **serialized** (no parallel
proto-touching task). Suggested order:

1. **Proto + gen** (serial first): edit `proto/cp/v1/cp.proto`, `make gen`, update
   `cmd/spawnery_cp/main.go` handler registration. Leaves the build red on the deleted CP/AS impl
   until task 2 lands — so fold the CP-handler deletion into the same task to keep master green.
2. **AS deletion**: `github_fanout.go`, `service.go`, `github_mint.go` (drop calls), `main.go`,
   AS tests. Disjoint files from the CP/proto task except the shared expectation that the RPCs are
   gone — sequence after (or merge-integrate with) task 1.
3. **Phase 2 sidecar 401-retry**: `internal/sidecar/githubproxy.go` + `internal/node/github_refresh.go`
   force flag + a unit test. Largely disjoint from 1/2 (different package); can run in parallel
   with the deletion once the `GetToken` signature change is agreed, else sequence last.

Because the deletion spans CP + AS + proto with tight cross-package coupling (the build is red
until all the impl/handler/wiring deletions land together), Phase 1 is best done as **one or two
tightly-coupled tasks rather than many parallel ones**; Phase 2 is the cleanly-separable parallel
task.

## Related epics

- sp-ache — .4 CP auto-resolve creator link → mount credential; .10 node mint-at-provision.
- sp-v40s — round-3 AS-custodial refresh + CP fanout (**the thing being removed**).
- sp-n7iy — sidecar MITM proxy / node credential server (the pull path being relied on).
- sp-u53.1 / sp-dl62 — github storage backend + integration.

## Post-Implementation Notes

*As this design is implemented and iterated on — bug fixes, adjustments, anything that diverged
from the assumptions above — append a dated note here, whether or not a formal debugging skill was
used.*
