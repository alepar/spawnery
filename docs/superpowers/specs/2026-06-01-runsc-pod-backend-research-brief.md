# Deep-Research Brief: How to run a gVisor (runsc) "pod" from a Go daemon

> **Status:** research brief, 2026-06-01. Feeds the design decision for the runsc pod backend
> (`sp-vaw` / epic `sp-7k8`). Companion to the gVisor adoption research
> (`2026-06-01-gvisor-isolation-research-results.md`). The output of this research selects the
> **containerd integration layer** for the runsc path, after which we write the implementation spec.

---

## 0. What we need answered (the one-paragraph version)

We run a two-container **pod** — an untrusted, user-steered coding **agent** (goose) plus an
inference **sidecar** that holds the model API key — that **share one network namespace** (agent
reaches sidecar on `127.0.0.1:8080`) but keep **separate mount/PID namespaces** (so the agent can
never read the sidecar's key). On the default `runc` runtime we build this with the Docker SDK
(sidecar container, then agent with `--network=container:<sidecar>`). We want to run the **same pod
under gVisor (`runsc`)** for the cloud tier. **Per-container runsc breaks the shared-netns trick** —
two separate Sentries don't bridge their user-space netstacks — so the pod must become **one gVisor
sandbox hosting both containers** (the Kubernetes pod model). **Question: what is the most mature,
maintainable way for our Go node daemon (`spawnlet`) to create, attach to, firewall, and tear down
such a gVisor pod — outside of a full Kubernetes cluster?**

Produce a recommendation with evidence, scoped to our concrete constraints (§2) and the candidate
approaches (§3), answering the specific questions in §4.

---

## 1. Background the researcher needs

- **gVisor / runsc**: a user-space kernel (Sentry) intercepting container syscalls. Each
  `runsc`-run sandbox is its own Sentry with its own user-space network stack (netstack). The
  containerd shim is `containerd-shim-runsc-v1` (runtime type `io.containerd.runsc.v1`). gVisor
  groups containers into a **pod sandbox**: the *first* container creates the sandbox; subsequent
  containers with the matching pod annotations run **inside the same Sentry** and can share the pod
  network namespace. This is exactly the k8s pod model and is gVisor's supported multi-container path.
- **Our prior finding (host-verified):** a single `docker run --runtime=runsc` works, but joining a
  second runsc container to the first's netns via `--network=container:<id>` does **not** give the
  two containers a shared, working loopback — confirmed by an agent that boots under runsc but
  cannot reach the sidecar on `127.0.0.1:8080`. So Docker's two-container shared-netns model is
  fundamentally incompatible with per-container gVisor; we need a real pod sandbox.
- **Egress floor (security-critical):** on cloud nodes we enforce a per-pod network floor as host
  iptables rules **matched by the pod's bridge source IP** (drop cloud-metadata `169.254/16` +
  RFC1918, allow DNS + an operator allow-list, default-allow public). On Docker we install these in
  the **`DOCKER-USER`** chain. We proved the *in-netns* floor is a **no-op under runsc** (netstack
  emits raw frames the host-netns netfilter never sees) and that **host-side** rules on the veth
  **do** enforce under both runtimes. `DOCKER-USER` is **Docker-specific** — a CRI/containerd pod on
  a CNI bridge won't traverse it, so the floor needs a different, equally-in-path host chain.
- **ACP bridge:** the node attaches a **persistent, bidirectional stdio stream** to the *agent*
  container — JSON-RPC (ACP) over the agent's stdin/stdout for the entire spawn lifetime. Today this
  is a Docker `Attach` returning demuxed stdout + a stdin writer (`internal/runtime` `AttachedStream`).
  Whatever backend we pick must give the node an equivalent long-lived stdio pipe to the agent.
- **Node constraints:** `spawnlet` is a Go daemon, already runs as root (for the host iptables
  floor), already depends on the Docker SDK and a little of the containerd module tree. It selects
  the runsc path via `CONTAINER_RUNTIME=runsc`. It must also run a **startup preflight** (smoke a
  throwaway pod/container under the runtime, exit hard on failure).

