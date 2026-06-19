# Agent Git Environment for GitHub Mounts

**Date:** 2026-06-19
**Status:** draft (revised post-roast 2026-06-19)
**Closes:** sp-7amh (agent can't set git identity) · sp-m859.1 (inject git identity from gh) · sp-n7iy (agent cannot push)
**Follow-up review:** sp-jg7x (verify agent↔sidecar isolation boundary across all pod lanes)
**Epic:** sp-m859 (MVP gaps)

## Problem

In a `github:owner/repo` mount spawn the agent owns the working tree and is promised (AGENTS.md)
that it can commit and push back. Today it can do neither out of the box:

1. **No commit identity (sp-7amh / sp-m859.1).** `GIT_CONFIG_GLOBAL` points at
   `/run/spawnery/secrets/github/gitconfig`, which (a) contains only a `[credential]` section, no
   `[user]`, and (b) lives under the secrets tmpfs, which under userns-remap is owned by a host uid
   outside the container's userns map → it appears as `nobody:nogroup drwx------` and the agent
   (container-root) cannot even traverse into it. So `git commit` prompts for identity and
   `git config --global user.email …` fails with `Permission denied`.

2. **No push credential (sp-n7iy).** The Approach-2 mint-at-provision path
   (`mintGitHubMountsAtProvision`) renders the GitHub credential **node-only** and never renders an
   agent-facing helper. The clone uses ephemeral `-c credential.helper=<node-path>` flags that are
   not persisted, so the agent's `.git/config` has no helper and `git push` fails with
   `could not read Username for 'https://github.com'`.

Both stem from the agent's git environment having been placed under the read-only, node-owned
secrets tmpfs. The mount working-tree itself does not have this problem —
`internal/storage/storage.go` already chowns mount dirs to `Manager.RemapBase()`, which is why
`/app/repo` is agent-writable and local edits work. The git *environment* never got the same
treatment, and the credential was never delivered to the agent at all.

## Threat model (load-bearing — corrected post-roast)

The agent is **untrusted**. The sidecar is the pod's **trusted credential boundary** (it already
holds the model/inference key). The two containers share **only the netns** (Docker:
`NetworkMode=container:<sidecar>`; CRI: shared PodSandbox); mount, PID, and IPC namespaces are
per-container, so the agent cannot read the sidecar's filesystem or ptrace its memory. The agent is
denied `CAP_NET_ADMIN` (floor-defeat guard) but, in the **userns-remap (Docker/runc)** lane, keeps
the engine **default cap set including `CAP_NET_RAW`** — so it **can open AF_PACKET raw sockets and
passively sniff the shared netns**. (In the **runsc** lane gVisor disables raw sockets unless `runsc
--net-raw` is set, which Spawnery does not — so the runsc agent's raw-socket capability is *off*;
sp-jg7x pins this per lane.)

Consequences this design must honor (the roast surfaced that the original draft violated the first
two):

