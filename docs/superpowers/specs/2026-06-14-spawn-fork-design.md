# Spawn Fork — Clone a Running Spawn (Both Stay Active)

**Status:** designed 2026-06-14 · **v2 2026-06-15** (roast BLOCK → isolated per-fork repos) ·
**v3 2026-06-15** (roast REVISE → pause-first consistent capture; cross-node + compensation hardening).
· **Beads:** `sp-li7h` (epic); children `sp-pmay` (this spec), `sp-dts5`, `sp-vr3v`, `sp-jdzb`,
`sp-6344`, `sp-jkpv`. Deferred: `sp-5h3.8` (opencode fork), `sp-3y92` (shared-repo dedup-free seed).
**Builds on:** the migration machinery (`MigrateSpawn`/`resumeLocked`), the transient-tier Kopia
journal (`2026-06-10-transient-tier-kopia-journal-design.md`), the encrypted transfer set
(`2026-06-13-encrypted-migration-transfer-set-design.md`), the writable-rootfs delta
(`2026-06-12-writable-rootfs-survival-design.md`), the transition-coordination claim primitive
(`2026-06-13-transition-coordination-design.md`), and owner-sealed key delivery
(`2026-06-10-owner-sealed-secrets-design.md`).
**Adversarial review:** `2026-06-14-spawn-fork-adversarial-review.md` (roast v1 → BLOCK drove v2; roast
v2 → REVISE drove v3).

## Problem

Fork a running spawn: stand up a **new** spawn seeded from the source's state **at the fork-point**,
with **both source and fork remaining ACTIVE** and diverging independently afterward. "`git branch`
for a live workspace" — the user is mid-task and wants to try an alternative approach in parallel
without losing the current line of work.

Contrast **migration** (`MigrateSpawn`, `internal/cp/lifecycle.go`), which **suspends** the source and
moves it under the **same** spawn id. Fork diverges: the source is **not** suspended; a **new spawn
id** is minted; and the new spawn gets its **own fully-isolated journal repo** seeded from the source.

## Decisions (settled in brainstorming; storage decision revised post-roast)