---

## 2. Hard constraints the recommendation must satisfy

1. **Key isolation preserved** — agent and sidecar stay in **separate containers** (separate
   mount/PID namespaces). Collapsing them into one container is off the table.
2. **Shared pod network namespace** — agent reaches sidecar on loopback `127.0.0.1:8080`.
3. **A routable pod IP the host can match** — so the egress floor can be applied by source IP on a
   host chain that is genuinely in the pod's forward path.
4. **Persistent bidirectional agent stdio** — suitable for a long-lived JSON-RPC (ACP) stream, with
   sane half-close / EOF / teardown semantics; not request/response `exec`.
5. **Single Go daemon, root, no Kubernetes control plane** — we are NOT running a kubelet / apiserver
   / scheduler. Any "CRI" use is the node talking directly to a local containerd/CRI-O socket.
6. **Mature & battle-tested** — prefer the boring, widely-deployed path over bleeding-edge. Note EOL,
   maintenance status, and breaking-change history of each dependency.
7. **Coexists with the unchanged Docker/runc path** — runc spawns keep using the Docker SDK; this is
   only the runsc branch. A clean Go seam both backends implement is desired.

---

## 3. Candidate approaches to evaluate (and find others)

**A. containerd CRI plugin via `k8s.io/cri-api` gRPC.** Node speaks CRI (`RunPodSandbox` +
`CreateContainer`×2 + `StartContainer`) to the local containerd CRI socket. Pods + CNI sandbox
networking are handled by containerd. Requires a `runsc` runtime handler + CNI conf in
`/etc/containerd/config.toml`. Open issues to research: stdio **attach** goes through the **CRI
streaming server** (a separate HTTP/SPDY or WebSocket endpoint) — is that suitable for a persistent
JSON-RPC bridge? Dependency weight of `k8s.io/cri-api` + the streaming client. How is the per-pod IP
discovered, and which host iptables chain is in-path with the CRI/CNI bridge?

**B. containerd **native** Go client + `libcni` + manual shared netns.** Node uses
`github.com/containerd/containerd`'s client API to launch two containers via the runsc shim, **creates
the shared netns itself** and runs **CNI ADD/DEL via `libcni`**, joining both containers to it. Task
stdio is direct FIFO/`cio` — a clean persistent ACP bridge like Docker attach. Open issues: does the
`io.containerd.runsc.v1` shim correctly host **two containers in one sandbox sharing a netns** when
driven through the *native* client (not CRI)? gVisor's pod grouping historically keys off **CRI/k8s
pod-sandbox annotations** — does it work without the CRI plugin, by setting the same annotations on
the native containers? How much pod/netns/CNI orchestration must we reimplement?

**C. CRI-O instead of containerd's CRI.** Same CRI API surface as (A) but a different daemon
purpose-built for CRI. Does it offer a cleaner runsc story or attach model? Maturity for non-k8s use?

**D. nerdctl as a library / `compose`-style pod.** Does nerdctl expose a stable Go API for pods +
runsc, or is it CLI-only? Attach model? (Likely CLI-only — confirm.)

**E. Podman pods.** Podman has first-class `pod` support (shared netns, separate containers) and a Go
binding. Does Podman support **runsc/gVisor** as an OCI runtime for a pod, and does its attach give a
persistent bidirectional stream? Rootful Podman + gVisor maturity? This may be the closest off-the-
shelf match to our exact shape (a pod of containers sharing netns) — evaluate seriously.

**F. Anything the research surfaces** — e.g., Kata-style integrations, `youki`, sandboxed-containers
operators, or a documented gVisor "standalone pod" recipe. Include if mature and non-k8s.

For each viable approach, the researcher should map it against **all seven constraints in §2** and
the specific questions in §4.

---

## 4. Specific questions to answer

**Pod / sandbox mechanics**
1. What is gVisor's **officially documented** way to run a multi-container pod (shared netns, separate
   roots) **outside Kubernetes**? Does it require the CRI plugin, or do containerd native containers
   with the right pod annotations land in one sandbox? Cite gVisor + containerd docs.
