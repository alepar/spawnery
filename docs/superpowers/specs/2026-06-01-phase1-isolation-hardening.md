# Phase 1 Isolation Hardening + runsc Readiness (Design)

**Beads:** `sp-ff2` (host-veth floor, **P0**), `sp-s9u` (container hardening), + runsc startup sanity check.
**Status:** Draft v1 · **Date:** 2026-06-01 · **Source:** gVisor research (`...gvisor-isolation-research-results.md`) + empirical host tests.

## 0. Why (empirically proven)
- **The egress floor is a no-op under gVisor.** On this host (runsc `release-20260525.0`): the in-netns
  `NsenterApplier` floor counted **0 packets** and RFC1918 was reachable under `runsc` (vs 3-pkt drops
  under runc). gVisor's netstack does TCP/IP in user space and emits raw frames on the veth, bypassing
  host-kernel netfilter in the container netns. **Adopting runsc without moving the floor silently
  disables it.**
- **Host-side floor works under both runtimes.** `DOCKER-USER` rules matched by the pod's bridge source
  IP enforced under runsc (metadata + RFC1918 dropped, 3-pkt counters; public reachable; DNS `:53`
  allowed). This is the fix.

## 1. `sp-ff2` — move the egress floor to the host `DOCKER-USER` chain (P0)

**Mechanism.** Instead of `nsenter`-ing iptables into the container netns, apply rules on the **host**
in the `DOCKER-USER` chain, matched by the **pod's bridge IP** (the sidecar owns the netns/IP; the
agent shares it via `container:<sidecar>` so has no own IP). Real host netfilter sees the veth's
egress under both runc and runsc.

- **Runtime:** add `ContainerIP(ctx, id) (string, error)` (Docker: `ContainerInspect` →
  `.NetworkSettings.IPAddress`; FakeRuntime returns a stub e.g. `"172.17.0.99"`). Mirrors `ContainerPID`.
- **Rule builder:** `Rules(ip, allowCIDRs) []Rule` produces `DOCKER-USER` arg-lists, each prefixed
  `-s <ip>`: order = `-s ip -p udp --dport 53 ACCEPT`, `-s ip -p tcp --dport 53 ACCEPT`, per-CIDR
  `-s ip -d <cidr> ACCEPT`, then `-s ip -d 169.254.0.0/16 DROP` + RFC1918 (`10/8`,`172.16/12`,
  `192.168/16`) DROP. (No `-o lo` — loopback is never forwarded, so the host chain never sees
  agent↔sidecar.)
- **Applier** (`HostFloorApplier`): `Apply(ctx, ip, rules)` runs `iptables -I DOCKER-USER <args>` for
  each rule **in reverse** (so the final top-to-bottom order is accepts-before-drops). `Remove(ctx,
  ip, rules)` runs `iptables -D DOCKER-USER <args>` for each (order-independent). Errors surface
  (CombinedOutput).
- **Interface change:** the `Applier` interface becomes `Apply(ctx, ip string, rules []Rule) error`
  **+ `Remove(ctx, ip string, rules []Rule) error`** (was pid-based). `NsenterApplier` is removed /
  replaced by `HostFloorApplier`. Keep `Rules`/`Rule` shape (args), now with `-s ip` + chain.
