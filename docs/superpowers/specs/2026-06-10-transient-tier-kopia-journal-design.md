# Transient Storage Tier — Kopia Journal + Local↔Cloud Migration (Design)

**Bead:** `sp-u53` (storage epic; transient-tier sub-epic created from this spec)
**Status:** Approved in brainstorming (Mode A — sections reviewed with user)
**Date:** 2026-06-10
**Research basis:** [brief](2026-06-10-tiered-storage-migration-research-brief.md) ·
[results + merged synthesis](2026-06-10-tiered-storage-migration-research-results.md) ·
[cloud run](2026-06-10-tiered-storage-migration-research-results-cloud.md)
**Amends:** [Spawn Lifecycle §5](2026-05-31-spawn-lifecycle-design.md),
[Per-Mount Data Backends](2026-05-29-data-mounts-design.md),
[E3 Storage](2026-05-28-spawnery-e3-storage-design.md)
**Depends on (final posture only):** owner-sealed secret delivery epic (new, see §4) — interim
CP-custodied path unblocks phases ①–③.

Adds the **transient tier**: continuous, encrypted journaling of every mount to a self-hosted
object store, making suspend/resume lossless to a seconds-level window and enabling **data-only
local↔cloud spawn migration** ("Move to …").

---

## 1. Architecture & tier responsibilities

Two tiers with distinct jobs:

- **Persistent tier (existing E3, unchanged):** git working tree per mount; commits/pushes to
  *user-owned* backends (GitHub / git-bundle blobs). The **user-facing artifact** — browsable,
  shareable, survives Spawnery itself. Publication, not recovery.
- **Transient tier (this spec):** a per-spawn **Kopia repo** (embedded Go library; CDC chunking,
  pack blobs, client-side AEAD encryption + zstd) on a Spawnery-managed, self-hostable S3-class
  store — **Garage** (single binary, ~512 MB; SeaweedFS if outgrown; MinIO disqualified —
  archived 2026-04). **Operational durability**: crash recovery, suspend/resume restoration,
  migration transport.

Coverage and shape:

- **All mounts, including scratch.** Scratch keeps "no user-visible remote" but now survives
  suspend/migration via the journal; the lifecycle's scratch-reset notice is removed.
- **Whole mount dir, `.git` included.** **Resume/migrate = one Kopia restore** — working tree,
  index, local branches, stash land exactly as they were. Git is never the recovery path. This
  deletes the WIP-commit/`spawnery-suspend/<id>/<gen>`-branch machinery from lifecycle §5 for
  journaled spawns (no GitHub branch pollution, no branch GC).
- **Repo-per-spawn**, snapshot source per mount. All generations + all mounts of one spawn dedup
  together; **no cross-spawn dedup** (deliberate — isolation boundary = spawn).
- The journaler is a **node-daemon (spawnlet) service** operating on mount host dirs. Pods are
  untouched; rootless-compatible; no FUSE anywhere (research: macOS interposition rejected —
  kext hostility, FSKit `/Volumes` confinement, FUSE-T = NFS-loopback semantics). Local mounts
  stay plain host directories — IDE-native.

## 2. Journal mechanics

- **Triggers (v1 ships all of):**
  - **Watcher** — FSEvents (macOS), inotify (rootless Linux; raised `max_user_watches`),
    fanotify (root cloud Linux); Mutagen-style hybrid (hot-path watches + polling) if descriptor
    limits bite. Debounced ~1–5 s into `kopia snapshot` of dirty mounts.
  - **Periodic fallback** (~60 s) — catches watcher gaps; Kopia's scan-on-snapshot is the rescan
    safety net (only changed content uploads regardless of trigger).
  - **Turn end** and **suspend** (the suspend snapshot is the final, marker-recorded one).
- **Loss window:** debounce + upload ≈ seconds-to-a-minute on unclean node death (the target).
  Snapshots are async — never block the agent.
- **Gitignored artifacts: included by default**; per-mount excludes configurable in
  manifest/spawn.yml. Dedup absorbs the bulk; including `node_modules` makes migration
  open-IDE-and-run instant. (Deliberate override of the research's lean-exclude, on the strength
  of CDC dedup + async triggers; revisit if upload-amplification telemetry disagrees.)
