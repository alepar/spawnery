# Writable-Rootfs Spike Plan (sp-ei4.1, pre-spec gates for sp-ei4.1.3)

**Date:** 2026-06-12 · **Decision:** spike-first, spec-after (brainstorm w/ alepar). The
"one artifact across both lanes" contract means the capture mechanism itself could change if
runsc can't surface writes to the snapshotter — so the design spec waits on these results.
Research basis: [research results](../specs/2026-06-12-writable-rootfs-survival-research-results.md).

**Environment:** all on this host (Docker 29.4.2, Podman 5.8.2 rootless, containerd + runsc
installed, kernel 7.0.8) via **dedicated isolated daemon instances** — own data-root/socket/
state dirs; the main Docker/containerd daemons are never reconfigured. Root needed for daemon
launches. Spike scripts/scratch live under `hack/spikes/` (not shipped product code).

## Spike 1 — Docker lane PoC (bd: see sp-ei4.1 children)

Second dockerd `--data-root /var/tmp/spike-docker-a --userns-remap=default` + third with a
**different subuid base** for the cross-node leg. Pass = ALL green:

1. Under remap with the **default cap set** (no `--cap-drop=ALL`): `apt update && apt install`,
   `useradd`, `chown -R` succeed in the agent-like container.
2. `docker commit` on a ~1 GB delta: measure the pause window; verify the layer tar **excludes
   bind mounts + tmpfs** content.
3. `docker save` → `docker load` on the differently-remapped daemon → run `pinned-base+delta`:
   packages + `/etc` edits intact; **in-container uid 0 round-trips** (uid-shift correctness).
4. Data-mount dir **chowned into the mapped range** is agent-writable (0777 replacement).
5. Floor integrity: agent with default caps **cannot** modify iptables in the pod netns
   (no `CAP_NET_ADMIN`); confirm against a sidecar-owned netns shape (`--network=container:`).
6. Rootless-Podman data point: same workload under `podman run --userns=auto` (no daemon cfg).

## Spike 2 — runsc/containerd capture visibility

Dedicated containerd instance (own config/root/state, CRI on, runsc runtime). Questions:

1. With runsc `--overlay2` at its **current default**: do container writes reach the host
   snapshotter upperdir at all? Flip to host-visible mode (or disable) and rough-measure the
   gofer cost (simple fio/dd + apt-install wall-clock).
2. Produce a **diff layer via containerd's Go `Diff` service** from a CRI-created container
   (snapshotter key discovery from the CRI container id, `k8s.io` namespace).
3. **Cross-lane leg:** import Spike 1's Docker-captured delta into containerd
   (`archive.ImportIndex`) and run `pinned-base+delta`.

## Spike 3 — KEP-127 pod userns from a bare CRI client

Same containerd instance. Questions:

1. Drive `UserNamespace`/`NamespaceMode` in the sandbox/container config from a **minimal Go
   CRI client** (no Kubernetes) — does containerd 2.x accept and construct the pod userns?
   Verify `setgroups=allow` inside (post-#10611 build).
2. Inside a **runsc** pod with userns: `apt update && apt install` — sentry userns/cap
   fidelity. This is the decisive cloud-lane gate.

## Spike 1 — RESULTS (2026-06-12, host: Docker 29.4.2, kernel 7.0.8) — ALL PASS

Setup: two dockerd instances, `--userns-remap=spikea` (base 700000) and `spikeb` (base 800000),
own data-roots/sockets/bridges. Second daemon needs `--iptables=false` (DOCKER-chain clash).

1. **apt/useradd/chown/chmod-setuid: PASS** with the **default cap set** under remap
   (`uid_map: 0→700000 #65536`). `--cap-drop=ALL` control reproduces today's
   setgroups/setegid/seteuid failures. No custom cap list needed — default set suffices.
2. **Commit: PASS** — 1.19 GB delta committed in **4.0 s**; container blocked only ~**93 ms**
   (exec probe during commit). Layer tar **excludes bind-mount + tmpfs content** (bare
   mountpoint dir entries only); deletion encoded as OCI **`usr/.wh.games`** whiteout —
   commit normalizes to the portable encoding. **Gotcha:** `/etc/hosts`, `/etc/resolv.conf`,
   `/etc/hostname` are engine-managed bind mounts → their edits are NEVER captured by commit.
3. **Cross-daemon uid-shift: PASS** — `docker save` writes **container-relative uids** (0 stays
   0, 1000 stays 1000) — the artifact is mapping-agnostic; load+run on base-800000 daemon:
   packages, useradd'd user, chowned files, whiteout deletion all intact.
4. **0777 replacement: PASS** — chown host dir to `<base>:<base>` → container-root writes fine;
   unmapped host uids surface as 65534 (nobody) as predicted.
5. **Floor integrity: PASS** — agent with default caps **cannot read or flush** iptables in the
   sidecar-owned netns (EPERM). **T5b:** with `--cap-add NET_ADMIN` the agent CAN flush it
   (netns owned by the shared remap userns) → **invariant: never grant the agent CAP_NET_ADMIN**.
6. **Podman data points:** default rootless mode runs the full workload green (root-in-userns
   already). `--userns=auto` is **broken on podman 5.8.2 + crun** (`gid_map` EPERM even with a
   1M-wide subuid allotment; also requires expanding the default 65536 allotment at all) —
   future-lane follow-up, not load-bearing now.

## Spike 2 — RESULTS (containerd 2.2.3 + runsc release-20260525.0) — ALL ANSWERED

Dedicated containerd (own root/state/socket, CRI on, runsc handler, bridge CNI conf).

1. **CONFIRMED — default `--overlay2` (root:self) swallows writes:** host upperdir contains
   only a sentry-private `.gvisor.filestore.<sandbox>` blob (1 GB sparse) + CRI's `etc` copies;
   the in-container write never materializes. Host-side diff would capture garbage.
2. **`overlay2=none` → full host visibility:** files + kernel char-dev 0:0 whiteouts appear in
   the upperdir. **Gofer cost is negligible for our workloads**: apt update+install 3.61 s vs
   3.34 s (default), dd 256 MB ≈ equal. → cloud lane runs `overlay2=none`.
3. **Diff capture works:** the CRI container id IS the snapshot key (`k8s.io` ns); containerd's
   DiffService emits a proper OCI layer tar — kernel whiteout translated to **`.wh.media`**,
   uids container-relative.
4. **Cross-lane PASS:** the Spike-1 `docker save` bundle imports via `ImportIndex` and runs
   under runsc — package, useradd'd user, chowned uids (1000) all intact.
5. Plumbing notes: host-net runsc pods need `network = "host"` (hostinet) in runsc.toml; CRI
   `dns_config` required (systemd-resolved stub unreachable from the sandbox); `apt-get update`
   exits 0 even when all fetches fail — never use it as a health signal (matches research S5).

## Spike 3 — RESULTS — KEP-127 runc PASS / runsc FAIL-BUT-UNNECESSARY

1. **KEP-127 pod userns from a bare CRI client (crictl ≅ spawnlet): PASS on runc.** Sandbox +
   container both carry `userns_options{mode:POD, uids/gids 0→900000 #65536}`; containerd 2.2.3
   constructs it; `setgroups=allow` (post-#10611); apt install, useradd+chown, chmod setuid all
   green. Constraints: container userns_options must MATCH the sandbox's; **hostNetwork pods are
   rejected** (userns pinning needs a sandbox netns → CNI required).
2. **KEP-127 + runsc: BROKEN** at this pairing — the gofer cannot `setns` into the pinned userns
   (`invalid argument`; multithreaded Go process cannot join a userns). Do not design for it.
3. **runsc does NOT need kernel userns:** without userns the sentry already virtualizes
   privilege — uid_map reads `0→0 #4G` (sentry-internal), and apt/useradd/chown/chmod-setuid/
   su-to-uid-1000 all PASS with default caps. Gofer writes host files with **container-relative
   uids** (u2's home = 1000:1000 in the upperdir), so diffs need no uid shifting; isolation
   comes from the sentry, not the mapping (upperdir lives under root-only containerd paths).

**Net design input:** Docker lane = daemon userns-remap + default caps + commit/save;
CRI/runsc lane = runsc with `overlay2=none` + default caps, NO kernel userns, capture via
containerd DiffService; both lanes' artifacts interchange cleanly (proven both directions for
the Docker→runsc leg; runsc-diff→Docker apply is the one untested direction — low risk, same
OCI encoding).

## Wiring

- Each spike = bd task under `sp-ei4.1`, **all three block `sp-ei4.1.3`** (the design spec).
- Findings: appended to each bead + consolidated into a results section in this note.
- Spikes 2+3 share the containerd instance → run sequentially; Spike 1 independent/parallel.
- Outcome shapes the spec: if runsc swallows writes and host-visible mode is too slow, the
  CRI lane needs an alternate capture (e.g. in-sandbox diff or runsc-native path) — that
  decision is exactly why the spec waits.
