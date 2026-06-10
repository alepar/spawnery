# Tiered Storage & Migration — Deep-Research Results (cloud run)

**Date:** 2026-06-10 · **Brief:** [research brief](2026-06-10-tiered-storage-migration-research-brief.md)
· **Bead:** `sp-u53.4` · **Companion:** [in-session run results](2026-06-10-tiered-storage-migration-research-results.md)
**Provenance:** parallel cloud deep-research session over the same brief; imported verbatim below
(only this header added). Fills the in-session run's coverage gaps; merged synthesis lives in the
companion doc.

---

# Continuous File-Level Journaling and Data-Plane Workspace Migration for Spawnery

## TL;DR
- **Build, don't adopt wholesale.** The recommended architecture is a **plain bind-mounted directory + a directory-level watcher (FSEvents on macOS, fanotify/inotify on Linux) feeding a debounced, content-addressed journal to a self-hostable S3 store (Garage), with a mandatory periodic full rescan as the safety net.** This is the only design that preserves a truly native local-IDE feel on macOS — where FUSE/FSKit remains too immature for read-write IDE workloads as of mid-2026 — while reaching a seconds-to-~minute loss window.
- **Reuse libraries for the journal, not the capture.** Kopia's content-addressable repo engine (Apache-2.0, Go, S3-native, CDC dedup) is the best journal substrate; restic is the fallback. Avoid Mutagen (two-way sync semantics, wrong shape), avoid putting the whole mount on JuiceFS/FUSE-on-object (kills the mac IDE story), avoid MinIO (its repo README was changed to "THIS REPOSITORY IS NO LONGER MAINTAINED" on Feb 12, 2026 and the repository was archived read-only by the owner on Apr 25, 2026, with the last community release being RELEASE.2025-10-15T17-29-55Z).
- **Your invariants do most of the work.** Single-writer + generation fencing collapses the hard distributed-systems problem to a crash-consistent, at-least-once, idempotent segment upload with a fencing token — Kafka/HDFS epoch-fencing precedent applies directly. The genuinely hard, unavoidable problem is macOS capture fidelity; that is where to concentrate engineering.

## Key Findings

1. **macOS dominates the design.** FUSE on macOS is in retreat: macFUSE's kext requires "Reduced Security" + Recovery-mode approval on Apple Silicon, and macFUSE's kextless FSKit backend (introduced in macFUSE 5.0.0, whose release notes state "Add experimental support for FSKit on macOS 15.4 and newer… When specifying the mount-time option `-o backend=fskit`, macFUSE will use FSKit to mount the file system") is explicitly "experimental," still maturing, and carries an Apple-acknowledged performance gap ("we haven't done much to optimize the FSVolumeReadWriteOperations path, so its current performance is not great"). Therefore the macOS local node should NOT use a FUSE/FSKit mount in the critical path; it should use a plain directory + FSEvents watcher.

2. **Watchers can hit seconds-level capture but only with a rescan safety net.** Both inotify/FSEvents drop/coalesce events under load and miss editor atomic-save rename patterns; this is well-documented. A watcher + debounce can achieve a seconds-level loss window in the common case, but the only way to *guarantee* convergence is a periodic full rescan (cheap incremental hash diff) that catches anything the event stream missed.

3. **Prior art persists at suspend/stop, not continuously — Spawnery's continuous journal is genuinely differentiated.** Codespaces, Gitpod/Ona, E2B, Modal all snapshot at stop/pause or VM level; none journal uncommitted working-tree deltas continuously, and essentially none support local↔cloud workspace handoff with a live local IDE mount. Gitpod's loss model on unclean node death is "back up on stop" — an unclean kill loses the delta.

4. **The journal substrate should be content-defined-chunking CAS on S3.** restic/kopia-style CDC gives the right upload amplification on coding-agent write patterns and is crash-consistent by construction. Git-native incremental bundles are attractive (every mount is already a git tree) and are the best fit for the *first* shipping stage.

