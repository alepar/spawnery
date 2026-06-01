# Deep-Research Brief — gVisor for Multi-Tenant Agent Sandboxing

**Date:** 2026-06-01 · **For:** evaluating whether/how Spawnery adopts gVisor for cloud spawns.

Copy the prompt below into a deep-research agent. It is self-contained.

---

## PROMPT

You are a security/systems researcher. Produce a rigorous, citation-backed report evaluating
**gVisor (`runsc`)** as a container-isolation layer for a **multi-tenant platform that runs untrusted,
user-steered code**. Be concrete, version-specific, and skeptical of vendor marketing — prefer
primary sources (gVisor docs/design papers, the gVisor security model & CVE history, peer-reviewed or
reproducible benchmarks, and public postmortems/engineering writeups from orgs running it in prod such
as Google Cloud Run / GKE Sandbox, plus microVM comparisons from e.g. Firecracker/Kata/Fly.io). Note
dates and versions; flag where evidence is thin or contested.

### Context (the system being secured)
- **Spawnery** runs "spawns": a pod of two containers (an **agent** container + an inference
  **sidecar**) sharing one network namespace, on Docker (runtime `runc` today).
- The **agent is a coding-capable model agent** with shell/file/network tools, **steered turn-by-turn
  by an untrusted end user**, often executing **third-party app code/personas on other users' data**.
  We treat the spawn as hostile.
- **Cloud** spawns run on **Spawnery-operated, multi-tenant shared hosts** (the hard case);
  self-hosted spawns run on the user's own box (out of scope for the threat that matters here).
- **Already in place** (host-verified): a per-pod iptables **egress floor** (drop cloud-metadata +
  RFC1918, allow DNS + public), **cgroup CPU/mem/pids limits**, and a per-user concurrency cap.
  gVisor is currently only an **opt-in runtime knob** (`CONTAINER_RUNTIME=runsc`), not installed or
  enforced — the baseline is plain `runc`.
- The open question: **for cloud, do we adopt gVisor, jump to VM-level isolation (Firecracker/Kata/
  microVMs), or stay on hardened `runc` + the floor?** The report must end with a recommendation
  framework for this specific workload.

### Research questions (address each as its own section)

**1. What gVisor actually protects against.**
- Explain the architecture: the **Sentry** (user-space kernel) intercepting guest syscalls, the
  **Gofer** for filesystem, **netstack**, and the **platform** options (`ptrace`, `KVM`, `systrap`).
  How does this shrink the host-kernel attack surface vs `runc` (where containers call the host kernel
  directly)?
- Which concrete attack classes does it mitigate, and how strongly: container escape via **host
  kernel vulnerabilities** (give examples of kernel CVEs gVisor would/would not have blocked), syscall
  abuse, /proc & /sys exposure, namespace/cgroup escapes? Quantify "reduced attack surface" (e.g.
  syscalls forwarded to the host vs emulated).
- What it explicitly does **not** protect against: side-channels (Spectre-class), the **Sentry's own
  attack surface** and its CVE history, bugs in the platform/KVM, resource-exhaustion/DoS, and
  application-level abuse (our agent has legit shell/net — isolation ≠ authorization). Be specific.
- Where gVisor sits relative to seccomp/AppArmor/SELinux-hardened `runc` and user-namespace remapping.

**2. Performance overhead vs a bare container runtime (`runc`).**
- Quantify, with real benchmarks (cite source + gVisor version + platform): **syscall latency**,
  **CPU-bound** vs **syscall/IO-bound** workloads, **memory overhead per sandbox** (Sentry RSS),
  **container/process startup latency**, **filesystem I/O** (Gofer + overlay), and **network
  throughput/latency** (netstack vs `--network=host`/passthrough).
- How does the **platform choice** (`ptrace` vs `KVM` vs `systrap`) change the overhead, and what are
  the host requirements (nested virt / `/dev/kvm`) for the faster platforms?
- Map overhead to **our** workload profile: an interactive agent doing lots of file ops, process
  spawns (shell tools), and outbound HTTPS to a model API — which gVisor costs bite hardest here?

**3. Gaps vs proper VM-level isolation (Firecracker / Kata / microVMs).**
- Where is gVisor's "emulated kernel in user space" model **weaker** than a real **guest kernel behind
  a hardware virtualization boundary**? (e.g. the Sentry runs in host user space — a Sentry/platform
  escape is a host compromise; a microVM escape requires breaking the VMM + hardware boundary.)
- **Compatibility gaps**: unimplemented/partially-implemented syscalls, `/proc` & `/sys` fidelity,
  GPU/device passthrough, performance pathologies — what real workloads break or degrade on gVisor
  that work on a microVM?
- Conversely, where do microVMs **lose** to gVisor (startup time, density/memory per sandbox,
  image/OCI compatibility, operational complexity)? Present the **security ↔ compatibility ↔ overhead
  ↔ density** tradeoff explicitly, with a comparison table (runc / runc+seccomp / gVisor / Kata /
  Firecracker).
- Industry signal: who runs which in production for untrusted-code workloads (CI runners, FaaS,
  notebook/agent sandboxes, code-exec APIs) and **why** — cite concrete examples.

**4. Best practices for integrating gVisor into a production environment.**
- Installation + wiring: `runsc` with **Docker** (`--runtime`), **containerd** (`runsc` shim), and
  **Kubernetes** (`RuntimeClass`, GKE Sandbox); which is appropriate for a Docker-SDK-driven node like
  ours.
- **Platform selection** for prod (KVM where `/dev/kvm` exists, `systrap`/`ptrace` otherwise) and the
  host prerequisites; running gVisor **inside** a cloud VM (nested virt availability per cloud).
- **Networking**: netstack vs host networking tradeoffs (security vs throughput) — and implications for
  a design where the agent reaches a sidecar over a shared netns + egress through an iptables floor.
- **Observability/debugging** under gVisor (logs, `runsc debug`, strace-like tooling), image
  compatibility testing, handling unsupported-syscall failures, and **rollout strategy** (canary a
  subset of spawns, fall back to runc, per-app or per-tier runtime selection).
- **Defense-in-depth**: how gVisor composes with seccomp, the egress floor, cgroup limits, read-only
  rootfs, dropped capabilities, user-namespace remapping — what the recommended *stack* is, not gVisor
  alone.
- Failure modes & ops cost: upgrade cadence, the gVisor security-fix process, performance regressions,
  and the maintenance burden of running it at scale.

### Deliverable
- A structured report with the four sections above, a **comparison table**, and a **decision
  framework + concrete recommendation** for Spawnery's exact workload (untrusted user-steered agents,
  multi-tenant cloud hosts, already-present egress/cgroup floor): when gVisor is sufficient, when to go
  straight to microVMs, and a pragmatic **adoption path** (e.g. start with `runc`+floor+seccomp →
  add gVisor for the cloud tier → microVMs if/when X). Include the **specific signals/thresholds**
  (tenancy model, blast-radius tolerance, perf budget, host capabilities) that should drive the choice.
- Cite versions, dates, and sources throughout. Call out where the evidence is weak or where our
  assumptions (shared-netns pod, sidecar-mediated inference, agent-as-untrusted-vector) change the
  analysis.
