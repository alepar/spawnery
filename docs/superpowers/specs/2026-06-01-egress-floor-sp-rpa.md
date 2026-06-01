# Egress Allowlist Floor (sp-rpa) — Design

**Bead:** `sp-rpa` (P0, demo-required) · relates to `sp-eha` (isolation), `sp-ach` (quotas), `sp-ba5` (consent)
**Status:** Draft v1 — build proceeding per "keep going" unless flagged
**Date:** 2026-06-01
**Source:** [Demo MVP Scope §4/§9](2026-05-28-spawnery-demo-mvp-scope.md) · [E1 runtime](2026-05-27-spawnery-e1-runtime-core-design.md)

## 0. Decision & context

Per the user decision, the floor is a **default-allow block-floor**: the pod may reach the public
internet, but **cloud-metadata (169.254.x) and RFC1918 internal ranges are dropped**. This closes
the highest-severity attacks the roast flagged — metadata credential theft (`sp-gtm`-adjacent),
SSRF/egress-proxy, and internal-network pivot — with simple static rules and no DNS/domain
resolution. Arbitrary public exfil remains open; the stricter allow-list (app-declared domains) is a
deliberate later slice.

Runtime facts (verified): a spawn is **two containers sharing one netns** — the sidecar creates the
netns, the agent joins it (`NetworkMode("container:"+sidecarID)`, `spawnlet/manager.go`). The sidecar
proxies inference to **`https://openrouter.ai/api`** (public; `cmd/sidecar/main.go`), so blocking
RFC1918 does **not** break inference. There is **no egress restriction today**.

## 1. Scope

**In (node-only):**
1. A `firewall` package: **build** the static rule set (pure, unit-testable) + **apply** it to a
   container's netns via `nsenter` (integration).
2. Wire it into `spawnlet/manager.go` `Create`: apply **after the sidecar starts, before the agent
   starts** (no unprotected window; one application covers the shared netns).
3. **Fail-closed:** if the floor can't be applied, abort the spawn (stop the sidecar, return error).
   A security floor that silently no-ops is worse than none.
4. Optional node config `EgressAllowCIDRs` (default empty) — `ACCEPT` rules inserted *before* the
   drops, for operators who point `SIDECAR_UPSTREAM` at a LAN model.

**Out:** app-declared egress domains + the allow-list model (needs the manifest `permissions` block
+ CP→node plumbing — later slice) · user consent UI (`sp-ba5`/E6) · gVisor/microVM isolation
(`sp-eha`) · cgroup/quota limits (`sp-ach`) · IPv6 (follow-up bead) · restricting the *sidecar's*
own upstream beyond the shared-netns rules.

## 2. The rules (pod netns `OUTPUT` chain)

Applied once to the **sidecar container's** netns (the agent shares it). Default policy stays
`ACCEPT`; we insert drops:

```
# allow loopback (agent <-> sidecar on 127.0.0.1) — usually already allowed, assert it
iptables -A OUTPUT -o lo -j ACCEPT
# operator escape hatch for a LAN model upstream (EgressAllowCIDRs), inserted BEFORE drops
iptables -A OUTPUT -d <cidr> -j ACCEPT        # for each configured CIDR (default: none)
# the floor
iptables -A OUTPUT -d 169.254.0.0/16 -j DROP  # link-local incl. 169.254.169.254 cloud metadata
iptables -A OUTPUT -d 10.0.0.0/8     -j DROP  # RFC1918
iptables -A OUTPUT -d 172.16.0.0/12  -j DROP  # RFC1918 (Docker bridge lives here, but the agent
                                              #   reaches the sidecar via lo, not the bridge IP)
iptables -A OUTPUT -d 192.168.0.0/16 -j DROP  # RFC1918
# (default ACCEPT -> public internet, incl. openrouter.ai, still reachable)
```

