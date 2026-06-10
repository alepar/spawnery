# Transient Storage Tier — Kopia Journal + Local↔Cloud Migration (Design)

**Bead:** `sp-u53` (storage epic; transient-tier sub-epic `sp-u53.5`)
**Status:** Approved + adversarially reviewed; amended 2026-06-10
**Date:** 2026-06-10
**Research basis:** [brief](2026-06-10-tiered-storage-migration-research-brief.md) ·
[results + merged synthesis](2026-06-10-tiered-storage-migration-research-results.md) ·
[cloud run](2026-06-10-tiered-storage-migration-research-results-cloud.md)
**Adversarial review:** [storage+secrets roast](2026-06-10-storage-secrets-adversarial-review.md)
(23 confirmed findings; amendments folded in below — change markers `[roast Cn/Mn]`).
**Amends:** [Spawn Lifecycle §5/§6](2026-05-31-spawn-lifecycle-design.md),
[Per-Mount Data Backends](2026-05-29-data-mounts-design.md),
[E3 Storage](2026-05-28-spawnery-e3-storage-design.md)
**Couples with:** [Owner-Sealed Secrets](2026-06-10-owner-sealed-secrets-design.md) (`sp-2ckv`) —
the **owner-sealed** custody tier; the node-local tier needs no secret primitive.

Adds the **transient tier**: continuous, client-side-encrypted journaling of every mount to a
self-hosted object store, making suspend/resume lossless to a seconds-level window and enabling
**data-only local↔cloud spawn migration** ("Move to …"). **The CP never holds a journal
decryption key** (custody is node-local or owner-sealed — §4); the store operator sees only
ciphertext.

---

## 1. Architecture, tiers & durability classes

Two **storage tiers** with distinct jobs:

- **Persistent tier (existing E3, unchanged):** git working tree per mount; commits/pushes to
  *user-owned* backends (GitHub / git-bundle blobs). The **user-facing artifact** — browsable,
  shareable, survives Spawnery itself. Publication, not recovery.
- **Transient tier (this spec):** a per-spawn **Kopia repo** (embedded Go library; CDC chunking,
  pack blobs, client-side AEAD encryption + zstd) on a Spawnery-managed, self-hostable S3-class
  store — **Garage** (single binary, ~512 MB; SeaweedFS if outgrown; MinIO disqualified —
  archived 2026-04). **Operational durability**: crash recovery, suspend/resume restoration,
  migration transport.

### 1a. Per-mount durability class `[roast M18]`

Each mount declares a **durability class** (`durability:` in the manifest, user-overridable in
`spawn.yml`); the journaler's behavior and the user-facing promise follow from it:

| Class | Journaled? | CP can read? | Owner ceremony? | Survives |
|---|---|---|---|---|
| **ephemeral** | no | n/a | no | nothing (suspend resets — today's scratch contract) |
| **node-local** (default for prior scratch) | yes, key node-held | **never** | no | same-node suspend/resume + crash |
| **owner-sealed** | yes, key owner-sealed | **never** | yes (lazy, at migration) | node death + cross-node migration |

- **Default:** mounts that were scratch default to **node-local** with a **one-time notice**
  ("this folder now persists to encrypted remote storage; it survives reboots on this machine").
  Apps that genuinely need no-residue set `ephemeral`. Persistent-backed mounts (git) journal at
  their class **and** keep their git publication path.
- **The CP is never a custodian** of journal keys in any class (§4) — this supersedes the earlier
  "interim CP-custodied KeyProvider." The honest no-ceremony default is a *durability* statement
  ("survives reboot, not node loss"), never a privacy concession.

### 1b. Shape

- **Whole mount dir, `.git` included.** **Resume/migrate = one Kopia restore** — working tree,
  index, local branches, stash land as they were. Git remains the *publication* path, not the
  recovery path.
- **Durability floor for git-backed mounts is retained `[roast M1/M3]`:** suspend still writes a
  **WIP commit + push** to the user's own backend for git-backed mounts (lifecycle §5's floor is
  **kept, not deleted**) — a key-independent copy recoverable with zero Spawnery keys. This is
  opt-out per mount. For `node-local`/`ephemeral` scratch (no git tier), the **honesty notice is
  retained**: uncommitted/scratch state has no key-independent floor.
