# Spawn Fork — Clone a Running Spawn (Both Stay Active)

**Status:** designed 2026-06-14 · **Beads:** `sp-li7h` (epic); children `sp-pmay` (this spec),
`sp-dts5`, `sp-vr3v`, `sp-jdzb`, `sp-6344`, `sp-jkpv`. Deferred: `sp-5h3.8` (opencode fork).
**Builds on:** the migration machinery (`MigrateSpawn`/`resumeLocked`), the transient-tier Kopia
journal (`docs/superpowers/specs/2026-06-10-transient-tier-kopia-journal-design.md`), the encrypted
transfer set (`2026-06-13-encrypted-migration-transfer-set-design.md`), the writable-rootfs delta
(`2026-06-12-writable-rootfs-survival-design.md`), the transition-coordination claim primitive
(`2026-06-13-transition-coordination-design.md`), and owner-sealed key delivery
(`2026-06-10-owner-sealed-secrets-design.md`).

## Problem

Fork a running spawn: stand up a **new** spawn seeded from the source's state **at the fork-point**,
with **both source and fork remaining ACTIVE** and diverging independently afterward. This is
"`git branch` for a live workspace" — the user is mid-task and wants to try an alternative approach
in parallel without losing the current line of work.

Contrast **migration** (`MigrateSpawn`, `internal/cp/lifecycle.go`), which **suspends** the source
and moves it under the **same** spawn id. Fork diverges from migration in three ways: the source is
**not** suspended (it keeps running); a **new spawn id** is minted; and the journal lineage
**branches** so post-fork writes on each side never cross-pollinate.

## Decisions (settled in brainstorming)

