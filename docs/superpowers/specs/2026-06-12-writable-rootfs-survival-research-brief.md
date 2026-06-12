# Deep-Research Brief — Survivable Writable Rootfs + Unrestricted System Writes for Agent Pods

**Date:** 2026-06-12 · **For:** designing the spawn container's filesystem permission +
persistence model (extends [E1 Runtime Core](2026-05-27-spawnery-e1-runtime-core-design.md),
[Transient Tier](2026-06-10-transient-tier-kopia-journal-design.md),
[runsc CRI Pod Backend](2026-06-01-runsc-cri-pod-backend-design.md)). **Bead:** `sp-ei4.1`.

Copy the prompt below into a deep-research agent. It is self-contained.

---

## PROMPT

You are a container-runtime/systems researcher. Produce a rigorous, citation-backed report on
**giving a sandboxed coding agent an unrestricted-feeling, writable root filesystem whose
modifications survive pod teardown and migrate between nodes as a delta**. Be concrete,
version-specific, and skeptical of marketing — prefer primary sources (runtime source/docs,
KEPs, kernel docs, benchmarks you can trace, postmortems, engineering blogs from platforms
running comparable systems). Note dates and versions; flag where evidence is thin or contested.

### Context (the system being designed)

- **Spawnery** runs "spawns": sandboxed coding-agent pods (an untrusted **agent** container + a
  trusted inference **sidecar** sharing one netns) on **nodes**. A Go node daemon ("spawnlet")
  manages pods via two pluggable backends ("lanes"):
  - **Docker/runc lane** — Docker Engine API; rootful today, rootless Podman desired later.
  - **CRI lane** — containerd via CRI, with **gVisor (`runsc`)** as the OCI runtime on cloud
    nodes (multi-tenant).
- **Agent container today:** runs as in-container **root (uid 0)** with `--cap-drop=ALL`. No
  `CAP_DAC_OVERRIDE`/`CAP_CHOWN`/`CAP_SETUID` means `apt update` (which setuids to `_apt` and
  chowns its partial dirs), `dpkg`, `useradd`, and plain `chmod`/`chown` of non-owned files
  **fail**. The image carries 0777/1777/0666 workarounds. There is an optional read-only-rootfs
  hardening flag (off by default) that this design will replace.
- **Data mounts:** an app declares N named mounts, each a host dir bind-mounted read-write at
  `/app/<path>`, realized by Go `StorageBackend{Prepare, Finalize}` plugins. A per-spawn
  **secrets tmpfs** is bind-mounted separately and must NEVER be persisted (owner-sealed
  secrets invariant).
- **Existing journal infra (FIXED):** per-spawn **embedded-Kopia** repositories journal the
  data mounts to a self-hosted **Garage** S3 store, watcher-triggered, generation-fenced.
  Suspend-based, data-only migration: suspend on node A → resume on node B. **No CRIU, no
  memory/VM snapshots.**
- **Single-writer + fencing:** exactly one live pod per spawn; a monotonic **generation**
  stamps every pod (pods are *recreated* per generation, not restarted) and storage writes;
  stale generations are rejected.

### Decisions already made (validate mechanics, do not re-litigate goals)

1. **Working design to validate — delta-as-OCI-layers + user namespaces:**
   - At suspend, capture the agent container's writable layer as an **OCI image layer**
     (Docker lane: `docker commit`; CRI lane: containerd snapshot **diff** via its Go API).
   - Resume runs `pinned-base + delta-layer(s)`. Migration ships **only the delta layer
     tar(s)** through the existing Kopia→Garage journal and replays them on the target node.
   - **User namespaces** give in-container root namespaced capabilities so apt/chown/chmod
     just work, while mapping to unprivileged high uids on the host.
2. **Image pinned per spawn:** each spawn records the image digest it started on and resumes
   on that exact digest for its whole life; upgrades apply to new spawns only. Nodes must not
   GC pinned images while a live spawn references them.
3. **Survival contract:** suspend/resume **on the same node** preserves rootfs writes with no
   journal involvement (keep the delta locally); **migration** is delta-only via Kopia.
   Bonus this should enable: agent transcript/session resume (agent state lives in the home
   dir, i.e. in the rootfs delta).
4. **Both lanes converge on the same semantics** (capture/replay artifacts portable across
   lanes); lane-specific capture code is acceptable, lane-specific *artifacts* are not.

