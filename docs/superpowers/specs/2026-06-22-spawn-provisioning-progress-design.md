# Spawn provisioning progress + failure surfacing (web + spawnctl)

**Status:** draft · **Date:** 2026-06-22 · Decided via brainstorm.

When a spawn is created, show **which provisioning step is running and how many are done of the
total** (clone repos, mint credentials, create pod, restore snapshot, pull image, start agent, …),
and on failure show **which step failed and the exact error**. Surfaced in both the **web** spawn
detail pane and **spawnctl**, by polling (no new streaming RPC).

## Motivation

Today a created spawn shows only a status enum. Provisioning runs **async** (`CreateSpawn` returns
`status=Starting`, then `go s.provisionSpawn(...)` in `internal/cp/server.go:1286`), and on failure
the spawn flips to `Errored` with **no reason retained** — the user sees only "error". The exact
cause (e.g. the github repo-create `403 [accepted-permissions=administration=write]`) lives only in
node logs.

**The failure data already reaches the CP and is then discarded.** The node calls
`a.status(spawnID, ERROR, err.Error())` (`internal/node/attach.go:710`) and the string travels in
`SpawnStatus.Detail` over the Attach stream; the CP scheduler forwards it to the in-flight
`Provision()` caller and drops it — `SetError(ctx, id)` (`internal/cp/store/spawns.go:367`) only
sets `status=Errored`, persisting no detail. So most of this feature is **stop discarding + extend
an existing progress pattern + render it**, not new plumbing.

There is already a progress precedent: suspend/resume surface live progress via
`transition_phase`/`transition_detail` on `SpawnSummary` (`proto/cp/v1/cp.proto:161-186`), shown in
web ("Suspending: syncing mounts") and spawnctl (`STATUS:phase`). It is in-memory (waiters), lost on
CP restart. We generalize it to the create path and persist the terminal failure.

## Decisions (brainstorm 2026-06-22)

1. **Coarse milestones, config-derived total.** ~22 fine operations roll up into a fixed ordered
   catalog of coarse milestones; each spawn includes the applicable subset (so "k of N" is honest and
   N is known at provision start). Not fine-grained, not two-level.
2. **Ephemeral live progress + persisted terminal failure.** Live step progress is in-memory
   (extends the `transition_phase` pattern); the failed step + full error is **persisted** on the
   spawn row so it survives CP restart and shows in a later `list`/detail.