1. **Use case = branch-&-explore.** The fork continues the same work, so conversation/context carries
   forward (see #2). Not a "template/duplicate" flow.
2. **Conversation inheritance = inherit + auto-continue.** The agents' session state lives in the
   **writable container rootfs** — claude-code `/root/.claude/projects`, codex `/root/.codex` (verified;
   neither is in the scrub list at `internal/spawnlet/manager.go`). Fork reuses migration's
   `captureRootfsArtifact=true` path (`docker commit` → OCI delta, `internal/runtime/docker_pod.go`), so
   that state branches into the fork. The fork's launcher starts the agent with `--continue`/`--resume`.
   **Tmux agents only** (claude-code, codex). opencode/ACP deferred (`sp-5h3.8`).
3. **Placement = cross-node allowed, user-picked target** (default = source's node). Mirrors
   `MigrateSpawn`'s `target_node_id`/`target_class`. Same-node is the fast path.
4. **Seeding = rehydrate + re-journal into a fully-ISOLATED per-fork repo.** *(Revised v2.)* The fork
   gets its **own** Garage bucket (`spawnery-spawn-<fork-id>`), its **own** per-generation S3 keys, and
   its **own** Kopia repo password — *nothing is shared with the source*. The fork is seeded by
   **rehydrating** the source's fork-point manifest into the fork's host dirs and **re-journaling** from
   gen 1 into its own repo. This pays a **full seed-upload cost** (no cross-source dedup) in exchange for
   keeping every substrate invariant intact: spawn-level isolation, per-`(spawn,gen)`-key delete-fencing,
   single-owner `force=true` maintenance, and per-spawn prune anchors all stay valid, exactly as for any
   freshly-created spawn. **No lineage refcount, no shared maintenance owner, no shared crypto domain.**
   *(The shared-repo "dedup-free seed" alternative was roasted → BLOCK for breaking isolation, the
   delete-fence, and Kopia's maintenance model; deferred to `sp-3y92` with the full challenge list.)*
5. **Fork-point capture = pause-first, single consistent instant.** *(Revised v3 — v2's live-mount +
   paused-rootfs split was roasted REVISE: it pinned the mounts at T0 and the rootfs/session at T1 with
   the agent writing in between, so the fork inherited a session referencing workspace edits the snapshot
   predates. v3 captures **both** artifacts under **one** pause so the fork-point is a single coherent
   instant — this restores the brainstormed "brief quiesce" intent.)* Sequence:
   - **(a) Warm pre-snapshot, live (off the critical path).** Trigger a normal live journal checkpoint of
     the mounts *before* pausing and await it; this pre-uploads almost all blobs (Kopia dedup), shrinking
     the work left to do under pause.
   - **(b) Pause the agent container** (the sidecar keeps running — see Source-liveness), `sync` the
     container filesystem, then under that single pause take **both**: the **final** mount checkpoint
     (re-walk picks up only post-warm deltas; pinned manifest) **and** the `docker commit` of the fork's
     **own** rootfs OCI delta. **unpause.**
   The freeze ≈ (under-pause incremental scan + commit), bounded by the warm pre-snapshot. Because Kopia
   has no dirty-path API the re-walk is still scan-bound, so the freeze has an explicit **SLO + a "forking…"
   UX**, and the duration is the #1 spike. The source's continuous journal **watcher is paused** for the
   capture (as suspend does) so no background snapshot races the final one. The source is restored to
   running before the fork stands up on the target.

### Lineage (display only)

The fork records `parent_spawn_id` for **UI/lineage display** only. It is **not** used for storage
addressing, key custody, or maintenance ownership (those are per-spawn, as for any spawn). Deleting the
source has **no** effect on the fork — fully independent repos.

### Kopia note (v2)

The deep-research confirmed Kopia *supports* concurrent multi-writer repos, but **v2 does not rely on
that** — each spawn (source and fork) keeps its own single-writer repo, exactly as the substrate is
built today. The single-maintenance-owner, synced-clock, and read-after-write constraints therefore
apply **per repo, single-writer**, i.e. nothing new beyond what every spawn already requires.

## Architecture

### `ForkSpawn` RPC + handler (`sp-vr3v`)

```
ForkSpawn(source_spawn_id, target_node_id | target_class)
  -> { fork_spawn_id, transfer_set_id, node_id }
```

Handler flow (reusing migration primitives in `internal/cp/lifecycle.go` / `server.go`):

1. **Auth + precondition.** Owner-guarded; source must be **Active**. Resolve target via
   `sched.PickNodeID` (default = source's live node).
2. **Durability guard + placement gate.** `guardCrossNodeDurability(...)` — node-local mounts block a
   cross-node fork (ephemeral seeded fresh; owner-sealed portable). **New:** a scheduler
   **disk-headroom gate** for fork placement. The transient peak is **~2.5–3×** the source's footprint,
   not 2×: a same-node fork concurrently holds the restored mount copy **+** the rootfs OCI delta **+**
   the fork's freshly re-journaled local Kopia seed staged for upload. The gate uses the scheduler's
   local-disk-headroom signal and is re-checked at materialization (TOCTOU against other spawns).
3. **Ceremony-first (cross-node).** Per the transfer-set contract, the owner-key ceremony + delivery of
   the **transfer key** to the **source** node happens **before** capture/upload — *not* after the pause.
   The fork's **own new repo password** is owner-sealed-minted and queued for delivery to the target now.
4. **Mint the fork.** `uuid.NewV7()` → new spawn id; `Spawns().Create` a new row + `Container{gen:1}`
   copying the source's app/version/model/runnable/mode/mounts. Record `parent_spawn_id` (display).
5. **Fork-point capture** on the source node, under a source claim, source in the **`Forking` transient
   status** (see Source-liveness): warm pre-snapshot (live) → pause agent + `sync` → final mount
   checkpoint **and** rootfs `docker commit` under one pause → unpause. **Hold** the source's fork-point
   generation (manifest + per-gen S3 key + referenced blobs) against the source's own
   revoke-on-supersede / prune / `force=true` maintenance until seeding completes (released on success
   **or** unwind). Source returns to **Active**; claim released.
6. **Stand up the fork** on the target via the `resumeLocked`/`Provision` path, seeding the fork's
   **own** repo:
   - **Same-node:** restore the held fork-point mount manifest + rootfs delta locally into the fork's host
     dirs; the fork's journaler re-journals from gen 1 into its **own** bucket/repo (own password sealed
     locally).
   - **Cross-node:** the **source node rehydrates** the pinned manifest into a staging dir (it has its own
     password), then exports those **plain files** + the rootfs delta into a **fork transfer-set variant**
     encrypted under the **transfer key** (keyed `source_id → fork_id`, *not* the migration `(spawn_id,
     gen)` chain — the existing transfer set must grow this variant). The target restores from the transfer
     set into the fork's own host dirs and re-journals into the fork's own repo. The target **never** holds
     the source's repo password — only the fork's new password is owner-sealed-delivered (`sp-jdzb`).
   - **Fork-ready SLO:** the fork is usable once its **first durable gen-1 snapshot** exists; standup
     blocks on that (full seed re-upload, no cross-source dedup). State the SLO; surface "seeding…".
   - Launch the fork agent with `--continue` (see Spike F for the torn-session repair).

### Source-liveness during capture (`sp-dts5` / `sp-vr3v`)

A fork pauses the source, so a fork-driver death mid-capture must not strand it. The source enters a
**`Forking` transient status** for the capture (claim + `status_seq` CAS, per transition-coordination).
The **recovery sweep** is extended to repair a stranded source, and must be **idempotent + pause-phase-
aware**:

- **Driver death, node alive** (the recoverable case): the sweep sees `Forking` + expired claim →
  `docker unpause` **only if the container is actually paused** (unpause of a running container errors —
  tolerate it) → revert status to **Active** under a `status_seq` CAS. Safe to re-drive: a sweep that
  unpaused but crashed before the CAS leaves a running+`Forking` source the next sweep fixes.
- **Wedged worker, claim never expires** (driver alive, heartbeating, but the scan hangs): add a
  **capture deadline** so a wedged `Forking` source is force-reverted rather than frozen forever.
- **Claim TTL vs commit duration:** the driver **heartbeats/renews** the claim across the (bounded but
  non-trivial) under-pause scan+commit, so a slow-but-alive driver is not mistaken for dead.
- **Node death** is **not** a new failure mode: an `Active` spawn already dies if its node reboots, and a
  paused container is lost the same way. A source-node death during `Forking` resolves like any node loss
  (`Unreachable`/`Errored` + user notice); only driver-death-with-node-alive needs the unpause sweep.
- **Pause scope = agent container only**; the sidecar (model proxy) keeps running. The frozen agent
  stops draining its localhost socket, so a fork taken **mid-turn may abort the source's in-flight LLM
  turn** (backpressure → provider idle-timeout; the sidecar may synthesize a clean `message_stop`,
  masking the truncation). The source recovers by erroring/retrying that turn. Bounded by the freeze SLO;
  see Spike E. MVP accepts this (forking mid-turn can interrupt the source's current turn).
- **Concurrent source ops:** the `Forking` claim also fences a user `Stop`/`Suspend`/`Migrate` of the
  source for the brief capture window (they queue or fail-fast, like any claimed transition).

### Failed-fork cleanup (compensating transaction)

Fork creation is independent of the source, but the unwind is a **multi-resource compensation** and must
be **idempotent, ordered, and re-drivable** (not a fire-and-forget `revertOnFail`):

- **Order:** fence the fork's `status_seq` → revoke the fork's gen key → **empty then drop** the fork
  bucket (Garage `DeleteBucket` fails `BucketNotEmpty` once a partial seed has written objects, so the
  unwind must delete objects first) → **delete the fork row last** (the row is the durable record an
  orphan-GC sweep keys on; deleting it first would orphan the bucket/key).
- **Orphan-GC sweep:** a periodic sweep reclaims half-created forks (row in a failed/aborted state with a
  bucket/key still present) and any fork stranded by a driver death **during standup** — the recovery
  sweep covers the source; this covers the fork side.
- Release the source's held fork-point generation (above) on both the success and unwind paths.

### Client surfaces (`sp-6344`)

- **Web:** spawn context-menu **"Fork"**; target picker reuses `ListMigrationTargets` (default
  same-node). `parent_spawn_id` shown in spawn list/detail.
- **spawnctl:** `spawnctl fork <id> [--node <id> | --class <class>]`.
- **`Forking` client semantics:** the source stays presented as Active, but while it sits in `Forking`
  (the brief capture freeze) client input to the source is **queued/stalled**, not lost, and resumes when
  the source unpauses — surfaced as a short "forking…" stall, not a status change. The **fork** shows
  "seeding…" until its first durable gen-1 snapshot (the fork-ready SLO).

## Scope / non-goals (MVP)

- ✅ Same-node fork **and** cross-node fork (owner-sealed mounts), isolated per-fork repos.
- ✅ Auto-continue for **claude-code + codex** only.
- ❌ **opencode/ACP fork** — deferred to `sp-5h3.8` (blocked on `sp-5h3.6` SQLite checkpoint).
- ❌ **Shared-repo dedup-free seed** — deferred to `sp-3y92` (roast BLOCK).
- ❌ Forking a **Suspended** source — MVP requires Active (smaller fast-follow).

## Risks / spikes (do early)

1. **[F — torn-session `--continue` repair]** Even captured under pause, the session JSONL has no fsync
   guarantee, so the trailing line can be torn. The pinned `spawnery/agent:dev` Spike F probe did **not**
   reproduce the assumed local JSON parse crash for either claude-code or codex: both clients still
   advanced far enough to hit the localhost fake provider with a torn trailing line. The same probe also
   confirmed that **deterministically truncating the session log to its last valid JSONL record** removed
   positive bytes and still let both clients reach the provider afterward. *Plan:* keep `sync` in the
   capture flow (already in capture step 5); treat truncate-to-last-valid-record as a conservative repair
   step before launching `--continue`, but do not rely on a hard-crash assumption when defining launcher or
   client fallback behavior. Fresh-session fallback remains out of scope for MVP unless later implementation
   evidence shows no robust continue path. (Each side has its **own copy** of the session dir → no
   shared-session-id collision; they resume independently.)
2. **[E — freeze SLO + source turn-abort]** The under-pause incremental scan (Kopia has no dirty-path
   API → scan-bound) + `docker commit` (~4 s / 1.2 GB measured) set the freeze. *Question:* real pause
   duration distribution, and does the **source's** in-flight LLM turn survive it (frozen agent →
   backpressure → provider idle-timeout; does the sidecar mask the drop)? *Cheapest test:* measure
   under-pause scan+commit on a representative tree (incl. node_modules); fork mid-turn and observe the
   source turn. *Kill criteria:* if pauses routinely exceed the upstream idle timeout, add a turn-boundary
   gate. State an explicit freeze SLO; the warm pre-snapshot bounds the under-pause scan.
3. **[G — recovery + unwind idempotency]** Verify the recovery sweep repairs a stranded `Forking` source
   idempotently and pause-phase-aware (driver death pre-pause, mid-pause, post-unpause-pre-CAS; wedged
   worker → capture deadline); and that the failed-fork unwind is re-drivable (empty-then-drop bucket;
   row deleted last; orphan-GC reclaims a half-created fork).
4. **[H — transfer-set fork variant]** *Result:* journal-level composition works when the source node
   restores the pinned mount manifest to staging, exports those plain files plus the source rootfs delta
   through a transfer-key envelope bound to `source_id → fork_id`, and the target imports that payload
   into fork host dirs before re-journaling as fork generation 1. The existing migration transfer-set
   row is still not expressive enough as the durable CP contract because it has one `spawn_id` and
   migration-style `source_generation`/`target_generation`; `sp-jdzb` must introduce an explicit fork
   transfer-set variant or equivalent fields naming both `source_spawn_id` and `fork_spawn_id`, with
   pins interpreted as source artifacts until import and fork artifacts after re-journal.

## Testing (`sp-jkpv`)

- **Same-node fork e2e:** fork an Active claude-code spawn → both stay Active → divergent writes do not
  cross-pollinate (independent repos) → fork agent resumes the inherited conversation (`--continue`).
- **Cross-node owner-sealed fork e2e:** owner-sealed mount fork to a different node via the transfer set +
  fork-password delivery; node-local mount on a cross-node fork is correctly **blocked** by the guard.
- **Isolation assertion:** deleting the **source** while the fork is live leaves the fork's repo fully
  readable/writable (independent bucket/password) — no shared teardown.
- **Fork-point coherence:** a file written during the (pre-pause) warm scan is captured consistently in
  **both** the mount snapshot and the session under the single pause — no T0/T1 skew (the v2 regression).
- **Stranded-source recovery (idempotent):** kill the fork driver pre-pause / mid-pause / post-unpause →
  the sweep reverts the source to Active exactly once; a wedged capture hits the deadline.
- **Failed-fork cleanup (re-drivable):** force a standup failure after a partial seed → unwind empties +
  drops the bucket, revokes the key, deletes the row last; re-running the unwind is a no-op; source intact.
- **Source turn-abort (Spike E):** fork mid-turn → the source's current turn errors/retries cleanly and
  the source stays Active.

## Task mapping

| Bead | Covers |
|------|--------|
| `sp-pmay` | this spec + settled decisions |
| `sp-dts5` | pause-first capture (warm pre-snapshot + single-pause mount-checkpoint+commit) · isolated per-fork repo seeding · fork-point generation **hold** · `Forking` transient + **idempotent pause-phase-aware** recovery + capture deadline |
| `sp-vr3v` | `ForkSpawn` RPC + handler · `parent_spawn_id` · disk-headroom gate (~2.5–3×) · **ordered/idempotent** failed-fork unwind + orphan-GC sweep |
| `sp-jdzb` | cross-node **ceremony-first** + source-side rehydrate-to-staging + **transfer-set fork variant** (`source_id→fork_id`) + fork-password owner-sealed delivery · reused durability guard |
| `sp-6344` | web context-menu + spawnctl + lineage display + `Forking`/"seeding…" client semantics |
| `sp-jkpv` | e2e suite above |

## Post-Implementation Notes

*As this design is implemented and iterated on — bug fixes, adjustments, anything that diverged from
the assumptions above — append a dated note here, whether or not a formal debugging skill was used.*

- **2026-06-15 (v2, pre-implementation):** roast v1 returned BLOCK. Root cause: decision #4's shared-repo
  lineage broke spawn isolation, the per-`(spawn,gen)`-key delete-fence, and Kopia's single-maintenance-
  owner model. Reverted #4 to **isolated per-fork repos**; deferred the shared-repo optimization to
  `sp-3y92`. (roast panel degraded by a monthly spend cap — 51/90 full panels.)
- **2026-06-15 (v3, pre-implementation):** roast v2 returned **REVISE** (full clean panel 85/85, no
  blockers). The headline major: v2's live-mount + paused-rootfs **split** pinned the two artifacts at
  different instants → torn fork-point (session refs workspace edits the snapshot predates); "same as
  suspend/resume" was false (suspend pauses first). Fixed by reverting #5 to a **pause-first single-pause
  capture** (warm live pre-snapshot bounds the under-pause scan), restoring the brainstormed quiesce
  intent. Other confirmed clusters folded in: hold the source's fork-point generation against its own
  prune/revoke/maintenance (Theme 6); make the `Forking` recovery + the failed-fork unwind **idempotent,
  ordered, pause-phase-aware** compensations with an orphan-GC sweep + capture deadline + claim heartbeat
  (Theme 2); ceremony-first + **source-side rehydrate** + a **transfer-set fork variant** for cross-node
  seeding (Theme 4); a **deterministic torn-JSONL truncate-and-`--continue`** repair (Theme 5); a
  corrected ~2.5–3× disk gate, a fork-ready SLO, and defined `Forking`/"seeding…" client semantics
  (Theme 7); documented that a mid-turn fork may abort the **source's** in-flight turn (Theme 3, accepted
  for MVP). Spikes E/F/G/H carried above.
- **2026-06-15 (Spike F):** Torn-session JSONL probe against pinned `spawnery/agent:dev` found that
  neither claude-code nor codex produced the expected local JSON parse failure on a corrupted trailing
  JSONL line; both still reached the localhost fake provider before and after deterministic truncation to
  the last valid record. The ForkSpawn launcher/client plan was adjusted accordingly; raw evidence is
  stored in Beads `sp-li7h.1`, with implications copied to `sp-dts5` and `sp-6344`.
- **2026-06-15 (Spike H):** Hermetic journal spike confirmed the substrate composes for `source_id →
  fork_id`: source repo rehydrate to staging, transfer-key-bound tar payload containing plain mount files
  plus rootfs delta, target import, and fork gen-1 re-journal into a fresh repo. The CP transfer-set model
  still needs a fork variant before `sp-jdzb` implementation because the current `migration_transfer_sets`
  schema has a single `spawn_id`; fork orchestration needs durable source and fork spawn IDs so the target
  never interprets source pins as fork-owned artifacts before import/re-journal.
