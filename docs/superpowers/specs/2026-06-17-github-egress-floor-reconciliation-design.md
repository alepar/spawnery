# GitHub Channel × Per-Pod Egress Floor — Reconciliation (sp-u53.1.6)

**Bead:** `sp-u53.1.6` (P0, mvp) · under epic `sp-dl62` (end-user GitHub mounts) · related `sp-u53.1`
**Status:** Draft v1 — design decision recorded; MVP requires **no floor code change**
**Date:** 2026-06-17
**Builds on:** [Egress Floor (sp-rpa)](2026-06-01-egress-floor-sp-rpa.md) ·
[GitHub Credentials + Storage Unified Design](2026-06-14-github-credentials-and-storage-unified-design.md) (§9, §12, §16)

## 0. Why this doc exists

The unified GitHub design flags, in §12, an unresolved integration:

> *"The required github.com egress channel must be reconciled with the per-pod egress floor … (whether
> the floor permits the node's mint/clone/push and the agent's exact-repo helper push); this is called
> out as an integration requirement, not left implicit."*

This document is that reconciliation. Its conclusion is short: **under the egress floor as shipped,
the GitHub channel already works for both the node-daemon and the agent pod, and no change to the floor
is required for MVP.** The rest of this doc proves *why* (with code refs), records the threat-model
posture deliberately, and captures the future deny-by-default guidance so the open questions are
answered on paper rather than re-litigated later.

## 1. Reconciliation finding (the floor is default-allow, applied host-side)

The bead and §12 are framed against a "deny-by-default" mental model. The **shipped** floor is the
opposite — a **default-*allow* block-floor** — and it is applied on **host netfilter**, matched by the
pod's source IP, not via `nsenter` inside the pod netns. (The original sp-rpa *doc* described an
in-netns `nsenter` mechanism; the implementation evolved to host-side `DOCKER-USER` /
`SPAWNLET-EGRESS` so it also enforces under gVisor/runsc, where in-netns iptables is a no-op. The
sp-rpa doc's *policy* — drop metadata + RFC1918, allow public otherwise — is unchanged.)

Ground truth, `internal/spawnlet/firewall/firewall.go` (`Rules`):

- ACCEPT DNS (`53/udp`, `53/tcp`) and any operator `EgressAllowCIDRs`
- DROP `169.254.0.0/16` (cloud metadata) + `10.0.0.0/8`, `172.16.0.0/12`, `192.168.0.0/16` (RFC1918)
- **no terminal DROP**: a non-matching packet RETURNs from `DOCKER-USER`/`SPAWNLET-EGRESS` to
  `FORWARD`, whose default policy is `ACCEPT` → **public internet is open**.

The package doc says it directly: *"drop cloud-metadata and RFC1918 for a spawn pod, allowing public
egress otherwise."* Rules are inserted matched by `-s <PodIP>` (`firewall.go` `Rules`,
`add := …{"-s", ip}…`) so multiple pods coexist in the shared host chain
(`manager.go` `Create`: `fw.Apply(ctx, firewall.Rules(h.PodIP, …))`, fail-closed, between sidecar
start and agent start).

### 1.1 The two contexts (resolves question b)

| Context | Network namespace | Subject to the pod floor? | Reaches github.com / AS today? |
|---|---|---|---|
| **Node-daemon**: clone/fetch/push + AS mint/refresh | **host** netns | **No** — floor matches only `-s <PodIP>`; the host's own source IP never matches | **Yes**, unaffected |
| **Agent**: git via exact-repo credential helper | **pod** netns (shares the sidecar netns; source = `PodIP`) | **Yes** | **Yes** — default-allow lets public hosts through |