5. **Garage is the best self-hostable sink** for a small control plane (single Rust binary, ~512 MB RAM, zero external deps), with SeaweedFS as the scale-up option. MinIO is disqualified for new builds (community repo archived read-only by the owner on Apr 25, 2026; AGPL + commercial pressure).

## Details

### 1. Local capture mechanism

**The central fork, evaluated on both platforms.**

**(a) Plain directory + watcher — RECOMMENDED.**
- *Linux:* inotify (via `fsnotify`) or, better for cloud-root nodes, **fanotify** (whole-mount, fewer descriptors, can report the writing PID, supports `FAN_REPORT_FID` for rename tracking on modern kernels). inotify needs one watch descriptor per directory; on a `node_modules` tree this exhausts `fs.inotify.max_user_watches` (commonly 8192 on many distros, must be raised to ~200k+). fanotify avoids per-dir descriptor blowup.
- *macOS:* **FSEvents** (directory-granular, recursive from a single stream, kernel-coalesced). The FSEvents kernel queue is a fixed-depth circular buffer (default 4096 events; overflow sets a dropped-events / `kFSEventStreamEventFlagMustScanSubDirs` flag). This is the key reliability property: **FSEvents tells you when it dropped events and which subtree to rescan.** Latency parameter is tunable; default 0.1 s, can be set to ~1 ms for near-real-time at higher CPU.
- *Reliability limits (documented, real):* Editors do atomic save = write temp + rename-over. `fsnotify` upstream explicitly warns "watching individual files … is generally not recommended"; the fix is **always watch the parent directory, never the file**. A single Ctrl-S can emit 3–5 events (VS Code: CREATE temp + RENAME + WRITE; Vim with `backupcopy=auto`: up to 5). You must coalesce/debounce (~50 ms human-typing window, with an adaptive extension for code generators that write hundreds of files in a burst).
- *Can it hit seconds-level?* Yes in the common case: watcher → debounce (50–200 ms) → incremental hash + upload changed chunks. The failure mode when it can't (event storm, queue overflow, watcher lag) is **silent missed events** — mitigated, not eliminated, by (i) the dropped-events flag (FSEvents) and (ii) a mandatory periodic full rescan (e.g. every 15–60 s, a recursive `lstat`+size/mtime+content-hash diff against the last manifest). The rescan is the correctness backstop; the watcher is the latency optimization.
- *Complexity:* Low–moderate. No kernel modules, no kexts, no FUSE. Native IDE feel is **perfect** because it *is* a plain directory.

