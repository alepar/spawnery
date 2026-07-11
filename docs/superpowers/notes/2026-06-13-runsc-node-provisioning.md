# runsc Node Provisioning Reference (CRI/runsc Lane)

**Date:** 2026-06-13 · **Scope:** cloud-node containerd requirements for the CRI/runsc lane
(sp-ei4.1, spec §2 "CRI/containerd + runsc lane"). All findings sourced from spike plan
`2026-06-12-writable-rootfs-spike-plan.md` (Spike 2 and 3 RESULTS) and the design spec.

## Version Pins (verified-good floor)

| Component   | Version                    | Notes                                              |
|-------------|----------------------------|----------------------------------------------------|
| containerd  | 2.2.3                      | Hard pin — validate upgrades against spike checklist |
| runsc       | release-20260601.0         | Hard **floor** — see below; do NOT go below this |
| kernel      | 7.0.8-200.fc44.x86_64      | Spike host only (Fedora workstation); not a cloud-node requirement |

**runsc release-20260601.0 is a hard minimum, not merely "verified-good".** It is the first
release carrying the gVisor containerd-shim pause/resume fix (commit 55b3fd17, gVisor
[#12647](https://github.com/google/gvisor/issues/12647) / #13305, 2026-05-28). On earlier
builds — including the previously-pinned release-20260525.0 — `task pause` corrupts the
shim/ttrpc Status: the task reports PID 0 / UNKNOWN, resume fails with `no running task
found`, and sandbox teardown wedges. That breaks **suspend/resume and fork** outright. The
live pin is `RUNSC_RELEASE` in `scripts/e2e-vm/provision/provision.sh`.

**containerd and runsc are the real version pins** for cloud-node provisioning. The kernel
entry records the spike environment; cloud nodes will run a different kernel (e.g. Ubuntu
LTS or a cloud-provider image). Newer containerd/runsc patch releases are expected to work,
but any new combination should be validated against the spike checklist before deploying to
production.

## runsc handler — `overlay2=none` is MANDATORY

The default runsc overlay mode (`root:self`) creates a sentry-private
`.gvisor.filestore.<sandbox>` blob under the snapshotter's state directory. Container writes
never reach the host upperdir — the host-side snapshotter sees only the opaque blob. A
containerd DiffService capture on a default-overlay container would capture garbage, not the
agent's actual file modifications.

Setting `overlay2=none` disables the sentry's internal overlay and lets gofer pass all writes
through to the host upperdir directly. Files and kernel-device whiteouts (char 0:0) appear in
the snapshotter's upperdir exactly as with a native runc container. This is required for the
delta-capture path (containerd DiffService, sp-ei4.1.11).

**Cost is negligible for agent workloads** (spike 2 result 2):
- `apt update && apt install`: 3.61 s (`overlay2=none`) vs 3.34 s (default) — +8%, noise-level
- `dd 256 MB`: approximately equal

### containerd runtime-handler config

Add a dedicated runsc runtime handler to containerd's config (path varies by distro; commonly
`/etc/containerd/config.toml` for containerd 2.x):

```toml
[plugins.'io.containerd.cri.v1.runtime'.containerd.runtimes.runsc]
  runtime_type = "io.containerd.runsc.v1"
  # ConfigPath points at the per-node runsc.toml below.
  [plugins.'io.containerd.cri.v1.runtime'.containerd.runtimes.runsc.options]
    ConfigPath = "/etc/runsc/runsc.toml"
```

The corresponding `/etc/runsc/runsc.toml` must contain:

```toml
[runsc_config]
platform = "systrap"
network = "sandbox"
overlay2 = "none"
```

The stanza is `[runsc_config]` — **not** `[runsc]`. `platform = "systrap"` needs no `/dev/kvm`
(so it works under nested virt, e.g. the e2e VM). `network = "sandbox"` (netstack) is required:
`hostinet` bypasses the sandbox netstack and with it the egress floor, so it is spike-only and
must never reach production. This block is written verbatim by
`scripts/e2e-vm/provision/provision.sh`, which is the authoritative copy.

## CRI `dns_config` is Required

Without a kubelet, a CRI pod inherits the host's `/etc/resolv.conf`. On a systemd-resolved
host this is `nameserver 127.0.0.53` — the resolved stub listener. The 127.0.0.53 address is
unreachable from inside the runsc sandbox (spike 2 result 5): `apt-get update` silently exits
0 with all fetches failing (network resolves to nothing), and the sidecar cannot resolve the
upstream inference API hostname.

The node must supply explicit nameservers via the CRI `dns_config` field. Use an RFC1918
resolver reachable from the pod (e.g. the VPC's internal DNS resolver). The egress floor's
`:53` carve-out permits outbound DNS to a non-loopback resolver.

This is already wired in the spawnlet: `CRIPodBackend.DNSServers` (backend.go) is forwarded
into the pod sandbox's `DnsConfig.Servers` when non-empty. Node operators must set the field
via the spawnlet's configuration; leaving it empty will break DNS inside every runsc pod.

## hostNetwork + hostinet is Spike-Environment Only

The spike used `network = "host"` (hostinet mode) in runsc.toml to give host-networked pods
access to the host network stack from inside the sandbox. This was a spike convenience for
running CRI pods without a CNI bridge configured.

**Do NOT carry hostinet to production nodes.** Production pods use CNI-networked pods (spec §2;
the design assumes a CNI bridge — see the `SPAWNLET-EGRESS` RFC1918 drop comment in
backend.go). Egress floor enforcement depends on iptables rules applied to the pod's CNI
interface by the spawnlet; hostinet bypasses this and would defeat the egress floor entirely.

## No Kernel User Namespaces Needed

runsc does not require KEP-127 pod user namespaces and they should not be provisioned for the
CRI/runsc lane. The sentry virtualizes privilege internally: `uid_map` inside the sandbox reads
`0→0 #4G` (sentry-synthetic), and `apt update`, `useradd`, `chown`, `chmod setuid`, and `su`
to non-root all pass with the **default capability set** — no `DropCapabilities` needed (spike
3 result 3).

This is why the CRI lane's cap policy is `CapDefaultSet` (no `DropCapabilities` emitted) on a
runsc node, matching the Docker lane's behavior under userns-remap: isolation comes from the
sentry, not from a kernel mapping.

Additionally, KEP-127 is **broken with runsc** at the verified version pairing: the gofer
cannot `setns` into a pinned user namespace (`invalid argument`; multithreaded Go process cannot
join a userns at this containerd 2.2.3 + runsc release-20260601.0 combination — spike 3 result
2). Do not provision for KEP-127 on runsc nodes.

## Summary Checklist for Node Operators

- [ ] containerd 2.2.3 + runsc release-20260601.0 installed
- [ ] containerd runtime handler `runsc` configured pointing at `/etc/runsc/runsc.toml`
- [ ] `/etc/runsc/runsc.toml` has `[runsc_config]` with `overlay2 = "none"`, `platform = "systrap"`, `network = "sandbox"`
- [ ] CNI bridge plugin configured (e.g. flannel or a simple bridge CNI conf); hostinet NOT used
- [ ] spawnlet `CRIPodBackend.DNSServers` set to a reachable RFC1918 DNS resolver
- [ ] Kernel user namespaces NOT required; no KEP-127 configuration needed for runsc
