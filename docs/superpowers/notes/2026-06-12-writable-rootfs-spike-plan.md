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

## Wiring

- Each spike = bd task under `sp-ei4.1`, **all three block `sp-ei4.1.3`** (the design spec).
- Findings: appended to each bead + consolidated into a results section in this note.
- Spikes 2+3 share the containerd instance → run sequentially; Spike 1 independent/parallel.
- Outcome shapes the spec: if runsc swallows writes and host-visible mode is too slow, the
  CRI lane needs an alternate capture (e.g. in-sandbox diff or runsc-native path) — that
  decision is exactly why the spec waits.