- **T1 — no cleartext secret on any netns-observable leg.** Anything the agent could sniff must
  carry no usable credential. The pre-existing node→sidecar control endpoint is **plain HTTP on
  `0.0.0.0:<port>` reached over the pod IP** (`manager.go:1074`, comment: *"the bearer token … is the
  access control"*) — a bearer token stops unauthorized *requests* but not passive *sniffing*. Any
  secret delivered there (the github token here; BYOK inference keys under sp-7h6.1) is readable by a
  `CAP_NET_RAW` agent. **This design moves secret delivery off the netns entirely** (§2.2).
- **T2 — token confidentiality on the upstream leg rests on strict TLS verification.** With
  `CAP_NET_RAW` the agent can DNS/ARP-spoof `github.com` to its own listener; the token (an
  `Authorization` header) leaks only if the sidecar completes a TLS handshake to an attacker cert.
  Strict cert verification is therefore a **hard invariant** (§2.5), not an implementation detail.
- **T3 — repo scope is only meaningful if the agent cannot reach github directly with a leaked
  token.** The egress floor is default-*allow* today, so a leaked token *is* directly usable; the
  proxy's scope narrows blast radius only while the token stays unleaked. T1+T2 keep it unleaked;
  scope (§2.4) is defense-in-depth, and tightening agent→github egress is a noted follow-up.

## Key decisions

Two independent halves over one shared delivery seam, **both shipping now** (per owner direction
2026-06-19; the boundary review sp-jg7x is a follow-up that formally proves the lane matrix, but the
**T1 control-channel hardening below ships with the push half**, not deferred):

1. **Identity half** — move the agent's global git config to a writable, agent-owned location and
   seed `[user]` from the linked GitHub identity (carried in the AS mint response).
2. **Push half** — a **sidecar-hosted git smart-HTTP reverse proxy** that injects the credential on
   the wire; the agent's `origin` is rewritten to a local plain-http endpoint and holds no token,
   helper, or credential config. The token reaches the sidecar over a **unix-domain socket on a
   node↔sidecar-only bind mount** (never the netns, never the agent).

---

## Section 1 — Identity half (sp-7amh + sp-m859.1)

### 1.1 Writable, agent-owned global git config (all spawns)
Introduce a per-spawn **`git-env` directory**, host-side under the node data root, **chowned to
`Manager.RemapBase()`** (reusing `storage.go`'s mechanism: chown to `agentUID`, EPERM-fallback to
0777 in the degraded/no-userns lane), bind-mounted into the agent at `/run/spawnery/git-env` — a
**sibling of, not under,** the read-only secrets mount. `GIT_CONFIG_GLOBAL` is repointed there
(`<git-env>/gitconfig`). This applies to **every** spawn (not just github mounts): a writable global
config is harmless and lets `git init`/commit work in scratch spawns too. The agent can
`git config --global …` freely.

Also set in the agent env: `GIT_TERMINAL_PROMPT=0` and `GIT_ASKPASS=/bin/false` so any
un-credentialed git network op **fails fast** instead of hanging a non-interactive agent.

### 1.2 Seed `[user]` identity
Identity is **per GitHub account**, not per mount (a spawn's owner links one GitHub account), so a
single global `[user]` is correct even for multi-github spawns. At provision the node renders into
`<git-env>/gitconfig`:

```
[user]
	name = <login>
	email = <id>+<login>@users.noreply.github.com
```

so a bare `git commit` records the real author with no prompt.

**Fallback rules (roast: login-xor-id and non-user links):**
- If **both** `login` and `id` are present → the canonical noreply form above.
- If **either** is missing, or the link is an **org/app-installation** link with no user login → seed
  a deterministic non-prompting identity: `name = <account handle or "spawnery">`,
  `email = <accountID>@users.noreply.spawnery.local`. Identity is best-effort and **never fails
  provisioning**.

### 1.3 Login source — AS mint response
The Approach-2 mint response (`githubRefresh.MintInitial`) is extended to return the GitHub `login`
and numeric `id` alongside the access token and expiry (the AS already knows them for the
`gh:<accountID>` link; they ride the same authenticated node↔AS channel — containment c unchanged).
`mintGitHubMountsAtProvision` threads them into the git-env render.

---

## Section 2 — Push half (sp-n7iy): sidecar git-proxy

### 2.1 Topology & fixed port
The sidecar runs a **git smart-HTTP reverse proxy** alongside its existing inference proxy, listening
on a **fixed, well-known loopback port** in the shared pod netns (`SIDECAR_GIT_ADDR`, analogous to
the fixed `SIDECAR_ADDR`). A fixed port is what makes the `origin` rewrite (§2.6) computable at
mount-provision time and **stable across resume** — there is no runtime port to discover.

The proxy is **path-routed per mount** to support multiple `github:` mounts in one spawn
(`mintGitHubMountsAtProvision` already loops N mounts). Route key = the mount name:

```
origin (mount "src", repo o/r)  = http://127.0.0.1:<gitport>/m/src/o/r
```

The sidecar holds a **per-route token map** `{ route → (owner, repo, token) }`. For each request the
proxy:
- canonicalizes the path (reject `.`/`..`, percent-encoding of `/`, duplicate slashes) **before**
  matching;
- matches the **exact** smart-HTTP endpoint set for that route's repo and nothing else:
  `…/o/r[.git]/info/refs?service=git-(upload|receive)-pack`, `…/o/r[.git]/git-upload-pack`,
  `…/o/r[.git]/git-receive-pack`. Anything else — including the **LFS batch path**
  `…/info/lfs/objects/batch` and dumb-HTTP fallbacks — is **403** (prevents the token-pivot the
  roast flagged);
- rewrites the HTTP **`Host` header to `github.com`** and sets outbound TLS **`ServerName=github.com`**
  (roast: required or GitHub 400s/serves the wrong vhost);
- forwards the **`Git-Protocol`** request header (so protocol v2 is not silently downgraded);
- injects `Authorization: Basic base64("x-access-token:" + token)`;
- **streams** request and response bodies **without buffering** (no full-body read; prompt flush so
  sideband progress and large chunked `git-receive-pack` packs work — `FlushInterval = -1` / a manual
  bidirectional pump, never a buffering proxy);
- rewrites any `Location:` response header from `https://github.com/o/r…` back to the loopback route
  so a canonicalizing 301 keeps git on the proxy (a **renamed-repo** 301 to a *different* repo is a
  surfaced error — documented edge, not silently followed direct).

The agent holds **no token, no credential helper, no `[credential]` config**. The agent→proxy hop is
plain http on loopback and carries **no** credential; the credential exists only on the
sidecar→github TLS leg.

### 2.2 Token delivery — unix socket, off the netns (T1)
The node delivers tokens to the sidecar over a **unix-domain socket on a bind mount shared only
between the node (host side) and the sidecar container** (e.g. host `…/<id>/control.sock` →
sidecar `/run/spawnery/control.sock`). The agent container does **not** mount that path and is on a
different mount ns, so the socket is invisible to it; nothing crosses the netns, so `CAP_NET_RAW`
buys the agent nothing here. The bearer token remains as authZ; the unix-socket bind is the
confidentiality boundary.

This replaces the cleartext `0.0.0.0:<port>` control endpoint for **secret** delivery (the
non-secret runtime model-switch may remain on the existing endpoint or migrate; carrying the github
token or BYOK keys over the old cleartext endpoint is prohibited). The control surface gains a
per-route credential setter (`SetGitHubToken(route, owner, repo, token)`), mirroring the model setter.

### 2.3 Lifecycle: provision, refresh, expiry, restart (roast blockers)
- **Provision / resume:** before `StartAgent`, the node mints (existing `mintGitHubMountsAtProvision`,
  which already runs on create *and* resume) and **pushes each mount's token to the sidecar** via the
  control socket. Push must complete **before** the agent starts (ordering guarantee) so the first
  git op has a live token.
- **Proactive refresh:** the existing refresher (`githubRefresh`) currently keeps only the expiry and
  lets the refreshed token land in node storage via `consumeGitHubSecret` — it **does not** reach the
  sidecar today. This design **wires the refreshed token to the sidecar**: on each refresh the node
  calls `SetGitHubToken(route, …)` over the control socket. (Both the JIT mint path and the
  sealed-fanout refresh consumer deliver to the sidecar for proxy-backed github mounts.)
- **Reactive backstop (in-flight expiry / missed refresh):** if the upstream returns **401/403**, the
  proxy makes a **single** re-mint request to the node over the control socket, updates the route
  token, and retries the operation **once**; a second failure surfaces a clean error. This closes both
  the “~8h proactive-only” gap and the two-request-handshake-straddling-expiry race.
- **Sidecar restart:** the token lives in sidecar memory; on a sidecar process restart the node
  re-pushes current tokens (tie into the existing control-replay path that re-establishes sidecar
  state after a restart/reconnect).

### 2.4 Repo scoping & abuse bounds (defense-in-depth)
The per-route exact-path allow-list + Host pin mean a compromised agent reaching the port cannot push
to or read any repo other than its mounts' `owner/repo`, cannot hit the LFS batch API to pivot the
token to arbitrary object URLs, and cannot reach general github.com API endpoints (only the three
smart-HTTP paths per route are proxied). Per **T3**, this is defense-in-depth atop token secrecy
(T1+T2); tightening agent→github direct egress is a tracked follow-up, not relied on here.

### 2.5 Upstream TLS — hard invariant (T2)
The sidecar→github HTTP client **MUST** use stock TLS with default certificate verification,
`ServerName="github.com"`; it **MUST NOT** set `InsecureSkipVerify`, **MUST NOT** add a custom CA
pool, and **MUST NOT** honor `HTTP(S)_PROXY`/`NO_PROXY` env (which a `CAP_NET_RAW`/spoofing agent or
a poisoned env could use to redirect the leg). **Spike S1:** evaluate pinning GitHub's certificate
chain / known IP set as additional hardening (see Spikes).

### 2.6 `origin` rewrite — placement & durability
The rewrite is an explicit **ensure-origin step** keyed off the fixed gitport, run by the node for
each github mount on **both create and resume**, **after** mount restore and **before** `StartAgent`
— *not* buried in `storage.GitHub.Prepare`'s clone-only branch (which `Prepare` returns before on the
restore path, and which has no port). It is idempotent (`git remote set-url origin …`), so resume
re-asserts it regardless of the journaled `.git/config`.

**Documented constraints (roast — accepted for MVP):**
- The agent owns `.git/config`; if it rewrites `origin` to the canonical `https://github.com` URL,
  pushes fail (no agent-side credential). This is a known limitation; the agent must not do so.
  `git remote -v` shows the `http://127.0.0.1:<gitport>/…` URL by design.
- **Out of MVP scope (explicit):** Git **LFS** (separate batch API + object store host), **private
  submodules** (other repos, not the rewritten origin), the **`gh` CLI**, and any additional remote
  or hardcoded `https://github.com` URL in the tree — these bypass the single-origin proxy and will
  fail auth. Tracked as follow-ups; not promised by this slice.

---

## Section 3 — Boundary review & per-lane posture (sp-jg7x, follow-up)

sp-jg7x formally proves, per lane, that the agent cannot extract the sidecar's token or model key,
and pins each lane's actual raw-socket capability (Docker userns-remap: `NET_RAW` **present**; runsc:
raw sockets **off** absent `--net-raw`; rootful/CapDropAll: no caps). It is a follow-up, **not** a
merge gate for this spec — but note the **T1 hardening (§2.2) ships now**, so the previously-cleartext
delivery leg is closed at merge; sp-jg7x verifies the residual (upstream-leg TLS reliance under
spoofing, mount/pid/ipc isolation across lanes). Fallback for any failing lane: node-only proxy with
a listener injected into the pod netns (token never in any pod container).

