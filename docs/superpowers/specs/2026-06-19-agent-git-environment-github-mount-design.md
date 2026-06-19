# Agent Git Environment for GitHub Mounts

**Date:** 2026-06-19
**Status:** draft (push half redesigned to a MITM forward proxy after the dev-goal relaxation; identity half stable)
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
the proxy buys is that the **real token never enters the agent**, so:
- every authenticated GitHub action is necessarily executed *through this spawn* (attributable; a
  malicious action can't be laundered through a stolen token used from elsewhere);
- killing or revoking the spawn cuts off access; the token cannot be exfiltrated and reused off-box.

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
| Docker rootless + userns-remap (self-hosted/dev) | **maybe** — default caps incl `CAP_NET_RAW`, but in a *user* namespace that cap is scoped to namespace-owned devices (spike S3) |
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
- `NO_PROXY=127.0.0.1,localhost` and the sidecar **inference** address (so the
  `OPENAI_BASE_URL=http://<sidecar>/v1` path and loopback are not double-proxied).

For **non-GitHub** hosts the proxy is a plain `CONNECT` tunnel (no interception). For **GitHub** hosts
it MITMs and injects (§2.2). Under an enforced egress floor (cloud lane) the floor permits only the
proxy's own egress to GitHub; the proxy is the single GitHub chokepoint — the §16.4 convergence.

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

**GitHub host set (MITM + inject):** `github.com`, `api.github.com`, `codeload.github.com`,
`gist.github.com`, `*.githubusercontent.com`, and the GitHub LFS endpoints. **Header form by surface:**
git smart-HTTP and LFS auth → `Authorization: Basic base64("x-access-token:"+token)`; the REST/GraphQL
API → the token as a bearer. **Do not inject** on LFS **object-store** transfer URLs (presigned S3/Azure
URLs returned by the batch API are already authorized) — those are tunneled, not re-authed. MITM is
restricted to the GitHub host set; everything else is tunneled untouched.

**Protocol fidelity (spike S2 — gating):** the proxy must faithfully carry git smart-HTTP (protocol-v2,
`Expect: 100-continue`, chunked `git-receive-pack`, sideband-64k flushing) **and** the API over
**HTTP/2**. After the MITM handshake the proxy can largely splice the decrypted byte stream, parsing
only enough of each HTTP request to overwrite `Authorization`. S2 validates this for HTTP/1.1 (git) and
HTTP/2 (api/gh); **kill criterion:** a surface can't be carried faithfully → for that surface, run git's
node-side `http-backend` (git) / a thin API reverse proxy (gh) as a fallback. Build on a vetted MITM
library (e.g. goproxy/martian-style) rather than hand-rolling TLS.

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
(the proxy caches and honors `minRemaining` itself). The proxy fetches a token valid ≥ `minRemaining`
**before** each exchange so it cannot expire mid-stream (set `minRemaining` near full token headroom;
a single multi-hour exchange exceeding it is a surfaced error, not corruption). One token per spawn
(one linked account); no per-repo routing.

**Control transport (T1), lane-aware** — selected by `UsernsMode`/`ContainerRuntime` (+ S3):
- **agent can sniff (userns-remap, pending S3):** a **pathname unix-domain socket on a node↔sidecar-only
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

### 2.5 Per-spawn local CA delivery
The **sidecar generates a per-spawn CA at startup**; the **private key stays sidecar-only** (used to
sign per-host leaf certs JIT). The **public CA cert** is delivered to the agent and trusted:
- written to an agent-readable path under `<git-env>` (chowned to RemapBase, agent-owned);
- **installed into the agent's system trust at launch** (the launcher copies it to
  `/usr/local/share/ca-certificates` + `update-ca-certificates`) so git, gh, curl, node, python all
  trust it via the system pool; plus belt-and-suspenders env (`GIT_SSL_CAINFO`, `SSL_CERT_FILE`,
  `NODE_EXTRA_CA_CERTS`, `REQUESTS_CA_BUNDLE`) pointing at a bundle that includes it.

The agent receives only the **public** cert (cannot mint certs). Trusting an extra CA only lets *the
sidecar* intercept the agent's own traffic — it grants the agent no capability. **Lane-agnostic:** CA
delivery + trust install are identical across docker and runsc (a file install in the agent container);
the proxy listens on loopback in the pod netns in every lane.

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
shared-PID); **spike S3** — whether the userns-remap agent's `CAP_NET_RAW` can actually sniff the shared
netns (decides §2.4's transport: UDS if it can, TCP+bearer everywhere if it can't — a simplification);
and that the agent cannot reach the control socket/listener or extract the real token / model key /
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
  The pre-existing cleartext exposure of the model-switch bearer / BYOK keys in a sniffable lane is
  **closed** by moving the control plane onto the confidential transport (§2.4).

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

- **S2 — MITM proxy fidelity (implementation-gating, do first):** can a localhost MITM proxy faithfully
  carry git smart-HTTP (v2 / 100-continue / sideband) over HTTP/1.1 **and** the gh/REST/GraphQL API over
  HTTP/2, with `Authorization` overwrite? *Test:* prototype on a vetted MITM lib; run `git push` (large
  pack) + `gh pr create` + `git lfs push` through it against real github. *Kill:* a surface can't be
  carried → node-side `git http-backend` (git) / thin API reverse proxy (gh) for that surface.
- **S3 — userns-remap raw-socket sniffability (in sp-jg7x, do early):** can the userns-remapped agent
  AF_PACKET-sniff the shared netns? *Kill:* if not → drop the UDS, use TCP+bearer in all lanes.
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
- `internal/sidecar/*` + `cmd/sidecar/main.go` — the MITM forward proxy (per-spawn CA gen, JIT leaf
  certs, GitHub-host MITM + `Authorization` overwrite, CONNECT tunnel for the rest, strict upstream TLS,
  HTTP/1.1 + HTTP/2); the `GetToken` control client over the UDS/TCP transport; no auxiliary
  agent-reachable listeners.
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
