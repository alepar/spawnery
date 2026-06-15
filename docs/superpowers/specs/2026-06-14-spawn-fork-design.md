# Spawn Fork — Clone a Running Spawn (Both Stay Active)

**Status:** designed 2026-06-14 · **revised v2 2026-06-15** (post-roast BLOCK → reverted to isolated
per-fork repos; shared-repo seed-dedup deferred to `sp-3y92`). · **Beads:** `sp-li7h` (epic);
children `sp-pmay` (this spec), `sp-dts5`, `sp-vr3v`, `sp-jdzb`, `sp-6344`, `sp-jkpv`. Deferred:
`sp-5h3.8` (opencode fork), `sp-3y92` (shared-repo dedup-free seed optimization).
**Builds on:** the migration machinery (`MigrateSpawn`/`resumeLocked`), the transient-tier Kopia
journal (`2026-06-10-transient-tier-kopia-journal-design.md`), the encrypted transfer set
(`2026-06-13-encrypted-migration-transfer-set-design.md`), the writable-rootfs delta
(`2026-06-12-writable-rootfs-survival-design.md`), the transition-coordination claim primitive
(`2026-06-13-transition-coordination-design.md`), and owner-sealed key delivery
(`2026-06-10-owner-sealed-secrets-design.md`).
**Adversarial review:** `2026-06-14-spawn-fork-adversarial-review.md` (roast v1 → BLOCK; this v2 is the
response).

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
5. **Fork-point capture = live mount checkpoint (no pause) + brief pause for the rootfs only.**
   *(Revised v2.)* The expensive part — scanning the journaled tree — happens **live**, off the critical
   freeze path:
   - **(a) Mount checkpoint, live + awaited.** Trigger a journal checkpoint snapshot of the source's
     mounts *while the source runs*, await its completion, and **pin** the resulting manifest. Consistency
     is the journal's existing best-effort live-snapshot semantics (same as suspend/resume); no pause.
   - **(b) Rootfs capture under a brief agent-only pause.** `docker pause` **the agent container only**
     (not the sidecar) → `docker commit` → the fork's **own** rootfs OCI delta → `docker unpause`. The
     freeze ≈ commit time, **not** journal scan time.
   The source is restored to running before the fork stands up on the target.

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
   **disk-headroom gate** for fork placement — a same-node fork fully materializes the restored mounts
   **and** the rootfs delta on the target's local disk (~2× the source's footprint), so placement must
   verify headroom/quota.
3. **Mint the fork.** `uuid.NewV7()` → new spawn id; `Spawns().Create` a new row + `Container{gen:1}`
   copying the source's app/version/model/runnable/mode/mounts. Record `parent_spawn_id` (display).