**(b) FUSE / FS-layer write-through — NOT RECOMMENDED on macOS, optional on Linux.**
- *macFUSE:* kext-based; on Apple Silicon requires lowering to "Reduced Security" and approving the extension in Recovery — hostile for a rootless self-hosted product. macFUSE 5.x has compatibility breakage churn on macOS 26 Tahoe (multiple reports of FUSE not detected).
- *FSKit (macOS 15.4+, shipped March 31 2025):* Apple's user-space FS API. Real, but immature for read-write: Apple's own engineer says the read/write path "is not optimized"; the macFUSE "FUSE Backends" wiki states that with the FSKit backend "Files are always opened in read/write mode. The FUSE notification API is not supported, yet. Context information (`fuse_context_t`) is not available due to the information not being provided by FSKit" (i.e. no process attribution) and that "I/O performance of FSKit volumes is not on par with volumes using the kernel extension backend"; a developer reported FSKit at ~100–150% CPU vs ~40% for the kext doing the same work. There is no mature shipping read-write FSKit filesystem as of mid-2026 — implementations are read-only (ext4 ExtendFS), PoC (OpenZFS), or carry write/space-reporting bugs (FUSE-T FSKit backend issue #98: "Mount free space is reported as zero (so no writes are possible)"). **Verdict: too risky for the IDE-native critical path now; re-evaluate in 12–18 months.**
- *FUSE-T (NFSv4-loopback, kextless):* clever, used by rclone-on-mac, but it presents as a *network volume* (shows as "localhost" in Finder), has caveats around file locking and attribute caching, and an IDE watching its own mount over a loopback NFS is a worse experience than a plain dir. Acceptable as a fallback, not a default.
- *Linux FUSE3:* fine on cloud nodes but unnecessary given fanotify/overlayfs exist.

**(c) Loopback network mounts (NFS/SMB to localhost) — NO.** This is exactly what FUSE-T does under the hood; standalone it adds the same network-volume IDE friction with no upside over a plain directory on the local node.

**(d) Linux-only kernel options (usable on cloud-root nodes).**
- **overlayfs upper-dir harvesting:** if the mount is an overlay (lowerdir = baseline, upperdir = scratch), the upperdir *is* the delta — you can journal it directly without any watcher. Strong fit for cloud nodes where the agent works on a copy-on-write layer. Gitpod found overlayfs+shiftfs interactions made Docker "unworkably slow" falling back to VFS — relevant caution, but idmapped mounts (recent kernels) fix this for root cloud nodes.
- **btrfs/ZFS snapshot + send/diff or dm-thin snapshots:** cheap atomic point-in-time snapshots + `btrfs send`/`zfs send` incremental streams. Excellent crash consistency and incrementality on cloud nodes; irrelevant on macOS.
- **fanotify:** as above, the best watcher on Linux.

**Hybrid (RECOMMENDED): mac-local uses FSEvents+rescan; cloud-linux uses fanotify or overlayfs-upperdir harvest; both emit the SAME journal segment format.** This is explicitly endorsed by the constraints and is the right call — the capture mechanism is platform-optimized, the journal format is shared, so resume/migration is platform-agnostic.

**Capture mechanism comparison table.**

| Mechanism | Platform support | Loss window achievable | IDE impact | Complexity | macOS story |
|---|---|---|---|---|---|
| Plain dir + watcher + rescan | mac (FSEvents), Linux (inotify/fanotify) | Seconds (common case); rescan-cadence worst case | **None** (it's a real dir) | Low–moderate | **Best**: native dir, no kext/FUSE |
| macFUSE (kext) | mac, Linux | Sub-second write-through | Good once mounted | High | **Bad**: Reduced Security + Recovery approval on Apple Silicon; Tahoe churn |
| FSKit (macFUSE 5 / native) | mac 15.4+ | Sub-second in theory | Degraded (perf, no FUSE notify) | High | **Immature**: experimental, perf gap, read-write bugs |
| FUSE-T (NFS loopback) | mac | Sub-second write-through | Network-volume feel, lock caveats | Moderate | Fallback only |
| overlayfs upperdir | Linux only | Continuous (upperdir = delta) | None (cloud-side) | Moderate | N/A (cloud nodes) |
| btrfs/ZFS/dm snapshot | Linux only | Seconds (snapshot cadence) | None (cloud-side) | Moderate–high | N/A (cloud nodes) |

### 2. Journal format & transport

**Incremental/dedup format comparison.**
- **Content-defined chunking (restic, kopia, borg, casync):** Rabin/rolling-hash chunk boundaries → insert/delete-resilient dedup. restic uses a 64-byte sliding-window Rabin fingerprint and per its design docs "Files smaller than 512 KiB are not split, Blobs are of 512 KiB to 8 MiB in size. The implementation aims for 1 MiB Blob size on average," with SHA-256 content addressing and blobs packed into pack files. Kopia: content-addressable blob store (CABS), also CDC, Apache-2.0. For coding-agent patterns (many small edits + occasional huge `node_modules` churn) CDC is near-optimal: a one-line edit re-uploads one ~1 MiB chunk; an unchanged file is free. **This is the right format.**
- **Git-native incremental packfiles/bundles of a throwaway WIP ref:** Every mount is already a git tree, so `git bundle create` against a baseline ref produces thin packs (`git bundle` uses `--thin` for exclusion-based bundles) that are cheap to produce and dedup against history. **Best fit for Stage 1** (piggyback the existing suspend-persist). Limitation: git tracks committed/added content well but untracked + ignored files need explicit `git add -A` to a scratch index, and binary churn doesn't delta as well as CDC.
- **Object-per-file CAS:** simplest, but poor amplification on large files with small edits (whole-file re-upload). Acceptable only for tiny mounts.
- **rsync deltas:** good wire efficiency but stateful (needs both ends); awkward against an object store.

**Crash consistency of replay.** Given the journal is built from filesystem snapshots/scans, the honest guarantee to *declare* is: **per-file atomic** (a file is journaled as a complete content-hash; you never replay a half-written file *if* you capture on `IN_CLOSE_WRITE`/post-rename, not mid-write), **cross-file best-effort** (a multi-file logical change may be split across segments; replay may land a torn cross-file state), and **git index may need rebuild** on resume (`git status` re-derives working-tree state from the materialized files, so a stale/absent index self-heals). restic's repository format already guarantees "repository modifications always maintain a correct repository even if the client or the storage backend crashes" — inherit that property by using a CAS engine rather than hand-rolling.

**Lifecycle mechanics.**
- *Baseline + delta chain:* periodic baseline snapshot (e.g. the per-turn git commit / a full manifest) caps replay chain length. Resume = pull baseline + replay deltas since.
- *Compaction/GC:* CAS engines GC unreferenced chunks (kopia/restic `prune`); prune old suspend generations.
- *Resume-side materialization latency:* cold cloud node pulling a multi-GB mount is bounded by object-store throughput; parallel chunk fetch (restic restorer downloads whole pack files in parallel) and a local block cache help. This is the dominant resume cost and should be measured.
- *Fencing integration:* every journal segment is stamped with the spawn's monotonic generation; the store/control-plane rejects segments from a stale generation (see §5).

**Self-hostable store.** See Key Finding 5 + table. **Garage** for the small self-hosted control plane (single binary, ~512 MB RAM, zero deps, S3 core: GET/PUT/DELETE/multipart/presigned). It lacks object-lock/versioning/lifecycle — fine, because the CAS engine handles immutability and GC itself. **SeaweedFS** if you outgrow a single binary. **MinIO is out** (community repo archived read-only by the owner Apr 25, 2026; last community release RELEASE.2025-10-15T17-29-55Z; AGPL + aggressive commercial licensing). Note Garage is AGPL-3.0 — acceptable when self-hosted/unmodified, but check your distribution model.

**Journal-format / tool comparison table.**

| Format / tool | Incrementality | Replay guarantees | Go-embeddability | S3 fit | License |
|---|---|---|---|---|---|
| CDC CAS (Kopia) | High (rolling-hash dedup) | Crash-consistent repo, content-addressed idempotent | Library | Native | Apache-2.0 |
| CDC CAS (restic) | High (1 MiB avg blobs) | Crash-consistent repo format | CLI/subprocess | Native | BSD-2 |
| Git thin packs/bundles | High vs history (tracked) | Git-native; index rebuildable | subprocess | Via blob PUT | GPL-2 |
| Object-per-file CAS | Low (whole-file) | Per-file atomic | Trivial DIY | Native | n/a |
| rsync delta | High (wire) | Stateful, not object-native | CLI | Poor | GPL |

### 3. Prior art

**Dev-env platforms.**
- **GitHub Codespaces:** persists the `/workspaces` directory (mounted, survives stop/start and rebuild); everything outside (except `/tmp`) is container-lifecycle-bound. **Loss model: stop/start safe, but unclean host failure loses uncommitted work** — documented real incident: a May 2025 maintenance window left users with empty `/workspaces` and lost ~48 uncommitted changes. No continuous journaling; no local↔cloud handoff (local VS Code attaches to the remote container, it does not move the workspace local).
- **Gitpod (now Ona):** classic model persists only `/workspace`, backed up **on stop** ("Stopping … backup is being taken"); on unclean node eviction Gitpod's own design issue describes backing up the persistent volume to object storage as a *separate, best-effort* path. Prebuilds snapshot `/workspace` only. **Loss window on unclean death: the whole since-last-backup delta.** No live local-IDE mount handoff.
- **Coder / DevPod / Daytona:** Coder persists via the workspace's underlying volume (Terraform-defined); DevPod is client-only and persists via Docker volumes/mounted project path ("changes only to the project path or mounted volumes will be preserved"), connecting **local** IDEs (VS Code Remote, JetBrains Gateway) over SSH to a workspace that can be local or remote — closest prior art to Spawnery's local/remote symmetry, but it's SSH-attach, not data migration, and not continuous journaling.

**Agent-sandbox platforms.**
- **E2B:** Firecracker microVM; pause/resume saves **filesystem + memory**; per E2B docs "Pausing a sandbox takes approximately 4 seconds per 1 GiB of RAM" and resume is roughly a second, with paused sandboxes kept indefinitely. Data plane = VM snapshot, not file-level.
- **Modal:** gVisor; Filesystem Snapshots (stable, stored as Images, persist indefinitely), Memory Snapshots (alpha, 7-day expiry), Volumes for persistence. Snapshot = new sandbox, not resume-in-place.
- **Morph / Fly Sprites / Daytona:** Morph/E2B give session-scoped persistence; Fly Sprites persist filesystem indefinitely with sub-second checkpoint/restore. **All snapshot at VM/memory level.**
- *What to steal at file level:* nothing directly — their data-plane stories are VM-snapshot, which Spawnery explicitly rules out (no CRIU/memory). The *file-level* continuous journal is Spawnery's differentiator.

**Sync-centric precedents.**
- **Mutagen:** Go, two-way sync engine; powers Docker Compose dev sync via a sidecar; handles 10k–100k files; defaults to bidirectional. Can be coerced one-way but the model and resumable-initial-sync behavior fight you (issue #255: initial sync restores deleted files). **Wrong shape for a single-writer journal-to-S3.**
- **Unison / Syncthing:** bidirectional file sync for *replica reconciliation*; both assume multi-endpoint convergence, neither targets object-store journaling or seconds-level crash recovery. Poor fit.

### 4. Adopt vs build — embeddable-tool landscape

| Tool | License | Maint. 2025–26 | Go-embeddability | Fit to single-writer journal→S3 | Verdict |
|---|---|---|---|---|---|
| **Kopia** | Apache-2.0 | Active | **Library** (`github.com/kopia/kopia` repo/snapshot Go APIs) | CDC CAS on S3, snapshot freq down to seconds (checkpoint interval ≤45 min, snapshots have been observed running every ~20–30 s); strong | **Top journal substrate** |
| **restic** | BSD-2 | Active | Mostly CLI/subprocess (library use unsupported-ish) | CDC CAS on S3, crash-consistent repo format | Strong fallback |
| **Mutagen** | MIT | Active | Library/daemon | Two-way sync; wrong semantics | Reject for journal |
| **rclone / librclone** | MIT | Very active | **C-shared/Go lib** (`librclone`, RPC shim) | sync/copy to S3, not incremental-dedup; good for bulk materialize/transport | Use for transport/restore, not as the dedup journal |
| **JuiceFS** | Apache-2.0 (CE) | Active | FUSE FS on S3 + Redis/DB metadata | Whole-mount-on-object solves capture+journal in one move **on Linux**, but needs a metadata DB and **FUSE — kills the mac IDE story** | Reject for mac; possible cloud-only option |
| **SeaweedFS-FUSE** | Apache-2.0 | Active | FUSE FS on object | Same FUSE/mac problem | Reject for mac |
| **git plumbing (bundles/packfiles)** | GPL-2 (git) | Active | subprocess | Incremental thin packs on a WIP ref; native to your git-tree mounts | **Best for Stage 1** |
| **git-annex** | AGPL | Active | subprocess | Large-file tracking, timer commits; heavyweight | Optional |
| **Syncthing / Unison** | MPL-2 / GPL | Active | daemon / CLI | Bidirectional replica sync | Reject |

**Conclusion: build-on-libraries.** Wrap **Kopia's repo engine** (or restic) as the journal CAS, drive capture yourself (FSEvents/fanotify/overlayfs), use **librclone** for bulk transport/restore, and use **git bundles** for the Stage-1 piggyback. Do not adopt a turnkey sync tool (Mutagen) or a whole-mount FUSE FS (JuiceFS) as the primary architecture.

### 5. Consistency theory (only what applies)

Single-writer + generation fencing means **CRDT/multi-master literature is inapplicable** — there is exactly one authoritative writer per spawn at a time. What applies:
- **Crash-consistency of replay:** declare per-file-atomic / cross-file-best-effort / index-rebuildable (see §2). Capture on close/rename, not mid-write, to avoid torn single files.
- **At-least-once + idempotent segment upload:** content-addressed segments are idempotent by hash; re-uploading a segment after a crash is a no-op. This gives effectively exactly-once materialization without distributed-transaction machinery.
- **Fencing tokens for storage writers:** Spawnery's monotonic generation *is* a fencing token. The direct precedent is Kafka's producer **epoch**: per the `ProducerFencedException` javadoc, "It is only possible to have one producer instance with a `transactional.id` at any given time, and the latest one to be started 'fences' the previous instances so that they can no longer make transactional requests," and the KIP-98 idempotent-producer design requires that "the generation must equal the generation stored by the server or be one greater. Incrementing the generation will fence off any messages from 'zombie' producers." HDFS/leadership-epoch fencing follows the same rule: leases provide liveness, fencing provides safety. The storage/control-plane MUST reject journal segments stamped with a generation lower than the current claim — this is what stops a partitioned-then-revived old container ("zombie writer") from corrupting the journal. **This must be enforced server-side at the write boundary, not trusted to the client** (a stalled process whose connection never died will otherwise keep writing as if nothing changed).
- **IDE-and-agent co-writing one directory:** This is *co-writing one local FS*, not replica conflict — last-writer-wins at the filesystem level is already the OS's behavior, and the journal simply records whatever the FS converges to. No advisory locking is required for correctness of the journal (the FS arbitrates). Optionally, advisory `flock` coordination between the agent and a Spawnery IDE helper can avoid the agent and user clobbering the same file mid-edit, but dev-env products generally do not do this and rely on FS-level last-writer-wins + git to surface conflicts.

## Recommendations

**Recommendation framework.** Optimize for, in priority order: (1) native local-IDE feel on macOS (hard constraint), (2) seconds-to-minute loss window on unclean death, (3) low ops for a self-hosted control plane, (4) migration completeness. These rank a plain-dir+watcher+CAS-journal architecture first because it's the only one that satisfies (1) on macOS without betting on immature FSKit.

**Concrete recommendation for Spawnery's exact constraints:**
- **Capture:** plain bind-mounted directory on all node types. mac-local: FSEvents (low latency, honor the must-rescan flag) + periodic full rescan. cloud-linux: fanotify (or overlayfs upperdir harvest if the mount is overlay-backed) + periodic rescan. rootless-linux-local: inotify with raised `max_user_watches`, or fanotify if permitted.
- **Journal:** content-addressed CDC segments via an embedded Kopia/restic-class engine, stamped with the spawn generation, written to **Garage** (self-hosted) / any S3.
- **Policy on `.gitignore`'d artifacts:** **journal them by default but in a separate, lower-priority chunk stream / object class** with aggressive dedup and shorter retention, OR exclude `node_modules`/build dirs and regenerate on resume. Recommended: **exclude large regenerable trees from the seconds-level journal but capture a manifest of them**; journal them on suspend/teardown only. Rationale: `node_modules` churn would otherwise dominate upload amplification and blow the loss-window budget, while being regenerable; but capturing them at suspend makes migration instant. Make it per-mount configurable.
- **Migration:** suspend = flush journal + WIP commit on `spawnery-suspend/<id>/<gen>` branch (existing) → resume on node B = materialize baseline + replay journal to current generation, rebuild git index, restart agent. mac↔linux works because the journal format is platform-neutral (store path/mode/content-hash; normalize line-ending/permission/xattr handling explicitly).

**Staged adoption path with trigger thresholds:**
1. **Stage 0 — Per-turn + suspend snapshot piggyback (ship first).** Reuse the existing suspend-persist: on each agent turn and on suspend, `git add -A` working-tree delta to a scratch index and write an **incremental git bundle / thin pack** of the WIP ref to the blob store. Loss window = one agent turn. Zero new capture infrastructure. **Trigger to advance:** measured loss window unacceptable (users lose >1 turn of work on crashes) OR turns run long enough that intra-turn loss matters.
2. **Stage 1 — Watcher-based continuous journal.** Add FSEvents/fanotify watcher + debounce + periodic rescan, emitting CDC segments continuously between turns. Loss window → seconds. **Trigger to advance:** watcher proves unable to hit the loss window on large trees (rescan cost too high, event storms), OR you need write-through durability stronger than rescan cadence, OR a customer needs guaranteed-no-loss.
3. **Stage 2 — FS-layer capture, IF justified.** Only if Stage 1's rescan/coalesce cannot meet the SLA: on **Linux cloud nodes**, move to overlayfs-upperdir or btrfs/ZFS-snapshot capture (strong, cheap, already root). On **macOS**, do NOT adopt FSKit until it is demonstrably production-grade for read-write IDE workloads (re-evaluate when macFUSE's FSKit backend drops "experimental" and Apple ships the requested I/O passthrough API; note macFUSE 5.3.0, released 08 Jun 2026, already added an MFMount.framework channel API that "allows for zero-copy reads and writes for large payloads when using the FSKit backend" — a signal the gap is actively closing). **Trigger:** Linux — Stage 1 CPU/IO overhead or loss window measured inadequate at scale; macOS — FSKit read-write maturity milestone reached AND benchmarked within ~10–20% of native.

**Signals/metrics to instrument from day one:** journal lag (time from write to durable segment), rescan duration per mount, dropped-event-flag frequency, upload amplification ratio (bytes uploaded ÷ bytes changed), resume materialization time, and fencing-rejection counts (stale-generation writes — a spike means zombie writers / claim bugs).

## Caveats

- **macOS FSKit evidence is thin and fast-moving.** The performance/maturity claims rest substantially on Apple Developer Forums posts and the macFUSE wiki, not formal Apple benchmarks; there is no published throughput/latency number comparing FSKit to the kext. The picture could improve materially within 12–18 months (macFUSE added a zero-copy channel API in macFUSE 5.3.0 on 08 Jun 2026). Re-evaluate before committing to any FUSE/FSKit path on Mac.
- **fanotify rename/PID-attribution features depend on kernel version** (`FAN_REPORT_FID`/`FAN_REPORT_DFID_NAME` need ~5.1+/5.9+). Verify the cloud node kernel baseline.
- **The "exclude vs journal node_modules" decision is a genuine tradeoff with no universal answer** — it's correctly an open per-mount policy. Measure both on representative workloads.
- **Garage's S3 feature coverage is partial** (no object-lock/versioning/lifecycle); this is fine for a CAS-engine-managed journal but verify your engine doesn't depend on bucket versioning.
- **Cross-platform fidelity (mac↔linux migration) needs explicit handling** of case-sensitivity (APFS default case-insensitive vs Linux case-sensitive), permission/uid mapping (rootless mac vs root linux), symlinks, xattrs, and line endings. The journal format must normalize or faithfully record these; silent divergence here is the most likely source of subtle migration bugs.
- **Prior-art loss-window numbers** for Codespaces/Gitpod are inferred from documented behavior and incident reports, not from published SLAs; treat as directional.