## Section 4 — Containment reconciliation

- **(b) minted token never in the agent container** — *preserved.* Token reaches the sidecar over a
  unix socket the agent cannot see, never the agent.
- **(a) refresh material stays AS-custodial** — *unchanged.* Only the short-lived access token is
  delivered, and only to the sidecar.
- **(c) token never relayed by the CP** — *unchanged.* Token + the new login/id ride the authenticated
  node↔AS mint response.
- **Secrets tmpfs stays unreadable to the agent** — *unchanged.* The new agent-writable surface is a
  separate `git-env` dir; the secrets tmpfs is not made writable.
- **Blast radius (roast):** co-locating the github token with the model key in the sidecar is an
  accepted, reviewed extension of the sidecar's existing trusted-proxy role; sp-jg7x covers the
  boundary. The pre-existing cleartext exposure of BYOK keys over the old control endpoint is closed
  for github here and folded into sp-n7iy's control-channel hardening.

## Section 5 — Testing

**Identity (hermetic + e2e):** config rendered with seeded `[user]`, dir chowned to RemapBase, file
writable; fallback identity on login-xor-id and org/app links; `GIT_TERMINAL_PROMPT=0` set; e2e
in-spawn `git commit` records the GitHub author with no prompt.

**Push (e2e):** clone → commit → **push** → suspend → resume → push, from **both** spawnctl and web;
multi-github-mount spawn (two routes, two tokens, no cross-talk); proxy rejects a different
`owner/repo`, the LFS batch path, and path-traversal/encoded variants (403); protocol-v2 push and a
large/sideband pack succeed through the streaming proxy; a forced token expiry mid-session triggers
the 401→re-mint→retry path and push still succeeds.

