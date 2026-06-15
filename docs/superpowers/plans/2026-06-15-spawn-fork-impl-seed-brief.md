# Spawn Fork — Implementation Seed Brief (sp-li7h)

**For:** a fresh session (or the autonomous-sdd multi-agent workflow) implementing the spawn-fork epic.
**Status going in:** design complete + roasted twice (v1 BLOCK → v2 REVISE → **v3** current). Beads carry
the binding deltas. No code written yet. **Read the spec before touching anything.**

## Mission

Implement **`ForkSpawn`**: clone a *running* spawn into a *new* spawn, both staying **Active** and
diverging independently — "git branch for a live workspace", with the agent conversation carried forward
(`--continue`). Epic **`sp-li7h`**.

## Read first (in order)

1. **Design spec (authoritative):** `docs/superpowers/specs/2026-06-14-spawn-fork-design.md` (v3).
2. **Why the design is shaped this way:** `docs/superpowers/specs/2026-06-14-spawn-fork-adversarial-review.md`
   (two roast passes — the failure modes the spec avoids are *here*; do not silently undo them).
3. **Substrate it builds on:** `2026-06-10-transient-tier-kopia-journal-design.md`,
   `2026-06-13-encrypted-migration-transfer-set-design.md`, `2026-06-12-writable-rootfs-survival-design.md`,
   `2026-06-13-transition-coordination-design.md`, `2026-06-10-owner-sealed-secrets-design.md`.
4. `bd show sp-li7h` and each child (`sp-dts5 sp-vr3v sp-jdzb sp-6344 sp-jkpv`) — the notes carry the
   per-task deltas; **trust the bead notes + spec over this brief if they ever disagree** (they were
   updated last).

## The 5 decisions (don't re-litigate — already brainstormed + roasted)

1. Branch-&-explore use case. 2. Inherit conversation + **auto-continue** (tmux agents claude-code/codex
only; opencode deferred `sp-5h3.8`). 3. Cross-node allowed, user-picked target (default same-node).
4. **Fully-isolated per-fork Kopia repo** (own bucket/password/keys; rehydrate + re-journal; full seed
upload — shared-repo dedup deferred `sp-3y92`). 5. **Pause-first single-pause** fork-point capture.

## Do the spikes FIRST (they gate real decisions)