- **Manager wiring:** in `Create`, after the sidecar starts, `ip, err := m.rt.ContainerIP(ctx,
  sidecarID)` then `m.fw.Apply(ctx, ip, firewall.Rules(ip, m.cfg.EgressAllowCIDRs))` — **before** the
  agent starts, **fail-closed** (on error: stop sidecar + finalize + return err). **Record the IP on
  the manager's `Spawn`** (`FloorIP string`). In `Stop`, if `sp.FloorIP != ""`, `m.fw.Remove(ctx,
  sp.FloorIP, firewall.Rules(sp.FloorIP, m.cfg.EgressAllowCIDRs))` (best-effort; log on error —
  `DOCKER-USER` persists across containers, unlike the auto-cleaned netns).
- **Node-class gating unchanged** (`egressEnforced()`). Requires host `iptables` + root (same as before).
- **Tests:** `Rules(ip,cidrs)` unit (DOCKER-USER + `-s ip` + dns-before-drops order); manager test with
  a fake applier asserting Apply-on-create (fail-closed) **and Remove-on-stop**; the existing
  `egress_e2e` is rewritten to apply host-side by IP (host-verified manually; build-tagged).

## 2. `sp-s9u` — container hardening (drop caps + ro-rootfs)

Apply to the **agent** container (the untrusted one); leave the sidecar as-is for now.
- **Drop all capabilities** (safe — agent shell work needs none): `ContainerSpec.DropAllCaps bool` →
  `host.CapDrop = ["ALL"]`. Default **on** for the agent.
- **Read-only rootfs** (riskier — the agent may write outside its `/app/<data>` mounts):
  `ContainerSpec.ReadonlyRootfs bool` → `host.ReadonlyRootfs = true`, plus a **tmpfs** for `/tmp`
  (`host.Tmpfs["/tmp"]=""`). **Config-gated** (`HARDEN_ROOTFS`, default **off**) until verified against
  the real agent image (goose) — verify in Phase 2 local run; flip the default on once it runs clean.
- **User-namespace remap** is a **daemon-level** Docker setting (`/etc/docker/daemon.json`
  `"userns-remap"`), NOT a per-container API knob — document it in `deployment.md`/`ISOLATION.md` as
  an ops recommendation, no spawnlet code.
- **Tests:** `buildHostConfig` unit asserts `CapDrop=["ALL"]`, `ReadonlyRootfs`, `/tmp` tmpfs when the
  spec flags are set; zero values omit them.

## 3. runsc startup sanity check (fail-fast)

When `CONTAINER_RUNTIME` (the manager's `ContainerRuntime`) is non-empty (e.g. `runsc`), the spawnlet
must **validate the runtime at startup**, not at first `CreateSpawn`: run a tiny smoke container under
that runtime and **`log.Fatal` (exit hard) if it fails**.
- Add `runtime.RuntimeCheck(ctx, runtimeName, image string) error` (or a `Manager.PreflightRuntime()`):
  `StartContainer({Image: <smoke image>, Cmd: ["true"], Runtime: runtimeName})` then `StopContainer`;
  any error → returned. Smoke image = the configured **agent image** (guaranteed present where spawns
  run; debian-based goose has `/bin/true`); `Cmd: ["true"]` exits immediately.
- Wire in `cmd/spawnlet/main.go` (the CP-attached startup path): if `ContainerRuntime != ""`, call the
  preflight; on error `log.Fatalf("runtime %q preflight failed: %v", rt, err)` → process exits. This
  catches a missing/broken `runsc` (or any configured runtime) at boot.
- Default (`ContainerRuntime==""` = runc) skips the check (runc is the daemon default; no preflight).
- **Test:** with a `FakeRuntime` that errors on a `Runtime`-set StartContainer, `PreflightRuntime`
  returns the error (caller fatals); with a healthy fake it returns nil.

## 4. Decision log
| # | Decision | Choice |
|---|---|---|
| P1.1 | Floor placement | host `DOCKER-USER`, matched by pod bridge IP (works runc+runsc, empirically) |
| P1.2 | Floor lifecycle | apply on create (fail-closed), **remove on stop** (DOCKER-USER persists); track `FloorIP` |
| P1.3 | Applier IP source | sidecar `NetworkSettings.IPAddress` (netns owner) |
| P1.4 | Caps | drop ALL on the agent (default on) |
| P1.5 | ro-rootfs | config-gated (`HARDEN_ROOTFS`, default off) + `/tmp` tmpfs; verify vs goose then default on |
| P1.6 | userns | daemon `userns-remap` (ops doc), not code |
| P1.7 | runsc preflight | startup smoke (`agent image` + `true`) under the configured runtime; `log.Fatal` on failure |