2. With approach B, what exactly must the daemon do to build the shared netns and wire CNI so the
   runsc sandbox uses it? Is there a reference (code or docs) for "containerd native client + runsc +
   shared netns + libcni" without k8s?

**Stdio attach (most important for us)**
3. How does **CRI `Attach`** work for a *persistent* stdin+stdout stream? Streaming-server transport
   (SPDY/WebSocket), reconnection, EOF/half-close behavior, and known latency/buffering issues for a
   chatty JSON-RPC stream. Is it production-sane for a multi-hour ACP session, or is it really meant
   for interactive `kubectl attach`?
4. How does **containerd native task stdio** (`cio`, FIFOs) compare for the same use — can the node
   hold a long-lived bidirectional pipe to the agent's stdin/stdout directly?
5. How does **Podman attach** compare?

**Networking & the egress floor**
6. With CNI bridge networking (containerd CRI or libcni), **which host iptables chain(s)** does pod
   egress traverse, and **where do we hook a per-pod source-IP DROP floor** so it is as reliably
   in-path as `DOCKER-USER` is for Docker? (Custom chain jumped from `FORWARD`? The CNI plugin's own
   chains? `nftables` equivalents?) We must be able to add/remove rules per spawn matched by pod IP.
7. How is the **pod IP** discovered programmatically in each approach (CRI `PodSandboxStatus` vs.
   libcni result vs. Podman inspect)?
8. Does running under **runsc** change any of the above vs. runc (does netstack still egress through
   the same host veth/bridge so host netfilter sees it)? Confirm our host-side-floor finding holds
   for the **pod** case, not just single containers.

**Images, lifecycle, ops**
9. Image store: with containerd CRI the images live in the **`k8s.io`** namespace, separate from
   Docker's store — confirm we must pull via the CRI `ImageService` (or `ctr -n k8s.io`), and how that
   interacts with our existing Docker-built images. For approach B/E, where do images live?
10. Teardown: the correct call sequence to stop + remove a pod and reclaim the netns/CNI/IP in each
    approach (e.g., `StopPodSandbox`+`RemovePodSandbox`; or task kill + CNI DEL + netns delete).
11. Preflight: cheapest reliable "can this host run a runsc pod?" smoke at daemon startup per approach.
12. **Dependency & maintenance cost:** module size and transitive-dep weight of `k8s.io/cri-api` +
    streaming client vs. the containerd native client vs. the Podman bindings; each project's release
    cadence, API stability, and any recent breaking changes. We value boring and stable.

**Production evidence**
13. How do real multi-tenant agent/code-execution platforms run gVisor pods from a daemon **without a
    full k8s cluster** (e.g., Modal, Fly Machines, Cloud Run gen2, E2B, Daytona, Tencent, replicate)?
    Anything public on their containerd-vs-CRI-vs-native choice and their stdio/attach approach.

---

## 5. Deliverable shape

- A **recommendation** (one of §3 A–F, or a hybrid) with the reasoning tied to our seven constraints.
- A **constraints × approaches matrix** (rows = §2 constraints, cells = how each approach satisfies /
  fails / partially-satisfies, with citations).
- **The egress-floor answer**: the specific host chain + rule mechanism for the chosen approach, with
  enough detail to implement (chain name, jump point, match syntax, add/remove per spawn by pod IP).
- **The stdio-attach answer**: the exact API/transport the node uses for the persistent ACP bridge,
  with the EOF/teardown semantics called out.
- **Risk list**: the top 3–5 things most likely to bite us (e.g., CRI streaming attach latency, gVisor
  native-client pod-annotation gaps, CNI floor placement, image-store split-brain) and how to de-risk
  each (a spike to run, a doc to confirm).
- **Ops prerequisites** for the chosen approach (containerd/CRI-O/Podman config, runsc handler
  registration, CNI conf, required host binaries).

Cite primary sources (gVisor docs, containerd/CRI-O/Podman docs + source, CNI spec) over blog posts
where possible; flag anything that is version-specific or that changed recently.
