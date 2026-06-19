# Agent Git Environment for GitHub Mounts

**Date:** 2026-06-19
**Status:** draft (revised ×3 post-roast 2026-06-19)
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

## Threat model (load-bearing — corrected across three roasts)

The agent is **untrusted**; the sidecar is the pod's **trusted credential boundary** (it already holds
the model key). The two containers share **only the netns** (Docker `NetworkMode=container:<sidecar>`;
CRI shared PodSandbox). What the agent can do to the sidecar varies by lane and must be pinned by
sp-jg7x before this is production-trustworthy:

| Lane | Agent raw-socket sniff of shared netns | PID ns | IPC ns | Mount ns |
|---|---|---|---|---|
| Docker rootless + userns-remap (self-hosted) | **maybe** — default caps incl `CAP_NET_RAW`, but in a *user* namespace `CAP_NET_RAW` only authorizes the devices that namespace owns (sp-jg7x spike S3) | separate (no ptrace) | separate | separate |
| Docker rootful / CapDropAll (degraded) | no | separate | separate | separate |
| containerd/CRI + runsc (cloud) | **no** — gVisor disables raw sockets absent `runsc --net-raw` (unset) | separate (MUST NOT enable shared-PID) | **shared** (CRI PodSandbox default) | separate |

Corrections the roasts forced:
- **IPC is shared in the CRI/runsc lane** — the confidentiality argument rests on **mount-ns** (agent
  can't read the sidecar FS / token) and **PID-ns** (no ptrace of sidecar memory), NOT IPC. The CRI pod
  **MUST NOT** set `shareProcessNamespace` (sp-jg7x verifies).
- **Whether the userns-remap agent can actually sniff the shared netns is unproven** (spike S3): a
  userns `CAP_NET_RAW` is scoped to namespace-owned devices, so the agent may be unable to sniff even
  there. If S3 shows it cannot, the lane-aware transport below collapses to "TCP+bearer everywhere" and
  the UDS is unnecessary — a simplification, not a new risk.

Three invariants:
- **T1 — no cleartext secret on any leg the agent can sniff.** The existing node→sidecar control
  endpoint is plain HTTP on `0.0.0.0:<port>` over the pod IP (`manager.go:1074`, comment: *"the bearer
  token … is the access control"*). A bearer stops unauthorized requests, not passive sniffing, and is
  itself sniffable. §2.2 moves the **entire** control plane (secrets *and* the model-switch) onto a
  confidential transport in any lane where S3 shows the agent can sniff.
- **T2 — upstream token confidentiality rests on strict TLS verification.** A sniffing/spoofing agent
  can DNS/ARP-spoof `github.com`; the token leaks only if the sidecar completes a handshake to an
  attacker cert. Strict verification is a hard invariant (§2.5).
- **T3 — token blast radius is the installation selection, not one repo (corrected).** The AS mints an
  **OAuth user-to-server access token** via `RefreshUserAccessToken`, which has **no repository
  parameter** — GitHub does not support per-repo scoping on this credential type (per-repo scoping
  needs *installation* access tokens, a different app-JWT flow). This matches the codebase's
  **already-spiked containment-e decision**: `repository_id` is audit/expected-target only, **never a
  scope reducer**; the blast radius of a leaked token is whatever the user's App installation selection
  covers. So repo narrowing is **in-band only** (the proxy allow-list, §2.4) — a leaked token (a
  T1/T2 failure) carries full installation scope. T1+T2 are therefore the *primary* boundary; §2.4 is
  defense-in-depth. (This corrects the prior draft, which wrongly demanded an infeasible repo-scoped
  mint and re-litigated containment-e.)

## Key decisions

Two independent halves, **both shipping now** (per owner direction 2026-06-19). T1 hardening ships
**with** the push half.

1. **Identity half** — writable agent-owned global git config; `[user]` seeded from the linked GitHub
   identity (carried in the AS mint response).
2. **Push half** — a **sidecar-hosted git smart-HTTP reverse proxy** that injects the credential on the
   wire (agent holds no token/helper/config). The credential flow is **pull-based**: the node is a
   credential *server* over a lane-appropriate confidential control transport, and the proxy pulls a
   guaranteed-fresh token per git operation. Pull-not-push dissolves the ordering / refresh-delivery /
   restart-replay / mid-push-expiry / re-mint-routing problems.

---

## Section 1 — Identity half (sp-7amh + sp-m859.1)

### 1.1 Writable agent-owned global git config (all spawns)
A per-spawn **`git-env` dir**, host-side under the node data root, **chowned to `Manager.RemapBase()`**
(reusing `storage.go`'s chown + EPERM/0777 degraded fallback), bind-mounted at `/run/spawnery/git-env`
— a **sibling of, not under,** the read-only secrets mount. Agent env:
- `GIT_CONFIG_GLOBAL=/run/spawnery/git-env/gitconfig` (writable);
- `GIT_CONFIG_NOSYSTEM=1` (neutralize any `/etc/gitconfig` `url.insteadOf`/`credential.helper` in the
  agent image that could bypass the loopback origin or re-introduce prompts);
- `GIT_TERMINAL_PROMPT=0` and `GIT_ASKPASS=/bin/false` (un-credentialed ops fail fast).

Applies to **every** spawn (scratch included).

### 1.2 Seed `[user]` identity (per account)
One linked GitHub account per owner ⇒ a single global `[user]` is correct even for multi-github spawns:
```
[user]
	name = <login>
	email = <id>+<login>@users.noreply.github.com
```
**Fallback (login-xor-id, org/app links):** the canonical form needs **both** login and id; if **either**
is missing, or the link is an org/app-installation link with no user login, seed
`name = <account handle or "spawnery">`, `email = <accountID>@users.noreply.spawnery.local`. Best-effort;
**never fails provisioning**.

### 1.3 Login source — AS mint response
`MintInitial` returns `login` + numeric `id` alongside token/expiry (the AS knows them for the
`gh:<accountID>` link; same authenticated channel — containment c unchanged). `mintGitHubMountsAtProvision`
threads them into the render.

---

## Section 2 — Push half (sp-n7iy): sidecar git-proxy, pull-based credential

### 2.1 The proxy
The sidecar runs a git smart-HTTP reverse proxy on a **fixed loopback port** `SIDECAR_GIT_ADDR`
(fixed ⇒ origin is computable at clone time and stable across resume). It is **path-routed per mount**
(route key = mount name) for N `github:` mounts:
```
origin (mount "src", repo o/r) = http://127.0.0.1:<gitport>/m/src/o/r
```
Per request the proxy:
- **canonicalizes** the path (reject `.`/`..`, `%2e`/`%2f`, duplicate slashes) before matching;
- matches the **exact** smart-HTTP endpoints for that route's repo and nothing else
  (`…/o/r[.git]/info/refs?service=git-(upload|receive)-pack`, `…/git-upload-pack`,
  `…/git-receive-pack`). Everything else — the **LFS batch path**, dumb-HTTP, the GitHub API — is
  **403**;
- **scrubs inbound client headers** before injecting credentials: strip any client-supplied
  `Authorization`, `Proxy-Authorization`, `Cookie`, and `X-Forwarded-*` (a credential-injecting proxy
  in front of an untrusted client must allow-list, not pass through);
- rewrites the HTTP **`Host` → `github.com`** and sets outbound TLS **`ServerName=github.com`**;
- forwards **`Git-Protocol`** (no silent v2 downgrade);
- injects `Authorization: Basic base64("x-access-token:" + token)` (token from §2.3);
- relays the response body **with prompt flushing** for sideband-64k progress, and handles a `30x`
  **without auto-following** (an upstream client with `CheckRedirect = ErrUseLastResponse`), rewriting
  a `Location: https://github.com/o/r…` back to the loopback route; a redirect to a *different* repo
  (rename/transfer) is a surfaced error (recovery = re-provision/relink, §2.6).

**Implementation reality (roast — S2 is load-bearing):** Go's `net/http` server **auto-acknowledges
`Expect: 100-continue`** to the client on first body `Read`, and `httputil.ReverseProxy` cannot
faithfully relay Expect handshakes or guarantee sideband flush semantics for a large chunked
`git-receive-pack`. The proxy implementation is therefore **determined by spike S2**, not assumed:
candidate forms are a **byte-level relay** (hijack the connection, splice after auth) or running git's
own **`http-backend`/`upload-pack`/`receive-pack` node-side** and proxying bytes. Vanilla
`ReverseProxy` is a starting point only. S2 **gates the proxy approach** (see Spikes); its kill
criterion is the node-side git-http-backend fallback.

### 2.2 Control transport — lane-aware, confidential, pathname-only
The node↔sidecar control plane (the `GetToken` service of §2.3 **and** the model-switch) uses a
transport the agent cannot sniff. Lane selection follows the node's known `UsernsMode`/`ContainerRuntime`
(and spike S3's verdict on whether the userns-remap agent can sniff at all):

- **Where the agent can sniff (userns-remap, pending S3):** a **unix-domain socket on a bind mount
  shared only between the node (host) and the sidecar container** (host `…/<id>/control.sock` → sidecar
  `/run/spawnery/control.sock`). The agent does not mount that path and is on a different mount ns ⇒
  invisible, and nothing crosses the netns. **Ownership (roast blockers — both the dir and the socket):**
  the **node** creates the dir mode **`0711`** and the socket mode **`0666`**, both owned by the host
  node uid (1001). `0711` gives the userns-remapped sidecar uid the **traverse** bit it needs (a `0700`
  dir would reproduce the §1.1 traverse-denied bug); `0666` lets the remapped sidecar **connect**; and
  the host node uid owns both, so the node can serve/connect. The dir is private to node+sidecar (not
  mounted into the agent), so world-rwx on these two inodes exposes nothing to the agent. The node is
  the **server**; the sidecar connects. **Pathname socket only** — abstract (`@`) sockets are
  netns-scoped and agent-reachable, so they are **prohibited** for any sidecar/node control listener.
- **Where the agent provably cannot sniff (runsc, CapDropAll, or userns-remap if S3 clears it):** a
  **node-hosted TCP `GetToken` listener** (the pull model needs node-as-server, the *reverse* of the
  existing sidecar-as-server model-switch endpoint). The node binds it on the **pod-bridge gateway
  address** reachable from the pod; its address is passed to the sidecar via sidecar env
  (`SPAWNERY_NODE_CONTROL_URL`), and it is **bearer-gated with the bearer in sidecar env only**
  (`SPAWNERY_NODE_CONTROL_TOKEN`), withheld from the agent env. The listener is reachable by the agent
  on the shared netns, so the bearer (not the bind scope) is the access control — sound **only because**
  the agent cannot sniff the bearer in these lanes (no raw sockets). The model-switch may stay on its
  existing endpoint in these lanes (non-sniffable); in a sniffable lane it moves to the UDS.

**No agent-reachable auxiliary listeners (roast):** the sidecar **MUST NOT** expose any other TCP
listener on the shared netns (pprof/metrics/health/debug) — such a port is connectable by the agent
with a plain `connect()` (no caps needed). Any such surface binds the UDS or is disabled.

### 2.3 Pull-based credential flow
The node is a **credential server**; the sidecar proxy is a client. Before forwarding each git op the
proxy calls, over the control transport:
```
GetToken(spawnID, route, minRemaining) → { owner, repo, token, accessExpiresAtUnix }
```
The **node** owns all state: it resolves `route → (spawnID, mount, secretID, generation)` from the
spawn's mount bindings (recoverable from durable store, so a **node restart** re-derives it), and
**mints a fresh token if the current one has < minRemaining left**, returning one valid for the
operation plus its **expiry** (so the sidecar's route-keyed cache can honor `minRemaining` itself). The
sidecar caches per route, invalidated on upstream 401.

This dissolves the roast cluster:
- **Mid-push expiry:** the proxy fetches a token valid ≥ `minRemaining` *before* streaming the
  `git-receive-pack` body → it cannot expire mid-POST, so no body replay is needed. **Residual
  (accepted):** a single push that *streams longer than `minRemaining`* would still 401 mid-POST with
  no replay; set `minRemaining` to the token's near-full headroom (e.g. ≥ 50 min for a 1h token) so
  only a multi-hour single push hits it — a surfaced error, documented, not silently corrupting.
- **Refresh / token-discard:** moot — the node mints on demand; nothing relies on the proactive `Tick`
  *delivering* a token (it remains a warming optimization).
- **Sidecar/node restart:** sidecar re-pulls; node re-derives from durable bindings. No replay path.
- **Re-mint routing:** the `route → secretID/generation` map lives in the node.
- **Startup ordering:** lazy pull removes "push before StartAgent"; the only requirement is
  **sidecar-ready before agent-start** (gitport + control client wired), enforced by a `StartPod`
  readiness probe — which also stops the agent **port-squatting** the fixed gitport (the sidecar owns
  it first). For a **CRI sidecar app-container crash-restart** within a live PodSandbox (agent + netns
  survive), the node must re-assert the readiness/port ownership or fail the spawn, since the restart
  reopens the squat window.

**Error semantics (401≠403; defined failure contract):**
- **401** — invalidate cache, `GetToken` fresh, retry the idempotent `info/refs` GET once; a body POST
  is not replayed. Persistent 401 surfaces cleanly.
- **403** — permission/branch-protection/secondary-rate-limit; a new token doesn't help → **pass
  through verbatim** (incl. `Retry-After`); **no re-mint**.
- **`GetToken` node/AS failures** (AS unreachable, mint `NotFound`, `RelinkRequired`/broken chain,
  rate-limited, installation/grant revoked mid-session) → the node returns a **typed error**; the proxy
  maps it to a distinct, non-retrying upstream status with a short diagnostic body so the agent's git
  (running `GIT_ASKPASS=/bin/false`) fails fast with a comprehensible message rather than an auth
  prompt loop.
- **Re-mint rate-limit:** the node bounds mints per route (token-bucket); `GetToken` uses a fresh
  request id when minting (never a dedup id that would return the already-expired token).

### 2.4 Repo scoping & accepted blast radius (defense-in-depth)
The per-route exact-path allow-list + Host pin narrow the agent's **in-band** access to the mount's
`owner/repo`. Per **T3 (corrected)** the minted token's *actual* scope is the user's App **installation
selection** (`repository_id` is audit-only, never a scope reducer — containment e); a leaked token
(T1/T2 failure) therefore carries full installation scope, so T1+T2 are the primary boundary. **Accepted,
documented:** within the allowed repo, `git-receive-pack` permits force-push / branch deletion / history
rewrite — bounded by GitHub **branch protection**, not Spawnery. The **sidecar→github:443** leg **MUST**
be permitted by the egress floor (the sidecar is the github client now). Tightening agent→github
**direct** egress is noted but **infeasible to scope per-host with the current source-IP floor** — a
tracked future item, not relied on here.

### 2.5 Upstream TLS — hard invariant (T2)
The sidecar→github client **MUST** use stock TLS with default verification, `ServerName="github.com"`;
**MUST NOT** set `InsecureSkipVerify`, add a custom CA pool, or honor `HTTP(S)_PROXY`/`NO_PROXY`. The
sidecar image **MUST** ship a clean, current CA bundle. **Spike S1:** evaluate cert/IP pinning.

### 2.6 `origin` rewrite — placement & durability (corrected: node-side clone, pre-chown)
`origin` is set **inside the node-side clone** (`storage.GitHub.Prepare`), **before** the tree is
chowned to `RemapBase` — at that moment `.git/config` is node-owned (uid 1001) and writable, avoiding
the §1.1 wall that a *post-chown* `git remote set-url` by the node would hit. The URL uses the fixed
gitport (`http://127.0.0.1:<gitport>/m/<route>/o/r`). On **resume** no clone runs and the journaled
`.git/config` already carries this origin (stable because the port is fixed) — no node write against the
chowned tree is needed.

**Documented constraints / out of MVP scope (accepted, tracked as follow-ups):**
- The agent owns `.git/config`; a tool that rewrites `origin` to the canonical https URL or adds a
  second remote breaks push. Known limitation; `git remote -v` shows the loopback URL by design.
- **Git LFS** (separate batch API + object host — common for asset/ML repos), **private submodules**
  (other repos; the node-side recursive clone will also fail to auth them), the **`gh` CLI**, and any
  extra remote / hardcoded `https://github.com` URL **bypass the single-origin proxy and fail auth**.
  Explicitly not promised by this slice.
- **Renamed/transferred repo:** surfaced error → recover by re-provision/relink.

---

## Section 3 — Boundary review & per-lane posture (sp-jg7x, follow-up)

sp-jg7x formally proves, per lane: mount-ns + PID-ns isolation (no FS read, no ptrace; CRI pods do not
enable shared-PID), and — **spike S3** — whether the userns-remap agent's `CAP_NET_RAW` can actually
sniff the shared netns (which decides §2.2's transport: UDS only if it can; TCP+bearer everywhere if it
cannot). It also verifies the agent cannot reach the control socket/listener or extract the token/model
key. Follow-up, **not** a merge gate — but the **T1 hardening (§2.2)** and **strict TLS (§2.5)** ship
now, so the cleartext-delivery hole is closed at merge for the sniffable lane; the node-only-proxy
fallback covers any failing lane.

## Section 4 — Containment reconciliation

- **(b) token never in the agent container** — preserved (lives in the sidecar; agent pushes through the
  proxy, never holds it).
- **(a) refresh material AS-custodial** — unchanged.
- **(c) token never relayed by the CP** — unchanged (pull flow is node↔sidecar + node↔AS; CP not in the
  credential path).
- **(e) `repository_id` is audit-only, never a scope reducer** — **now honored** (the prior draft
  violated it with the infeasible repo-scoped-mint MUST; T3 corrected).
- **Secrets tmpfs stays agent-unreadable** — unchanged (the new writable surface is the separate
  git-env dir).
- **Blast radius:** co-locating the github token with the model key in the sidecar is an accepted,
  reviewed extension of the sidecar's trusted-proxy role (sp-jg7x covers the boundary). The pre-existing
  cleartext exposure of the model-switch bearer / BYOK keys in a sniffable lane is **closed here** by
  moving the control plane onto the confidential transport (§2.2).

## Section 5 — Testing

**Identity (hermetic + e2e):** config rendered with seeded `[user]`; dir chowned to RemapBase; writable;
fallback on login-xor-id and org/app links; `GIT_CONFIG_NOSYSTEM`/`GIT_TERMINAL_PROMPT=0` set; e2e
in-spawn `git commit` records the GitHub author with no prompt.

**Push (e2e):** clone → commit → **push** → suspend → resume → push, both clients; **multi-github** spawn
(two routes/tokens, no cross-talk); proxy rejects a different `owner/repo`, the LFS batch path, and
traversal/encoded variants (403); inbound client `Authorization`/`Cookie` are scrubbed; **protocol-v2**
push and a **large/sideband** pack succeed; a near-expiry token triggers the `GetToken(minRemaining)`
fresh-mint path; a 403 (protected branch) surfaces verbatim without a re-mint; a `GetToken` node error
yields a fast, comprehensible git failure (no prompt loop).

**Control transport (hermetic + lane):** dir `0711` / socket `0666` pathname (not abstract), traversable
+ connectable by the remapped sidecar uid, invisible from the agent mount ns; in the TCP lane the bearer
is absent from agent env; sidecar-ready-before-agent ordering holds (incl. CRI app-restart); no
auxiliary agent-reachable sidecar listener exists.

**Boundary (sp-jg7x):** token absent from agent FS/env; control channel unreachable from the agent;
per-lane raw-socket (S3) + PID/IPC matrix.

All integration/e2e tests are build-tagged and fail (never skip) when their lane dep is down.

## Recommended spikes

- **S1 — upstream TLS pinning** *(hardening, optional):* does pinning GitHub's cert chain / IP set beat
  default verification against a spoofing agent? *Kill:* breaks on cert rotation ops can't track →
  strict default verification only.
- **S2 — git smart-HTTP fidelity through re-origination** *(implementation-gating, do first):* can
  protocol-v2, `Expect: 100-continue`/chunked `git-receive-pack`, sideband-64k flushing, and
  `Location`/redirect handling be carried faithfully given Go's `net/http` auto-100-continue and
  `ReverseProxy` limits? *Test:* prototype a byte-level relay (and a node-side `git http-backend`
  variant), push a large-pack v2 repo against real github. *Kill:* no faithful relay → ship the
  node-side git-http-backend backend (or the node-only proxy). **Gates the proxy during implementation.**
- **S3 — userns-remap raw-socket sniffability** *(in sp-jg7x; do early):* can the userns-remapped agent
  actually AF_PACKET-sniff the shared netns, given `CAP_NET_RAW` in a user namespace is scoped to
  namespace-owned devices? *Test:* run tcpdump in the agent against node→sidecar/pod traffic in the
  `just node` lane. *Kill:* if it cannot sniff → drop the UDS, use TCP+bearer in all lanes (simpler).

## Implementation sketch (files)

- `internal/spawnlet/secrets.go` / `manager.go` — git-env dir (chown RemapBase, bind); repoint
  `GIT_CONFIG_GLOBAL`, set `GIT_CONFIG_NOSYSTEM`/`GIT_TERMINAL_PROMPT`/`GIT_ASKPASS`; create the control
  UDS (node-owned, dir `0711` + socket `0666`, pathname) + bind mount in the sniffable lane, or the
  node-hosted bearer-gated TCP `GetToken` listener otherwise; lane-select via `UsernsMode`/`ContainerRuntime`
  + S3; sidecar-readiness gate (gitport + control) incl. CRI app-restart re-assertion.
- `internal/githubcred/render.go` — identity-only gitconfig with the fallback rules; keep node
  credential render for the node-side clone.
- `internal/node/*` — node credential server: `GetToken(spawnID, route, minRemaining)` →
  `{owner, repo, token, accessExpiresAtUnix}`, resolving `route→secretID/generation` from mount
  bindings, minting fresh when stale, rate-limited, typed failures; thread login/id from the mint
  response. **No repo-scope change to the mint** (T3 corrected — it isn't supported).
- AS mint endpoint + mint client — add `login`+`id` to the response (no repository param).
- `internal/sidecar/*` + `cmd/sidecar/main.go` — the path-routed git smart-HTTP proxy on
  `SIDECAR_GIT_ADDR` (canonicalize, exact allow-list, header scrub, Host/SNI rewrite, `Git-Protocol`,
  `Location` rewrite, strict upstream TLS, 401 vs 403, S2-determined transport); the `GetToken` control
  client over the UDS/TCP transport; no auxiliary agent-reachable listeners.
- `internal/storage/github.go` — set `origin` (fixed gitport, per-mount route) **in the node-side clone,
  pre-chown**; resume relies on the journaled config.
- proto/gen — mint response (`login`,`id`); control messages (`GetToken` req/resp incl. expiry,
  per-route); `make gen`. `proto/`-touching work serialized ahead of consumers.

## Post-Implementation Notes

*As this design is implemented and iterated on — bug fixes, adjustments, anything that diverged from the
assumptions above — append a dated note here, whether or not a formal debugging skill was used.*

- **2026-06-19 (roast r1 BLOCK):** corrected the cleartext control channel and the never-reaches-sidecar
  refresh; added fixed gitport, per-mount routing, exact allow-list, Host/SNI rewrite, streaming, strict
  TLS, identity fallbacks, runsc net-raw correction, LFS/submodules out of scope.
- **2026-06-19 (roast r2 BLOCK):** flipped credential flow push→pull (node = credential server) —
  dissolved stream-vs-retry, refresh-delivery, restart-replay, re-mint routing, ordering; made the
  control transport lane-aware (UDS where sniffable, TCP+bearer otherwise); corrected the threat model
  (IPC shared in CRI; rest on mount+PID); split 401/403; added re-mint rate-limit, `GIT_CONFIG_NOSYSTEM`,
  100-continue, port-squat ordering, egress rule, abstract-socket prohibition.
- **2026-06-19 (roast r3 BLOCK):** **dropped the infeasible repo-scoped-mint MUST** — GitHub user-access
  tokens have no repository param and containment-e already settled that `repository_id` never narrows
  scope; T3 now states the honest installation-selection blast radius (proxy allow-list = in-band
  narrowing only). Fixed three introduced-complexity blockers: `origin` rewrite moved into the node-side
  clone **pre-chown** (a post-chown node write hit the userns wall); control-socket **dir `0711` +
  socket `0666`** (a `0700` dir reproduced the traverse-denied bug); defined the **node-as-server TCP
  `GetToken`** transport (bind/discovery/bearer-withheld) for the non-UDS lanes. Added: inbound header
  scrub, `GetToken` returns expiry + a typed failure contract, no auxiliary agent-reachable sidecar
  listeners, CRI app-restart port-squat re-assertion, the large-push-exceeds-`minRemaining` accepted
  residual, and **spike S3** (userns `CAP_NET_RAW` sniffability — may simplify the transport to TCP
  everywhere). Reframed S2 (proxy fidelity) as implementation-gating with a node-side `git http-backend`
  fallback, given Go's auto-100-continue / `ReverseProxy` limits.
