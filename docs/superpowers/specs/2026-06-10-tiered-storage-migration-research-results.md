# Tiered Storage & Migration — Deep-Research Results (in-session run)

**Date:** 2026-06-10 · **Brief:** [research brief](2026-06-10-tiered-storage-migration-research-brief.md)
· **Bead:** `sp-u53.4`
**Run:** deep-research harness, 108 agents, 26 sources fetched, 127 claims extracted, 25
adversarially verified (3-vote): **24 confirmed, 1 refuted**. A parallel cloud-session run of the
same brief is expected to fill the coverage gaps listed at the end — merge before designing.

---

## Headline synthesis

For the transient tier, the verified evidence points **away from FS-interposition on macOS** and
**toward a git-native journal driven by a watcher with mandatory rescan fallback**:

- On macOS every interposition path is compromised: macFUSE's default backend still needs a kext;
  its FSKit backend confines mounts to `/Volumes` (fatal for the "plain local directory in the
  IDE" constraint) with worse I/O; and FUSE-T shows "FUSE" and "loopback NFS" on macOS largely
  collapse into the same userspace-NFS mechanism with network-volume semantics.
- Watchers (FSEvents) are documented by Apple as **advisory and lossy** — correctness requires
  periodic full scans; seconds-level capture is achievable only probabilistically, with the
  per-turn git checkpoint as the hard floor.
- Journal format: **git incremental thin bundles with basis revisions** are a strong fit (every
  mount is already a git tree), provided uncommitted state is first captured as a throwaway WIP
  commit — bundles cannot carry working-tree/index/untracked state directly, and gitignored
  artifacts stay uncovered.
- Prior art is **unanimously snapshot-at-stop, not continuous journaling** (Gitpod Classic backs
  up `/workspace` at stop; Codespaces keeps a per-user VM disk; E2B pause-snapshots the whole
  microVM — and shipped a multi-cycle pause/resume data-loss race, fixed 2026-05). No platform
  offers an adoptable continuous-journal design or a local↔cloud handoff. Mutagen's hybrid Linux
  watching (polling + limited inotify watches on hot paths) is the proven watcher-scaling
  blueprint.

---

## Verified findings

### 1. macOS FUSE-layer capture is constrained on every axis — `high` (3-0, ×3 merged)
macFUSE 5.x offers two backends — the default kernel-extension VFS path and an FSKit path
(`-o backend=fskit`) — but the FSKit backend confines mount points to `/Volumes`, always opens
files read/write, lacks the FUSE notification API, and has I/O the maintainers themselves call
"not on par" with the kext backend. The `/Volumes` restriction alone disqualifies FSKit-backed
macFUSE for IDE-native project paths.
*Sources:* [macFUSE FUSE-Backends wiki](https://github.com/macfuse/macfuse/wiki/FUSE-Backends),
[macFUSE 5.2.0 notes](https://macfuse.github.io/2026/04/09/macfuse-5.2.0.html). Restrictions
persist through 5.2.0 (2026-04); the FSKit perf gap is unquantified and narrowing (5.1.0
zero-copy reads).

### 2. macOS "FUSE" ≈ "loopback NFS" — they collapse into one mechanism — `high` (3-0, ×2)
FUSE-T implements kext-free FUSE by running a userspace server translating the FUSE protocol to
NFS/SMB/FSKit, with macOS mounting the result as a **network volume**. Kext-free and root-free,
but inherits network-volume semantics (NFS attribute caching, the "Network Volumes" privacy
prompt) rather than native-local feel; the server binary is **closed-source**.
*Source:* [FUSE-T README](https://github.com/macos-fuse-t/fuse-t). Caveat: with the newer FSKit
backend (macOS 26+) the mount is not literally a network volume; the collapse applies to the
NFS/SMB transports.

### 3. Watcher scaling limits + the proven mitigation (Mutagen hybrid) — `high` (3-0, ×4)
On Linux/BSD, descriptor-per-file watching can exhaust kernel quotas on `node_modules`-scale
trees (classic ENOSPC). Mutagen's production answer: on Linux, **polling for accuracy + a
restricted set of inotify watches on the most recently updated content** for latency; on
macOS/Windows a single native recursive watch (FSEvents / ReadDirectoryChangesW); three exposed
modes (portable/force-poll/no-watch). This is the blueprint for a watcher-stage design.
*Sources:* [Mutagen watching docs](https://mutagen.io/documentation/synchronization/watching/),
[Mutagen v0.14.0 notes](https://github.com/mutagen-io/mutagen/releases/tag/v0.14.0). Caveat:
Linux 5.11+ scales `max_user_watches` with RAM — mitigates, doesn't eliminate.

### 4. Watcher-only capture on macOS is fundamentally lossy — rescan is mandatory — `high` (3-0, ×4)
FSEvents (the only native recursive mechanism; Go wrapper `fsnotify/fsevents` is darwin-only)
performs **temporal coalescing** controlled by a latency parameter, **drops events under load**
(signaled via `MustScanSubDirs` → recursive rescan required), and Apple's own guide says to treat
the event list as **advisory**, with periodic full sweeps for correctness. Seconds-level loss
window via watcher alone is probabilistic; the design needs debounced incremental upload +
periodic full-scan reconciliation + the per-turn git checkpoint as the hard floor.
*Sources:* [fsnotify/fsevents README](https://github.com/fsnotify/fsevents/blob/main/README.md),
[Apple FSEvents Programming Guide](https://developer.apple.com/library/archive/documentation/Darwin/Conceptual/FSEvents_ProgGuide/UsingtheFSEventsFramework/UsingtheFSEventsFramework.html)
(archived but canonical; semantics stable since 10.5).

### 5. Git-native journaling is viable + size-efficient, but needs WIP-commit capture first — `high` (3-0, ×3)
Bundles support incremental transfer via **basis (prerequisite) revisions** — a bundle of
`old..new` unbundles only into a repo holding `old` — and exclusion bundles are **thin packs**
(deltas against receiver-held objects), minimizing per-increment size. But bundles carry only
refs + reachable commits: **index, working tree, untracked files, stash are all excluded**. The
natural journal unit: a periodic throwaway WIP commit on a journal ref, shipped as an incremental
thin bundle. Gitignored artifacts (`node_modules`) remain uncovered unless force-added or handled
by a separate channel.
*Sources:* [git-bundle](https://git-scm.com/docs/git-bundle),
[git-pack-objects](https://git-scm.com/docs/git-pack-objects). One verifier empirically
reproduced the prerequisite-failure behavior.

### 6. Gitpod Classic = stop-time snapshot, not continuous — `high` (3-0, ×2)
Only `/workspace` is kept between state transitions; at stop it's backed up to object storage and
the container destroyed; restart restores into a fresh ephemeral container. Loss window on
unclean death = everything since last stop/snapshot.
*Sources:* [Gitpod workspace lifecycle](https://www.gitpod.io/docs/configure/workspaces/workspace-lifecycle)
(now 308→[ona.com](https://ona.com/docs/classic/user/configure/workspaces/workspace-lifecycle);
applies to Gitpod *Classic*, not current Ona).

### 7. Codespaces persists at the VM-disk layer; no journal, no handoff — `high` (3-0, ×3)
Each codespace is a dedicated per-user VM; repo cloned into `/workspaces` on the VM disk,
bind-mounted into the dev container. Only `/workspaces` survives stop/start **and** rebuild;
uncommitted changes survive because the disk is kept — and are unrecoverable if that storage is
lost (no external journal). No local↔cloud handoff documented.
*Sources:* [Codespaces deep dive](https://docs.github.com/en/codespaces/about-codespaces/deep-dive),
[rebuild docs](https://docs.github.com/en/codespaces/developing-in-a-codespace/rebuilding-the-container-in-a-codespace).

### 8. E2B = whole-sandbox pause snapshots; shipped a real multi-cycle data-loss race — `high` (3-0, ×3)
Persistence = Firecracker microVM pause snapshot (filesystem + full memory together); no
file-level journaling. Issue #884: with autoPause, file changes survived the first pause/resume
but were **lost on subsequent cycles**; engineer-confirmed orchestrator race (2026-03-23), closed
2026-05-15, similar-symptom follow-up filed 2026-05-16. A concrete cautionary postmortem for
snapshot-orchestration persistence vs an append-only journal.
*Sources:* [E2B persistence docs](https://e2b.dev/docs/sandbox/persistence),
[e2b-dev/E2B#884](https://github.com/e2b-dev/E2B/issues/884).

### 9. Cross-cutting: no adopt-wholesale precedent exists — `medium` (synthesis)
None of the surveyed platforms continuously journals a live workspace to an external store; all
are stop/pause-time snapshots or kept disks; none supports local↔cloud handoff. The
assemble-from-parts path: **Mutagen-style hybrid watching** (capture) + **git incremental thin
bundles over a WIP ref** (format — uniquely cheap given mounts are already git trees) +
**generation-fenced segment upload** (single-writer removes the multi-master problem that
dominates general-purpose sync tools). *Absence-of-evidence caveat:* Coder/DevPod/Daytona/Modal/
Morph were not covered by surviving claims.

### 10. Recommended staged adoption path — `medium` (derived)
- **Stage 1 — piggyback the persistent tier:** per-agent-turn WIP commit on a journal ref +
  incremental thin bundle to the S3-class store. Loss window = turn length; near-zero new
  machinery.
- **Stage 2 — debounced watcher journal:** inotify/fsnotify on Linux with Mutagen-style hybrid
  (hot-path watches + polling), FSEvents on macOS; both backed by periodic full-scan
  reconciliation and `MustScanSubDirs`-triggered rescans. Trigger: measured turn lengths exceed
  the loss-window target.
- **Stage 3 — interposition only where cheap:** overlayfs upper-dir harvesting on root
  cloud-Linux nodes; on macOS defer FUSE-T/FSKit unless Stage-2 telemetry shows the watcher loss
  window empirically violated (every macOS interposition option degrades the native-IDE
  constraint: kext approval, `/Volumes` confinement, or network-volume semantics).
- Different capture mechanisms per platform behind the **shared bundle-based journal format** is
  the explicitly acceptable hybrid.
- *Note:* the overlayfs and store-selection legs are brief-derived, **not evidence-backed** (see
  gaps).

---

## Refuted claim (do not assert)

- ~~"FSKit was introduced in macOS 15.4, so kext-free FUSE only exists on 15.4+"~~ — **0-3.**
  FUSE-T provides kext-free FUSE on earlier macOS via NFS/SMB.

---

## Coverage gaps — what the parallel cloud run must answer

No claims survived verification in these brief sections; the recommendation is weakest here:

1. **Chunking-format quantification:** restic/kopia/casync content-defined chunking at
   seconds-level snapshot frequency to S3 — upload amplification vs git incremental thin bundles
   on coding-agent write patterns; coverage of gitignored artifacts bundles can't carry.
2. **Linux kernel-side capture:** overlayfs upper-dir harvesting while the container is live
   (safe?), fanotify, snapshot-diff — does anyone do this in production?
3. **Uncovered platforms:** Coder, DevPod, Daytona (beyond volumes docs), Modal, Morph, Fly
   machines — persistence mechanisms, loss windows, any local↔cloud story.
4. **Self-hostable store choice:** MinIO post-2025 license/feature-gating vs Garage vs SeaweedFS
   (single-binary ops, S3-compat fidelity, **conditional-write support for generation fencing**).
5. **Consistency-theory precedents:** fencing tokens for storage writers, exactly-once vs
   at-least-once segment upload, crash-consistent replay guarantees.
6. **Benchmarks:** measured FUSE-T / macFUSE-FSKit latency/throughput under IDE workloads — the
   IDE-feel argument currently rests on documented mechanisms, not numbers.

---

## Source quality

26 sources fetched; per-claim sources are primary (vendor docs, project wikis, GitHub issues)
with unanimous 3-0 verification. Notable: macFUSE wiki, FUSE-T README, Mutagen docs, git docs,
Apple FSEvents guide, Gitpod/Codespaces/E2B docs + issues, Kleppmann on fencing, OSDI'14
crash-consistency (Pillai et al.) — the latter two fetched but their claims didn't survive to
verification (gap #5).