- **GC/maintenance:** Kopia maintenance runs **only on CP command** (typically at suspend),
  never on a node's own schedule — part of fencing (§3). Retention: last-N + per-generation
  heads; prune superseded generations' manifests after a successful resume.

## 3. Fencing & consistency

- **Generation rides in snapshot tags.** The CP already threads `generation` through every
  command; the node stamps it on each manifest. **Resume restores only the latest manifest of
  the current generation.**
- **Zombie writers are harmless by construction:** Kopia blobs are immutable + content-addressed
  — a stale generation's writes are additive ciphertext garbage, never overwrites; swept by the
  next CP-commanded maintenance. The only deleting operation is maintenance, and only the CP
  triggers it, only on the current generation's node. (This replaces lifecycle §6.1's
  "backend fences server-side compare-and-set" for the journal: fencing is reader-side
  manifest selection + maintenance discipline.)
- **Store isolation:** **bucket-per-spawn + per-spawn Garage access key** granted rw on that
  bucket only (Garage's permission model is per-access-key-per-bucket — no IAM/prefix policies),
  minted by the CP via the Garage admin API at create, revoked on spawn **delete** (not
  per-generation — generations share the repo). Cross-spawn access is impossible at the store;
  confidentiality is additionally covered by Kopia's client-side encryption (manifests/indexes
  included). Keys live only in the node daemon — spawn containers never hold them. Residual
  (accepted): within its own bucket the key holder can delete (Garage has no object-lock);
  blast radius = that one spawn, durability floor = the git persistent tier.
- **Declared replay guarantee:** per-file atomic (debounce quiesces; scan sees complete files),
  cross-file best-effort, git index self-heals (and travels in the snapshot anyway).
- **Persist markers (sp-a7fs):** `spawn_mounts.persist_marker` = the suspend snapshot's
  **manifest ID**; lifecycle §6.6 marker-probing probes Kopia manifests. This is the concrete
  marker the suspend protocol was waiting on.

## 4. Keys & isolation

- **Per-spawn Kopia repo password**, generated client-side at create.
- **Target posture (owner-sealed, E2E):** CP stores only ciphertext sealed to the owner's key;
  on create/resume the owner's client unseals and re-seals/delivers to the (verified) node over
  the E2E channel. CP can never read journal plaintext or keys.
- **Hard dependency:** no sealing/unsealing/E2E-delivery machinery exists today (E0 §10 channel
  and E4 vault are design-only; sp-zdd is an empty umbrella; sp-ova node-auth WIP provides the
  identity anchor only). A **separate epic with its own brainstorm** owns: owner keypair
  custody, client-side seal/unseal of small secrets, verified node pubkeys (builds on sp-ova,
  resolves sp-gtm), CP ciphertext-only storage/relay. It also serves sp-7h6.1 (user secrets
  store) — same primitive.
- **Interim (phases ①–③):** key custody behind a node/CP `KeyProvider` seam — CP-held KEK,
  key relayed to the node like storage tokens (E3 §3 pattern). Explicitly marked: CP *can*
  read journals until phase ④ swaps the provider. Node holds plaintext keys **in memory only**,
  for the active episode.
- **Deferred:** headless key delegation (sp-3rtm spawn-spawning, scheduled runs cannot resume
  owner-sealed spawns until a delegation story exists — accepted).

## 5. Migration flow & UX

- **`MigrateSpawn{spawn_id, target}`** CP RPC: claim-guarded **suspend → resume with placement
  override**, atomic from the user's view, progress surfaced ("persisting… moving… starting on
  <node>"). Targets: the user's own nodes by name + "cloud" (scheduler class). Reuses the
  lifecycle state machine verbatim — migration is not a new state.
- Surfaced as **"Move to …"** (web kebab menu) + **`spawnctl move`**.
- **Cross-platform fidelity (mac↔linux):** journal records paths/modes/symlinks faithfully;
  APFS case-insensitivity collisions and uid/permission mapping (rootless mac ↔ root linux) are
  explicit normalization concerns and **required test fixtures** (the research's
  most-likely-subtle-bug callout).