4. **Fork-point capture** on the source node, under a source claim, with the source in a new
   **`Forking` transient status** (see Source-liveness below): (a) live awaited mount checkpoint → pinned
   manifest; (b) agent-only `docker pause` → `docker commit` (fork's own rootfs delta) → `docker
   unpause`. Source returns to **Active**; claim released.
5. **Stand up the fork** on the target via the `resumeLocked`/`Provision` path, seeding the fork's
   **own** repo:
   - **Same-node:** restore the pinned mount manifest + the rootfs delta locally into the fork's host
     dirs; the fork's journaler re-journals from gen 1 into its **own** bucket/repo (own password sealed
     locally).
   - **Cross-node:** ship the rootfs delta **and** the mount checkpoint to the target via the existing
     **encrypted transfer set** (`sp-ei4.1.13`); the target restores them into the fork's own host dirs
     and re-journals into the fork's own repo. The target **never** holds the source's repo password —
     only the fork's new password is owner-sealed-delivered to it (`sp-jdzb`).
   - Launch the fork agent with `--continue`.

### Source-liveness during capture (`sp-dts5` / `sp-vr3v`)

The roast's sharpest correctness finding: a fork pauses the source but the existing recovery sweep keys
on transient statuses, so a fork-driver/node death mid-capture would strand the source **frozen and
unrecoverable**. v2:

- The source enters a **`Forking` transient status** for the duration of capture (claim + `status_seq`
  CAS, per the transition-coordination model). The **recovery sweep** is extended to catch a
  `Forking` + expired-claim source and **unpause + revert to Active**.
- The capture is **local to the source node** (live snapshot + local pause/commit/unpause); the source
  claim is **not** held across any cross-node round-trip — the fork's cross-node standup happens **after**
  the source is unpaused, back to Active, and the claim released (the capture artifacts are already
  pinned/exported).
- Pause scope is the **agent container only**; the sidecar (model proxy) keeps running. A brief
  agent-only pause may still interrupt an in-flight LLM/SSE turn — bounded by the commit time and treated
  as a residual risk (spike below).

### Failed-fork cleanup

Fork creation is **independent** (no refcount to corrupt). On standup failure (provision / restore /
re-journal fails), reuse migration's `revertOnFail` pattern to **unwind**: delete the fork row, revoke
the fork's newly-minted gen key, drop the fork's new bucket. The source is untouched throughout.

### Client surfaces (`sp-6344`)

- **Web:** spawn context-menu **"Fork"**; target picker reuses `ListMigrationTargets` (default
  same-node). `parent_spawn_id` shown in spawn list/detail.
- **spawnctl:** `spawnctl fork <id> [--node <id> | --class <class>]`.

## Scope / non-goals (MVP)

- ✅ Same-node fork **and** cross-node fork (owner-sealed mounts), isolated per-fork repos.
- ✅ Auto-continue for **claude-code + codex** only.
- ❌ **opencode/ACP fork** — deferred to `sp-5h3.8` (blocked on `sp-5h3.6` SQLite checkpoint).
- ❌ **Shared-repo dedup-free seed** — deferred to `sp-3y92` (roast BLOCK).
- ❌ Forking a **Suspended** source — MVP requires Active (smaller fast-follow).

## Risks / spikes (do early)

1. **[F — mid-turn `--continue` tolerance]** The agent-only pause does not fsync; `docker commit` can
   capture the session JSONL mid-assistant-turn (torn/truncated trailing line). *Question:* does
   claude-code/codex `--continue` cleanly resume from a session captured mid-turn? *Cheapest test:* pause
   an agent mid-turn → commit → restore → `--continue`, observe load. *Kill criteria:* if `--continue`
   fails on a torn capture, fall back to "history present, fresh session" (decision #2's lower rung) or
   capture only at a turn boundary. (Each side has its **own copy** of the session dir, so there is no
   shared-session-id collision between source and fork — they resume independently.)
2. **[E — freeze SLO]** Even agent-only, `docker commit` of a large rootfs delta is seconds (writable-
   rootfs spike measured ~4 s / 1.2 GB). *Question:* what's the real pause duration distribution, and does
   an in-flight LLM request survive it? *Cheapest test:* measure commit-time on a representative delta;
   pause mid-request and observe upstream timeout. *Kill criteria:* if pauses routinely exceed the
   upstream idle timeout, add request-draining or a turn-boundary gate. State an explicit freeze SLO + a
   "forking…" UX.
3. **[G — stranded-source recovery]** Verify the recovery sweep unpauses+reverts a `Forking`+expired-
   claim source after a simulated fork-driver death.

## Testing (`sp-jkpv`)

- **Same-node fork e2e:** fork an Active claude-code spawn → both stay Active → divergent writes do not
  cross-pollinate (independent repos) → fork agent resumes the inherited conversation (`--continue`).
- **Cross-node owner-sealed fork e2e:** owner-sealed mount fork to a different node via the transfer set +
  fork-password delivery; node-local mount on a cross-node fork is correctly **blocked** by the guard.
- **Isolation assertion:** deleting the **source** while the fork is live leaves the fork's repo fully
  readable/writable (independent bucket/password) — no shared teardown.
- **Stranded-source recovery:** kill the fork driver mid-pause → recovery sweep unpauses + reverts the
  source to Active.
- **Failed-fork cleanup:** force a standup failure → fork row + bucket + key are unwound; source intact.

## Task mapping

| Bead | Covers |
|------|--------|
| `sp-pmay` | this spec + settled decisions |
| `sp-dts5` | live mount-checkpoint primitive · isolated per-fork repo seeding · `Forking` transient + recovery wiring |
| `sp-vr3v` | `ForkSpawn` RPC + handler · `parent_spawn_id` column + lineage display · failed-fork unwind |
| `sp-jdzb` | cross-node transfer-set seeding + fork-password owner-sealed delivery · reused durability guard |
| `sp-6344` | web context-menu + spawnctl + lineage display |
| `sp-jkpv` | e2e suite above |

## Post-Implementation Notes

*As this design is implemented and iterated on — bug fixes, adjustments, anything that diverged from
the assumptions above — append a dated note here, whether or not a formal debugging skill was used.*

- **2026-06-15 (v2, pre-implementation):** roast v1 returned BLOCK. Root cause: decision #4's shared-repo
  lineage broke spawn isolation, the per-`(spawn,gen)`-key delete-fence, and Kopia's single-maintenance-
  owner model (the substrate fuses repo+source identity to one `spawnID` and runs `force=true`
  maintenance gated on "no live container row"). Reverted #4 to **isolated per-fork repos** (accept full
  seed-upload cost), which dissolves those clusters; deferred the shared-repo optimization to `sp-3y92`.
  Also split the fork-point capture (#5) so the scan-bound journal snapshot runs **live** and only the
  rootfs `docker commit` is under a brief **agent-only** pause; added a `Forking` transient status +
  recovery wiring for stranded-source safety; added a disk-headroom placement gate and a failed-fork
  unwind. Residual spikes E/F/G carried above. (roast panel was degraded by a monthly spend cap — 51/90
  findings got full panels — so the confirmed set is a floor; v2 should be re-roasted when the cap allows.)
