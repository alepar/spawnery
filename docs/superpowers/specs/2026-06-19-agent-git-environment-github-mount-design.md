# Agent Git Environment for GitHub Mounts

**Date:** 2026-06-19
**Status:** draft (revised twice post-roast 2026-06-19)
**Closes:** sp-7amh (agent can't set git identity) · sp-m859.1 (inject git identity from gh) · sp-n7iy (agent cannot push)
**Follow-up review:** sp-jg7x (verify agent↔sidecar isolation boundary across all pod lanes)
**Epic:** sp-m859 (MVP gaps)

## Problem

In a `github:owner/repo` mount spawn the agent owns the working tree and is promised (AGENTS.md) it
can commit and push. Today it can do neither:

1. **No commit identity (sp-7amh / sp-m859.1).** `GIT_CONFIG_GLOBAL` points at
   `/run/spawnery/secrets/github/gitconfig`, which has only a `[credential]` section (no `[user]`) and
   lives under the secrets tmpfs — owned, under userns-remap, by a host uid outside the container's
   userns map, so it appears `nobody:nogroup drwx------` and the agent can't traverse it. `git commit`
   prompts; `git config --global …` fails with `Permission denied`.
2. **No push credential (sp-n7iy).** `mintGitHubMountsAtProvision` renders the credential **node-only**
   and never an agent-facing helper; the clone's ephemeral `-c credential.helper=<node-path>` is not
   persisted, so `.git/config` has no helper and `git push` fails (`could not read Username`).

Both stem from the agent's git environment sitting under the read-only, node-owned secrets tmpfs.
`internal/storage/storage.go` already chowns mount dirs to `Manager.RemapBase()` (why `/app/repo` is
agent-writable and local commits work); the git *environment* never got that treatment, and the
credential is never delivered to the agent.

## Threat model (load-bearing — corrected across two roasts)

The agent is **untrusted**; the sidecar is the pod's **trusted credential boundary** (it already holds
the model key). The two containers share **only the netns** (Docker `NetworkMode=container:<sidecar>`;
CRI shared PodSandbox). What the agent can do to the sidecar varies by lane and must be pinned by
sp-jg7x before this is production-trustworthy:

| Lane | Agent raw sockets (netns sniff) | PID ns | IPC ns | Mount ns |
|---|---|---|---|---|
| Docker rootless + userns-remap (self-hosted) | **YES** — default caps incl `CAP_NET_RAW` | separate (no ptrace) | separate | separate |
| Docker rootful / CapDropAll (degraded) | no | separate | separate | separate |
| containerd/CRI + runsc (cloud) | **NO** — gVisor disables raw sockets absent `runsc --net-raw` (unset) | separate (MUST NOT enable shared-PID) | **shared** (CRI PodSandbox default) | separate |

Corrections the roasts forced (the first draft got these wrong):
- **IPC is shared in the CRI/runsc lane** (PodSandbox default) — so the confidentiality argument must
  NOT rest on IPC isolation. It rests on **mount-ns** (agent can't read the sidecar's FS / the token)
  and **PID-ns** (agent can't ptrace the sidecar's memory). The CRI pod **MUST NOT** set
  `shareProcessNamespace` — an explicit requirement sp-jg7x verifies.
- **The agent can passively sniff the shared netns only in the userns-remap lane** (it has
  `CAP_NET_RAW` there; runsc and CapDropAll lanes do not). This is what makes a cleartext control leg
  unsafe in *that* lane specifically, and drives the lane-aware control transport (§2.2).