- **Node-daemon is host-netns.** `spawnlet` runs `git` via `exec.CommandContext`
  (`internal/storage/github.go`, `execGitRunner.RunGit`) and calls the AS mint/refresh over an mTLS
  client (`cmd/spawnlet/main.go` `nodeGitHubMint` → `internal/node/github_refresh.go`). Both originate
  from the host network namespace, which the `-s <PodIP>` floor never matches. The node's
  clone/push/mint channel is therefore **already permitted** and is independent of the floor entirely.
- **Agent is pod-netns, default-allow.** The agent's git (driven by the rendered exact-repo helper,
  `internal/spawnlet/githubcred/render.go`; `GH_CONFIG_DIR` / `GIT_CONFIG_GLOBAL` wired in
  `manager.go` `StartAgent`) runs in the shared sidecar netns. github.com, `codeload.github.com`,
  `*.githubusercontent.com`, and LFS hosts are all **public**, so they fall through to `FORWARD ACCEPT`.
  No allowlist entry is needed.

### 1.2 No CDN IP-churn problem in MVP (resolves question a, for now)

The hard part of question (a) — allowlisting `github.com` / `codeload` / `*.githubusercontent.com` /
LFS in an IP-based iptables floor when those IPs churn (DNS-pinning vs IP-ranges vs proxy) — **only
arises under deny-by-default.** Under the shipped default-allow floor there is nothing to allowlist:
public is already reachable. Question (a) is deferred with the deny-by-default slice (see §3).

## 2. Threat-model posture (the explicit decision §12 asked for)

This is recorded deliberately rather than left implicit:

1. **The agent already has unrestricted public egress.** This is sp-rpa decision R.1:
   *"Arbitrary public exfil remains open; the stricter allow-list (app-declared domains) is a
   deliberate later slice."* **GitHub is not a new exfil channel** — it is one public host among all
   the others the agent can already reach. Opening "the GitHub channel" adds nothing the floor wasn't
   already permitting.
2. **The agent's GitHub *token* exfil risk is the already-accepted §16/§9 relaxation**, not a new risk
   introduced here. It is bounded by short-lived installation-selection-scoped access tokens, the
   exact-repo credential helper (`credential.useHttpPath=true`, answers only for the bound
   `{host,owner,repo}`), journal-excluded tmpfs storage, and the `DELETE /grant` kill switch (§16.5).
   This reconciliation does **not** widen that blast radius. The residual `git remote set-url
   https://<token>@…` exfil path (round-2 F34) is GitHub's, unchanged, and only closable by the future
   git proxy.
3. **Load-bearing host assumption (a documented prerequisite, not a new requirement):** the
   default-allow property depends on the host `FORWARD` chain defaulting to `ACCEPT`. The
   **sidecar→openrouter inference path already depends on this** (`cmd/sidecar/main.go`,
   `SIDECAR_UPSTREAM=https://openrouter.ai/api`). GitHub inherits the same assumption and adds **no new
   host requirement**. If an operator sets `FORWARD` policy to `DROP`, *both* inference and GitHub
   break together — surfaced here so it is not a surprise during deployment.
4. **Self-hosted / GitHub Enterprise caveat:** MVP supports only `host == "github.com"` (public;
   `internal/storage/github.go` validates this). A future private-IP GitHub Enterprise host *would* be
   caught by the RFC1918 DROP and require an `EgressAllowCIDRs` entry for that host. This is a future
   caveat, not MVP work (GHE is tracked separately, `sp-ei4.2`).

**Decision:** for MVP, accept the existing default-allow floor for the GitHub channel. No floor code
change. The GitHub channel is permitted by construction: node = host-netns (unfloored), agent =
default-allow public.

## 3. Future: deny-by-default + GitHub allowlist (captured, not built)

When the deferred "app-declared egress domains" slice (sp-rpa decision R.5) eventually flips the agent
pod to deny-by-default, the GitHub allowlist problem (a) returns. The recommended approach, recorded
now so that slice does not start from scratch:

