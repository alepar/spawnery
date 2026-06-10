# Deep-Research Brief — Tiered Storage & Local↔Cloud Spawn Migration

**Date:** 2026-06-10 · **For:** designing Spawnery's transient storage tier + data-plane spawn
migration (extends [E3 Storage](2026-05-28-spawnery-e3-storage-design.md),
[Per-Mount Data Backends](2026-05-29-data-mounts-design.md),
[Spawn Lifecycle](2026-05-31-spawn-lifecycle-design.md)). **Bead:** `sp-u53` (storage epic).

Copy the prompt below into a deep-research agent. It is self-contained.

---

## PROMPT

You are a storage/distributed-systems researcher. Produce a rigorous, citation-backed report on
**continuous file-level journaling and data-plane workspace migration** for a platform that runs
sandboxed coding agents. Be concrete, version-specific, and skeptical of marketing — prefer primary
sources (project docs/source, design writeups, benchmarks you can trace, postmortems, and
engineering blogs from platforms running comparable systems in production). Note dates and
versions; flag where evidence is thin or contested.

### Context (the system being designed)

- **Spawnery** runs "spawns": sandboxed coding-agent pods (an agent container + an inference
  sidecar) on **nodes**. Nodes come in two flavors: **self-hosted** (the user's own machine —
  rootless, **macOS and Linux**) and **cloud** (Spawnery-operated, root, Linux). A Go node daemon
  ("spawnlet") manages pods via Docker/runc or CRI backends.
- **Storage is per-mount:** an app declares N named data mounts; each is a host directory
  bind-mounted read-write into the pod at `/app/<path>`. Each mount is realized by a pluggable
  Go `StorageBackend{Prepare, Finalize}` running in the node daemon.
- **Two-tier model (the persistent tier is FIXED — do not redesign it):**
  - **Persistent tier:** each mount is a **git working tree**. Durability checkpoints are git
    commits pushed to the user's own backend (GitHub repo via fine-grained app tokens, or
    `git bundle` blobs in Google Drive/iCloud). Suspend captures dirty state as a WIP commit on a
    `spawnery-suspend/<id>/<gen>` branch. Cadence: per agent turn + on suspend/teardown.
  - **Transient tier (THE SUBJECT OF THIS RESEARCH):** the **working-tree delta** — uncommitted
    edits and untracked files in each mount — continuously, asynchronously journaled to a
    **Spawnery-managed, self-hostable object/blob store** (MinIO-class; the store software itself
    is a research sub-question). Target **loss window on unclean node death: seconds to ~a
    minute**. Whether `.gitignore`'d artifacts (`node_modules`, build outputs) are journaled is an
    open sub-question — they are regenerable but slow to regenerate; weigh both policies.
- **Migration is data-only and suspend-based:** migrate = suspend on node A → resume on node B
  (possibly mac→linux or local→cloud). The agent process restarts fresh; **no CRIU, no memory/VM
  snapshots** — out of scope. The transient journal must make migration complete (no lost
  uncommitted work) and make an *unclean* node death recoverable within the loss window.
- **Single-writer is enforced:** exactly one live container per spawn, guaranteed by DB claims +
  monotonic **generation fencing** (writes stamped with a generation; stale generations rejected
  server-side). Concurrent multi-master sync is NOT a requirement. The realistic concurrency on a
  local node is the **user's own IDE/tools editing the same mount directory the agent writes** —
  same host FS, so this is co-writing one directory, not replica conflict.
- **The local-IDE requirement (key UX constraint):** on a self-hosted node the user must be able
  to open the mount in their local IDE, run local tools against it, and hand work back and forth
  with the agent. The mount must therefore be (or feel exactly like) a **plain local directory** —
  including on macOS, where FUSE kexts are increasingly hostile territory.
- **Motivating flows:**
  1. Start a spawn in the cloud → plan + implement there → at PR-review stage, **migrate it to the
     local node** so the mount is a local path opened in the IDE.
  2. Start locally, hand-develop in the IDE → hand off to the agent → **migrate to the cloud** for
     uninterrupted execution.

### Research questions (address each as its own section)

**1. Local capture mechanism — how to observe/journal writes to a live directory.**
The central open fork. Evaluate at least these candidate architectures, on BOTH macOS and Linux:
- **Plain directory + watcher:** fsnotify/inotify (Linux), FSEvents (macOS), kqueue. Real
  reliability limits: event coalescing/drops, rename/atomic-save patterns editors actually use,
  watch-descriptor scaling on big trees (`node_modules`), missed-event recovery via rescan. Can a
  watcher + debounced incremental upload genuinely hit a seconds-level loss window, and what's the
  failure mode when it can't?
- **FUSE / FS-layer write-through:** macFUSE (kext status, SIP friction, Sequoia/Tahoe outlook),
  **FSKit** (macOS 15+ user-space FS API — maturity, performance, what real projects use it),
  FUSE-T (NFS-backed kextless FUSE), Linux FUSE3. Latency/throughput overhead numbers for
  IDE-style workloads (many small files, git operations, file watchers *inside* the IDE watching
  the FUSE mount).
- **Loopback network mounts:** NFS/SMB served from the node daemon to localhost — does any real
  product do this for the same purpose, and how bad is the IDE experience?
