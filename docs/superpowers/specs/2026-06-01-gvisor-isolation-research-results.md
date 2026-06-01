# gVisor for Spawnery: A Production Security & Systems Evaluation

**TL;DR**
- For Spawnery's cloud tier (untrusted, user-steered agents on multi-tenant hosts), **adopt gVisor (runsc) now** on top of the existing egress/cgroup floor — it is the same isolation layer Google Cloud Run, GKE Sandbox, App Engine, Cloud Functions, and Modal use for almost exactly this workload, and it neutralizes the host-kernel CVE class that runc+seccomp does not.
- **Plan a migration to microVMs (Firecracker or Kata) once any of three thresholds is crossed**: (a) a single tenant's blast radius becomes unacceptable (regulated data, paying enterprise, model weights), (b) workloads need full Linux syscall fidelity (FUSE, eBPF, io_uring, ptrace, GPU passthrough), or (c) host fleet acquires `/dev/kvm` (bare-metal or nested-virt). MicroVMs are what AWS Lambda, AWS Fargate, Fly.io Machines/Sprites, and E2B use precisely because they put a hardware boundary between tenants.
- **Do not stay on hardened runc+egress floor for the cloud tier.** runc has had repeated container-escape CVEs (CVE-2019-5736 "Runcscape", CVE-2024-21626 "Leaky Vessels") that a kernel-syscall-filter alone does not stop, and Spawnery's agent has shell/file/network tools — the exact pre-conditions these exploits assume.

---

## Section 1 — What gVisor protects against

### Architecture (versions current as of mid-2026)

gVisor's `runsc` is an OCI-compatible runtime that runs each sandbox as a normal Linux process tree but interposes a Go-implemented user-space kernel between the application and the host. Four components matter:

- **Sentry** — the application kernel. A from-scratch Go reimplementation of the Linux syscall ABI, memory management, signals, futexes, /proc, /sys, namespaces, pipes, ptrace, eventfd, epoll, etc. Per gVisor's own intro doc (gvisor.dev/docs/architecture_guide/intro/): *"The gVisor Sentry contains a Go-based, from-scratch reimplementation of the Linux system call interface, memory management, filesystems, a network stack, process management, signal handling, namespaces, etc. gVisor never passes through any system call to the host."*
- **Gofer** — a separate, more-trusted process per container that mediates filesystem access via the **LISAFS** protocol (a gVisor-specific FD-based replacement for 9P2000.L). Per Google Cloud's GKE/serverless filesystem improvements blog: *"We built a new protocol called LISAFS (Linux Sandbox File system protocol) to replace 9P… VFS2 and LISAFS are now launched 100% across all of GKE and serverless products."* App Engine cold starts improved >25% on average from the LISAFS rollout (data from August 2022).
- **Netstack** — a user-space TCP/IP stack written in Go inside the Sentry. The default. The container's veth packets are read/written as raw frames; the host kernel never parses TCP/IP from the untrusted workload. An alternative `--network=host` (a.k.a. "hostinet" / passthrough) lets the Sentry use Berkeley sockets directly, which *"weakens gVisor's security model by increasing the Host OS's Attack Surface… the Sentry is allowed to use 15 additional syscalls"* (gvisor.dev/blog/2020/04/02/gvisor-networking-security/).
- **Platform** — the mechanism for trapping the workload's syscalls. Three options:
  - **systrap** (default since mid-2023; the official platform doc states *"systrap replaced ptrace as the default platform in mid-2023"*; release blog announced April 28 2023 with a September 2023 default-switch target). Uses `SECCOMP_RET_TRAP` and a stub signal handler; works inside any Linux VM without `/dev/kvm`.
  - **KVM** — Sentry acts as both guest OS and VMM via `/dev/kvm`. Best on bare metal; suffers under nested virtualization.
  - **ptrace** — legacy; per the platform doc *"no longer supported and is expected to eventually be removed entirely."*

### Attack surface reduction (concrete numbers)

- The Sentry implements **274 of ~350 Linux x86_64 syscalls** with full or partial implementations, with 76 unsupported as of the auto-generated compatibility doc; arm64 has 240/294 supported, 54 unsupported (gvisor.dev/docs/user_guide/compatibility/linux/amd64/).
- After Sentry handling, the Sentry process itself is only permitted **~53 host syscalls without networking, ~68 with netstack** (per the original "gVisor Security Basics — Part 1" post). Enforced by seccomp-bpf on the Sentry. With **directfs** enabled (now default), the Sentry must also be allowed `openat(2)` with restrictions (O_NOFOLLOW required, no procfs, no host directory FDs); this is documented as a deliberate tradeoff in the Google Open Source blog (Jun 27 2023): *"we've decreased our dependence on the syscall filters to catch bad behavior, but correspondingly increased our dependence on Linux's filesystem isolation protections."*
- Compare this with hardened runc: even with the most aggressive seccomp profile, the workload's syscall surface IS the host kernel's syscall surface. Docker's default seccomp blocks ~44 syscalls out of ~350; the rest reach the host kernel directly.

### What gVisor stops

**Host-kernel CVE class.** The canonical positive examples:

- **CVE-2020-14386** (Linux AF_PACKET memory corruption, container escape via CAP_NET_RAW): explicitly mitigated for GKE Sandbox, Cloud Run, Cloud Functions, App Engine. Google's own write-up: *"the problematic C code in Linux is not used in the gVisor networking stack."*
- **CVE-2022-0185** (Linux fs_context heap overflow → container escape). gVisor's docs (gvisor.dev/docs/user_guide/tpu/): *"CVE-2022-0185 is mitigated because gVisor itself handles the syscalls required to use namespaces and capabilities, so the application is using gVisor's implementation, not the host kernel's."*
- **Dirty COW (CVE-2016-5195)** is used by the gVisor Security Model doc as a representative example of the host-kernel-CVE class that the architecture neutralizes (exploit relies on a race in the host kernel's COW handling reachable via specific ptrace/`/proc` paths, none of which the Sentry forwards).
- **Dirty Pipe (CVE-2022-0847)** — not called out by name on gvisor.dev, but the pipe/splice path is fully reimplemented in the Sentry; the host kernel pipe code is not reachable from the sandboxed workload. Treat this as "structurally mitigated" rather than "vendor-documented." (Modal's "what is an AI code sandbox?" piece also frames Dirty Pipe in this category.)

**Container-runtime CVE class.**

- **CVE-2019-5736 ("Runcscape", runc /proc/self/exe overwrite)** — gVisor is structurally immune because the `runsc` binary is a different Go binary, not runc, and the application never has access to a writable host-binary FD. Not formally documented as "mitigated" in a blog by gVisor.
- **CVE-2024-21626 ("Leaky Vessels", runc fd leak → host fs access)** — explicitly mitigated. gVisor's docs: *"CVE-2024-21626 is mitigated by gVisor because the application would use gVisor's implementation of ioctl(2). For a compromised sentry, ioctl(2) calls with the needed arguments are not in the seccomp filter allowlist."*

**Syscall abuse, /proc and /sys exposure, namespace/cgroup escapes.** /proc and /sys inside a runsc sandbox are served by the Sentry (a curated subset; e.g. `top` on the host won't show sandboxed PIDs). Namespace and cgroup-related syscalls (`unshare`, `setns`, `mount`, `clone` with namespace flags) are either implemented in the Sentry or rejected — they never reach the host's namespace code.

### What gVisor does NOT stop

- **Spectre/Meltdown-class transient-execution side channels.** gVisor runs in the same address space and on the same physical cores as the host kernel; it provides no defense against speculative-execution leaks. Microarchitectural mitigations are the host's responsibility (KPTI, retpoline, IBRS/IBPB, SMT-aware scheduling). The microVM threat-model paper for Firecracker (Sharma et al., 2024, link.springer.com/content/pdf/10.1007/978-3-031-80020-7_1) is explicit that even Firecracker requires same-physical-core co-residency to be prevented to fully mitigate; gVisor offers strictly less.
- **The Sentry's own attack surface.** Compromise of the Sentry yields a process running with ~53–68 host syscalls allowed. Published gVisor-specific CVEs to date (primary-source verified via GHSA, NVD, Snyk, Ubuntu, and gvisor.dev):
  - **CVE-2018-16358** — pre-2018-08-22 pagetable reuse panic (DoS, found by Jann Horn / Project Zero).
  - **CVE-2018-16359** — pre-2018-08-23 seccomp filter incorrectly permitted `renameat` (allowed renaming host files; sandbox-boundary crossing).
  - **CVE-2018-19333** — SysV shm refcount mishandle, in-sandbox LPE to root processes (disclosed Nov 14 2018, justi.cz).
  - **CVE-2023-7258** — mount-point refcount DoS panicking the sandbox (published May 15 2024; CVSS 6.5 / Ubuntu 4.8 medium; requires in-sandbox root + mount permission).
  - **CVE-2024-10026** — weak hash + small seed sizes in `pkg/tcpip/stack` and `pkg/tcpip/transport/tcp`, permit a remote attacker to compute the local IP and a per-boot identifier (NDSS 2025 paper by Kaplan/Even/Klein; CVSS v4 6.3).
  - **CVE-2024-10603** — predictable TCP/UDP source ports / header fields (same research; CVSS v4 6.3).
  - **CVE-2025-2713** — runsc local privilege escalation, disclosed Mar 28 2025, GHSA-4fj4-9m67-3mj3 (CVSS v4 6.8 medium): runsc ran with root-like permissions until the first fork, allowing unprivileged users to access restricted files.
  
  This is a remarkably short list relative to runc, QEMU, or Linux itself — but note gVisor's policy is to only assign CVEs for issues that cross the sandbox boundary in an attacker-uncontrolled config and are gVisor-specific (gvisor.dev/security/); plenty of in-sandbox-only bugs are fixed silently. Severity numbers for several of these come from Snyk/Ubuntu/GHSA, not NVD-canonical.
- **Denial-of-service / resource exhaustion.** gVisor does not enforce CPU/memory limits inside the sandbox; per its own compatibility doc: *"in-sandbox cgroups (CPU, memory) exist and can be used for resource accounting, resource limits are not enforced within the sandbox. It is possible to restrict a sandbox's resources by placing gVisor in a Linux-native host cgroup, however gVisor cannot currently enforce resource limits between competing processes within the same sandbox."*
- **Application-level abuse.** Isolation ≠ authorization. If the agent has cloud creds, gVisor does nothing to stop their abuse. The Spawnery iptables floor that drops the metadata endpoint and RFC1918 ranges remains essential.

### Comparison with seccomp/AppArmor/SELinux-hardened runc

Seccomp blocks a hard-coded list of syscalls; AppArmor/SELinux are MAC over Linux objects. Both still expose the full host-kernel syscall implementation for whatever they permit. None reimplements the syscalls. A kernel-bug exploit reachable via a seccomp-allowed syscall (e.g. CVE-2022-0185's `fsconfig`, CVE-2020-14386's `setsockopt` on AF_PACKET) defeats them. gVisor reimplements the path in Go; even if the workload's exploit logic runs, it runs against the Sentry's code, not the host kernel's.

---

## Section 2 — Performance overhead vs runc

Caveat up front: benchmarks vary wildly by workload, gVisor version, and platform. Cite versions, not folklore.

### System call latency

- gVisor's own performance guide (using the legacy ptrace platform for clarity, not for production): a raw syscall in a tight loop costs **2–11× native** depending on syscall and platform. The 2019 HotCloud paper "The True Cost of Containing" measured `gettimeofday` round-trip with the systrap platform's predecessor at **2.8× slower than native on KVM (Sentry-only)**, **9× when calling to the host**, **72× when calling to the Gofer**, and **42–232× on ptrace**.
- Systrap (default since mid-2023) closed most of the ptrace gap. Ant Financial's production blog post on gvisor.dev (Dec 2 2021) reports Sentry-implemented syscalls at *"about 800ns while that of the native syscalls is about 70ns"* — so roughly an order of magnitude on a tight syscall, but with vDSO shortcuts for cheap calls (`gettimeofday`, `clock_gettime`) being near native.
- Tencent's April 2026 gVisor blog (gvisor.dev/blog/2026/04/23/scaling-agentic-rl-sandboxes-to-the-millions-with-gvisor-at-tencent/) reports running *"millions of gVisor sandboxes daily for Agentic-RL"* with the correctness gap closed after fixes to roughly **0.13 percentage points (86.91% vs 86.78% pass rates between runsc and runc)** — directly relevant precedent for Spawnery's "untrusted agent runs third-party code" profile.

### CPU-bound vs syscall/IO-bound

- gVisor's production guide is blunt: *"I/O-heavy (e.g. databases) and network-heavy (e.g. load balancers) workloads will see degraded performance, whereas CPU-bound workloads (e.g. API servers, non-static web servers, data pipelines) will see minimal or no overhead."*
- Memory bandwidth (sysbench memory): no measurable overhead — *"gVisor does not introduce any additional costs with respect to raw memory accesses."* The Sentry only mediates page faults, not loads/stores after mappings are installed.
- A widely cited production data point — **not Google's, but Ant Financial's (gvisor.dev, Dec 2 2021)**: *"70% of our applications running on runsc have <1% overhead; another 25% have <3% overhead."* This was for Ant's app mix on its custom platform; treat as a ceiling, not a floor, for Spawnery's fork-heavy shell workload.

### Filesystem I/O through the Gofer

This is the single biggest historical cost for Spawnery's workload profile. Successive optimizations:

- **VFS2** (re-architected VFS): launched 100% across GKE Sandbox + Cloud Run + App Engine + Cloud Functions by late 2022. Per Google Cloud blog "gVisor File system Improvements for GKE and Serverless": runsc overhead reduced **50–75%** depending on mount mode; App Engine cold starts improved >25% on average after LISAFS rollout (data from August 2022).
- **LISAFS** (gvisor.dev/blog/2023/06/27/directfs/): replaced 9P2000.L; FD-based, fewer round-trips. Enables one-shot path walks (e.g. `stat(/a/b/c)` is one RPC instead of three).
- **Rootfs overlay** (announced Apr 2023): pushes container writes to an in-Sentry tmpfs; eliminates Gofer RPCs for upper-layer writes. The Google Open Source blog reports `fsstress -d /test -n 500 -p 20` going from **262.79 s to 3.18 s** with rootfs overlay enabled (microbenchmark; not representative).
- **directfs** (announced Jun 27 2023, now **default**): gives the Sentry direct, isolated FDs into the container filesystem via SCM_RIGHTS, eliminating the Gofer RPC critical path entirely. The same blog reports stat-microbenchmark **>2× faster** with directfs.

For Spawnery's "many file ops, fork/exec heavy" agent loop, you want runsc with **systrap + directfs + rootfs overlay + LISAFS**, all of which are defaults in current releases.

### Network throughput / latency

- Netstack iperf3 numbers from the gVisor team (2020 networking-security post): ~17 Gbps download, ~8 Gbps upload vs ~42–43 Gbps native — i.e. **~40% of native throughput** on download, less on upload, with steady improvement since. For Spawnery's outbound-HTTPS-heavy agent traffic, netstack's headline throughput is rarely the bottleneck; latency is.
- Latency is moderate: extra goroutine hops in netstack's TCP path. Reasonable for interactive agent traffic; not appropriate for high-PPS network appliances.
- Host-networking passthrough (`--network=host`) recovers most performance but, per gVisor's own networking-security post, *"trades the security and isolation of netstack for the performance of native Linux networking"* — and adds 15 syscalls to the Sentry's seccomp allowlist including ones that create FDs.

### Per-sandbox memory footprint

- Sentry RSS scales primarily with the number of open files/sockets and runtime memory the workload uses. Fixed overhead is small (single-digit MB) but the heap grows with workload activity; there are historical reports of Sentry RSS not shrinking after spikes (github.com/google/gvisor/issues/232) — relevant if Spawnery plans long-lived spawns.

### Startup latency

- gVisor cold-start for an Alpine container running `true` is comparable to runc (most of the latency is Docker/containerd itself). The 2023 Anger seminar comparison reports: Firecracker ~60% slower startup than gVisor; Kata ~72% slower (in Kubernetes context). gVisor adds tens to low-hundreds of ms.
- For pre-warmed pools (GKE Agent Sandbox's `SandboxWarmPool`), Google reports **<1 s** to bind a pre-initialized gVisor sandbox.

### Platform choice impact on overhead

- **systrap** is the right default for Spawnery: works in any VM without /dev/kvm, ~10× the ptrace getpid throughput. The 2023 release blog: *"Just about any workload will generally suffer from high overhead in system call performance. Since running in a virtualized environment is the default state for most cloud users these days, it's important that gVisor performs well in this context. Systrap is the new platform targeting this important use case."*
- **KVM** is best on bare-metal hosts (better address-space-switch performance), but worse under nested virtualization. GCP supports nested virt on Cascade Lake+; AWS provides bare-metal `.metal` instances; Azure offers nested virt on Dv3/Ev3+.

### Mapping to Spawnery's profile (worst case)

Spawnery's agent is the gVisor worst case: many small file ops, frequent `fork`/`exec`, outbound HTTPS, interactive turn-by-turn. Expected overhead with systrap + directfs + rootfs overlay (current defaults):

- CPU-bound math/inference inside spawn: **near native** (<few %).
- Shell-command pipelines (`grep`/`find` over many files): **10–30%** wall-clock penalty.
- `npm install`-class workloads (heavy fs + many short-lived processes): **15–50%** wall-clock penalty depending on cache hit rates. Tencent and Google's published benchmarks support this range with directfs/LISAFS on systrap.
- Network: HTTPS to public APIs: **negligible to ~20%** added latency on netstack; throughput well above any agent's real demand.

If the inference sidecar handles model calls (CPU-bound, syscall-light), keeping it in the same sandbox is fine. If it does heavy local file I/O for KV cache, consider running it under runc on a trusted side and gVisor only the agent (Spawnery's shared-netns pod design supports per-container runtime selection only at the pod level under Kubernetes — see Section 4 caveats).

---

## Section 3 — Gaps vs VM-level isolation

### The structural difference

gVisor's trust boundary is *the Sentry's correctness plus its host-syscall seccomp policy*. A Sentry RCE plus a host-kernel exploit reachable via the ~53–68 allowed syscalls equals host compromise. By contrast, Firecracker/Kata put a **hardware virtualization** boundary (KVM ring 0/non-root mode) between guest and host. A guest-kernel RCE still has to find a VMM or KVM CVE to escape — historically rare.

Fly.io's own framing of this trade is direct (fly.io/learn/microvm-vs-container/): *"A sufficiently motivated attacker with a kernel exploit doesn't care about your seccomp profile. If the threat model genuinely requires hardware-enforced isolation, the only answer is a hardware-enforced boundary."*

### Compatibility gaps in gVisor (the cost of "Linux, but not really")

- **Syscalls**: 76 unsupported on x86_64 (e.g. `io_uring` family is partial — most language runtimes fall back to epoll silently; `userfaultfd`, `bpf`, `kexec_load`, full `msg*` SysV message queues, some IPC and scheduling syscalls). The auto-generated table at gvisor.dev/docs/user_guide/compatibility/linux/amd64/ is authoritative.
- **/proc and /sys fidelity**: a curated subset. Tools that introspect `/sys/class/net` deeply, `/sys/fs/cgroup` v2 details, or hardware-specific `/proc` entries can fail or misreport.
- **No eBPF**, no FUSE mounts, no `seccomp_user_notify`. Workloads using these need fallbacks (see Modal's `agentsh` adopting ptrace-based syscall interception specifically because *"Modal's gVisor kernel doesn't support FUSE or seccomp user-notify"*).
- **GPU / device passthrough**: gVisor added selective NVIDIA CUDA support in GKE Sandbox starting 1.29.2-gke.1108000, but explicitly: *"GKE Sandbox doesn't mitigate all NVIDIA driver vulnerabilities, but retains protection against Linux kernel vulnerabilities."* GPU time-sharing is not recommended; only a subset of GPUs is supported. Firecracker has no PCIe passthrough at all (work paused for 2025 per E2B's documentation); Cloud Hypervisor / QEMU + Kata can do GPU passthrough.
- **Spawnery-specific compatibility risks**: `ptrace` (yes, supported), `fanotify`/`inotify` (partial — see the Tencent blog's `IN_MODIFY`-on-truncate bug fix), `copy_file_range` (recently filled), Docker-in-Docker (works with caveats; nested namespacing emulated).

### Where microVMs lose

- **Startup latency**: Firecracker's official SPECIFICATION.md states: *"It takes <= 125 ms to go from receiving the Firecracker InstanceStart API call to the start of the Linux guest user-space /sbin/init process. The boot time is measured using a VM with the serial console disabled and a minimal kernel and root file system,"* with *"<=5 MiB"* VMM memory overhead. In real deployments, per-workload startup including rootfs setup runs 200 ms – 1 s (E2B's current homepage advertises *"The E2B Sandboxes in the same region as the client start in 80 ms"*; Fly.io Sprites documents 1–12 s for sandbox creation). gVisor is faster from cold (no kernel boot) and supports warm pools <1 s.
- **Density / memory overhead per instance**: each microVM carries a guest kernel image (~5–30 MiB if minimal; tens to hundreds of MiB with full distro). gVisor's per-sandbox overhead is the Sentry process; substantially lower in steady state for small workloads.
- **OCI compatibility**: gVisor is drop-in OCI; microVMs need a containerd shim (Kata, firecracker-containerd) and root-disk conversion. Not hard, but operational complexity.
- **Operational complexity**: Firecracker has no built-in orchestration. You build kernel images, root filesystems, networking, jailer config, and lifecycle yourself. Kata wraps this but adds a layer.

### Comparison table

| Property | runc | runc + seccomp/AppArmor | gVisor (runsc, systrap+directfs) | Kata Containers (QEMU/Cloud Hypervisor) | Firecracker microVM |
|---|---|---|---|---|---|
| Host kernel attack surface | ~350 syscalls reachable | ~300 syscalls reachable | ~53–68 host syscalls from Sentry; workload syscalls do not reach host kernel | KVM hypercalls + virtio devices only | KVM hypercalls + 5 virtio devices |
| Boundary | Linux ns + cgroups + LSM | Linux ns + LSM + seccomp filter | Userspace re-implemented kernel + seccomp on Sentry | Hardware (Intel VT-x/AMD-V via KVM) + guest kernel | Hardware (KVM) + minimal VMM in Rust |
| Container-escape CVE class (CVE-2019-5736, CVE-2024-21626) | Vulnerable | Vulnerable | Mitigated (CVE-2024-21626 explicit per gVisor docs) | Mitigated | Mitigated |
| Host kernel CVE class (CVE-2020-14386, CVE-2022-0185) | Vulnerable | Often vulnerable | Mitigated (explicit per gVisor blog/docs) | Mitigated (guest kernel still vulnerable but irrelevant to host) | Mitigated |
| Spectre/Meltdown cross-tenant | Vulnerable | Vulnerable | Vulnerable | Largely vulnerable (KVM doesn't prevent SMT side-channels without scheduling) | Largely vulnerable (Firecracker's NSDI '20 paper acknowledges; SMT must be off / co-scheduled) |
| Sentry / VMM own CVEs | n/a (runc CVEs above) | n/a | 7 gVisor-specific CVEs to date (all medium/DoS) | QEMU has hundreds; Cloud Hypervisor smaller; Kata-shim ~handful | Firecracker has <10 reported in its lifetime (Rust + minimal device model) |
| Startup latency (cold) | ~100 ms | ~100 ms | ~150–300 ms | 500 ms – 2 s | ≥125 ms VMM + kernel boot (E2B 80 ms in-region; Fly Sprites 1–12 s end-to-end) |
| Per-instance memory overhead | minimal | minimal | ~15–50 MiB Sentry + Gofer | 50–150 MiB guest kernel + rootfs | ≤5 MiB VMM + guest kernel (~5–30 MiB) |
| Syscall overhead (interactive shell) | none | very low | 10–30% wall clock | low (guest kernel native) | low (guest kernel native) |
| Filesystem overhead | none | none | low–moderate with directfs; high without | virtio-fs / 9p (moderate) | virtio-fs (moderate) |
| GPU passthrough | yes | yes | partial (CUDA only, select drivers, GKE 1.29.2+) | yes (with VFIO) | no (PCIe passthrough not implemented, work paused 2025) |
| OCI / Kubernetes drop-in | yes | yes | yes (RuntimeClass) | yes (RuntimeClass) | requires shim + image conversion |
| Production users for untrusted code | (rare for hostile multi-tenant) | (used as floor) | **Cloud Run Gen1, GKE Sandbox, App Engine, Cloud Functions, DigitalOcean App Platform, Modal, GKE Agent Sandbox, Tencent Agentic-RL** | Northflank, GKE Agent Sandbox (option), Alibaba sandboxes | **AWS Lambda, AWS Fargate, Fly.io Machines & Sprites, E2B, Koyeb, Northflank (option)** |

### Who runs what (publicly documented)

- **Google Cloud Run (Gen1 / first-gen execution env)**: gVisor + hardware-backed VMM layer underneath. Per Google's "Security design overview": *"First generation: Based on the gVisor container security platform, this option has a small codebase, which provides a smaller attack surface."* Gen2 uses Linux microVM + seccomp.
- **GKE Sandbox** (incl. **GKE Agent Sandbox** for LLM-generated code, GA-tracked from 1.35.2-gke.1269000): gVisor by default; Kata Containers as an option.
- **App Engine Standard, Cloud Functions**: gVisor — confirmed by Google's CVE-2020-14386 blog.
- **AWS Lambda, AWS Fargate**: Firecracker. Per the AWS Firecracker announcement blog (Nov 2018): *"Today, Lambda processes trillions of executions for hundreds of thousands of active customers every month."*
- **Fly.io Machines**: Firecracker — *"Application code runs in Firecracker microVMs. These are lightweight, secure virtual machines based on strong hardware virtualization."*
- **Fly.io Sprites** (their LLM/agent code-execution product): Firecracker with checkpoint/restore.
- **E2B**: Firecracker — *"Each sandbox is powered by Firecracker, a microVM made to run untrusted workflows."*
- **Modal**: gVisor — *"Sandboxes are built on top of gVisor… gVisor has custom logic to prevent Sandboxes from making malicious system calls, giving you stronger isolation than most other container runtimes."* Per Amplify Partners' 2025 profile: *"One of Modal's customers, a major AI lab, is already running on the order of 100,000 concurrent sandboxes for RL workloads, with a stated goal of reaching 1 million."*
- **OpenAI ChatGPT code-interpreter / Responses API Code Interpreter**: OpenAI's API doc says only *"a fully sandboxed virtual machine that the model can run Python code in,"* without naming the technology. Treat as undisclosed; do not assume gVisor.
- **Tencent**: gVisor for Agentic-RL — *"we run millions of gVisor sandboxes daily for Agentic-RL training in production."*
- **DigitalOcean App Platform**: gVisor (per gVisor's compatibility doc).

---

## Section 4 — Best practices for production integration

### Wiring runsc

**Docker** (`/etc/docker/daemon.json`):
```json
{
  "runtimes": {
    "runsc": {
      "path": "/usr/local/bin/runsc",
      "runtimeArgs": ["--platform=systrap", "--network=sandbox"]
    }
  }
}
```
Then `docker run --runtime=runsc …`. This matches Spawnery's existing `CONTAINER_RUNTIME=runsc` opt-in plumbing.

**containerd** (`/etc/containerd/config.toml`):
```toml
version = 2
[plugins."io.containerd.grpc.v1.cri".containerd.runtimes.runsc]
  runtime_type = "io.containerd.runsc.v1"
```
Requires `containerd-shim-runsc-v1`. Minimum containerd 1.3.9 / 1.4.3.

**Kubernetes** RuntimeClass:
```yaml
apiVersion: node.k8s.io/v1
kind: RuntimeClass
metadata:
  name: gvisor
handler: runsc
---
apiVersion: v1
kind: Pod
metadata:
  name: spawn
spec:
  runtimeClassName: gvisor
  containers: [ ... ]
```
On GKE, `gcloud container node-pools create … --sandbox type=gvisor`. **GKE rule**: the default node pool cannot be a sandbox pool — keep at least one non-sandboxed pool for system workloads.

### Platform selection and host prerequisites

| Environment | Recommended platform | Reasoning |
|---|---|---|
| AWS EC2 `.metal` (e.g. `c5n.metal`, `m7a.metal-48xl`) | KVM | Bare-metal /dev/kvm available |
| AWS EC2 non-metal | systrap | Nested virt unsupported on most EC2 |
| GCP nested-virt-enabled VM (Cascade Lake+) | systrap (KVM possible but slower under nested) | gVisor docs explicitly recommend systrap inside VMs |
| Azure Dv3/Ev3+ | systrap | Nested virt available but adds overhead |
| Bare-metal colo | KVM | Best performance |

Spawnery is multi-tenant cloud; default to **systrap** unless on dedicated bare-metal.

### Netstack vs host networking + Spawnery's shared-netns pod

This is the most Spawnery-specific consideration. Three things must hold simultaneously:

1. **Shared-netns pod**: both containers (agent + inference sidecar) share one network namespace. In Kubernetes / Docker Compose, all containers in a pod share the pod's veth.
2. **Per-pod iptables egress floor**: drops cloud metadata + RFC1918; allows DNS + public internet.
3. **runsc**.

Key facts:

- On Kubernetes, RuntimeClass is **per-pod**, not per-container. All containers in a sandboxed pod run inside one runsc sandbox sharing the Sentry. Per gVisor docs: *"Kubernetes treats the pod as the sandbox boundary, meaning typically the entire pod (all its containers) are placed in a single gVisor sandbox for efficiency and to preserve intra-pod Linux semantics (like shared IPC and loopback networking)."* — this is exactly what Spawnery wants: the agent and inference sidecar share loopback inside the same Sentry.
- gVisor's **netstack** implements iptables in user space — but only **partial** iptables support (the github.com/github/gh-aw-firewall research issue documents this: *"From gVisor docs: 'iptables are only partially supported.'"*). Critical for Spawnery: if your egress floor rules live *inside* the pod's network namespace (the conventional spot for per-pod policies), they will be applied by netstack's partial iptables implementation, and some matchers/extensions may silently behave differently than the host kernel's iptables. **Recommendation**: put the egress floor on the **host** in the `DOCKER-USER` chain (or in the CNI plugin / host filter table), not inside the pod. The host's real Linux netfilter is unaffected by netstack and gVisor packets exit through the veth which the host firewall sees normally.
- Host-networking passthrough (`--network=host`) lets the Sentry use the host's TCP stack — this would make the in-pod iptables behavior identical to runc, but it adds 15 syscalls to the Sentry's seccomp policy and weakens gVisor's security model. **Do not enable for hostile workloads.**

For Spawnery: keep `--network=sandbox` (default), enforce the egress floor on the host's veth ingress hook or CNI plugin, and treat any in-container iptables rules as best-effort defense-in-depth that may behave differently under runsc.

### Observability and debugging

- `runsc debug --strace --stacks <sandbox-id>` for live syscall trace and stuck-goroutine dumps.
- `runsc --debug --debug-log=/var/log/runsc-%TIMESTAMP%.log` — heavy: degrades performance by an order of magnitude. Use only for triage.
- **eBPF-based tools (Falco, bpftrace) won't see inside the sandbox**, because the Sentry is not the real kernel. You see Sentry's host syscalls but not the workload's. Plan for gVisor's own `runsc trace` (seccheck) sinks for in-sandbox observability, or run an in-sandbox agent (e.g. Modal's `agentsh` uses ptrace inside the sandbox precisely because eBPF/seccomp-notify don't work).
- For Spawnery's audit/forensics: log every spawn's seccheck stream to a SIEM. Run a watchdog that ships container stdout/stderr + `runsc events` to a tamper-resistant store.

### Image-compatibility testing

- Run your existing agent images against `runsc` early. Most Python/Node/Go/Rust tooling works because Glibc/Musl probe and fall back to supported syscalls. Compile-time things like `io_uring`-only Rust crates may need re-compilation against pure-epoll backends.
- The Tencent blog provides a real-world methodology: 74,000+ side-by-side runs vs runc, with AI-assisted triage. After fixes, *"the correctness gap between runsc and runc is only about 0.13 percentage points (86.91% vs 86.78%)."* The 1.7% figure is the share of test cases where they had to invest in gVisor-side fixes — useful as an engineering-budget signal. For Spawnery, allocate engineering time for a similar (smaller) compatibility sweep over the third-party app code Spawnery's agent is expected to execute.
- Per gVisor: *"any syscall not implemented in gVisor returns ENOSYS… The Sentry does not 'pass through' any syscall."* So failures are visible as `function not implemented` rather than silent corruption — debuggable.

### Rollout strategy

1. **Canary** the cloud tier under runsc with logging of `runsc unsupported-syscall` events. Keep the runtime selector (`CONTAINER_RUNTIME`) at the spawn level. Start with 1–5% of spawns; ratchet up.
2. **Per-tier runtime**: keep local dev / trusted internal spawns on runc; force cloud spawns to runsc. Use Kubernetes RuntimeClass to make this declarative.
3. **Fallback**: if a spawn fails with ENOSYS or a known-incompatible exit code, automatically retry once on runc with a flagged `untrusted=false` annotation that triggers extra audit logging. Decide policy (allow / deny / page) per tenant.
4. **Compat gate in CI**: run the agent's smoke tests under both runtimes before shipping a new agent image. Cache the per-image result.
5. **Maintenance**: gVisor ships frequent releases via the `release` apt channel; cadence is several releases/month. Pin to a known-good build; subscribe to gvisor-announce; bake monthly upgrades into a maintenance window.

### Defense-in-depth composition (recommended Spawnery layered config)

For each cloud spawn:

- **gVisor (runsc, systrap, --network=sandbox, --directfs, --overlay2=all:self)** — the new layer.
- **Host iptables egress floor** — keep; move to host's veth hook so it's not subject to netstack's partial iptables.
- **cgroups CPU/memory/pids limits** on the host — keep.
- **Read-only rootfs** for the agent container, with a tmpfs-backed scratch dir — add.
- **Dropped capabilities**: `--cap-drop=ALL`, optionally `--cap-add` only what the agent needs (typically none in user-mode shell work). gVisor honors capabilities.
- **User namespaces**: gVisor supports them; map the in-sandbox root to an unprivileged host UID so even Sentry-level escapes hit unprivileged host context.
- **seccomp** at the workload level (Docker default profile or stricter). Redundant with gVisor for the most part — gVisor's own seccomp on the Sentry is stricter than any application-level profile — but cheap and harmless.
- **Per-user concurrency cap** — keep.
- **Sentry-level resource limits**: pin sandbox to host cgroup with strict memory & CPU caps. gVisor cannot self-enforce these across processes inside a single sandbox.

### Failure modes & maintenance cost

- **Sentry panic / OOM**: kills the sandbox; pod restart policy handles. Spawnery should treat each spawn as ephemeral; lost state is recoverable.
- **Sentry RSS bloat**: known historical issue (github.com/google/gvisor/issues/232); if Spawnery runs long-lived spawns, cap memory at the host cgroup and prefer ephemeral spawn lifetimes.
- **Slow gVisor regressions**: at scale, run the official `runsc do --no-color check` and seccheck sinks; alert on per-image-version perf drift.
- **Operational ownership**: 1 engineer can keep gVisor humming on a fleet of ~thousands of hosts. Upgrade cadence is moderate; the project has been on a stable release cadence since 2019. Compared with operating Firecracker yourself (kernel images, jailer, networking, snapshot/restore plumbing), gVisor is far less work.

---

## Recommendations — Decision framework and concrete next steps for Spawnery

### Top-line: adopt gVisor for the cloud tier now; plan microVM migration as Phase 3

**Stage 1 (today → 2 weeks)**: runc + egress floor + read-only rootfs + dropped caps + user namespaces. Required even before gVisor. Most of this Spawnery already has.

**Stage 2 (2–8 weeks)**: **gVisor on the cloud tier**, via Kubernetes RuntimeClass `gvisor` or Docker `--runtime=runsc`. systrap platform. netstack default. directfs default. Host-level iptables egress floor on the veth — *not* inside the sandbox's network namespace. Canary at 5% → 25% → 100%. Keep runc as fallback for images that fail compatibility, with the fallback explicitly audit-logged and rate-limited per tenant. This is the same architecture Google Cloud Run, GKE Sandbox, App Engine, Modal, and Tencent's Agentic-RL all use for nearly the same workload.

**Stage 3 (cross any threshold X below)**: **Firecracker or Kata for the hardest tier**, leaving gVisor for the general tier. Use Kata Containers with Cloud Hypervisor backend for OCI compatibility (it's the path GKE Agent Sandbox supports as a non-Google option, and Northflank uses for the hardest workloads). Roll your own Firecracker only if you need Fly-Sprites-like checkpoint/restore semantics and can pay the orchestration tax.

### Thresholds that drive escalation to microVMs

Escalate one tier whenever any of these become true:

1. **Tenancy model shifts to dedicated/regulated**: enterprise tenant with PCI/HIPAA/SOC2 audit requirements asking for "VM-level isolation between tenants" in a contract.
2. **Blast-radius tolerance shrinks**: a single tenant's data/secret becomes high-value enough that a Sentry RCE compromising a co-tenant is unacceptable (e.g. paid model weights, API keys to expensive services, customer prod databases).
3. **Performance budget allows**: spawns are long-lived enough that the 200–1000 ms VM boot is amortized, AND host fleet has /dev/kvm or nested virt available (AWS .metal, GCP nested-virt-enabled families, Azure Dv3+).
4. **Workload needs unsupported features**: GPU passthrough (Kata, not Firecracker), eBPF, FUSE, io_uring, or arbitrary kernel modules — anything beyond gVisor's syscall coverage.
5. **Compatibility failure rate exceeds budget**: if your CI compat gate finds >2–3% of agent images fail under runsc and runc-fallback rate is too high to audit, microVMs solve this (real Linux kernel inside).

### Where Spawnery-specific assumptions change the analysis

- **Shared-netns two-container pod**: gVisor handles this naturally — agent and inference sidecar share one Sentry, with loopback in netstack. This is *more* coupling than they'd have under microVMs (where putting two containers in one VM requires Kata-style sandbox composition). For Spawnery, this is a feature: the sidecar mediates inference under the same sandbox boundary, so a compromised agent cannot reach inference-side state by escaping the agent container alone.
- **Sidecar-mediated inference**: because the sidecar runs inside the same Sentry, it shares the gVisor compatibility constraint. Check that your inference runtime (e.g. vLLM, ggml, llama.cpp) doesn't use io_uring or FUSE in a way runsc rejects. Most don't. GPU inference is the hard case — see GPU note above.
- **Agent as untrusted attack vector**: the agent is *steered turn-by-turn by an untrusted user* and has shell/file/network tools. Treat this exactly like ChatGPT code interpreter, Modal Sandbox, or Fly Sprites — i.e. the published gold-standard threat model for this is microVM (E2B/Fly/Lambda) or strict gVisor (Modal, Cloud Run, Tencent). Hardened runc is *not* in the gold-standard set for this profile.
- **Existing egress floor**: keep it. Move it to the host's veth hook so it's enforced by the real Linux kernel, not by netstack's partial iptables. The floor is necessary but not sufficient: it stops post-compromise exfiltration; it does not stop in-spawn lateral movement, side-channel inference, or kernel-CVE-driven escape. gVisor closes the last category at the cost addressed in Section 2.

### When gVisor suffices

- The platform's tenants are anonymous or low-value; blast radius of a Sentry RCE compromising another tenant is tolerable.
- Workloads fit gVisor's compatibility envelope (general-purpose Linux + standard language runtimes).
- Host fleet is regular VMs (no /dev/kvm available cheaply).
- You need OCI drop-in and Kubernetes RuntimeClass simplicity.
- Cold-start latency budget is tight (<200 ms).

This is Spawnery's current state.

### When to go straight to microVMs

- Tenants are paying enterprise customers with contractual VM-isolation expectations.
- Per-tenant data is high-value (financial, health, model weights, source code).
- Workloads need GPU passthrough, eBPF, FUSE, io_uring, or in-spawn kernel modules.
- The product is an external "run my arbitrary code" API (E2B, Fly Sprites, Lambda) where users actively probe sandbox boundaries.
- /dev/kvm is available cheaply (bare metal or nested virt).

### Concrete adoption path

```
Phase 0 (done):          runc + egress floor + cgroups + concurrency cap
Phase 1 (≤2 weeks):      + read-only rootfs + drop-all caps + user namespaces
                         + move egress floor to host veth (out of pod netns)
Phase 2 (2–8 weeks):     + gVisor (runsc, systrap, directfs, netstack) for cloud tier
                         canary 5% → 25% → 100%
                         compat gate in CI; per-image runc fallback w/ audit
Phase 3 (threshold X):   + Kata Containers (Cloud Hypervisor backend) for hardest tier
                         gVisor remains the general-tier default
                         reserve Firecracker for a future product tier that needs snapshot/restore
```

---

## Caveats

- Several performance numbers in this report originate in vendor blogs (Ant Financial's 70%-of-apps-<1%-overhead figure; Tencent's correctness-gap-of-0.13pp figure). Spawnery should run its own benchmark against its real agent traffic before sizing capacity.
- gVisor's CVE history is short partly because of the project's explicit, narrow CVE policy (only sandbox-boundary-crossing, attacker-unconfigured spec, gVisor-specific bugs get CVE numbers). Many bugs are fixed silently. This is *not* unique to gVisor (Firecracker's CVE history is similarly short), but be aware that absence of CVEs ≠ absence of vulnerabilities. Severity ratings cited (CVSS v4 6.3, 6.5, 6.8) come from Snyk/Ubuntu/GHSA, not NVD-canonical.
- "Production users" lists are subject to vendors evolving silently. Cloud Run's Gen2 environment, for example, moved away from a gVisor-only model to a microVM + seccomp hybrid for Gen2 — recheck before contractually claiming "we use the same isolation as $vendor."
- Spectre/Meltdown and the broader transient-execution side-channel class are *not* addressed by either gVisor or Firecracker without additional host-level mitigations (SMT scheduling, KPTI, indirect-branch barriers). For Spawnery, treat as a separate workstream owned by the host OS/kernel team.
- OpenAI's Code Interpreter / Responses API container technology is not publicly documented in detail; do not assume it uses gVisor, Firecracker, or anything specific.
- The Spawnery-specific Kubernetes RuntimeClass-per-pod (not per-container) constraint means you cannot put the agent under runsc and the inference sidecar under runc within the same pod. If that ever becomes desirable, you must split into two pods (and lose the shared-netns property, which is part of why you have the design you have).
- Where evidence is thin or contested, flagged inline: Dirty Pipe (CVE-2022-0847) and CVE-2019-5736 are structurally mitigated by gVisor's architecture but not explicitly called out by Google in a blog/CVE response, unlike CVE-2020-14386, CVE-2022-0185, and CVE-2024-21626 (which are explicit). The "partial iptables" assertion for netstack comes from a research issue (github.com/github/gh-aw-firewall) quoting gVisor docs — confirm the exact supported-extension list against your egress floor's rules before relying on in-pod iptables under runsc.