### Research questions (address each as its own section)

**1. User namespaces per engine — the support matrix.**
- **Docker Engine:** daemon-wide `userns-remap` vs per-container userns — what exists as of
  Docker 25–28? Interaction with `--cap-drop`/`--cap-add` inside a userns; what cap set inside
  the userns makes `apt`, `dpkg`, `useradd`, `chown -R` work end to end (verify against apt's
  actual privilege-drop behavior). Rootless **Podman** (`--userns=auto`) as the
  self-hosted-node future.
- **containerd/CRI:** KEP-127 pod user namespaces — exact state in containerd 1.7 vs 2.x, the
  CRI `NamespaceMode`/`UserNamespace` plumbing, idmapped-mount requirements (kernel ≥? which
  filesystems), and whether a non-Kubernetes CRI client (our node daemon) can drive it
  directly.
- **gVisor/runsc:** how the sentry implements user namespaces; does runsc-under-containerd
  support KEP-127 userns pods; do namespaced caps inside the sentry behave like kernel userns
  for apt-style workloads?
- **Mounts under userns:** what uid does the agent see on the `/app` bind mounts and the
  secrets tmpfs — are **idmapped bind mounts** needed (kernel/runtimes that support them per
  lane), or do we chown host dirs into the mapped range? What happens to the existing
  world-writable-0777 workaround?
- **Gotchas:** overlayfs-as-upper inside a userns, AppArmor/SELinux denials, `/proc`/`sysctl`
  restrictions, unprivileged-userns kernel CVE history (is enabling userns a net security gain
  or loss vs cap-drop-ALL without it — see also Q7), nested userns (agent running its own
  containers later).

**2. Delta capture mechanics.**
- **Docker lane:** `docker commit` semantics on a running vs paused vs stopped container
  (consistency of the captured layer, default pause behavior, latency on multi-GB deltas);
  repeated suspend cycles → layer-chain growth (overlayfs lowerdir limits ~128; when and how
  to squash); does commit capture xattrs/whiteouts/opaque dirs faithfully; what does commit
  do with the container's mounts (verify bind mounts and tmpfs are excluded).
- **CRI/containerd lane:** producing a diff layer from a **CRI-created** container's snapshot
  using containerd's Go client (diff service, `k8s.io` namespace interop, snapshotter key
  discovery from the CRI container id); doing this while the container is stopped-but-not-
  removed vs after removal; required privileges.
- **runsc interaction:** runsc sits above the snapshotter, but verify: with runsc's
  `--overlay2` rootfs overlay enabled (default `root:self` in recent versions?), do container
  writes ever reach the host snapshotter upperdir at all — i.e., **must `--overlay2` be
  disabled (or set to a host-visible mode) for delta capture to see anything**, and what is
  the gofer-performance cost of that?
- **Capture timing:** can capture run at suspend without racing the dying container; fsync/
  quiesce needs; behavior when the delta contains huge transient junk (`/var/cache/apt`,
  build artifacts) — exclusion/trimming options at capture time vs accepting the bytes.

**3. Resume + cross-node replay.**
- Assembling `pinned-base + delta(s)` on a target node: `docker load`-style import vs
  containerd image import from layer tars; building a valid OCI image manifest around an
  out-of-band-shipped delta layer; digest bookkeeping.
- **Cross-lane portability:** a layer tar captured on the Docker lane applied on a containerd
  node (and vice versa) — format pitfalls (legacy Docker tarball vs OCI layout, whiteout
  encoding differences, xattr namespaces under userns remapping — uids inside the tar!).
  Uid-shift correctness when source and target nodes use different userns mappings.
- **Kopia fit:** chunking/dedup behavior on layer tarballs (does content-defined chunking
  dedupe successive deltas well, or should the journal store the *unpacked* delta tree
  instead); size accounting; generation fencing of delta uploads.
- Node-side **image GC + pinning**: keeping pinned digests alive per live spawn on Docker and
  containerd (lease APIs), and reclaiming when spawns are deleted.

**4. Fallback architecture — engine-native upperdir preservation (compare honestly).**
- Suspend = stop-but-keep the container (Docker `stop`/`start`; CRI stopped container with
  sandbox kept or recreated) so the engine retains the writable layer; migration = read the
  graph-driver/snapshotter **upperdir** directly and journal that tree via Kopia.
