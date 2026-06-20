# Agent Git Environment for GitHub Mounts

**Date:** 2026-06-19
**Status:** draft (MITM forward-proxy push half; spikes S2 + S3 resolved 2026-06-19 — both confirm the approach)
**Closes:** sp-7amh (agent can't set git identity) · sp-m859.1 (inject git identity from gh) · sp-n7iy (agent cannot push — now: git **and** gh fully functional)
**Follow-up review:** sp-jg7x (verify agent↔sidecar isolation boundary across all pod lanes)
**Epic:** sp-m859 (MVP gaps)

## Problem

In a `github:owner/repo` mount spawn the agent owns the working tree and is promised (AGENTS.md) it can
commit and push. Today it can do neither, and the GitHub CLI (`gh`) does not work either:

1. **No commit identity (sp-7amh / sp-m859.1).** `GIT_CONFIG_GLOBAL` points at the read-only,
   node-owned secrets tmpfs (`nobody:nogroup` under userns-remap), with no `[user]`. `git commit`
   prompts; `git config --global …` fails with `Permission denied`.
2. **No push / no gh (sp-n7iy).** The credential is rendered **node-only** and never delivered to the
   agent; the clone's ephemeral helper flags aren't persisted. `git push` fails (`could not read
   Username`), and `gh` (which talks to `api.github.com`) has no token at all.

`internal/storage/storage.go` already chowns mount trees to `Manager.RemapBase()` (why `/app/repo` is
agent-writable and local commits work); the git *environment* never got that treatment, and no GitHub
credential is delivered to the agent.

## Goal & security model (set by owner 2026-06-19)

**Goal:** git **and** gh (and LFS, submodules — any GitHub tool) fully functional in the spawn, across
**all pod lanes**.

**Security model — the proxy's *only* job is to make the token unstealable.** The agent may do whatever
the token can do *through the spawn's proxy* — capability narrowing is an explicit **non-goal**. What
the proxy buys, precisely:
- the **real token never enters the agent** — it cannot be exfiltrated and reused off-box;
- every authenticated GitHub action is necessarily executed *through this spawn* (attributable; a
  malicious action can't be laundered through a stolen token used from elsewhere);
- killing/revoking the token cuts off *further token-based* access.

**Explicit residual (roast r4 — do not overclaim "revocable"):** because narrowing is a non-goal, an
agent acting legitimately *through the proxy* can use the token's full installation capabilities to
plant **spawn-outliving** side effects that revocation does **not** undo — GitHub **deploy keys** (which
survive even GitHub-App uninstall), repo/org **webhooks** to attacker infra, **self-hosted-runner**
registration, or a committed malicious **workflow**. None require seeing the token value. So
"unstealable + attributable" is delivered; "revoking the spawn cuts off *all* access" is **not** — the
proxy stops token *reuse*, not in-session abuse or its durable artifacts. Mitigating these would require
a high-risk-endpoint denylist, which contradicts the no-narrowing decision; documented and accepted, not
prevented.

Consequently the prior design's repo-scoping, exact-path allow-list, and per-mount routing are
**removed**. The token's blast radius is whatever the user's GitHub App **installation selection**
grants (containment e: `repository_id` never narrows scope) — accepted by design.

## Threat model (load-bearing; pinned by sp-jg7x)

The agent is untrusted; the sidecar is the pod's trusted credential boundary (it already holds the
model key). The two share **only the netns**; mount/PID namespaces are per-container (IPC is **shared**
in the CRI/runsc lane — so confidentiality rests on mount-ns + PID-ns, never IPC; the CRI pod **MUST
NOT** set `shareProcessNamespace`).

| Lane | Agent raw-socket sniff of shared netns |
|---|---|
| Docker rootless + userns-remap (self-hosted/dev) | **YES** — spike S3 (2026-06-19) confirmed it: a userns-remapped container (default caps incl `CAP_NET_RAW`, `NET_ADMIN` dropped) joined to another's netns read a plaintext secret straight off the shared loopback via AF_PACKET/tcpdump |
| Docker rootful / CapDropAll | no |
| containerd/CRI + runsc (cloud) | no — gVisor disables raw sockets absent `runsc --net-raw` (unset) |

Two invariants the proxy rests on:
- **T1 — no real secret on any leg the agent can sniff.** The node→sidecar control plane is moved off
  the sniffable netns onto a confidential transport (§2.4). The agent→proxy leg carries only a **dummy**
  token (§2.2), so sniffing it is worthless.
- **T2 — upstream confidentiality rests on strict TLS verification.** The proxy→github leg uses strict
  verification (§2.3); a spoofing agent that redirects it leaks the token only if the sidecar completes
  a handshake to an attacker cert, which strict verification prevents. The real token only ever appears
  on this leg, encrypted.

## Key decisions

Two independent halves, **both shipping now**.
1. **Identity half** — writable agent-owned global git config; `[user]` seeded from the linked GitHub
   identity (carried in the AS mint response).
2. **Push/gh half** — a **sidecar-hosted MITM forward proxy** that swaps a dummy token for the real one
   on the wire for all GitHub hosts, so git/gh/LFS/submodules work against canonical URLs while the real
   token never enters the agent.

---

## Section 1 — Identity half (sp-7amh + sp-m859.1)

### 1.1 Writable agent-owned global git config (all spawns)
A per-spawn **`git-env` dir**, host-side under the node data root, **chowned to `Manager.RemapBase()`**
(reusing `storage.go`'s chown + EPERM/0777 degraded fallback), bind-mounted at `/run/spawnery/git-env`
(a sibling of, not under, the read-only secrets mount). Agent env:
- `GIT_CONFIG_GLOBAL=/run/spawnery/git-env/gitconfig` (writable);
- `GIT_CONFIG_NOSYSTEM=1` (neutralize any `/etc/gitconfig` that could re-introduce prompts/bypass);
- `GIT_TERMINAL_PROMPT=0` (un-credentialed ops fail fast rather than hang).

### 1.2 Seed `[user]` identity (per account)
One linked GitHub account per owner ⇒ a single global `[user]`:
```
[user]
	name = <login>
	email = <id>+<login>@users.noreply.github.com
```
**Fallback (login-xor-id / org-app links):** the canonical form needs **both** login and id; if either
is missing or the link is an org/app-installation link with no user login, seed `name = <handle or
"spawnery">`, `email = <accountID>@users.noreply.spawnery.local`. Best-effort; never fails provisioning.

### 1.3 Login source — AS mint response
`MintInitial` returns `login` + numeric `id` alongside token/expiry (same authenticated node↔AS channel
— containment c unchanged); `mintGitHubMountsAtProvision` threads them into the render.

---

## Section 2 — Push/gh half (sp-n7iy): sidecar MITM forward proxy

### 2.1 Topology
The sidecar runs an **HTTP(S) forward proxy** on a fixed loopback port in the shared pod netns,
alongside its existing inference proxy. The agent is configured to route all egress through it:
- `HTTPS_PROXY` / `HTTP_PROXY` / `ALL_PROXY` = the sidecar proxy;
- git: `http.proxy` = the sidecar proxy (set in the agent-owned `<git-env>/gitconfig`, §1.1);
- **SSH→HTTPS rewrite (roast r4):** `url."https://github.com/".insteadOf` for both `git@github.com:` and
  `ssh://git@github.com/` in `<git-env>/gitconfig`, so SSH-form remotes/submodules (common in
  `.gitmodules`) are rewritten to HTTPS and traverse the proxy. A *pure* SSH remote that escapes the
  rewrite has no credential path and is out of scope (documented).
- `NO_PROXY=127.0.0.1,localhost` and the sidecar **inference** address (so the
  `OPENAI_BASE_URL=http://<sidecar>/v1` path and loopback are not double-proxied).

For **non-GitHub** hosts the proxy is a plain `CONNECT` tunnel (no interception). For **GitHub** hosts it
MITMs/injects per the §2.2 per-host policy. **Egress floor (enforced/cloud lane, roast r4):** the floor
must permit the sidecar proxy's egress to **the GitHub hosts AND the tunneled object stores**
(`*.s3.amazonaws.com` / Azure / CDN that serve LFS/release/archive objects) — not just `github.com:443`
— or LFS/large-asset traffic breaks; non-GitHub agent egress is CONNECT-tunneled subject to the floor.
The proxy is the single GitHub chokepoint — the §16.4 convergence.

### 2.2 Dummy-token swap via MITM (the unstealable mechanism)
The agent holds only a **dummy** credential, mirroring spawnery's existing sidecar-injects-real-key
pattern (the codex sidecar swaps a dummy `CODEX_SPAWNERY_KEY` for the real OpenRouter key):
- git: a credential helper in `<git-env>/gitconfig` that emits `username=x-access-token`,
  `password=<dummy>` for `github.com` (so git sends an `Authorization` header and never prompts);
- gh and other tools: `GH_TOKEN=<dummy>` (and `GITHUB_TOKEN=<dummy>`) so they believe they are
  authenticated and emit real API requests.

The sidecar proxy **MITMs the GitHub hosts** using a **per-spawn local CA** (§2.5): it presents a
just-in-time cert for the requested GitHub host signed by that CA (which the agent trusts), terminates
the agent's TLS, **overwrites the `Authorization` header with the real token** (pulled per §2.4),
re-originates a **strictly-verified** TLS connection to the real host, and streams the exchange. The
dummy is discarded; the agent never sees the real token. Because the agent's *only* authenticated path
to GitHub is through the proxy, a bypassing direct connection is merely unauthenticated (the dummy is
worthless) — so the token is **unstealable** and all authed traffic is **attributable** to the spawn.

**Host predicate is security-critical (roast r4 BLOCKER).** Whether a CONNECT target "is a GitHub host"
decides whether the **real token is injected**, so a loose match (substring / unanchored suffix) lets the
agent CONNECT to an attacker-registered look-alike (`github.com.attacker.example`, `evilgithubusercontent.com`),
obtain a valid DV cert, and have the proxy **inject the real token into the attacker's connection** —
defeating the whole goal. The predicate **MUST** be an **exact** host-name allow-list plus a
**dot-anchored** suffix match (`host == "github.com"` … or `strings.HasSuffix(host, ".githubusercontent.com")`
with the leading dot), case-folded, IDNA-normalized, port-stripped — never a substring/contains check.

**Per-host action policy (roast r4 BLOCKER — inject vs tunnel must be precise):**

| Host(s) | Action |
|---|---|
| `github.com`, `codeload.github.com` (git smart-HTTP, LFS *batch*) | MITM + inject `Authorization: Basic base64("x-access-token:"+token)` |
| `api.github.com`, `uploads.github.com`, `gist.github.com` (REST/GraphQL, release-asset upload) | MITM + inject `Authorization: Bearer <token>` |
| `raw.githubusercontent.com` (private raw content) | MITM + inject (bearer) |
| **presigned object stores** — `*-cloud.githubusercontent.com`, `objects.githubusercontent.com`, `*.s3.amazonaws.com`, Azure/CDN object hosts | **CONNECT-tunnel, NO MITM, NO inject** (presigned URLs carry their own auth; injecting → S3 "Only one auth mechanism allowed", breaks LFS/archive) |
| anything not in the allow-list | CONNECT-tunnel untouched |

So `*.githubusercontent.com` is **not** a blanket MITM target — the object-serving subdomains are
tunnel-only and take precedence over the raw-content rule. Also leave any request already carrying an
**LFS-issued action token** (`lfs.github.com` verify/action leg) untouched. Note (S2): because the proxy
overwrites `Authorization` unconditionally on GitHub hosts, git's first request is already authed and it
never sees a 401 / never invokes its credential helper — so the agent-side dummy helper is
belt-and-suspenders; the proxy authing unconditionally is the actual mechanism.

**Protocol fidelity — spike S2 RESOLVED 2026-06-19: PASS.** A `goproxy`-based localhost MITM proxy
faithfully carried **git clone (protocol-v2 + sideband)**, a **60 MB `git push`** (git streamed the pack
as `Transfer-Encoding: chunked`, which the proxy carried cleanly), **`gh api` + `gh pr create`**, and
**full Git LFS** (batch on the github host MITM'd; object transfer tunneled to S3). **No kill-criterion
fallback was needed** — the node-side `git http-backend` / thin API reverse-proxy fallback is *not*
required. Two findings folded into the design: (1) **HTTP/1.1-only on the MITM leg is sufficient** —
goproxy downgrades the gh API's HTTP/2 to 1.1 and it was immaterial (git/gh/LFS all work over 1.1), so
the proxy need not implement HTTP/2 MITM; (2) the CA must be delivered as a **combined bundle** (§2.5).
Build on a vetted MITM library (goproxy/martian-style), not hand-rolled TLS.

### 2.3 Upstream TLS — hard invariant (T2)
The proxy→github client **MUST** use stock TLS with default verification and the correct `ServerName`;
**MUST NOT** set `InsecureSkipVerify`, add a custom CA pool, or honor `HTTP(S)_PROXY`/`NO_PROXY`. The
sidecar image **MUST** ship a clean, current CA bundle. **Spike S1:** optional GitHub cert/IP pinning.

### 2.4 Pull-based token (lane-aware confidential control transport)
The node is a **credential server**; the proxy pulls the real token on demand:
```
GetToken(spawnID, minRemaining) → { token, accessExpiresAtUnix }
```
The node resolves the spawn's GitHub link from durable mount bindings (so a **node restart** re-derives
it), mints a fresh token if the current one has `< minRemaining` left, returns it plus its **expiry**
(the proxy caches and honors `minRemaining` itself). The token is the AS-custodial **user-to-server**
access token (≈8 h lifetime, refreshable — *not* a 1 h installation token; containment a/e). The proxy
fetches a token valid ≥ `minRemaining` **before** each exchange so it cannot expire mid-stream.
**`minRemaining` is a small buffer (≈5 min), not "near full headroom"** (roast r4: a near-headroom
threshold would force a fresh mint before almost every request and defeat the cache); against an ~8 h
token a 5-min buffer makes mid-exchange expiry require a single >~8 h streaming op — a surfaced error,
not corruption. One token per spawn (one linked account); no per-repo routing.

**GetToken authentication (roast r4):** in the non-UDS (TCP) lane the listener is reachable by other pods
on the shared bridge, so `GetToken` is **not** trust-by-reachability: it requires the **per-spawn** bearer
(in sidecar env only) **and** the node validates the **caller's source pod IP == this spawn's pod IP**. In
the UDS lane the socket is per-spawn (its own `0711` dir), so scoping is the filesystem boundary. Either
way the binding is per-spawn — never a node-wide credential oracle.

**Control transport (T1), lane-aware** — selected by `UsernsMode`/`ContainerRuntime` (+ S3):
- **agent can sniff (userns-remap — confirmed by S3, so the UDS is required here, not optional):** a **pathname unix-domain socket on a node↔sidecar-only
  bind mount** (host→sidecar). Off the netns, invisible to the agent (different mount ns). The node
  creates the **dir `0711`** and **socket `0666`** owned by the host node uid, so the userns-remapped
  sidecar can traverse + connect (a `0700` dir would reproduce the §1.1 traverse-denied bug). **Pathname
  only** — abstract (`@`) sockets are netns-scoped/agent-reachable and are **prohibited**.
- **agent cannot sniff (runsc, CapDropAll, or userns-remap if S3 clears it):** a **node-hosted TCP
  `GetToken` listener** on the pod-bridge gateway, address passed to the sidecar via env, **bearer-gated
  with the bearer in sidecar env only** (withheld from the agent). Sound because the agent can't sniff
  the bearer in these lanes. The model-switch control moves onto the same transport in the sniffable
  lane.

**No agent-reachable auxiliary listeners:** the sidecar exposes no other TCP port (pprof/metrics/health)
on the shared netns; any such surface binds the UDS or is disabled.

**Error semantics:** `GetToken` node/AS failures (AS unreachable, mint `NotFound`,
`RelinkRequired`/broken chain, rate-limited, revoked mid-session) → typed error → the proxy returns a
distinct non-retrying upstream status with a short diagnostic body, so the agent's git/gh fails fast
and comprehensibly (no prompt loop). The node **rate-limits** mints per spawn; `GetToken` uses a fresh
request id when minting (never a dedup id returning the expired token).

### 2.5 Per-spawn local CA — node-owned, delivered both ways (roast r4)
The **node generates the per-spawn CA** (not the sidecar — the sidecar shares only the netns with the
agent and cannot write the agent's filesystem, so a sidecar-generated cert has no path to the agent's
trust store; and a sidecar app-restart would regenerate a CA the agent no longer trusts). The node:
- delivers the **CA private key to the sidecar** over the confidential control transport (§2.4) — the
  sidecar uses it to sign per-host leaf certs JIT; the key never touches the agent;
- writes the **CA public cert** into the agent-visible `<git-env>` bind mount (the node owns both
  containers' bind mounts), chowned to RemapBase (agent-readable);
- re-delivers the same CA (key to sidecar, cert already in git-env) on a **sidecar app-restart**, so the
  agent's already-installed trust stays valid (no regen/mismatch).

**Trust install — env-var-primary, distro-agnostic (roast r4):** agent images are user-supplied
(Alpine/RHEL/distroless, possibly non-root, no `update-ca-certificates`), so trust **MUST NOT** depend on
a system-trust install. Primary mechanism is **env vars** pointing at the combined bundle in git-env:
`GIT_SSL_CAINFO`, `SSL_CERT_FILE`, `SSL_CERT_DIR`, `NODE_EXTRA_CA_CERTS`, `REQUESTS_CA_BUNDLE`,
`CURL_CA_BUNDLE` (covers git, gh/Go, curl, node, python). A `update-ca-certificates` system install at
launch is **best-effort** belt-and-suspenders where the image supports it.

**Combined bundle, not a replacement (S2 finding — load-bearing):** the bundle **MUST** be **system CAs +
the per-spawn CA concatenated**, never the per-spawn CA alone — the tunneled legs (the presigned object
store) still need the real roots, so a replacement bundle breaks LFS (S2 reproduced this).

The agent receives only the **public** cert (cannot mint certs). The **CA private key now co-resides in
the sidecar** alongside the real token and the model key — an accepted, reviewed extension of the
sidecar trust boundary, covered by sp-jg7x (mount/PID isolation keeps all three from the agent).

### 2.6 Lifecycle & ordering
- **Provision/resume:** no `origin` rewrite, no pre-StartAgent token push — the agent clones/uses
  **canonical** `https://github.com/...` URLs and pulls auth lazily through the proxy. The node-side
  clone (`storage.Prepare`) is unchanged (it uses the node-only credential as today). The agent's later
  pushes/fetches/gh-calls flow through the proxy.
- **Sidecar-ready before agent-start:** the sidecar (Phase 1) must have bound the proxy port, generated
  the CA, and wired the control client before the agent (Phase 2) starts — a `StartPod` readiness probe
  (also prevents the agent **port-squatting** the proxy port). For a **CRI sidecar app-container
  crash-restart** within a live PodSandbox, the node re-asserts readiness/port ownership or fails the
  spawn (the CA is regenerated; the agent already trusts the CA *cert file*, so a new CA requires
  re-delivery — handle by persisting the per-spawn CA in the sidecar's tmpfs across an app-restart, or
  re-running the trust install; pinned in the impl).
- **Egress floor:** in enforced lanes the floor **MUST** permit the sidecar→github hosts on :443.

---

## Section 3 — Boundary review (sp-jg7x, follow-up)

sp-jg7x proves per lane: mount-ns + PID-ns isolation (no FS read / no ptrace; CRI pods don't enable
shared-PID); **spike S3 — resolved 2026-06-19: the userns-remap agent CAN sniff the shared netns** (so
the UDS transport stays mandatory there; sp-jg7x still confirms runsc/CapDropAll cannot sniff); and that
the agent cannot reach the control socket/listener or extract the real token / model key /
**CA private key**. Follow-up, not a merge gate — T1 (§2.4) and strict TLS (§2.3) ship now.

## Section 4 — Containment reconciliation

- **(b) real token never in the agent** — preserved (agent holds only a dummy; the proxy injects the
  real token on the upstream leg).
- **(a) refresh material AS-custodial** — unchanged (only short-lived access tokens are minted/served).
- **(c) token never relayed by the CP** — unchanged (pull flow is node↔sidecar + node↔AS).
- **(e) `repository_id` never narrows scope** — honored: narrowing is now an explicit non-goal; the
  token's blast radius is the installation selection, accepted.
- **Secrets tmpfs stays agent-unreadable** — unchanged (new writable surface is the separate git-env).
- **Blast radius:** co-locating the github token (and the per-spawn CA key) with the model key in the
  sidecar is an accepted, reviewed extension of the sidecar's trusted-proxy role (sp-jg7x covers it).
  The **github** control plane moves onto the confidential transport (§2.4), closing *its* exposure in a
  sniffable lane. The pre-existing **model-switch-bearer / BYOK-key** cleartext exposure uses the same
  mechanism but its migration is **tracked separately** (folded into sp-n7iy's control-channel hardening
  per owner direction; the broader BYOK item is not fully designed here) — do not read §4 as already
  closing it.

## Section 5 — Testing

**Identity (hermetic + e2e):** seeded `[user]` rendered; git-env chowned to RemapBase + writable;
login-xor-id / org-app fallback; `GIT_CONFIG_NOSYSTEM`/`GIT_TERMINAL_PROMPT` set; e2e in-spawn `git
commit` records the GitHub author with no prompt.

**Push/gh (e2e, per lane):** in-spawn, against a real private repo, with the proxy + CA in place:
`git clone`/`fetch`/**`push`** (protocol-v2 + a large/sideband pack), **`gh pr create`** / `gh api`,
**Git LFS** push/pull of a tracked file, and a **private submodule** update all succeed; suspend →
resume → push still works; a near-expiry token triggers the `GetToken(minRemaining)` fresh-mint;
`GetToken` node error yields a fast comprehensible failure (no prompt loop).

**Unstealable / boundary (sp-jg7x + hermetic):** the real token and the CA **private key** are absent
from the agent FS/env; the agent holds only the dummy; the agent cannot reach the control socket/listener;
a direct (proxy-bypassing) agent connection to github is unauthenticated; control-socket dir `0711` /
socket `0666` pathname; per-lane raw-socket (S3) + PID/IPC matrix; no auxiliary agent-reachable sidecar
listener.

All integration/e2e tests are build-tagged and fail (never skip) when their lane dep is down; the per-lane
push/gh suite runs in the docker-userns-remap and runsc lanes.

## Recommended spikes

- **S2 — MITM proxy fidelity — RESOLVED (2026-06-19): PASS.** A `goproxy`-based localhost MITM proxy
  carried `git clone` (v2/sideband), a 60 MB `git push` (chunked pack), `gh api`/`gh pr create`, and full
  Git LFS, with `Authorization` overwritten on the wire and the real token never reaching the client.
  **HTTP/1.1-only on the MITM leg sufficed** (gh's HTTP/2 downgraded to 1.1, immaterial). **No fallback
  needed** — the node-side `git http-backend` / thin gh reverse-proxy kill-path is unnecessary. Required
  by the design: a **combined CA bundle** (§2.5) and tunneling (not MITM'ing) the presigned object store.
- **S3 — userns-remap raw-socket sniffability — RESOLVED (2026-06-19): the agent CAN sniff.** A
  userns-remapped container (default caps incl `CAP_NET_RAW`, `NET_ADMIN` dropped) joined to another's
  netns read a plaintext secret off the shared loopback via tcpdump/AF_PACKET. So the UDS confidential
  transport **is required** in the userns-remap lane (no TCP-everywhere simplification), and the
  pre-existing cleartext control endpoint **is** agent-exposed today. (sp-jg7x still pins runsc + the
  other lanes.)
- **S1 — upstream TLS pinning (optional hardening).**

## Implementation sketch (files)

- `internal/spawnlet/secrets.go` / `manager.go` — git-env dir (chown RemapBase, bind); repoint
  `GIT_CONFIG_GLOBAL`, set `GIT_CONFIG_NOSYSTEM`/`GIT_TERMINAL_PROMPT`, dummy-cred helper +
  `HTTPS_PROXY`/`http.proxy`/`NO_PROXY` + `GH_TOKEN=<dummy>` + CA env; deliver the per-spawn CA cert
  (agent-readable); create the control UDS (`0711` dir / `0666` pathname socket) or the node-hosted
  TCP `GetToken` listener per lane (+ S3); sidecar-readiness gate.
- `deploy/agent/launch` (or launcher) — install the per-spawn CA into the agent system trust
  (`update-ca-certificates`) at startup.
- `internal/githubcred/render.go` — identity-only gitconfig + dummy credential helper; keep node
  credential render for the node-side clone.
- `internal/node/*` — node credential server `GetToken(spawnID, minRemaining)` →
  `{token, accessExpiresAtUnix}` (resolve link from mount bindings, mint-when-stale, rate-limited,
  typed failures); thread login/id from the mint response. **No repo-scope change to the mint.**
- AS mint endpoint + mint client — add `login`+`id` to the response (no repository param).
- `internal/sidecar/*` + `cmd/sidecar/main.go` — the MITM forward proxy: the **exact/anchored host
  predicate** + **per-host action table** (§2.2), JIT leaf certs signed by the node-delivered CA key
  (**ECDSA P-256** for fast keygen off the hot path, SAN-based, `EKU=serverAuth`, **per-host cached**),
  `Authorization` overwrite, CONNECT tunnel for the rest (incl. presigned object stores), strict upstream
  TLS, **HTTP/1.1 only on the MITM leg** (S2: sufficient; do not build h2 MITM); the `GetToken` control
  client over the UDS/TCP transport; no auxiliary agent-reachable listeners. Validate fidelity **under the
  runsc gVisor netstack**, not just native localhost (S2 was a localhost prototype).
- proto/gen — mint response (`login`,`id`); control messages (`GetToken` req/resp incl. expiry);
  `make gen`. `proto/`-touching work serialized ahead of consumers.

## Post-Implementation Notes

*As this design is implemented and iterated on — bug fixes, adjustments, anything that diverged from the
assumptions above — append a dated note here, whether or not a formal debugging skill was used.*

- **2026-06-19 (roasts r1–r3):** drove the cleartext-control-channel fix, the push→pull credential flip,
  lane-aware transport, and the dropping of the infeasible repo-scoped-mint MUST (see git history).
- **2026-06-19 (dev-goal relaxation → MITM redesign):** owner relaxed the push half to "git **and** gh
  fully functional; the proxy need not narrow access — its sole job is to make the token unstealable
  (all GitHub activity executed through the spawn, attributable, revocable)." Replaced the per-repo
  reverse-proxy + origin-rewrite + allow-list apparatus with a **sidecar MITM forward proxy** that swaps
  a **dummy token** for the real one on the wire for all GitHub hosts (per-spawn local CA installed into
  the agent trust; agent holds only the dummy). git/gh/LFS/submodules work against canonical URLs; the
  origin-rewrite/routing/allow-list/Host-SNI/path-canonicalization complexity is deleted. Kept the
  pull-based `GetToken`, lane-aware confidential transport (T1), strict upstream TLS (T2), identity half,
  and the threat-model corrections. New load-bearing risk = MITM protocol fidelity (S2, HTTP/1.1+HTTP/2)
  and CA trust install across the tool ecosystem. Scope: **all lanes** (incl. runsc/CRI).
- **2026-06-19 (spikes S2 + S3 — both confirm the approach):** **S3** (docker userns-remap experiment) —
  the agent CAN sniff the shared netns (read a plaintext secret off the shared loopback), so the UDS
  confidential transport is mandatory in the userns-remap lane and the existing cleartext control endpoint
  is genuinely exposed today. **S2** (goproxy MITM prototype vs real github) — **PASS**: clone (v2/sideband),
  a 60 MB push (chunked pack), `gh api`/`gh pr create`, and full LFS all worked with the dummy→real
  `Authorization` swap and zero real-token leakage to the client; HTTP/1.1-only on the MITM leg sufficed;
  no node-side `http-backend` fallback needed. Folded in: the CA must be a **combined** bundle (system +
  per-spawn CA), the presigned object store is **tunneled not MITM'd**, and LFS-action tokens are left
  untouched.
- **2026-06-20 (roast r4 BLOCK — real-world hardening, no architecture change):** folded two blockers and
  a cluster of GitHub/PKI majors. **Blockers:** (1) the GitHub host predicate is **security-critical** —
  must be an exact/dot-anchored allow-list, never substring/suffix (a look-alike host would get the real
  token injected); (2) `*.githubusercontent.com` must **not** be blanket-MITM'd — presigned object
  subdomains are tunnel-only (injecting → S3 "Only one auth mechanism allowed"). Added a precise per-host
  action table (§2.2) incl. `uploads.github.com`. **Majors:** corrected the **revocability overclaim** —
  the agent can plant **spawn-outliving** side effects through the proxy (deploy keys survive even App
  uninstall, webhooks, runner registration, workflows); documented as an accepted residual (prevention
  would need a high-risk-endpoint denylist, which contradicts no-narrowing). **SSH→HTTPS `insteadOf`
  rewrite** so submodules traverse the proxy (pure-SSH out of scope). **Node owns the per-spawn CA** (key
  → sidecar via the control transport, cert → agent via git-env) — fixes the "sidecar can't write the
  agent FS" contradiction and the sidecar-restart CA-regen mismatch; the CA **private key now co-resides
  in the sidecar** (sp-jg7x boundary). **CA trust is env-var-primary** (distro-agnostic; user-supplied
  images may be Alpine/distroless/non-root). Pinned the token as **user-to-server (~8 h, refreshable)** and
  fixed `minRemaining` to a **small buffer** (the prior "near full headroom" was self-defeating).
  **`GetToken` per-call auth** (per-spawn bearer + source-pod-IP check) in the TCP lane. **Egress floor**
  must also allow the proxy→**object-store/CDN** hosts. Noted: `CAP_NET_RAW` also enables **active** netns
  attacks (DNS/RST injection) — defended by T2 strict TLS + off-netns UDS; RST-DoS is self-inflicted,
  accepted. Leaf-cert params (ECDSA P-256, SAN, EKU, per-host cache) and **runsc-netstack fidelity** are
  implementation requirements (S2 was a native-localhost prototype). The model-switch/BYOK migration onto
  the confidential transport uses the same mechanism but is tracked with the broader BYOK exposure, not
  fully designed here.