Three invariants follow:
- **T1 — no cleartext secret on any leg the agent can sniff.** The existing node→sidecar control
  endpoint is plain HTTP on `0.0.0.0:<port>` over the pod IP (`manager.go:1074`, comment: *"the bearer
  token … is the access control"*). A bearer token stops unauthorized requests, not passive sniffing,
  and it is itself sniffable. In the userns-remap lane this leaks any secret (and the bearer) to a
  `CAP_NET_RAW` agent. §2.2 moves the **entire** control plane (secrets *and* the model-switch bearer)
  onto a confidential transport in that lane.
- **T2 — upstream token confidentiality rests on strict TLS verification.** With `CAP_NET_RAW` the
  agent can DNS/ARP-spoof `github.com`; the token (an `Authorization` header) leaks only if the
  sidecar completes a handshake to an attacker cert. Strict verification is a hard invariant (§2.5).
- **T3 — repo scope is defense-in-depth, not the primary boundary.** Egress is default-*allow*, so a
  leaked token is directly usable; scope (§2.4) narrows blast radius only while T1+T2 keep the token
  unleaked. The AS **MUST** mint a **repo-scoped** token so the residual blast radius is one repo.

## Key decisions

Two independent halves, **both shipping now** (per owner direction 2026-06-19). The T1 control-channel
hardening ships **with** the push half, not deferred.

1. **Identity half** — writable agent-owned global git config, `[user]` seeded from the linked GitHub
   identity (carried in the AS mint response).
2. **Push half** — a **sidecar-hosted, repo-scoped git smart-HTTP reverse proxy** that injects the
   credential on the wire (agent holds no token/helper/config). The credential flow is **pull-based**:
   the node is a credential *server* over a lane-appropriate confidential control transport, and the
   proxy pulls a guaranteed-fresh token per git operation. Pull-not-push is what dissolves the
   ordering / refresh-delivery / restart-replay / mid-push-expiry / re-mint-routing problems the
   roasts surfaced.

---

## Section 1 — Identity half (sp-7amh + sp-m859.1)

### 1.1 Writable agent-owned global git config (all spawns)
A per-spawn **`git-env` dir**, host-side under the node data root, **chowned to `Manager.RemapBase()`**
(reusing `storage.go`'s chown + EPERM/0777 degraded fallback), bind-mounted at `/run/spawnery/git-env`
— a **sibling of, not under,** the read-only secrets mount. Agent env:
- `GIT_CONFIG_GLOBAL=/run/spawnery/git-env/gitconfig` (writable; `git config --global` works);
- `GIT_CONFIG_NOSYSTEM=1` (neutralize any `/etc/gitconfig` `url.insteadOf`/`credential.helper` in the
  agent image that could bypass the loopback origin or re-introduce prompts);
- `GIT_TERMINAL_PROMPT=0` and `GIT_ASKPASS=/bin/false` (un-credentialed ops fail fast, never hang).

Applies to **every** spawn (scratch included) — a writable global config is harmless and lets `git
init`/commit work everywhere.

### 1.2 Seed `[user]` identity (per account, not per mount)
A spawn's owner links one GitHub account, so a single global `[user]` is correct even for multi-github
spawns. Node renders into `<git-env>/gitconfig`:
```
[user]
	name = <login>
	email = <id>+<login>@users.noreply.github.com
```
**Fallback (roast: login-xor-id, org/app links):** the canonical noreply form requires **both** login
and id; if **either** is missing, or the link is an org/app-installation link with no user login, seed
`name = <account handle or "spawnery">`, `email = <accountID>@users.noreply.spawnery.local`. Identity
is best-effort and **never fails provisioning**.

### 1.3 Login source — AS mint response
`MintInitial` is extended to return `login` + numeric `id` alongside token/expiry (the AS knows them
for the `gh:<accountID>` link; same authenticated node↔AS channel — containment c unchanged).
`mintGitHubMountsAtProvision` threads them into the render.

---

## Section 2 — Push half (sp-n7iy): sidecar git-proxy, pull-based credential

### 2.1 The proxy
The sidecar runs a git smart-HTTP reverse proxy on a **fixed loopback port** `SIDECAR_GIT_ADDR`
(fixed ⇒ the `origin` rewrite is computable at provision and stable across resume). It is **path-routed
per mount** (route key = mount name) to support N `github:` mounts:
```
origin (mount "src", repo o/r) = http://127.0.0.1:<gitport>/m/src/o/r
```
Per request the proxy:
- **canonicalizes** the path (reject `.`/`..`, `%2e`/`%2f` encodings, duplicate slashes) before
  matching;
- matches the **exact** smart-HTTP endpoints for that route's repo and nothing else:
  `…/o/r[.git]/info/refs?service=git-(upload|receive)-pack`, `…/git-upload-pack`,
  `…/git-receive-pack`. Everything else — the **LFS batch path**, dumb-HTTP fallbacks, the GitHub
  API — is **403** (kills the token-pivot the roast flagged);
- rewrites the HTTP **`Host` → `github.com`** and sets outbound TLS **`ServerName=github.com`**;
- forwards **`Git-Protocol`** (no silent v2 downgrade) and relays **`Expect: 100-continue`** (git
  pushes large packs with it — the proxy must pass github's interim 100/final status back before the
  pack streams, or the push stalls);
- injects `Authorization: Basic base64("x-access-token:" + token)` (token from §2.3);
- **streams half-duplex** (git smart-HTTP is full-request-then-response, not bidirectional): implement
  with `httputil.ReverseProxy`, `FlushInterval = -1`, correct hop-by-hop header handling, and an
  upstream client with **`CheckRedirect = ErrUseLastResponse`** so a 30x is **not** auto-followed
  (auto-follow would replay `Authorization` to the redirect target and skip the allow-list re-check).
  The proxy then rewrites a `Location: https://github.com/o/r…` back to the loopback route. A redirect
  to a **different** repo (renamed/transferred) is surfaced as a clean error; recovery is re-provision
  / relink (documented edge, §2.6).

### 2.2 Control transport — lane-aware, confidential, pathname-only
The node↔sidecar control plane (token service **and** the model-switch) uses a transport the agent
cannot sniff:
- **userns-remap lane (agent has `CAP_NET_RAW`):** a **unix-domain socket on a bind mount shared only
  between the node (host) and the sidecar container** (host `…/<id>/control.sock` → sidecar
  `/run/spawnery/control.sock`). The agent does not mount that path and is on a different mount ns ⇒
  the socket is invisible and nothing crosses the netns. **Ownership (roast blocker):** the **node**
  creates the socket (host uid 1001) and `chmod 0666`; the dir is private to node+sidecar so 0666 is
  safe, and a 0666 socket is connectable by the userns-remapped sidecar — this is the exact
  chown/mode treatment §1.1 applies to git-env, applied here too. **Pathname socket only** — abstract
  (`@`) sockets are netns-scoped and agent-reachable, so they are **prohibited** for any sidecar
  control listener.
- **runsc and CapDropAll lanes (agent has no raw sockets — cannot sniff):** the existing **TCP +
  bearer** control endpoint over the pod IP is acceptable; confidentiality holds because the agent
  cannot observe the netns. This avoids broadening gVisor isolation with `--host-uds` (which defaults
  to `none` and would weaken the sandbox). The lane's raw-socket-absence is exactly what sp-jg7x pins.

The transport choice is driven by the node's known lane (`UsernsMode`/`ContainerRuntime`), the same
inputs that already select cap policy.

### 2.3 Pull-based credential flow (dissolves the push-model blockers)
The node is a **credential server**; the sidecar proxy is a client. Before forwarding each git
operation the proxy calls, over the control transport:
```
GetToken(spawnID, route, minRemaining) → { owner, repo, token }   // token valid ≥ minRemaining
```
The **node** owns everything stateful: it resolves `route → (spawnID, mount, secretID, generation)`
from the spawn's mount bindings (recoverable from durable store, so a **node restart** re-derives it),
consults/advances the existing refresher, and **mints a fresh token if the current one has
< minRemaining left**, returning one valid for the whole operation. The sidecar keeps only a short
in-memory cache keyed by route, invalidated on upstream 401.

This single change resolves the roast cluster:
- **Mid-push expiry race:** the proxy fetches a token guaranteed valid ≥ `minRemaining` (e.g. 5 min)
  *before* streaming the `git-receive-pack` body, so the token cannot expire mid-POST. **No body
  replay is needed or attempted** (the unbuffered-stream-vs-retry contradiction is gone).
- **Refresh delivery / token-discard:** moot — the node returns the current token on demand and mints
  when stale; nothing depends on the proactive `Tick` having *delivered* a token. The proactive
  refresher remains only as a warming/scheduling optimization.
- **Sidecar restart / node restart:** the sidecar re-pulls on the next op; the node re-derives route
  state from durable mount bindings. No replay path to build, no token retained on the node beyond a
  mint's lifetime.
- **Re-mint routing:** the `route → secretID/generation` map lives in the node, which has it; the
  sidecar passes only `route`.
- **Startup ordering:** lazy pull removes the "push token before StartAgent" requirement. The only
  ordering needed is **sidecar-ready before agent-start**: the sidecar (Phase 1) must have **bound the
  gitport and the control listener before the agent (Phase 2) starts** — enforced by a sidecar
  readiness probe in `StartPod` (this also prevents the agent **port-squatting** the fixed gitport,
  since the sidecar already owns it when the agent starts).

**Error semantics (roast: 401≠403):**
- **401** (expired/invalid token) — invalidate cache, `GetToken` fresh, retry the idempotent
  `info/refs` GET once; a body POST is not replayed (and with `minRemaining` should not 401 mid-op). A
  persistent 401 surfaces cleanly.
- **403** (permission, branch protection, secondary rate-limit) — a new token does **not** help;
  **pass through verbatim** (including `Retry-After`) so the agent sees the real cause; **no re-mint**.
- **Re-mint rate-limit:** the node bounds mints per route (token-bucket) so a compromised agent
  spamming 401-inducing ops cannot drive unbounded AS/GitHub mint round-trips. `GetToken` forces a
  genuinely fresh request id when minting (never reuses a dedup id that would return the expired
  token).

### 2.4 Repo scoping & accepted blast radius (defense-in-depth)
Per-route exact-path allow-list + Host pin + a **repo-scoped minted token (T3 requirement)** bound the
blast radius to the mount's `owner/repo` even if the proxy is bypassed. **Accepted, documented:**
within the allowed repo, `git-receive-pack` permits force-push / branch deletion / history rewrite —
the agent owns the tree and is *meant* to push, so this is bounded by GitHub **branch protection**, not
by Spawnery. Tightening agent→github **direct** egress (so a leaked token isn't directly usable) is a
tracked follow-up; the **sidecar→github:443** leg, conversely, **MUST** be permitted by the egress
floor (the sidecar is now the github client) — a stated floor rule (default-allow today; explicit when
deny-by-default lands).

### 2.5 Upstream TLS — hard invariant (T2)
The sidecar→github client **MUST** use stock TLS with default verification, `ServerName="github.com"`;
**MUST NOT** set `InsecureSkipVerify`, add a custom CA pool, or honor `HTTP(S)_PROXY`/`NO_PROXY` env.
The sidecar image **MUST** ship a clean, current CA bundle. **Spike S1:** evaluate pinning GitHub's
cert chain / IP set as extra hardening.

### 2.6 `origin` rewrite — placement & durability
An explicit **ensure-origin step** (`git remote set-url origin http://127.0.0.1:<gitport>/m/<route>/o/r`),
run by the node for each github mount on **both create and resume**, **after** mount restore and
**before** `StartAgent` — *not* in `storage.GitHub.Prepare`'s clone-only branch (which returns early on
restore and has no port). Idempotent; the fixed port makes it stable across resume.

**Documented constraints / out of MVP scope (roast — accepted):**
- The agent owns `.git/config`; if a tool runs `git remote set-url origin <canonical https>` or adds a
  second remote, push breaks (no agent-side credential). Known limitation; `git remote -v` shows the
  loopback URL by design.
- **Out of scope (explicit, tracked as follow-ups):** Git **LFS** (separate batch API + object host),
  **private submodules** (other repos), the **`gh` CLI**, any extra remote / hardcoded
  `https://github.com` URL. These bypass the single-origin proxy and will fail auth.
- **Renamed/transferred repo:** surfaced error → recover by re-provision/relink (no auto-rename
  handling in MVP).

---

## Section 3 — Boundary review & per-lane posture (sp-jg7x, follow-up)

sp-jg7x formally proves, per lane: mount-ns + PID-ns isolation (no FS read, no ptrace; CRI pods do not
enable shared-PID), the raw-socket capability matrix above (so the §2.2 lane-aware transport rests on
verified facts, not assumption), and that the agent cannot reach the control socket or extract the
token/model key. It is a follow-up, **not** a merge gate — but the **T1 hardening (§2.2) and strict TLS
(§2.5) ship now**, so the cleartext-delivery hole is closed at merge; sp-jg7x verifies the residuals and
defines the node-only-proxy fallback for any failing lane.

## Section 4 — Containment reconciliation

- **(b) token never in the agent container** — preserved (it lives in the sidecar; the agent pushes
  through the proxy and never holds it).
- **(a) refresh material AS-custodial** — unchanged (only short-lived access tokens are minted/served).
- **(c) token never relayed by the CP** — unchanged. The pull flow is node↔sidecar (control socket) and
  node↔AS (mint); the CP is not in the credential path.
- **Secrets tmpfs stays agent-unreadable** — unchanged; the new agent-writable surface is the separate
  git-env dir.
- **Blast radius:** co-locating the github token with the model key in the sidecar is an accepted,
  reviewed extension of the sidecar's trusted-proxy role (sp-jg7x covers the boundary). The pre-existing
  cleartext exposure of the model-switch bearer / BYOK keys in the userns-remap lane is **closed here**
  by moving the whole control plane onto the confidential transport (§2.2).

## Section 5 — Testing

**Identity (hermetic + e2e):** config rendered with seeded `[user]`; dir chowned to RemapBase; writable;
fallback identity on login-xor-id and org/app links; `GIT_CONFIG_NOSYSTEM`/`GIT_TERMINAL_PROMPT=0` set;
e2e in-spawn `git commit` records the GitHub author with no prompt.

**Push (e2e):** clone → commit → **push** → suspend → resume → push, both clients; **multi-github** spawn
(two routes, two tokens, no cross-talk); proxy rejects a different `owner/repo`, the LFS batch path, and
traversal/encoded variants (403); **protocol-v2** push and a **large/sideband + `Expect: 100-continue`**
pack succeed through the streaming proxy; a forced near-expiry token triggers the `GetToken(minRemaining)`
fresh-mint path and push still succeeds; a 403 (e.g. protected branch) surfaces verbatim without a
re-mint.

**Control transport (hermetic + lane):** the UDS is `0666`, pathname (not abstract), connectable by the
remapped sidecar uid, invisible from the agent mount ns; sidecar-ready-before-agent ordering holds; the
agent cannot bind the gitport.

**Boundary (sp-jg7x):** token absent from agent FS/env; control socket unreachable from the agent;
per-lane raw-socket + PID/IPC matrix.

All integration/e2e tests are build-tagged and fail (never skip) when their lane dep is down.

## Recommended spikes

- **S1 — upstream TLS pinning** *(hardening, optional):* does pinning GitHub's cert chain / IP set beat
  default verification against a spoofing `CAP_NET_RAW` agent? *Test:* pinned-pool client + spoofed-DNS
  negative. *Kill:* breaks on cert rotation ops can't track → strict default verification only.
- **S2 — git smart-HTTP fidelity through re-origination** *(implementation-gating, do first):* do
  protocol-v2, `Expect: 100-continue`/chunked `git-receive-pack`, sideband-64k flushing, and
  `Location`/redirect handling survive the plain-http→TLS re-originating proxy? *Test:* standalone proxy
  prototype, push a repo with a large pack + v2 against real github. *Kill:* a protocol case can't be
  made faithful → fall back to a node-side full git-http backend (run git's `http-backend`/`upload-pack`
  node-side and proxy bytes) or the node-only proxy. **This gates the push half during implementation,
  not post-merge.**

## Implementation sketch (files)

- `internal/spawnlet/secrets.go` / `manager.go` — git-env dir (chown RemapBase, bind
  `/run/spawnery/git-env`); repoint `GIT_CONFIG_GLOBAL`, set `GIT_CONFIG_NOSYSTEM`/`GIT_TERMINAL_PROMPT`/
  `GIT_ASKPASS`; create the control UDS (node-owned, `0666`, pathname) + bind mount in the userns-remap
  lane; lane-select the control transport; sidecar-readiness gate for gitport + control listener;
  ensure-origin step (create+resume, pre-StartAgent).
- `internal/githubcred/render.go` — identity-only gitconfig with the fallback rules; keep node
  credential render for the node-side clone.
- `internal/node/*` — node credential server: `GetToken(spawnID, route, minRemaining)` resolving
  `route→secretID/generation` from mount bindings, minting fresh when stale, rate-limited; thread
  login/id from the mint response into identity.
- AS mint endpoint + mint client — add `login`+`id`; ensure **repo-scoped** token minting (T3).
- `internal/sidecar/*` + `cmd/sidecar/main.go` — the path-routed git smart-HTTP reverse proxy on
  `SIDECAR_GIT_ADDR` (canonicalize, exact allow-list, Host/SNI rewrite, `Git-Protocol` + 100-continue,
  `FlushInterval=-1`, `ErrUseLastResponse`+`Location` rewrite, strict upstream TLS, 401 vs 403); the
  control client (`GetToken`) over the UDS/TCP transport; move the model-switch onto the same transport
  in the sniffable lane.
- `internal/storage/github.go` — origin URL helper (fixed gitport, per-mount route); ensure-origin call
  stays in the manager flow.
- proto/gen — mint response (`login`,`id`); control messages (`GetToken` req/resp, per-route); `make gen`.
  `proto/`-touching work serialized ahead of consumers.

## Post-Implementation Notes

*As this design is implemented and iterated on — bug fixes, adjustments, anything that diverged from the
assumptions above — append a dated note here, whether or not a formal debugging skill was used.*

- **2026-06-19 (roast r1 BLOCK):** corrected the control channel (cleartext on the pod IP, not
  unix-socket-hardened) and that proactive refresh never reached the sidecar; added fixed gitport +
  ensure-origin placement, per-mount routing, exact allow-list, Host/SNI rewrite, streaming, strict
  TLS, identity fallbacks, runsc net-raw correction, LFS/submodules out of scope.
- **2026-06-19 (roast r2 BLOCK):** flipped the credential flow from **push to pull** (node = credential
  server, proxy fetches a `minRemaining`-fresh token per op) — this dissolved the unbuffered-stream-vs-
  retry contradiction, refresh-delivery/token-discard gap, sidecar/node-restart replay, re-mint routing,
  and startup-ordering blockers. Made the control transport **lane-aware** (UDS in the raw-sniffable
  userns-remap lane with node-owned `0666` pathname socket; existing TCP+bearer in runsc/CapDropAll where
  the agent provably can't sniff) — resolving the userns socket-ownership bug and the gVisor `--host-uds`
  gap without weakening the sandbox. Corrected the threat model (IPC **shared** in CRI; rest on mount+PID
  isolation, no shared-PID); split 401 vs 403; added re-mint rate-limit, repo-scoped-token requirement,
  `GIT_CONFIG_NOSYSTEM`, `Expect: 100-continue`, `ErrUseLastResponse`, port-squat ordering, sidecar→github
  egress-floor rule, abstract-socket prohibition; moved the model-switch bearer onto the confidential
  transport; reframed S2 as an implementation-gating spike.