| Spike | Question | Cheapest test | Gates |
|---|---|---|---|
| **H** | Does the encrypted transfer set compose for a `source_id→fork_id` seed into a **fresh** repo (vs migration's same-`spawn_id` gen chain)? | Source-rehydrate to staging → export under transfer key → import + re-journal on target into a new repo. | **`sp-jdzb`** — do before its impl |
| **F** | Does a torn trailing JSONL line hard-crash `--resume`? Does truncate-to-last-valid-record + `--continue` load cleanly (claude-code + codex)? | Corrupt a session JSONL trailing line → `--continue`. | conversation inherit (`sp-dts5`/launcher) |
| **E** | Real under-pause freeze duration (incl. node_modules); does a mid-turn fork abort the **source's** in-flight LLM turn? | Measure warm-pre-snapshot + under-pause scan+commit; fork mid-turn, watch the source turn. | freeze SLO; whether a turn-boundary gate is needed |
| **G** | Is the `Forking` recovery + failed-fork unwind idempotent across all death points? | Kill the fork driver pre-pause / mid-pause / post-unpause-pre-CAS; re-run unwind. | `sp-dts5`/`sp-vr3v` correctness |

File spike results back into the relevant bead notes; if a spike kills an assumption, amend the spec
before building on it.

## Substrate map (so you don't re-explore)

**CP (`internal/cp/`)** — fork mirrors migration:
- `lifecycle.go`: `MigrateSpawn` (the template), `resumeLocked` (provision/standup path — reuse for fork
  standup), `suspendLocked` (capture template, incl. `captureRootfsArtifact`), `withClaim`,
  `placementOverride`, `rootfsRestorePins`.
- `server.go`: `CreateSpawn` / `provisionSpawn` (new-spawn minting — `uuid.NewV7`, `Spawns().Create`).
- `migration_targets.go`: `ListMigrationTargets` / `EligibleTargets` (reuse for the fork target picker).
- `durability.go`: `guardCrossNodeDurability`, `classifyMounts` (reuse unchanged).
- `secrets.go` + `delivery_pending.go`: `GetSpawnNodeKey` / `DeliverSecrets` / `deliveryPendingTracker`
  (owner-sealed ceremony — deliver the **fork's new** password).
- `store/spawns.go`: `Create`, `ClaimStarting`, `SetActive`, `TransitionClaimed` (status_seq CAS).
  `store/types.go`: `Spawn`, `Container`, `TransferSet`.

**Journal (`internal/storage/journal/`)** — the new primitives live here:
- `manager.go`: `RequestSnapshot` (live), `FinalSnapshot` (suspend barrier: drain+snapshot+teardown),
  `state`/`mountState`, `passwordFor`. **Build:** a *non-tearing* final snapshot under pause +
  watcher pause/resume; **generation hold**.
- `repo.go`: `snapshotMount`, `restore` (uses `SkipOwners:true`), `latestForGeneration`, `sourceInfo`
  (`SourceInfo.UserName = spawnID`). `blob_s3.go`: `prefixFor`. `queue.go`: `Suspend` (drain pattern).
- `genkey.go`: `GenerationKeyManager` `Mint`/`BucketFor`/`RevokeGeneration`/`RevokeSuperseded`/`BackendFor`
  (fork mints with its **own** fork-id → isolated; **add** a hold so the source can't revoke/prune the
  pinned fork-point gen mid-seed). `ownersealed.go` / `custody.go`: password custody (fork seals its own).
- `artifact.go`: `putArtifact`/`getArtifact` (rootfs delta rides here).

**Node/runtime:**
- `internal/runtime/docker_pod.go`: `docker commit` / delta capture (`CaptureDelta`/`ExportTopLayer`) —
  **agent container only** pause. Note: the existing suspend path *stops* the container; fork **pauses**
  (cgroup freezer) then unpauses — different.
- `internal/spawnlet/manager.go`: mount layout + scrub list (`/root/.claude`, `/root/.codex` are NOT
  scrubbed — that's what carries conversation). `internal/node/attach.go`: `startSpawn`, `ProgressFunc`.
- `deploy/agent/launch`: per-runnable launch — add `--continue`/`--resume` + the torn-JSONL truncate.

**Contract:** `proto/cp/v1/cp.proto` — add `ForkSpawn` RPC near the migration RPCs; `make gen` after.

## Task graph + parallelization

```
spikes E/F/G/H  ──►  sp-vr3v (proto + RPC + store: parent_spawn_id, Forking status, disk gate, unwind+orphan-GC)
                          │
              ┌───────────┼───────────────┐
              ▼           ▼                ▼
          sp-dts5     sp-jdzb          sp-6344
       (pause-first  (cross-node:     (web ctx-menu +
        capture +     ceremony-first   spawnctl +
        gen-hold +    + source-side    Forking/seeding
        Forking       rehydrate +      client UX)
        recovery)     transfer-set
                      fork variant;
                      needs Spike H)
              └───────────┴───────────────┘
                          ▼
                       sp-jkpv (e2e suite)
```

- **`sp-vr3v` is the foundation** and owns `proto/` + the store schema — **serialize it first** (proto/gen
  conflicts otherwise). Everything else rebases onto it after `make gen`.
- `sp-dts5`, `sp-jdzb`, `sp-6344` then parallelize — **disjoint file sets** (journal+recovery vs
  transfer-set+secrets vs web+spawnctl). `sp-jdzb` waits on **Spike H**.
- `sp-jkpv` last (depends on all). Same-node path should be greenable before cross-node.

## Hard constraints (from CLAUDE.md — non-negotiable)

- **Build/test/lint/gen run inside the `dev-spawnery` distrobox**, never the bare host:
  `distrobox enter --root dev-spawnery -- bash -lc 'cd <wt> && <cmd>'`. Race tests need CGO:
  `CGO_ENABLED=1 go test -race ./...`. Lint: `GOTOOLCHAIN=go1.26.0 golangci-lint run ./...`, **0 issues**.
  `make gen` after any `proto/` change — never hand-edit `gen/`.
- **Every integration/e2e/lane test is build-tagged and FAILS (never `t.Skip`s) when its dep is down.**
  Fork e2e needs images (`make images`) + Docker; the journaled-data tests need Garage (`just garage` +
  `deploy/garage/dev-creds.env`). Tag them (`e2e`/`garage_e2e`); don't leave them untagged in the
  hermetic suite.
- **Each implementer works in its OWN git worktree + branch** (`spawnery-wt-<task>` / `feat/<task>`, cut
  from current master), never the main working tree. `bd` commands run **only from the main repo dir**
  (worktrees lack the Dolt DB). Merge `--no-ff`, run full gates in the distrobox, then `bd close` +
  export-commit + `git push` + `bd dolt push`. **Never push a red master.**
- **Model tiering (multi-agent):** planners + spec/quality reviewers = **opus**; implementers + fixers +
  merge integrators + final gate = **sonnet**. Don't economize the reviewers below opus.
- `git commit --no-verify` is the project norm; verify `bd close/update` survived after any branch op.

## Definition of done

- **Per task:** spec-compliant; quality gates green in the distrobox (race unit tests, lint 0, `make gen`
  clean); merged `--no-ff` to a green master; bead closed; pushed (`git push` + `bd dolt push`).
- **Epic:** the `sp-jkpv` e2e suite passes — same-node fork (both Active, no cross-pollination,
  `--continue` resumes), cross-node owner-sealed fork, node-local cross-node correctly blocked,
  fork-point coherence (no T0/T1 skew), idempotent stranded-source recovery, re-drivable failed-fork
  unwind, source mid-turn turn-abort recovers.

## How to run it

Per CLAUDE.md "Implementing Specced bd Tasks": run as **parallel subagents scheduled via the
`autonomous-sdd` workflow** (Workflow tool) — *not* a long-lived coordinator spawning opaque headless
processes. Per-task pipeline: planner → implementer(worktree) → spec-compliance review → code-quality
review → bounded fix loops (≤2/stage) → merge integrator (gates + `bd close` + export-commit + push +
`bd dolt push` + worktree cleanup). The workflow script is the coordinator; it encodes the graph above,
serializes `proto/`-touching `sp-vr3v` first, and serializes merges to master (one integrator at a time).

## Watch-outs (where this design has bitten reviewers)

- **Do not** re-split the capture into live-mount + paused-rootfs — that was the v2 REVISE regression
  (torn T0/T1 fork-point). Capture both under **one** pause.
- **Do not** share the source's repo/bucket/password with the fork — that was the v1 BLOCK. Isolated repos.
- **Do not** treat the failed-fork unwind or `Forking` recovery as fire-and-forget — they must be
  ordered, idempotent, re-drivable (bucket: empty-then-drop; row deleted **last**; orphan-GC sweep).
- **Do** hold the source's fork-point generation against its own prune/revoke/maintenance until seeding
  completes, and pause the source's journal watcher during capture.
- The source's claim must be **heartbeat-renewed** across the under-pause scan+commit (don't let a slow
  driver look dead to the recovery sweep).
