# Spawn Fork — Adversarial Review (roast)

**Reviewed:** `2026-06-14-spawn-fork-design.md` (v1) · **Date:** 2026-06-14 · **Verdict:** **BLOCK**
**Beads:** `sp-li7h` (epic) · deferral epic `sp-3y92` · **Response:** the spec's v2 revision (2026-06-15).

## Method + coverage caveat

find → dedup → verify: 9 critic lenses (6 core + 3 domain experts — distributed object-storage
consistency, live process snapshotting/checkpoint-restore, distributed refcounting/shared-resource
lifecycle) → 90 candidate findings → 3-judge panel each. **independence: same-family (Claude)** — panel
agreement, not independent verification.

**Degraded panel:** the run hit a **monthly spend cap** mid-verification; ~115 of 270 judge calls
failed, so only **51 of 90** findings got a complete 3-judge panel. Confirmed counts below are a
**floor**, not a ceiling — degradation suppresses confirmations, so the BLOCK is if anything understated.
46 findings reached confirmation (1 blocker-tagged, 43 major, 2 minor); 41 went to escalation (mostly
incomplete panels).

## The lone blocker-tagged finding (trivial)

The bead notes (`sp-li7h`, `sp-dts5`) still described the **old** storage model (new per-fork bucket +
branching chain), contradicting the spec — implementers reading the beads would build the wrong thing.
**Fixed** by syncing the bead descriptions to the v2 isolated-repo model.

## Where the real weight was: decision #4 (shared-repo lineage) vs the actual substrate

The deep-research confirmed Kopia *supports* multi-writer repos in the abstract, but the roast proved the
shared-repo/shared-password lineage collides with the **implemented journal substrate** in several
independent, serious ways. Clusters (representative confirmed findings):

| # | Cluster | What was proven |
|---|---------|-----------------|
| B | **Maintenance ownership** | Substrate runs quick+full maintenance `force=true` (sole-owner) and gates *deleting* GC on "no live container row for the repo." A shared repo with multiple live members → concurrent `force=true` violates Kopia's single-owner lock; deleting GC can **never run** while any member lives → index blobs accumulate until the repo **wedges**; per-spawn prune anchors delete blobs a fork's seed references; no process owns maintenance after root deletion. |
| C | **Isolation / security** | One repo password across the lineage → every node hosting any fork can decrypt the source's + all siblings' **entire** journaled history. Forking to a less-trusted node hands it the whole lineage's data. Breaks the substrate's "isolation boundary = spawn" invariant. |
| D | **Shared-bucket delete-fence** | Garage has no prefix policy / object-lock → a per-`(spawn,gen)` key allow-listed on the shared bucket can delete **any** blob; a zombie/partitioned source key can delete blobs a live fork depends on. "Per-spawn key revocation stays safe" is false on a shared bucket. |
| A | **Substrate identity fusion** | repo dir + password + bucket + `SourceInfo.UserName` are all `= spawnID`; "shared repo, distinct SourceInfo per spawn" requires splitting an abstraction (`BlobBackend.Open`, `openOrCreateRepo`, `manager.state`, `passwordFor`) the spec never named as surgery. |
| E | **Freeze is scan-bound** | Kopia has no dirty-path API → the under-pause checkpoint re-walks the whole tree → freeze is tens of seconds–minutes on node_modules, not "sub-second-to-seconds"; `docker commit` ~4 s/1.2 GB. No SLO. |
| F | **Mid-turn capture** | `docker pause` doesn't fsync → session JSONL captured mid-turn → torn/truncated → `--continue` fails to load → defeats the core value prop. Only the *paths* were verified present, not that resume tolerates an inconsistent capture. |
| G | **Source liveness** | Fork holds the source's transition claim across the freeze (forbidden) and keeps the source `Active`, but the recovery sweep keys on transient statuses → a dead fork-driver leaves the source **frozen and unrecoverable**. Pod pause-scope (agent + sidecar, in-flight LLM/SSE) unspecified. |
| H | **Refcount** | Lineage-refcounted teardown had no atomicity/rollback: concurrent last-member deletes race; a failed fork mints a phantom member pinning the bucket; doesn't ride the existing `status_seq`/CAS primitive. |

## Resolution (spec v2, 2026-06-15)

| Cluster | v2 resolution |
|---------|---------------|
| B, C, D | **Revert to fully-isolated per-fork repos** (own bucket + password + keys). No shared crypto domain → isolation, delete-fence, and single-owner `force=true` maintenance all stay valid, exactly as for any fresh spawn. |
| A | Mooted — no repo/source identity split needed; the fork is an ordinary new-spawn repo. |
| H | Mooted — independent repos need **no lineage refcount**; replaced with a migration-style **failed-fork unwind** (delete row + revoke key + drop bucket). |
| E | Split the capture: the scan-bound journal checkpoint runs **live (no pause)**, awaited + pinned; only the rootfs `docker commit` is under a **brief agent-only pause**. Freeze ≈ commit time. Residual SLO measurement carried as a spike. |
| F | Carried as the #1 spike: verify `--continue` tolerates a mid-turn capture (each side has its own session-dir copy → no shared-session-id collision); fall back to "history-present, fresh session" if not. |
| G | Source enters a **`Forking` transient status** during capture (claim + `status_seq` CAS); **recovery sweep extended** to unpause+revert a stranded `Forking` source. Capture is local; the source claim is released before any cross-node standup. Pause scope = **agent container only**. |

The shared-repo "dedup-free seed" optimization (and its full challenge list) is preserved as a deferred
backlog epic, **`sp-3y92`**, to revisit only if seed-upload cost proves material at scale.

## Notes

- 25 load-bearing assumptions and 41 escalations (mostly incomplete panels from the spend cap) are in
  the run output; the clusters above capture the load-bearing ones.
- v2 should be **re-roasted** when the spend cap allows, on its residual risks (E/F/G), rather than
  resuming v1's degraded panel (whose B/C/D findings v2 already moots).
