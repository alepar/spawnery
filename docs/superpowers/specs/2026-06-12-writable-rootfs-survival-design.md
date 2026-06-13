# Writable Rootfs Survival — Design (sp-ei4.1.3)

**Date:** 2026-06-12 · **Bead:** `sp-ei4.1` (epic) / `sp-ei4.1.3` (this spec) ·
**Basis:** [research results](2026-06-12-writable-rootfs-survival-research-results.md) +
[spike results](../notes/2026-06-12-writable-rootfs-spike-plan.md) (all 3 spikes green/answered).
Supersedes the read-only-rootfs hardening flag and the cold-start-every-wake rootfs contract.

## 1. Goal & contract

The agent gets an **unrestricted-feeling writable rootfs**: `apt install`, `useradd`,
`chown`/`chmod` anywhere just work. Rootfs writes **survive suspend/resume on the same node**
with no journal involvement, and (stage 2) **migrate between nodes as a delta** through the
existing Kopia→Garage journal. Only the delta ever travels. Persistent contract:

- **Base image pinned per spawn** by digest at create; resume always uses that digest; image
  upgrades apply to new spawns only. Nodes must not GC a pinned base while a spawn references it.
- **One portable artifact across lanes:** an OCI layer tar (`.wh.` whiteouts, container-relative
  uids). Lane-specific capture code, lane-agnostic artifact — spike-verified both directions
  for Docker→runsc (runsc-diff→Docker is the same encoding; an e2e test covers it).
- Data mounts (`/app/*`) and the secrets tmpfs are **excluded** from the delta (engine capture
  excludes mounts — spike-verified). The secrets-never-persist invariant holds.
- **Gotcha (document in app-facing docs):** `/etc/hosts`, `/etc/resolv.conf`, `/etc/hostname`
  are engine-managed bind mounts — edits to them are never captured.

Two orthogonal features, separately deployable:
1. **Writable userspace** (cap relaxation) — needs userns on the Docker lane; native on runsc.
2. **Delta survival** (capture/resume) — works on any daemon, userns or not.

## 2. Per-lane mechanism (spike-verified)

### Docker/runc lane (self-hosted, dev)
- **Userns:** daemon-wide `userns-remap` (node provisioning: a remap user with a ≥65536-wide
  `/etc/subuid`/`/etc/subgid` range). No per-container userns exists in Docker — remap is the
  lane's mechanism. Nodes without remap run **degraded**: caps stay dropped as today (a
  root+full-caps+no-userns agent would be a regression), but capture/survival still works.
- **Caps:** with userns active, drop `--cap-drop=ALL`; run the agent with Docker's **default
  cap set**. **Invariant: never grant `CAP_NET_ADMIN`** — spike T5b proved it lets the agent
  flush the egress floor in the shared (sidecar-owned, same-userns) netns. Default set cannot.
- **Capture:** suspend = stop agent container → `docker commit` (4 s / 1.2 GB delta, ~90 ms
  block — spike T2) → record delta image id → remove container. The sidecar is torn down as
  today (nothing to capture there).
- **Resume (same node):** start the pod from the recorded delta image (which layers on the
  pinned base) instead of the base image. Repeat suspends chain delta layers.
- **uid-shift:** none needed — `docker save`/commit artifacts carry container-relative uids
  (spike T3: 0→0, 1000→1000 across daemons with different remap bases).

