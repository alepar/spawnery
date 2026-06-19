# Agent Git Environment for GitHub Mounts

**Date:** 2026-06-19
**Status:** draft
**Closes:** sp-7amh (agent can't set git identity) · sp-m859.1 (inject git identity from gh) · sp-n7iy (agent cannot push)
**Follow-up review (not a merge gate):** sp-jg7x (verify agent↔sidecar isolation boundary across all pod lanes)
**Epic:** sp-m859 (MVP gaps)

## Problem

In a `github:owner/repo` mount spawn the agent owns the working tree and is promised (AGENTS.md)
that it can commit and push back. Today it can do neither out of the box:

1. **No commit identity (sp-7amh / sp-m859.1).** `GIT_CONFIG_GLOBAL` points at
   `/run/spawnery/secrets/github/gitconfig`, which (a) contains only a `[credential]` section, no
   `[user]`, and (b) lives under the secrets tmpfs, which under userns-remap is owned by a host uid
   outside the container's userns map → it appears as `nobody:nogroup drwx------` and the agent
   (container-root) cannot even traverse into it, let alone write it. So `git commit` prompts for
   identity and `git config --global user.email …` fails with `Permission denied`.

2. **No push credential (sp-n7iy).** The Approach-2 mint-at-provision path
   (`mintGitHubMountsAtProvision`) renders the GitHub credential **node-only** into
   `GitHubCredentialsRoot` (containment invariant b) and explicitly never renders an agent-facing
   helper. The clone uses ephemeral `-c credential.helper=<node-path>` flags that are not persisted,
   so the agent's `.git/config` has no helper and `git push` fails with
   `could not read Username for 'https://github.com'`.

Both ultimately stem from the same fact: **the agent's git environment was placed under the
read-only, node-owned secrets tmpfs.** The mount working-tree itself does not have this problem —
`internal/storage/storage.go` already chowns mount dirs to `Manager.RemapBase()` (the host uid the
in-container agent-root maps to), which is why `/app/repo` is agent-writable and local edits work.
The git *environment* simply never got the same treatment, and the credential was never delivered to
the agent at all.

## Main challenges

- **Credential containment.** The minted access token must not be exfiltratable by the untrusted
  agent, yet the agent must be able to push. Handing the agent a token file is the simplest fix but
  the chosen posture is stronger: the agent never holds a token. That forces credential injection to
  happen **on the wire, outside the agent** — a credentialed proxy.
- **userns ownership.** Anything the agent must read/write has to be chowned to the userns-remap base
  uid; anything credential-bearing must stay outside the agent's reachable filesystem.
- **Lane breadth.** The pod runs across four lanes (rootless+userns-remap docker, rootful docker,
  rootless podman, containerd/CRI+runsc). The agent shares only the netns with the sidecar but, in
  the default-cap lanes, keeps `CAP_NET_RAW`. The design must hold — or degrade safely — in all of
  them.
- **Login plumbing.** Seeding a real `[user]` identity needs the GitHub login/id, which the
  Approach-2 mint response does not carry today.

## Key decisions

The fix is two independent halves over one shared delivery seam, **both shipping now** (the boundary
review sp-jg7x is a follow-up, not a pre-merge gate, per owner direction 2026-06-19):

1. **Identity half** — move the agent's global git config to a writable, agent-owned location and
   pre-seed `[user]` from the linked GitHub identity (carried in the AS mint response).
2. **Push half** — a **sidecar-hosted git smart-HTTP reverse proxy** that injects the credential on
   the wire; the agent's `origin` is rewritten to a local plain-http endpoint and holds no token,
   helper, or credential config.

The token reaches the sidecar (the pod's already-credentialed proxy — it holds the model key today),
never the agent, preserving containment invariant (b) for the agent and (a) for refresh material.

---

## Section 1 — Identity half (sp-7amh + sp-m859.1)

### 1.1 Writable, agent-owned global git config
Introduce a per-spawn **`git-env` directory**, host-side under the node data root, **chowned to
`Manager.RemapBase()`** (reusing the exact mechanism `internal/storage/storage.go` uses for mount
trees: chown to `agentUID`, EPERM-fallback to legacy 0777 in the degraded/no-userns lane). It is
bind-mounted into the agent at a fixed path (e.g. `/run/spawnery/git-env`, a sibling of the
read-only secrets mount — **not** under it).

`GIT_CONFIG_GLOBAL` is repointed from `/run/spawnery/secrets/github/gitconfig` to
`<git-env>/gitconfig`. Because the dir is agent-owned, `git config --global …` now succeeds and the
agent can override anything.

### 1.2 Seed `[user]` identity
At provision the node renders into `<git-env>/gitconfig`:

```
[user]
	name = <login>
	email = <id>+<login>@users.noreply.github.com
```

so a bare `git commit` records the real GitHub author with no prompt. If a credential-bearing config
is ever needed agent-side it is referenced as a **read-only `[include]`**, keeping the user-writable
surface free of secrets — but with the sidecar-proxy push design (Section 2) the agent needs **no**
credential config at all, so in the common case `<git-env>/gitconfig` is identity-only.

### 1.3 Login source — AS mint response
The Approach-2 mint response (`githubRefresh.MintInitial`) is extended to return the GitHub **login**
and **numeric id** alongside the access token and expiry. The AS already knows them for the
`gh:<accountID>` link; they ride the same authenticated node↔AS channel that already carries the
token. `mintGitHubMountsAtProvision` threads them into the git-env render.

**Fallback:** if login/id are absent (older AS, dev relaxation), seed a generic non-prompting
identity (`name = spawnery`, `email = spawnery@users.noreply.github.com`) so commits never block.
Identity is best-effort and never fails provisioning.

### 1.4 Independence
This half touches no credential material and has no boundary implications, so it is correct on its
own and is the smaller, lower-risk change.

---

## Section 2 — Push half (sp-n7iy): sidecar git-proxy

### 2.1 Topology
The sidecar gains a **git smart-HTTP reverse proxy**, repo-scoped, alongside its existing
OpenAI/OpenRouter proxy (same process, same role: the pod's credentialed egress proxy). It listens
on a loopback port in the shared pod netns (e.g. `127.0.0.1:<gitport>`).

The agent's `origin` remote is rewritten **at clone time** (node-side, in `storage.GitHub.Prepare`)
to a local plain-http URL:

```
origin = http://127.0.0.1:<gitport>/owner/repo
```

The proxy:
- accepts the agent's plain-http git requests (`/info/refs?service=git-(upload|receive)-pack`,
  `/git-upload-pack`, `/git-receive-pack`);
- enforces a **path allow-list** of exactly the mount's `owner/repo` (rejects anything else 403);
- injects `Authorization: Basic base64("x-access-token:" + token)`;
- re-originates a **TLS** connection to `https://github.com/owner/repo` and streams bidirectionally.

Both fetch and push flow through it. The agent holds **no token, no credential helper, and no
`[credential]` config**. The only credential transit is the sidecar→github leg, which is inside TLS;
the agent→proxy hop on loopback carries no credential.

### 2.2 Token delivery to the sidecar
The node delivers the short-lived access token to the **sidecar** (never the agent) over the
**existing trusted control channel** — the same pre-`StartAgent` sidecar control endpoint used for
BYOK/model-key delivery (Unix-socket-hardened). The node already schedules proactive GitHub refresh
(`githubRefresh.Note`); refresh pushes the new token to the sidecar control endpoint the same way.
Refresh material stays AS-custodial (invariant a) — only the access token is delivered, and only to
the sidecar.

### 2.3 Repo scoping & abuse bounds
The proxy's path allow-list and host pin mean a compromised agent that reaches the port:
- cannot push to or read any repo other than the mount's `owner/repo`;
- cannot pivot the token to general github.com API calls (only the git smart-HTTP endpoints for the
  one repo are proxied);
- cannot retrieve the raw token (it is added by the sidecar after the agent's request, on the TLS
  leg the agent cannot read in cleartext).

### 2.4 Suspend/resume
On resume the node re-mints (it already calls `mintGitHubMountsAtProvision` on resume) and
re-delivers the token to the sidecar; `storage.Prepare` skips the clone but the `origin` rewrite is
idempotent (re-asserted), so the agent's push path is live again with no agent-visible change.

---

## Section 3 — Boundary review & per-lane posture (sp-jg7x, follow-up)

Per owner direction the push half ships now; sp-jg7x is the **follow-up** security review that
formally proves the agent cannot extract the sidecar's token (or model key) in each lane, and
defines a node-only-proxy fallback for any lane that fails. It is **not** a merge gate for this
spec, but it is a hard P0 before the design is considered production-trustworthy.

Lane matrix and properties (a–f) are enumerated in sp-jg7x. Headline risk: in the userns-remap and
runsc lanes the agent keeps `CAP_NET_RAW` on the shared netns; the design's safety rests on the
token only ever crossing inside TLS (sidecar→github) with no plaintext credential leg observable on
loopback or the external interface. sp-jg7x verifies exactly this.

**Fallback (if a lane fails):** node-only proxy with a listener injected into the pod netns; the
token never enters any pod container. Heavier (netns/listener lifecycle, per-pod scoping), applied
per-lane only where required.

---

## Section 4 — Containment reconciliation

- **(b) minted token never in the agent container** — *preserved.* The token reaches the sidecar,
  not the agent. This is a deliberate, reviewed extension of the sidecar's existing role as the
  pod's credentialed proxy (it already holds the model key); formalized by sp-jg7x.
- **(a) refresh material stays AS-custodial** — *unchanged.* Only the short-lived access token is
  delivered, and only to the sidecar.
- **(c) token never relayed by the CP** — *unchanged.* Token still arrives only via the authenticated
  node↔AS mint response; the new login/id fields ride the same channel.
- **Secrets tmpfs stays unreadable to the agent** — *unchanged.* The new agent-writable surface is a
  **separate** `git-env` dir; the secrets tmpfs is not made writable.

---

## Section 5 — Testing

**Identity (hermetic + e2e):**
- Unit: `git-env/gitconfig` rendered with the seeded `[user]`; dir chowned to RemapBase; file
  writable by the mapped uid; fallback identity when login absent.
- e2e (github mount lane): in-spawn `git commit` records the GitHub author with no prompt; agent can
  `git config --global` override.

**Push (e2e):**
- Full path: clone → commit → **push** → suspend → resume → push, from **both** spawnctl and web.
- Proxy repo-scope: a request for a different `owner/repo` is rejected (403); a non-git path is
  rejected.
- Token isolation: the access token is not present anywhere in the agent's filesystem or environment
  (assertion folded into sp-jg7x's per-lane matrix).

All integration/e2e tests are build-tagged and fail (never skip) when their lane dep is down, per
project convention.

---

## Implementation sketch (files)

- `internal/spawnlet/secrets.go` / `manager.go` — add the `git-env` dir (chown to RemapBase, bind at
  `/run/spawnery/git-env`); repoint `GIT_CONFIG_GLOBAL`; drop `GH_CONFIG_DIR`/secrets-dir gitconfig
  for github mounts.
- `internal/githubcred/render.go` — render an identity-only gitconfig (`[user]`) into the git-env
  dir; keep node credential render for the node-side clone.
- `internal/node/secrets.go` (`mintGitHubMountsAtProvision`) — thread login/id from the mint
  response into the identity render; deliver the access token to the sidecar control endpoint.
- AS mint endpoint + `githubRefresh.MintInitial` / mint client — add login + id to the mint response.
- `internal/sidecar/*` — add the repo-scoped git smart-HTTP reverse proxy + a control-endpoint
  setter for the github access token (mirrors the model-key setter).
- `internal/storage/github.go` — rewrite `origin` to the sidecar git-proxy URL at clone (idempotent
  on resume).
- proto/gen — extend the mint response message (login, id) and the sidecar control message (github
  token); `make gen`.

`proto/`-touching work (mint response, sidecar control) must be serialized ahead of the consumers.

## Post-Implementation Notes

*As this design is implemented and iterated on — bug fixes, adjustments, anything that diverged from
the assumptions above — append a dated note here, whether or not a formal debugging skill was used.*