- **Repo-per-spawn**, snapshot source per mount. All generations + all mounts of one spawn dedup
  together; **no cross-spawn dedup** (isolation boundary = spawn).
- The journaler is a **node-daemon (spawnlet) service** over mount host dirs. Pods untouched;
  rootless-compatible; **no FUSE** (research: macOS interposition rejected). Local mounts stay
  plain host directories — IDE-native.

## 2. Journal mechanics

- **Triggers (v1):**
  - **Watcher** — FSEvents (macOS), inotify (rootless Linux; raised `max_user_watches`),
    fanotify (root cloud Linux); Mutagen-style hybrid (hot-path watches + polling) if descriptor
    limits bite. Coalesced by an **adaptive debounce `[roast M9]`**: the next snapshot is never
    scheduled sooner than `k ×` the last scan duration for that mount (Kopia has **no dirty-path
    API** — every snapshot re-walks the tree, so on a large tree the floor is scan-bound, not
    1–5 s; the debounce must track measured scan cost, not a fixed interval).
  - **Periodic fallback** (~60 s) — catches watcher gaps; Kopia's scan-on-snapshot is the rescan
    safety net.
  - **Turn end** and **suspend** (the suspend snapshot is the final, marker-recorded one).
- **Per-mount serialized snapshot queue + suspend barrier `[roast M17]`:** snapshots for a mount
  run one at a time. Suspend = cancel pending debounce timers → drain/abort in-flight snapshots →
  take the final snapshot → write markers (§3) → then maintenance/teardown. **"Latest" is defined
  as the latest `StartTime` among _complete_ manifests**; clean resume restores the **marker**
  manifest unconditionally (latest-of-gen is only the crash fallback, §3/C1).
- **Loss window:** adaptive-debounce + upload, async — never blocks the agent. Advertised honestly
  as **seconds-to-a-minute on a quiescent tree; degrades to scan-time during high churn** (the
  revisit trigger below watches for this).
- **Gitignored artifacts: included by default**, per-mount excludes configurable. Dedup absorbs
  the bulk; including `node_modules` makes migration open-IDE-and-run instant.
- **Secret tmpfs mounts are excluded from the journal `[session decision]`:** secrets are
  injected as files on per-path tmpfs mounts by the vault-sidecar
  ([owner-sealed-secrets §6](2026-06-10-owner-sealed-secrets-design.md)); the journaler **skips
  those mount paths by mount** (distinct mounts, no content scan). Rationale is redundancy +
  rotation-hygiene, not plaintext-safety (Kopia caches are encrypted) — secrets are re-delivered
  fresh each episode, so journaling them would only leave rotated-away values lingering encrypted
  in old snapshots.
- **Maintenance split by safety class `[roast M5]`:**
  - **Quick (index-compacting) maintenance runs on a regular cadence** (node-local on Kopia
    defaults, or CP-commanded hourly via heartbeat). It **never deletes a blob without another
    copy**, so it does *not* reopen the zombie-deletion hole. This is mandatory — seconds-cadence
    snapshots otherwise accumulate thousands of index blobs and wedge the repo (verified: field
    reports of 28k–90k index blobs).
  - **Full (deleting) maintenance is CP-commanded only**, pinned to Kopia **SafetyFull** (24 h
    blob min-age), run **after** the spawn is marked `suspended` (decoupled from the
    persist-failure path) and only when no live container row exists and the prior generation's
    node is confirmed stopped or its 24 h margin elapsed.
- **Retention / prune `[roast M7]`:** per-generation manifest **heads are retained**; prune
  generation N only after **at least one durable gen-(N+1) snapshot manifest exists for every
  mount** (CP gates on the node reporting its first post-resume snapshot) — a happens-before
  anchor, so a crash right after resume never leaves a generation with zero restorable manifests.
- **Revisit trigger `[roast M9]`:** escalate capture strategy on **scan-duration p95**, not
  upload-amplification (CDC dedup keeps uploads quiet while scan cost blows up). Telemetry alarm
  on per-repo index-blob count.

## 3. Fencing, restore-pinning & consistency

- **Generation rides in snapshot tags.** The node stamps `generation` (CP-threaded) on each
  manifest.