- Costs: coupling to graph-driver internals and paths, overlayfs whiteout (char-0:0 device
  nodes) + `trusted.overlay.*` xattr survival through Kopia snapshot/restore, conflict with
  the recreate-per-generation fencing model, rootless access to upperdirs.
- Verdict vs the layers approach: when (if ever) is this the better mechanism, e.g. for
  same-node suspend/resume where zero capture cost matters?

**5. Package-manager reality check.**
- Enumerate what `apt`/`dpkg` actually require (caps, setuid transitions to `_apt`, chown of
  `partial/`, maintainer scripts running `useradd`/`chmod`), and verify the proposed userns
  cap set satisfies them. Same, briefly, for `npm -g`, `pip`, `cargo install`, `curl | sh`
  installers.
- `nosuid`/`noexec` mount-flag interactions (setuid binaries installed into the delta;
  sudo's setuid bit under userns).
- Should the image ship **sudo + a non-root default user** for UX parity with dev machines,
  or stay root-by-default once userns makes root safe? What do comparable products ship?

**6. Prior art — who persists agent/dev-env rootfs writes, and how.**
- **Modal** (image layers + filesystem snapshots — their sandbox `snapshot_filesystem` story
  seems closest to our working design: mechanism, limits, pricing of the bytes), **E2B**
  (Firecracker memory+disk snapshots), **Daytona**, **Replit** (historical overlayfs/btrfs
  approaches), **GitHub Codespaces / Gitpod-Ona** (persist `/workspace` only vs full rootfs —
  what do they lose on rebuild), **Fly machines**, **Sprites/Morph** if documented.
- For each: capture mechanism, granularity (full disk vs delta), restore latency, size
  limits/quotas, and any published postmortems or known failure modes.
- What do they do about **image upgrades under preserved deltas** (our pinning decision —
  does anyone do better)?

**7. Security delta.**
- Honest before/after: today = root + cap-drop-ALL, no userns. Proposed = root-in-userns with
  a generous in-userns cap set. Which is the stronger sandbox against kernel LPE, given
  unprivileged-userns CVE history vs the caps the agent currently lacks? How much does runsc
  change this calculus on the cloud lane (sentry-mediated syscalls)?
- Interaction with the **egress floor** (iptables rules applied in the pod netns keyed by pod
  IP): does userns change netns/iptables behavior or the node's ability to apply the floor?
- **Secrets leakage:** agent copies a secret from the secrets tmpfs into the rootfs → it
  lands in the delta → journaled to Garage. Mitigations: owner-sealed Kopia keys (already
  designed), capture-time exclusion lists, or acceptance with documentation?
- **Disk bombs:** bounding delta growth — overlay upperdir quotas (xfs project quotas,
  containerd snapshotter size limits, Docker `--storage-opt size=` support matrix per
  graph driver/backing fs), and what to do at the cap (fail writes vs suspend-and-warn).
  Ties into the existing resource-limits design (per-spawn CPU/mem/pids already enforced).

### Deliverable

- A structured report with the seven sections above, plus:
  - a **support matrix**: userns capability (per-container userns, idmapped mounts, cap
    re-grant inside userns) × {Docker rootful, Podman rootless, containerd/CRI+runc,
    containerd/CRI+runsc}, with minimum versions;
  - a **comparison table** for capture mechanisms (layers-via-commit/diff vs upperdir
    harvesting vs runsc-internal overlay): same-node resume cost, migration artifact,
    cross-lane portability, fencing fit, failure modes;
  - a **concrete recommendation** for Spawnery's exact constraints (both lanes, same
    artifact, pinned base images, same-node survival without journal, delta-only migration
    via Kopia→Garage, generation fencing, owner-sealed secrets invariant);
  - a staged **adoption path** (e.g. userns + writable rootfs first → same-node delta
    survival → migratable deltas), with the signals/thresholds that should trigger each
    stage;
  - explicit callouts where **runsc constraints dominate** (cloud lane) and where evidence
    is weak or contested.
- Cite versions, dates, and sources throughout. Call out where our assumptions (pinned
  images, recreate-per-generation pods, suspend-based migration, single-writer fencing)
  materially change the analysis versus general-purpose container checkpointing.