1. **Use case = branch-&-explore.** The fork continues the same work, so conversation/context carries
   forward (see #2). Not a "template/duplicate" flow.
2. **Conversation inheritance = inherit + auto-continue.** The agents' session state lives in the
   **writable container rootfs** — claude-code `/root/.claude/projects`, codex `/root/.codex`
   (verified; neither is in the scrub list at `internal/spawnlet/manager.go`). Fork reuses migration's
   `captureRootfsArtifact=true` path (`docker commit` → OCI delta, `internal/runtime/docker_pod.go`),
   so that state branches into the fork verbatim. The fork's launcher starts the agent with
   `--continue`/`--resume` so it picks up the inherited session. **Tmux agents only** (claude-code,
   codex). opencode/ACP is deferred (`sp-5h3.8`): its SQLite/WAL state needs the `sp-5h3.6`
   quiesce-checkpoint before a live capture is clean.
3. **Placement = cross-node allowed, user-picked target** (default = source's node). Mirrors
   `MigrateSpawn`'s `target_node_id`/`target_class`. Same-node is the fast path (no key ceremony).
4. **Seeding = rehydrate + separate lineage in a SHARED Kopia repo.** The fork's journal lives in the
   **same Kopia repo / bucket / repo-password as the source**, addressed by a `lineage_root_id` — **not**
   a fresh `spawnery-spawn-<fork-id>` bucket. The fork is a **distinct snapshot lineage** within that
   repo (its own Kopia `SourceInfo`/tag), so manifests diverge and never cross-pollinate, while the
   shared content-addressed blob store makes the fork's gen-1 **seed snapshot dedupe to ~zero upload**.
   *(This supersedes the bead's original "new per-generation key + new bucket" sketch.)*
5. **Fork-point = brief quiesce.** The source stays Active, so the capture is taken under a momentary
   freeze — `docker pause` the source → `docker commit` (rootfs delta) + a drained **checkpoint
   snapshot** of the journaled mounts (pinned manifest) → `docker unpause`. Sub-second-to-seconds of
   source freeze for a crisp, mutually-consistent fork-point.

### Kopia multi-writer — confirmed supported (research, 2026-06-14)

Deep-research over Kopia official docs + maintainer/community sources confirmed (high confidence) that
**concurrent multi-client writes to one repository are officially supported**: repo-wide dedup,
per-(user,host) manifest partitioning, and an epoch-index design that tolerates uncoordinated
concurrent writers with **no corruption** (worst case is redundant compaction work). Sources:
kopia.io/docs/features, kopia.io/docs/repository-server, kopia issues #1090 / #3638. The shared-repo
lineage model (#4) is therefore sound. Derived **operational constraints** (baked into the design
below, not blockers):

- **Single Maintenance Owner per repo.** GC runs under one exclusive owner; an inactive owner *stalls*
  GC (no corruption). → pin the maintenance owner to the **lineage root / CP-coordinated identity**, so
  deleting the source spawn never strands GC for live forks.
- **Synced clocks** across nodes (epoch + maintenance depend on it) — an NTP requirement for cross-node
  forks.
- **Strong read-after-write** object storage — confirm Garage provides it (AWS S3 does by default).
  *(Early spike in `sp-dts5`.)*

## Architecture

### `ForkSpawn` RPC + handler (`sp-vr3v`)

New RPC mirroring `MigrateSpawn`'s shape but minting a new spawn rather than moving one:

```
ForkSpawn(source_spawn_id, target_node_id | target_class)
  -> { fork_spawn_id, transfer_set_id, node_id }
```

Handler flow (reusing migration primitives in `internal/cp/lifecycle.go` / `server.go`):

1. **Auth + precondition.** Owner-guarded; source must be **Active**. Resolve target via
   `sched.PickNodeID` (default = source's live node).
2. **Durability guard.** `guardCrossNodeDurability(source, sourceNode, targetNode, …)` — node-local
   mounts block a cross-node fork exactly as in migration; ephemeral seeded fresh; owner-sealed portable.
3. **Mint the fork.** `uuid.NewV7()` → new spawn id; `Spawns().Create` a new row + `Container{gen:1}`
   copying the source's app/version/model/runnable/mode/mounts. Record **lineage**:
   `parent_spawn_id` + `lineage_root_id` (= source's `lineage_root_id`, or the source id if the source
   is itself a root).
4. **Quiesce + capture the fork-point** on the **source** node, under a source claim that is
   **non-mutating to source status** (source returns to Active): `docker pause` → `docker commit`
   (rootfs OCI delta artifact) + **checkpoint snapshot** of journaled mounts (pinned manifest) →
   `docker unpause`.
5. **Stand up the fork** on the target via the `resumeLocked`/`Provision` path: restore the rootfs
   delta + restore the journaled mounts **from the shared repo at the pinned manifest** into the fork's
   host dirs (rehydrate), launch the fork agent with `--continue`.

### Storage — shared-repo lineage (`sp-dts5`)

- **One Kopia repo per lineage**, addressed by `lineage_root_id` (bucket, repo prefix, repo password
  all resolve to the lineage root — *not* `BucketFor(fork-id)`).
- **Distinct snapshot lineage per spawn** within that repo via a per-spawn `SourceInfo`/tag, so
  `latestManifest` stays correctly partitioned and the two chains diverge.
- **Dedup-free seed:** the rehydrated content already exists as blobs in the shared repo, so the fork's
  first snapshot uploads ~nothing.
- **`GenerationKeyManager` change:** add a "mint a per-`(fork-id, gen)` S3 key **allow-listed on the
  lineage-root bucket**" variant (no `EnsureBucket` of a new per-fork bucket); bucket/password custody
  keyed to `lineage_root_id`.
- **Checkpoint snapshot — new journal primitive.** Today: `RequestSnapshot` (live, best-effort) and
  `FinalSnapshot` (suspend barrier: drain queue → snapshot → teardown). Fork needs a third: **drain the
  serial queue + take one pinned snapshot, leave the mount live, re-arm the watcher** — no suspend, no
  status change. Paired with `docker pause` for the rootfs, this is the consistent fork-point.
- **Lifecycle refcount.** The shared repo/bucket/password + the maintenance-owner assignment are
  torn down only when the **last** lineage member is deleted. Deleting the source must not revoke the
  shared password, delete the bucket, or drop the maintenance owner while a fork is live. Per-`(spawn,gen)`
  key revocation stays per-spawn (safe); **bucket teardown becomes lineage-refcounted.**

### Cross-node delivery + keys (`sp-jdzb`)

When target ≠ source node:

- **Rootfs delta** rides the existing **encrypted transfer-set / Garage artifact** path
  (`sp-ei4.1.13`) to the target.
- **Shared repo password** (keyed by `lineage_root_id`) is delivered to the fork's target node via the
  existing owner-sealed ceremony (`GetSpawnNodeKey` → owner seals → `DeliverSecrets`). The fork's
  `journalKeys` entry **points at the lineage-root ciphertext**, not a fresh per-fork key.
- **Same-node fork needs no ceremony** — the password is already on the node. This is the demo default.

### Client surfaces (`sp-6344`)

- **Web:** spawn context-menu **"Fork"** action; target picker reuses `ListMigrationTargets`
  (default same-node). Fork lineage shown in the spawn list/detail.
- **spawnctl:** `spawnctl fork <id> [--node <id> | --class <class>]`.

## Scope / non-goals (MVP)

- ✅ Same-node fork (no ceremony) **and** cross-node fork (owner-sealed mounts).
- ✅ Auto-continue for **claude-code + codex** only.
- ❌ **opencode/ACP fork** — deferred to `sp-5h3.8` (blocked on `sp-5h3.6` SQLite checkpoint).
- ❌ Forking a **Suspended** source — MVP requires Active. (A suspended source already has a pinned
   manifest, so this is a smaller fast-follow.)
- ❌ Recursive fork-of-a-fork is *permitted* by the lineage model but only lightly tested in MVP.

## Risks / spikes (do early)

1. **[highest] Kopia concurrent writers on Garage, in practice.** Docs confirm support (above); the
   spike validates *our* backend: two independent writers (distinct `SourceInfo`) snapshotting one
   Garage-backed repo concurrently — verify no corruption, correct per-lineage `latestManifest`
   partitioning, and **Garage strong read-after-write** consistency. Gates `sp-dts5`.
2. **`docker pause` during in-flight inference.** Freezing the source mid-LLM-call could time out the
   upstream request. Keep the quiesce window minimal; verify the agent recovers on unpause.
3. **Refcounted teardown correctness.** Deleting the source while a fork is live must not revoke the
   shared password, delete the bucket, or strand the maintenance owner. Needs a dedicated test.

## Testing (`sp-jkpv`)

- **Same-node fork e2e:** fork an Active claude-code spawn → both stay Active → divergent writes on
   each side do not cross-pollinate → fork agent resumes the inherited conversation (`--continue`).
- **Cross-node owner-sealed fork e2e:** owner-sealed mount fork to a different node via the delivery
   ceremony; node-local mount on a cross-node fork is correctly **blocked** by the durability guard.
- **Refcounted-delete:** delete the source while the fork is live → fork's journal remains
   readable/writable; bucket/password survive; GC owner intact.
- **Seed-is-cheap assertion:** fork's gen-1 snapshot uploads ≈0 new blobs (dedup) — guards the
   shared-repo win.

## Task mapping

| Bead | Covers |
|------|--------|
| `sp-pmay` | this spec + the settled decisions |
| `sp-dts5` | checkpoint-snapshot primitive · shared-repo lineage · lineage-root key mint · refcount · **concurrent-writer spike** |
| `sp-vr3v` | `ForkSpawn` RPC + handler · `parent_spawn_id`/`lineage_root_id` columns + lineage |
| `sp-jdzb` | cross-node owner-sealed password delivery (lineage-root) · reused durability guard |
| `sp-6344` | web context-menu + spawnctl + lineage display |
| `sp-jkpv` | e2e suite above |

## Post-Implementation Notes

*As this design is implemented and iterated on — bug fixes, adjustments, anything that diverged from
the assumptions above — append a dated note here, whether or not a formal debugging skill was used.*