- **IP-allowlisting is the wrong primitive for GitHub's CDN surface.** `*.githubusercontent.com` and
  `codeload.github.com` ride a shared CDN (Fastly); allowlisting their IP ranges ≈ allowlisting a large
  slice of the public internet — weak isolation. GitHub's published ranges
  (`https://api.github.com/meta`: `git`, `web`, `packages` arrays) churn and still don't cleanly cover
  the CDN. DNS-pinning (resolve→pin at clone time) is fragile under IP rotation and LFS redirects to
  arbitrary hosts.
- **Recommended mechanism: a hostname/SNI-aware egress proxy.** Allowlist by hostname at an
  HTTP(S)-CONNECT / SNI-filtering proxy in (or in front of) the pod netns, sidestepping IP churn
  entirely. This **converges with the git proxy already tracked in §16.4** ("a transparent git proxy …
  is a tracked future upgrade, not MVP"): the same proxy that would enable reactive token refresh on
  `401` is the right enforcement point for hostname-based GitHub egress under deny-by-default. One
  future component, two payoffs (reactive refresh + hostname egress control). The openrouter inference
  upstream would be allowlisted at the same proxy.

This section answers questions (a) and (c) on paper for the future slice without building anything in
MVP.

## 4. Scope / non-goals

**In (this bead):** the reconciliation finding (§1), the recorded threat-model decision (§2), the
future deny-by-default guidance (§3), this doc + its INDEX row, and a `sp-u53.1.6` note pointing here.

**Out:** any change to `internal/spawnlet/firewall` · flipping the floor to deny-by-default · building
the egress/git proxy (§16.4 future) · GHE/private-host support (`sp-ei4.2`) · the agent token-exfil
relaxation itself (already decided in §16/§9) · IPv6 (sp-rpa follow-up).

## 5. Verification posture

No new code, so no new tests are warranted. The properties this doc relies on are already covered:

- **Agent → public reachable under the floor** is asserted by the existing `egress_e2e` lane
  (`internal/spawnlet/firewall/...egress_e2e_test.go`): metadata + an RFC1918 IP unreachable, a public
  IP reachable. github.com is a public IP, so that test *is* the agent-side proof.
- **Node → github.com / AS unfloored** follows from the node running in the host netns
  (`-s <PodIP>` cannot match the host source IP) — a structural property of `firewall.Rules`, already
  covered by the hermetic `Rules()` unit test asserting the rule set is source-IP-scoped.

If the future deny-by-default slice (§3) lands, *that* slice owns the github-reachability allowlist
test; it must not silently regress this MVP property.

## 6. Decision log

| # | Decision | Choice |
|---|---|---|
| G.1 | MVP floor posture for GitHub | **No floor change.** Default-allow already permits the channel. |
| G.2 | Node-daemon egress (clone/push/mint) | Host-netns; not subject to the `-s <PodIP>` pod floor → permitted by construction. |
| G.3 | Agent egress (exact-repo git) | Pod-netns; default-allow lets public github hosts through; no allowlist. |
| G.4 | CDN IP-churn (question a) | Not an MVP problem (only under deny-by-default); deferred with the allow-list slice. |
| G.5 | Agent exfil posture (question c) | GitHub is one public host among all; not a new channel. Token-exfil relaxation is the already-accepted §16/§9 decision; not widened here. |
| G.6 | Host prerequisite | Default-allow depends on host `FORWARD=ACCEPT`; openrouter already depends on it; no new requirement. Documented. |
| G.7 | GHE / private host | Future caveat: a private-IP GHE host hits the RFC1918 DROP and needs `EgressAllowCIDRs`; out of MVP (`sp-ei4.2`). |
| G.8 | Future deny-by-default mechanism | Hostname/SNI-aware egress proxy (converges with the §16.4 git-proxy upgrade), not IP-allowlisting. |

## Post-Implementation Notes

*As this design is implemented and iterated on — bug fixes, adjustments, anything that diverged from
the assumptions above — append a dated note here, whether or not a formal debugging skill was used.*