> Order matters: the `ACCEPT` exceptions (lo, EgressAllowCIDRs) must precede the `DROP`s. Use `-A`
> on a fresh chain state per pod (each pod has its own netns, so there's no cross-pod accumulation).
> DNS to a public resolver is unaffected (public IP). DNS to an RFC1918 resolver (home LAN) would be
> dropped — if the node's resolver is on the LAN, add it to `EgressAllowCIDRs` (documented).

## 3. Mechanism

The node runs privileged on the home-box host (it already holds the Docker socket). To apply rules
to the pod netns:

1. `docker inspect` the sidecar container → `.State.Pid` (via the Docker SDK `ContainerInspect`).
2. `nsenter -t <pid> -n -- iptables <rule>` for each rule (enter the container's **net** namespace
   only; run the host's `iptables` against it).

Requires `iptables` + `nsenter` on the host and `CAP_NET_ADMIN` / root for the node process. These
are host prerequisites; document them. The container images need **no** new capabilities (rules are
applied from the host, not inside the container).

**Injection point** (`spawnlet/manager.go` `Create`): immediately after the sidecar `StartContainer`
returns its ID, before the agent `StartContainer`. So the netns is firewalled before the (untrusted)
agent ever runs.

## 4. Fail-closed

If inspect/nsenter/iptables fails (missing tool, no privilege, error), `Create` must:
1. log loudly (`egress floor failed for spawn <id>: <err>`),
2. stop the already-started sidecar,
3. return an error (spawn does not start).

A node-level config flag `EgressEnforce` (default **true**) exists ONLY so a developer on a platform
without iptables can explicitly opt out for local non-security testing; when false, log a loud
`WARNING: egress floor DISABLED` on every spawn. Default true = fail-closed.

## 5. Components / files

- `internal/spawnlet/firewall/firewall.go` — `Rules(cfg) []Rule` (pure rule builder) + `Apply(ctx,
  pid int, rules []Rule) error` (nsenter exec). `Rule` is a small struct (the iptables args).
- `internal/spawnlet/firewall/firewall_test.go` — unit test `Rules` (asserts the metadata + 3
  RFC1918 drops, lo accept, and EgressAllowCIDRs ordering). Hermetic.
- `internal/spawnlet/firewall/firewall_egress_e2e_test.go` — `//go:build egress_e2e`: start a real
  container, apply, assert 169.254.169.254 + an RFC1918 IP are unreachable and a public IP is
  reachable. Fail-loud, never silently skip (the build tag gates it; when built it must run for
  real). Needs privileged Docker + iptables.
- `internal/spawnlet/manager.go` — call `firewall.Apply` between sidecar and agent start;
  fail-closed.
- `cmd/spawnlet/main.go` — read `EgressEnforce` (default true) + `EgressAllowCIDRs` (csv) from env
  into the manager `Config`.
- `internal/spawnlet/manager.go` `Config` — add `EgressEnforce bool`, `EgressAllowCIDRs []string`.

## 6. Testing posture (honest)

- **Hermetic unit:** `Rules()` builds the expected rule set — runs anywhere, in CI.
- **Build-tagged integration** (`egress_e2e`): real enforcement; needs privileged Docker + iptables.
  **Confirmed: the dev sandbox has Docker but NO `iptables` and runs as non-root**, so this test
  **cannot run here** — it is written to **compile** (kept green via the build tag) and to **run on
  the deploy host** (the privileged home-box node). It must NOT silently skip when built (per the
  project's fail-loud-never-skip rule): a tagged run without iptables/privilege fails loudly rather
  than passing vacuously. The packet-drop guarantee is therefore **unverified in this environment**
  and must be validated on the node host — call this out at merge.
- Manager wiring is exercised by constructing the manager with `EgressEnforce:false` in existing
  spawnlet tests (so they don't require iptables) + a unit assertion that `EgressEnforce:true` with a
  failing applier aborts `Create` (inject a fake applier that errors → assert sidecar stopped + error).

## 7. Decision log

| # | Decision | Choice |
|---|---|---|
| R.1 | Policy model | Default-allow **block-floor**: DROP metadata (169.254/16) + RFC1918; ACCEPT lo + EgressAllowCIDRs; default ACCEPT (public ok) |
| R.2 | Mechanism | Host `nsenter -t <sidecar pid> -n -- iptables`; applied after sidecar start, before agent start |
| R.3 | Failure | **Fail-closed** — abort spawn + stop sidecar; `EgressEnforce=false` is an explicit, loud dev opt-out |
| R.4 | Model upstream | Public OpenRouter → no RFC1918 carve-out; `EgressAllowCIDRs` knob for LAN-model operators |
| R.5 | App-declared domains | **Out** — block-floor is static; allow-list/manifest plumbing is a later slice |
| R.6 | Manifest/CP/proto | **No changes** (floor is static, node-only) |
| R.7 | IPv6 | Out (follow-up bead); metadata is IPv4 |
| R.8 | Testability | unit (rule builder, hermetic) + build-tagged `egress_e2e` (real, privileged) + a fail-closed manager unit test with a fake applier |