- **Telemetry from day one:** journal lag (write→durable), rescan/scan duration, upload
  amplification (bytes uploaded ÷ bytes changed), resume materialization time (the dominant
  migration cost), stale-generation restore attempts, watcher drop frequency.

## 6. Spec/epic ripple

- **Lifecycle §5:** scratch row becomes journal-restored; suspend persist = final Kopia
  snapshot (+ git push for persistent-backed mounts); persist markers = manifest IDs; WIP-branch
  mechanics dropped for journaled spawns; "scratch honesty" notice removed.
- **Data-mounts design:** the journal is **orthogonal to per-mount `StorageBackend`** — a
  node-level service over all mount host dirs, not another backend. `Prepare` for a journaled
  resume = Kopia restore into the host dir before bind.
- **E3:** persistent tier re-scoped to publication/sharing; conflict-handling (§7) unchanged.
- **New infra:** Garage — dev via a `just` recipe/container; prod single binary. Verify: no
  bucket-versioning dependency in Kopia (Garage lacks it); pin the Kopia library version (API
  not formally stable; Velero-proven).
- **Testing:** hermetic unit tests against filesystem-backed Kopia repos (no Garage, no
  network); build-tagged e2e with a Garage container — kill-node-mid-write → resume elsewhere;
  zombie-writer generation filtering; mac↔linux fixture fidelity; restore-latency benchmark.

## 7. Implementation phases (under `sp-u53`)

| # | Slice | Notes |
|---|---|---|
| ① | Node Kopia journal + restore; periodic trigger only; suspend/resume via journal on one node | interim KeyProvider; filesystem repo OK for tests, Garage for e2e |
| ② | Watcher triggers (FSEvents/inotify/fanotify) + generation tagging/filtering + CP-commanded maintenance | loss window hits target |
| ③ | `MigrateSpawn` RPC + scheduler placement override + web "Move to…" + `spawnctl move` | cross-platform fixtures |
| ④ | Owner-sealed key delivery via the secret-delivery epic | **blocked** on that epic |

Git backends (sp-u53.1/.2/.3) proceed in parallel as the persistent tier.

## 8. Deferred

Headless key delegation · cross-spawn dedup (deliberately forfeited) · FSKit/kernel-side capture
(re-evaluate 12–18 mo; macFUSE 5.3.0 zero-copy channel API signals the gap closing) ·
conversation continuity (sp-qjy, orthogonal) · Garage multi-node replication tuning ·
overlayfs upper-dir harvesting (cloud-linux optimization, only if scan cost demands).

---

## Appendix — decision log

| # | Decision | Choice |
|---|---|---|
| T.1 | Migration semantics | Data-only, suspend-based (no CRIU/VM snapshots) |
| T.2 | Transient scope | Mount working-tree state; whole dir **incl. `.git`** and (default) gitignored artifacts |
| T.3 | Capture | Plain host dirs + watcher (FSEvents/inotify/fanotify) + debounce + periodic fallback; **no FUSE**; watcher ships in v1 |
| T.4 | Journal substrate | **Embedded Kopia** (Apache-2.0, Go, CDC+CAS, client-side encryption); no git thin bundles for the transient tier |
| T.5 | Repo granularity | **Repo-per-spawn** (generations + mounts dedup together; no cross-spawn dedup) |
| T.6 | Sink | Self-hosted **Garage**; per-spawn prefix-scoped creds; MinIO rejected (archived), Duplicacy rejected (license), Mutagen-as-tier rejected (POSIX-box isolation/fencing/mirror≠journal) |
| T.7 | Fencing | Generation in snapshot tags + reader-side latest-of-current-gen selection + CP-commanded-only maintenance; creds revoked on delete |
| T.8 | Coverage | **All mounts incl. scratch**; scratch-reset notice removed |
| T.9 | Resume path | Pure Kopia restore; git = publication only; lifecycle WIP-branch machinery dropped |
| T.10 | Keys | Per-spawn repo password, **owner-sealed** (CP ciphertext-only) via new secret-delivery epic; interim CP-custodied `KeyProvider` for phases ①–③ |
| T.11 | Migration UX | First-class `MigrateSpawn` RPC + "Move to …" / `spawnctl move` |
| T.12 | Loss window | Seconds-to-a-minute on unclean death; per-turn/suspend snapshots as floors |