- **Linux-only kernel options** (usable on cloud nodes even if macs need a different answer):
  overlayfs upper-dir harvesting, device-mapper/ZFS/btrfs snapshots + diff, fanotify.
- It is acceptable for **local-mac and cloud-linux nodes to use different capture mechanisms** if
  the journal format is shared — evaluate that hybrid explicitly.
- Answer: which architecture(s) give seconds-level capture **without degrading the native-IDE
  feel** on a Mac, and what does each cost in complexity?

**2. Journal format & transport — what travels to the blob store and how it's replayed.**
- **Incremental/dedup formats:** content-defined chunking (restic/rustic, borg, casync, kopia),
  rsync-style deltas, **git-native reuse** (incremental packfiles/bundles of a throwaway WIP ref —
  attractive since every mount is already a git tree), simple object-per-file CAS. Compare upload
  amplification on typical coding-agent write patterns (many small edits + occasional huge
  `node_modules` churn).
- **Crash consistency of replay:** torn multi-file states, rename atomicity, fsync semantics —
  what guarantees can a journal replay actually make about the materialized tree, and what should
  it declare (e.g. "per-file atomic, cross-file best-effort, git index may need rebuild")?
- **Lifecycle mechanics:** journal compaction/GC, baseline-snapshot + delta-chain length vs resume
  speed, resume-side materialization latency (cold cloud node pulling a multi-GB mount), and
  fencing integration (rejecting journal segments from a stale generation).
- **The self-hostable store itself:** MinIO (note its 2025 license/feature-gating drama), Garage,
  SeaweedFS, Ceph/RGW-lite options — which fits a small self-hosted control plane best (single
  binary, low ops, S3-compat fidelity)?

**3. Prior art — how cloud dev-env / agent platforms persist and move workspaces.**
- **Dev-env platforms:** GitHub Codespaces, Gitpod (now Ona) and its backup/prebuild model, Coder
  (incl. how local IDEs attach), DevPod, Daytona. What exactly do they persist (volume? overlay?
  git?), at what cadence, with what loss window on node death, and do ANY support local↔cloud
  workspace handoff?
- **Agent-sandbox platforms:** E2B, Modal, Morph, Daytona-as-sandbox, Fly machines. They mostly
  snapshot at VM/memory level — what do their *data-plane* stories look like, and what can we
  steal at file level?
- **Sync-centric precedents:** Mutagen (its session model powers docker-compose dev sync), Unison,
  Syncthing — anyone using them for agent/dev-env migration specifically?
- For each: cite the mechanism, cadence, loss window, and any public postmortems/complaints.

**4. Adopt vs build — the embeddable-tool landscape.**
For each serious candidate: license, maintenance health (2025–2026 activity), Go-embeddability
(library vs subprocess), and fit to our shape (single-writer journal to S3, not bidirectional sync):
- **Mutagen** (Go, two-way sync engine — can it run one-way continuous with seconds latency?),
- **rclone** (mount + copy/sync as a library — `librclone`),
- **restic/rustic/kopia** (snapshot repos on S3 — can snapshot frequency reach our loss window?),
- **JuiceFS / SeaweedFS-FUSE / ObjectiveFS-class** (full FUSE-on-object-store FS — does putting
  the WHOLE mount on such an FS satisfy both capture and journal in one move, and what's the mac
  + IDE story?),
- **git-annex / git-native plumbing** (incremental bundles on a timer),
- **Syncthing, Unison** (likely poor fit — say why briefly).
Conclude: adopt-and-wrap vs build-on-libraries, with a shortlist.

**5. Consistency theory — only what applies.**
Given strict single-writer + generation fencing, most CRDT/multi-master literature is
inapplicable — keep this section short. What DOES apply: crash-consistency of journal replay,
exactly-once/at-least-once segment upload semantics, fencing tokens for storage writers (Kafka/
HDFS-style epoch fencing precedents), and the IDE-and-agent-co-writing-one-directory hazard
(advisory locking? last-writer-wins within one FS? what do dev-env products do?).

### Deliverable

- A structured report with the five sections above, plus:
  - a **comparison table** for capture mechanisms (platform support, loss window achievable, IDE
    impact, complexity, mac story);
  - a **comparison table** for journal formats/tools (incrementality, replay guarantees,
    Go-embeddability, S3 fit, license);
  - a **recommendation framework + concrete recommendation** for Spawnery's exact constraints
    (per-mount git working trees, suspend-based data-only migration, rootless mac+linux local
    nodes, root linux cloud nodes, self-hostable S3-class journal sink, seconds-to-a-minute loss
    window, IDE-native local mounts);
  - a staged **adoption path** (e.g. per-turn snapshot piggybacking the existing suspend persist →
    watcher-based continuous journal → FUSE/FSKit capture if/when justified), with the specific
    signals/thresholds that should trigger each stage;
  - explicit callouts where **macOS constraints dominate** the design, and where the evidence is
    weak or contested.
- Cite versions, dates, and sources throughout. Call out where our assumptions (single-writer
  fencing, git-tree mounts, suspend-based migration) materially change the analysis versus the
  general-purpose sync problem.