**Boundary (in sp-jg7x):** token not present in the agent's filesystem/env; the delivery socket is
not visible/connectable from the agent; per-lane raw-socket capability matrix.

All integration/e2e tests are build-tagged and fail (never skip) when their lane dep is down.

## Recommended spikes (from the roast)

- **S1 — upstream TLS pinning:** *Question:* does pinning GitHub's cert chain / IP set meaningfully
  raise the bar over default verification, given a `CAP_NET_RAW` spoofing agent? *Cheapest test:*
  prototype the sidecar client with a pinned pool and confirm push works against real github + a
  spoofed-DNS negative. *Kill criteria:* pinning breaks on GitHub cert rotation in a way ops can't
  track → ship strict default verification only.
- **S2 — git smart-HTTP fidelity through re-origination:** *Question:* do protocol-v2,
  100-continue/chunked `git-receive-pack`, and sideband flushing survive the plain-http→TLS
  re-originating proxy? *Cheapest test:* a standalone proxy prototype, push a repo with a large pack +
  v2. *Kill criteria:* a protocol case can't be made faithful → fall back to a node-side full git
  http backend or the node-only proxy.

## Implementation sketch (files)

- `internal/spawnlet/secrets.go` / `manager.go` — add the `git-env` dir (chown RemapBase, bind at
  `/run/spawnery/git-env`); repoint `GIT_CONFIG_GLOBAL`; set `GIT_TERMINAL_PROMPT=0`/`GIT_ASKPASS`;
  add the unix-socket control bind mount; add the ensure-origin step (create+resume, pre-StartAgent).