3. **Full node error exposed** to clients (incl. GitHub's `accepted-permissions=…` diagnostic). No
   secrets are in these strings (the github client logs only the token-type prefix). Audience is the
   operator/creator. Not sanitized.
4. **spawnctl mirrors web's mechanism = polling.** Web polls `ListSpawns`; spawnctl polls the same
   `SpawnSummary` fields. No new streaming RPC.
5. **Web checklist lives in the spawn detail pane** (not the sidebar; the sidebar keeps its dot +
   short label).

## Milestone catalog (node-side)

A fixed, ordered catalog of milestone keys; the node includes the subset that will actually run for
a given spawn (computed from its config at provision start → that count is **N**). Fine operations
roll up under a milestone (per-mount clone → `prepare-mounts` with a per-mount label).

| key | covers (file:line) | included when |
|---|---|---|
| `authorize` | intent verify (`attach.go:629-655`) | always (instant unless `NODE_AUTH_MODE=enforced`) |
| `mint-credentials` | `mintGitHubMountsAtProvision` (`attach.go:678`), `consumeStartupGitHubSecrets` (`attach.go:687`) | github mount + mint configured, or github-token secrets present |
| `prepare-mounts` | mount `Prepare` loop / clone / seed (`manager.go:941-998`) | always (≥ the app's mounts; label = clone vs seed, per-mount detail) |
| `restore-snapshot` | journal restore (`manager.go:1021-1058`), rootfs delta restore (`manager.go:1346-1377`) | journaled resume with pins, or rootfs artifacts present (cross-node resume) |
| `create-pod` | secrets/artifacts/git-env dirs (`manager.go:1094-1141`), `StartPod` (`manager.go:1253`) | always |
| `setup-network` | ghControl `Serve` (`manager.go:1275`), git-proxy (`manager.go:1297`), egress floor (`manager.go:1312`), sidecar ready probe (`manager.go:1413`) | ghControl or egress enforced configured |
| `pull-image` | `EnsureImage` (`manager.go:1384`) | always |
| `start-agent` | `BeforeStartAgent` secret injection (`manager.go:1393`), `StartAgent` (`manager.go:1421`) | always |
| `await-ready` | ACP `initialize` + `session/new` (`attach.go:766`, `pump.go:258`) | non-tmux mode |

The catalog (keys, order, labels, inclusion predicates) is defined **once** on the node (it executes
provisioning and knows the config). Adding/reordering a milestone is a one-place change.

## Design

### Node emission
- At provision start, compute the applicable milestone list → `total`. Emit a step event at each
  milestone boundary: `{step_index, step_total, step_key, step_label, step_state}` where
  `step_state ∈ {running, done}`. This extends the progress emission already present
  (`a.status` + the `ProgressFunc`/resume-progress milestones at `attach.go:661,714,766` and
  `manager.go` callbacks) — now tagged with step identity.
- On failure, the failing step is known at the error site (each step already returns a
  step-identifying error). Emit `ERROR` carrying `step_key` + the full error in `detail` (the node
  already sends `detail`; we add `step_key`).
- Non-fatal steps (journal restore falling back to seed, `manager.go:1040,1052`) report `done`, never
  fail the spawn.

### Proto / transport
- `proto/node/v1/node.proto` `SpawnStatus`: add `step_index` (uint32), `step_total` (uint32),
  `step_key` (string), `step_label` (string). `detail` keeps the error string.
- `proto/cp/v1/cp.proto` `SpawnSummary`: add live-progress fields `provision_step` (uint32),
  `provision_total` (uint32), `provision_step_label` (string), and persisted-failure fields
  `error_step` (string), `error_detail` (string). Regenerate (`make gen`).

### CP
- **Live progress (ephemeral):** on a node `SpawnStatus` during `STARTING`, update an in-memory
  `spawnID → {index,total,label}` map (mirror the existing transition-waiter mechanism, extended to
  the create path). `ListSpawns` populates the `provision_*` `SpawnSummary` fields from it. Lost on
  CP restart, by design.
- **Persisted failure:** change `SetError` (`store/spawns.go:367`) to accept and store `error_step`
  + `error_detail` (new columns on the spawn row; truncate `error_detail` to ~8KB).
  `provisionSpawn` (`server.go:1296-1427`) already has the node error in hand — thread the step + the
  error string into `SetError` instead of dropping them. `ListSpawns` reads them onto
  `SpawnSummary.error_step`/`error_detail`.

### Clients (poll-based, full detail)
- **web** (`web/src/api/spawnlet.ts`, spawn detail pane component): map the new `SpawnSummary` fields
  into `SpawnView`. In the **detail pane**, render a provisioning checklist — the N milestones as
  done / running / pending with "k of N" — driven by the existing `ListSpawns` poll. On `error`,
  highlight the failed step (`error_step`) and show the full `error_detail` (expandable/monospace).
  The sidebar keeps its existing dot + short label.
- **spawnctl** (`cmd/spawnctl`): poll `SpawnSummary` (consistent with web). During create, print
  `[k/N] <label>` transitions until terminal; on failure print `✗ failed at <error_step>:
  <error_detail>`. `spawnctl list` shows the failed step for `ERROR` spawns; `spawnctl status <id>`
  (or the create tail) shows the full error.

### Edge cases
- CP restart mid-provision loses live steps (ephemeral); the terminal failure persists.
- `error_detail` truncated for store + proto.
- tmux-mode spawns omit `await-ready`.
- A spawn that reaches `ACTIVE` clears live progress; no stale steps linger.

## Testing
- **Node (hermetic):** milestone subset derived correctly from config (github vs not, resume vs
  fresh, tmux vs ACP); steps emitted in order with correct index/total; a failure attributes to the
  right `step_key` and carries the full error.
- **CP (hermetic):** status handler updates live progress during STARTING; `SetError` persists
  `error_step`+`error_detail` (and truncates); `ListSpawns`/`SpawnSummary` carries both live and
  persisted fields; restart drops live but keeps persisted.
- **web:** checklist renders done/running/pending + k/N; failure shows step + full error.
- **spawnctl:** progress line + failure formatting.

## File-touch map
- `proto/node/v1/node.proto`, `proto/cp/v1/cp.proto` + `make gen` (serialize the proto edit).
- `internal/node/attach.go`, `internal/spawnlet/manager.go` — milestone catalog + emission.
- `internal/cp/server.go` (status handler, `provisionSpawn`), `internal/cp/store/spawns.go` +
  migration (new columns), the scheduler if the detail path runs through it (`OnStatus`).
- `cmd/spawnctl/{create,list,status}.go` — poll + render.
- `web/src/api/spawnlet.ts` + spawn detail pane component — map + render checklist.

## Sequencing (keep master green)
1. Proto + gen + store columns/migration + `SpawnSummary`/`SpawnStatus` field plumbing (one change;
   build-coupled).
2. Node milestone catalog + emission.
3. CP live-progress map + `SetError` persistence + `ListSpawns` population.
4. Clients: spawnctl, then web (independent files; can parallelize once 1-3 land).

## Post-Implementation Notes

*As this design is implemented and iterated on — bug fixes, adjustments, anything that diverged
from the assumptions above — append a dated note here, whether or not a formal debugging skill was
used.*