### CRI/containerd + runsc lane (cloud, multi-tenant)
- **No kernel userns.** KEP-127 is broken under runsc (gofer cannot `setns` a pinned userns —
  spike 3) and **unnecessary**: the sentry virtualizes privilege natively; apt/useradd/chown/
  setuid all pass with default caps and isolation comes from the sentry. (KEP-127 from our
  bare CRI client *does* work on runc — kept as a documented option if a CRI+runc lane ever
  ships; requires CNI-networked pods, container userns_options matching the sandbox's.)
- **runsc config:** `overlay2=none` — the default (`root:self`) swallows writes into a
  sentry-private filestore invisible to the host snapshotter (spike 2). Cost of `=none` is
  negligible for agent workloads (apt 3.61 s vs 3.34 s). Set via the runsc handler's
  `ConfigPath` toml in the node's containerd config.
- **Capture:** suspend = stop container (not remove) → containerd Go client `DiffService`
  (snapshot key = CRI container id, `k8s.io` namespace) → OCI layer tar → import as an image
  layer on the pinned base (lease-pinned) → remove container/sandbox.
- **Resume:** create the CRI container from the assembled `base+delta(s)` image.

## 3. Artifact, assembly, and squash

- Delta = OCI layer tar (`tar+gzip` when shipped). On capture the node assembles/refreshes a
  per-spawn image: manifest = `[pinned base layers…, delta layers…]`, config rewritten with
  the new diff-ids. Docker lane gets this for free via `commit`; CRI lane synthesizes the
  manifest around the DiffService output (containerd image import APIs).
- **Validate on every capture** that the manifest references the expected delta descriptor
  (guards the moby#47065 zero-layers bug class).
- **Squash:** at a configurable chain depth (default **16**, well under the ~122 overlayfs
  ceiling), merge all delta layers into one by tar-level application (respecting `.wh.`
  semantics) — a small node-side `deltamerge` helper, lane-agnostic by construction. Squash
  runs at suspend time (the pod is already down; no freeze cost).
- **Scrub at capture (config, default on):** drop `/var/cache/apt`, `/var/lib/apt/lists`,
  `/tmp`, plus a per-node extra-exclusions list (also the secrets-copy mitigation hook).

## 4. Pinning, refs & GC

- `Spawn` (node store) gains `BaseImageDigest` (set at create from the resolved image) and
  `DeltaImageRef` (node-local tag `spawnery/delta:<spawnid>`, updated per capture).
- CP store records `BaseImageDigest` on the spawn row (proto: additive field on the spawn
  object + StartSpawn carries the digest so a resume on any node pulls the exact base).
- GC: the per-spawn delta tag (Docker) / lease (containerd) keeps base+delta blobs alive.
  Spawn delete = remove tag/lease + delete delta artifacts. Reconcile treats an orphaned
  stopped agent container as **capturable**: commit it before reaping (crash-survival for
  free — the writable layer is on disk even if spawnlet died).

## 5. Data mounts & storage perms

- Replace the world-writable `0777` workaround (sp-ei4.1.1): `storage.Backend.Prepare` gains
  the node's **agent-uid mapping** (Docker lane: remap base, probed once from the daemon;
  runsc lane: 0 — container uids are literal there; degraded lane: current behavior). Host
  dirs are `chown`ed to the mapped root uid instead of 0777. Seeds keep 0644 owned by the
  mapped uid.
- Secrets tmpfs and data mounts stay `nosuid` (and data mounts `noexec` stays off — agents
  legitimately execute from work trees).

## 6. Lifecycle & component changes

- `internal/runtime`: `AgentSpec` drops `ReadonlyRootfs` (flag retired) and `DropAllCaps`
  becomes a per-node-mode decision; backends gain `CaptureDelta(ctx, h) (ref, error)` and
  resume-side `EnsureImage(base, deltas)`.
- `internal/spawnlet/manager.go`: teardown gains the capture step (suspend only, pre-removal);
  create/resume resolves `DeltaImageRef` → launches from it; squash + scrub hooks; reconcile
  capture-before-reap arm.
- CP scheduler/proto: spawn row carries `BaseImageDigest`; `StartSpawn` passes it; no other
  RPC changes for stage 0–1.
- Node config: `USERNS_MODE=remap|native|off` (off = degraded caps-dropped), `DELTA_CAPTURE=on|off`,
  `DELTA_SQUASH_DEPTH`, `DELTA_SCRUB_PATHS`, quota knobs (§7). `HARDEN_ROOTFS` is removed.
- Agent image: unchanged (still root user); the 0777/1777 hacks in the Dockerfile can stay
  (harmless) and be cleaned opportunistically.

## 7. Security & limits

- **Cap policy:** exactly the default Docker/CRI cap set when userns/sentry shields the host —
  never any `--cap-add`; an assertion in the backends rejects specs that add caps, naming
  `CAP_NET_ADMIN` (floor-defeating, spike T5b) as the canonical reason.
- **Egress floor:** unchanged mechanics; integrity holds because the agent never has
  `CAP_NET_ADMIN` (spike T5/T5b). Rootless-Podman future lane needs a different enforcement
  point (known, deferred).
- **Secrets-copy residual:** an agent that copies a secret into its rootfs puts it in the
  delta. Accepted and documented; mitigations = scrub list (§3) + owner-sealed Kopia keys
  (migration path) + the tmpfs itself never captured.
- **Disk quota:** containerd's overlayfs snapshotter is unbounded; Docker `--storage-opt size=`
  is backing-fs-dependent. Stage-1 ships a **watchdog quota**: periodic `du` of the upperdir /
  delta-size check with a soft threshold (suspend-and-warn) and hard threshold (stop). A
  proper pquota/snapshotter-native cap is a follow-up keyed to node-image provisioning.

## 8. Migration (stage 2, after same-node survival)

**2026-06-13 amendment:** Stage 2 is governed by
[Encrypted Migration Transfer Set](2026-06-13-encrypted-migration-transfer-set-design.md). Rootfs
deltas are keyed by `(spawn_id, generation)` where `generation` is the source generation that
produced the delta. Cross-node migration does not depend on a permanent "owner-sealed spawn" trait;
it depends on ceremony-first migration preflight, encrypted Garage contents, and transfer key
delivery through the owner-sealed path. Delta artifacts should be fed to Kopia uncompressed (not
pre-gzipped) so content-defined chunking can deduplicate successive deltas.

Delta layer tars ship as Kopia files in the existing per-spawn journal (CDC dedup across
successive deltas; revisit unpacked-tree storage only if dedup measures poorly). Upload is
generation-fenced like data-mount snapshots. Resume on the target: pull pinned base (by
digest), import delta tars, assemble, run. Cross-lane import is spike-verified
(`ImportIndex` normalizes Docker bundles). v3 file-capability `rootid` binding is moot in the
no-kernel-userns design (runsc) and mapping-stable on a single remapped daemon; cross-node
Docker→Docker with different remap bases loses only fcaps (not setuid bits) — documented,
acceptable.

## 9. Staged delivery

1. **Stage 0 — writable userspace:** remap provisioning + default caps (Docker lane),
   `overlay2=none` + caps (runsc lane), storage chown-into-range replacing 0777.
2. **Stage 1 — same-node survival:** capture on suspend, resume from delta, squash/scrub,
   pinning/GC, reconcile capture-before-reap, watchdog quota.
3. **Stage 2 — migratable deltas:** Kopia shipping + target-side assembly (depends on the
   journal epic's migration path, sp-u53).
4. **Stage 3 — niceties:** snapshotter-native quotas, rootless-Podman lane, KEP-127 if a
   CRI+runc lane materializes.

## 10. Testing

- Unit: manifest assembly + deltamerge (whiteout semantics, golden tars); storage Prepare
  ownership logic; cap-denylist assertion.
- e2e (build-tagged, per lane): apt-install → suspend → resume → package present; delta
  excludes mounts/secrets; uid round-trip across two remapped daemons; runsc lane capture
  with `overlay2=none`; floor-integrity probe (`iptables` denied in-pod).