- `internal/githubcred/render.go` — render identity-only gitconfig (`[user]`) into git-env with the
  fallback rules; keep node credential render for the node-side clone.
- `internal/node/secrets.go` (`mintGitHubMountsAtProvision`) + `internal/node/github_refresh.go` —
  thread login/id; deliver each mount's token to the sidecar control socket on provision **and on
  refresh**.
- AS mint endpoint + `githubRefresh.MintInitial` / mint client — add `login` + `id` to the response.
- `internal/sidecar/*` + `cmd/sidecar/main.go` — add the unix-socket secret-control listener
  (`SetGitHubToken(route, …)` + node re-mint callback); add the repo-scoped, path-routed git
  smart-HTTP reverse proxy on `SIDECAR_GIT_ADDR` (canonicalize, exact allow-list, Host/SNI rewrite,
  `Git-Protocol` passthrough, unbuffered streaming, `Location` rewrite, strict upstream TLS, 401
  re-mint+retry).
- `internal/storage/github.go` — origin URL helper (fixed gitport, per-mount route); leave the
  ensure-origin call in the manager flow, not the clone-only branch.
- proto/gen — extend the mint response (login, id) and the sidecar control message (per-route github
  token, re-mint); `make gen`. `proto/`-touching work serialized ahead of consumers.

## Post-Implementation Notes

*As this design is implemented and iterated on — bug fixes, adjustments, anything that diverged from
the assumptions above — append a dated note here, whether or not a formal debugging skill was used.*

- **2026-06-19** — Revised after a `roast` BLOCK (53 raw findings). Corrected two factual errors in
  the first draft: the node→sidecar control channel was claimed "Unix-socket-hardened" but is
  cleartext HTTP on `0.0.0.0:<port>` over the pod IP (sniffable by a `CAP_NET_RAW` agent) → moved
  secret delivery to a unix socket on a node↔sidecar-only bind mount (T1, §2.2); and proactive
  refresh does **not** currently reach the sidecar → wired it (§2.3). Added: fixed gitport + explicit
  ensure-origin placement (was unschedulable in `Prepare`), per-mount path routing for multi-github
  spawns, exact path allow-list rejecting the LFS-batch token-pivot + traversal, Host/SNI rewrite,
  unbuffered streaming + `Git-Protocol` passthrough, `Location` rewrite, strict-TLS hard invariant
  (T2), 401→re-mint→retry, `GIT_TERMINAL_PROMPT=0`, identity fallback for login-xor-id/org-app links,
  corrected runsc `CAP_NET_RAW` (off absent `--net-raw`), and scoped LFS/submodules/`gh` out of MVP.