- **Restore is pinned, not selected `[roast C1]`** (the load-bearing fix — reader-side filtering
  alone cannot fence the generation being restored *from*, because a partitioned same-gen zombie
  keeps writing well-formed manifests):
  - **Clean resume** restores the recorded **`persist_marker` manifest IDs** (per mount).
  - **Recreate** (from `unreachable`/`error`, no clean marker) samples **latest-of-prior-gen
    exactly once inside the lifecycle §6.3 claim transaction** and records the per-mount manifest
    IDs on the new container row **before** provisioning. All restores of that episode use the
    pin **idempotently**; any prior-gen manifest newer than the recorded cutoff is fenced garbage.
- **Per-generation Garage access key `[roast M1]`:** mint a **fresh access key per generation** on
  the spawn's bucket; **revoke the superseded key** as part of suspend-complete / recreate /
  migrate (Garage per-key-per-bucket admin API supports this cheaply). This fences a
  partitioned/zombie node's *delete* capability (Garage has no object-lock), not just its writes.
  Key revoked entirely on spawn **delete**.
- **Zombie writers — scoped claim:** additive corruption is harmless by construction (immutable,
  content-addressed blobs); **delete/GC by a stale key is the real risk**, closed by per-generation
  key revocation above. The durability floor for `node-local`/`ephemeral` scratch is bounded
  accordingly (no git floor — see §1b notice).
- **Store isolation:** **bucket-per-spawn + per-bucket access key** (Garage has no IAM/prefix
  policies — `[roast m3]`). Cross-spawn access impossible at the store; Kopia client-side
  encryption (manifests/indexes included) covers confidentiality from the store operator. Keys
  live only in the node daemon; spawn containers never hold them.
- **Key-free durability witness `[roast M6]`:** after the final suspend snapshot flush, the node
  writes a tiny **plaintext sentinel** per `(spawn, generation, mount)` at
  `s3://<bucket>/markers/gen-<N>/<mount>` containing the manifest ID. Lifecycle §6.6
  marker-probing becomes an **S3 HEAD/GET the (key-less) CP can perform** — content integrity stays
  Kopia-AEAD-verified at restore time. This removes the impossible "CP reads encrypted manifests"
  probe; the DB marker row remains the incremental none/partial/all signal.
- **Declared replay guarantee:** per-file atomic (debounce quiesces; scan sees complete files),
  cross-file best-effort, git index self-heals (and travels in the snapshot).

## 4. Key custody — node-local default, owner-sealed for travel

**The CP is never a journal-key custodian.** `[roast — supersedes interim CP KeyProvider]`

- **node-local class:** the **node** generates the per-spawn Kopia repo password and stores it
  **node-local, encrypted at rest** (under a node key). The CP and the Garage operator see only
  Kopia ciphertext — satisfying "only nodes ever see plaintext" with **zero owner ceremony**.
  Same-node suspend/resume and crash recovery read the local password. **A different node cannot
  resume** a node-local spawn (it lacks the password); node death loses the journal (git floor
  still covers committed work for git-backed mounts).
- **owner-sealed class:** the password is sealed to the owner's keys and re-delivered to any
  verified target node via the [owner-sealed-secrets](2026-06-10-owner-sealed-secrets-design.md)
  primitive (`sp-2ckv`). This is the **only** tier that survives node death and enables
  cross-node migration — and the **only** one that needs the (lazy) key ceremony.
- **Upgrade node-local → owner-sealed is cheap:** the *same* repo password is additionally sealed
  to the owner; **no re-encryption of the repo** (so no kopia#309 master-key-rewrite problem). The
  trigger to upgrade is the user invoking migration / opting a mount into owner-sealed durability
  (the lazy ceremony moment).
- **Sequencing consequence `[roast]`:** phase ① (node-local) ships **without** `sp-2ckv`.
  Cross-node migration / node-death survival (owner-sealed) **hard-depends** on `sp-2ckv` phase ①.
  Cloud spawns resume cross-node more often than self-hosted, so **node-local journaling ships
  self-hosted-first**; cloud durable cross-node resume arrives with owner-sealing (or a separate
  cloud-internal custody design — deferred).
- **Deferred:** headless key delegation (scheduled/agent-initiated resume of owner-sealed spawns)
  — accepted; node-local spawns resume headlessly on their own node without it.

## 5. Migration flow & UX

- **`MigrateSpawn{spawn_id, target}`** CP RPC: claim-guarded **suspend → resume with placement
  override**, reusing the lifecycle state machine (migration is not a new state). Requires the
  source mount(s) to be **owner-sealed** (auto-prompts the lazy ceremony / node-local→sealed
  upgrade if not). Targets: user's own nodes by name + "cloud".
- Surfaced as **"Move to …"** (web kebab) + **`spawnctl move`** with a **preflight ETA + cancel**
  and honest copy `[roast m1]`: size/transfer estimate up front; on failure "move failed — your
  data is safe, resume here." Restore artifact dirs materialize **lazily/in background**.
- **Re-placement / mid-flow / unenrolled `[roast M8]`** (delivery sub-protocol lives in the
  secrets spec §3): a failed start on node X forces a fresh owner-client seal to node Y within the
  same interactive resume session; a `starting` episode whose key delivery **times out** returns to
  a **defined** state (back to `suspended`, target restore artifacts wiped) — never a silent hang.
- **Cross-platform fidelity (mac↔linux):** journal records paths/modes/symlinks faithfully; APFS
  case-insensitivity + uid/permission mapping (rootless mac ↔ root linux) are **required test
  fixtures**.
- **Telemetry from day one:** journal lag, scan duration (p95), upload amplification, resume
  materialization time, stale-generation restore attempts, watcher drop frequency, **per-repo
  index-blob count**, **journal-lag alarm threshold** (§6 degraded modes).

## 6. Garage failure semantics & degraded modes `[roast M13]`

- **Outage at suspend:** if Garage is unreachable but the **git tier persisted**, suspend completes
  as **`suspended(journal-stale, last-good-snapshot T)`** — *not* `error` (which would flip the
  fleet to user-driven recovery on a transient blip).
- **Outage at create:** **lazy bucket/key mint on first snapshot**, so spawn creation never blocks
  on Garage.
- **Outage mid-run:** journaling spools to **bounded local disk** with a **lag alarm threshold**
  surfaced in UI/telemetry (note: a same-node spool does **not** survive node death — the loss
  window is honestly unbounded while Garage is down).
- **Orphans:** a CP reconciler sweeps buckets/keys with no live spawn.
- **Capacity (low-confidence `[roast M13]`):** Garage publishes no bucket/key count limits;
  bucket-per-spawn at ~10k spawns is a **load-test gate** before phase ② (§ benchmarks), with a
  fallback of bucket-per-owner + per-spawn prefix if limits bite.

## 7. Spec/epic ripple

- **Lifecycle §5/§6:** scratch row → durability-class-driven (§1a); **WIP-branch floor retained**
  for git-backed mounts (not deleted); persist markers = manifest IDs **+ plaintext sentinel**
  (§3, M6); suspend persist = barrier→final snapshot→markers (+ git push for git-backed); §6.6
  marker-probe = S3 HEAD on the sentinel; §6.1 server-side CAS → **restore-pinning + per-generation
  key revocation** (§3).
- **Data-mounts design:** journal is **orthogonal to per-mount `StorageBackend`**; adds the
  `durability:` class. `Prepare` for an owner-sealed/node-local resume = Kopia restore (pinned
  manifest) into the host dir before bind.
- **E3:** persistent tier re-scoped to publication; conflict-handling unchanged.
- **New infra:** Garage — dev `just` recipe/container; prod single binary. Verify no
  bucket-versioning dependency in Kopia; pin the Kopia library version (Velero-proven, API not
  formally stable); set **per-spawn Kopia cache limits** and **delete caches on spawn
  delete/migrate-away** `[roast m1]`; add node disk headroom to scheduler inputs.
- **sp-2ckv coupling:** owner-sealed custody (§4) consumes the secrets primitive; node-local does
  not.

## 8. Implementation phases (under `sp-u53.5`)

| # | Slice | Custody | Notes |
|---|---|---|---|
| ① | Node Kopia journal + restore; **periodic + adaptive-debounce watcher**; same-node suspend/resume; restore-pinning; per-gen Garage key; quick-maintenance cadence; sentinel marker | **node-local** | self-hosted-first; ships **without** sp-2ckv; fs-repo for unit tests, Garage for e2e |
| ② | Generation fencing hardening + CP-commanded full maintenance + prune anchor + degraded modes + telemetry/alarms | node-local | **gated on the benchmarks below** |
| ③ | `MigrateSpawn` + placement override + "Move to…" + `spawnctl move` + cross-platform fixtures + delivery sub-protocol | **owner-sealed** | depends on **sp-2ckv phase ①** (node leg) |
| ④ | Owner-sealed cloud durable cross-node resume; node-local→sealed upgrade UX | owner-sealed | depends on sp-2ckv phase ② |

Git backends (sp-u53.1/.2/.3) proceed in parallel as the persistent tier.

### Pre-implementation spike/benchmark gates `[roast §4]`

| Gate | Phase | Proves |
|---|---|---|
| Zombie double-Recreate e2e (partitioned node journals through Recreate ×2; restore == pinned manifest both times) | ① | C1 pin closes the reader-side hole |
| Suspend-race e2e (slowed watcher snapshot racing suspend; restore == marker) | ① | M17 barrier + "latest" def |
| Per-generation Garage key mint/revoke spike (CreateKey/AllowBucketKey/DeleteKey + revocation timing) | ① | M1 fix is as cheap as claimed |
| Kopia scan benchmark — 500k-file fixture; **gate ② on scan p95 < debounce**; verify adaptive debounce | ②-gate | M9 |
| 48 h churn soak (busy spawn, no suspend) — bound snapshot p95, repo-open time, index-blob count under the quick-maintenance cadence | ②-gate | M5 |
| Restore-latency over a constrained (~50 Mbps) link, not just LAN | ② | m1 — LAN passes while the real path fails |
| Garage many-bucket load test (~10k buckets/keys) + orphan reconciliation | ①/② | M13 capacity flag |

## 9. Deferred

Headless key delegation · cross-spawn dedup (forfeited) · cloud-internal node-local custody ·
FSKit/kernel-side capture (re-eval 12–18 mo) · conversation continuity (sp-qjy) · Garage
multi-node replication tuning (commit a minimal RPO before phase ②) · overlayfs upper-dir
harvesting (cloud-linux; only if scan cost demands).

---

## Appendix — decision log

| # | Decision | Choice |
|---|---|---|
| T.1 | Migration semantics | Data-only, suspend-based (no CRIU/VM snapshots) |
| T.2 | Transient scope | Mount working-tree state; whole dir **incl. `.git`** + (default) gitignored artifacts |
| T.3 | Capture | Plain host dirs + watcher + **adaptive debounce** + periodic fallback; **no FUSE** |
| T.4 | Journal substrate | **Embedded Kopia** (Apache-2.0, CDC+CAS, client-side encryption) |
| T.5 | Repo granularity | **Repo-per-spawn** (generations + mounts dedup; no cross-spawn dedup) |
| T.6 | Sink | Self-hosted **Garage**; **bucket-per-spawn + per-bucket access key** (no prefix policies `[m3]`); MinIO/Duplicacy/Mutagen rejected |
| T.7 | Fencing | **Restore-pinning** (`[C1]`) + generation tags + **per-generation Garage key** (`[M1]`) + quick-maintenance-cadence/full-CP-commanded split (`[M5]`) + SafetyFull |
| T.8 | Coverage & class | **All mounts**, per-mount `durability: ephemeral\|node-local\|owner-sealed` (`[M18]`); scratch → node-local default + one-time notice |
| T.9 | Resume path | Pure Kopia restore (pinned manifest); git = publication; **WIP-branch floor retained for git-backed mounts** (`[M1/M3]`) |
| T.10 | Key custody | **CP never custodian.** node-local (node-held password) default; owner-sealed (sp-2ckv) for migration/cross-node/node-death; node-local→sealed = seal same password, no re-encrypt |
| T.11 | Migration UX | `MigrateSpawn` + "Move to…"/`spawnctl move`; preflight ETA + cancel + defined timeout/failure states (`[M8/m1]`) |
| T.12 | Loss window | Honest: seconds-to-a-minute quiescent, scan-time under churn; per-turn/suspend snapshots + git floor |
| T.13 | Durability witness | Plaintext per-(spawn,gen,mount) sentinel for key-less CP marker probe (`[M6]`) |
| T.14 | Garage outage | Defined degraded modes; lazy bucket mint; bounded spool + lag alarm (`[M13]`) |
