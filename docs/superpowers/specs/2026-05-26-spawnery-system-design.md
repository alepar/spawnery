# Spawnery — High-Level System Design

**Status:** Draft v1 (approved in brainstorming)
**Date:** 2026-05-26
**Scope:** Platform architecture, MVP-first. This document defines the full conceptual
architecture and tags each decision **MVP-now** or **deferred**. The LLM Wiki is App #1,
not a throwaway — the abstractions are designed so it is a genuine instance of the platform.

> **⚠️ Demo-MVP overlay:** This doc is the **full target architecture**. The first ship is a
> deliberately reduced **demo MVP** (execution = Spawnery home server only; inference = local
> DeepSeek only; no BYOK, no self-host, no burst, no vault, no E2E channel — everything audited).
> See **[Demo MVP Scope](2026-05-28-spawnery-demo-mvp-scope.md)** for what actually ships first vs.
> what this doc targets. Where the two differ on *what ships first*, the scope doc wins.

---

## 1. Thesis & product shape

**One-liner:** Spawnery is a marketplace for personal AI agents. A creator publishes an
"App" (a *coach*) once; anyone spawns their own private, personalized copy. **The App
provides the *how*; your repo + your model provide the *what*.**

Two non-negotiable principles:
- **Your data stays yours** — durable state lives in storage you own and can take with you.
- **Your choice of AI** — model-agnostic; you pick the provider.

### Editions (open-core)

| | Self-hosted edition | Cloud edition |
|---|---|---|
| **Delivery** | `docker compose` node agent(s) on a user's server or Mac, **attached to the central CP** | One-click SaaS, zero setup |
| **Apps** | Open Apps only | Open **and** private Apps |
| **Inference** | BYO keys, or call Spawnery's managed central gateway | BYO keys, or managed |
| **Audit** | None (their box) | Content audited for abuse (disclosed) |

The **cloud edition** splits into two subparts, invisible to customers:
- **(A) Home machine** — one always-on box running local **DeepSeek v4 Flash** for
  inference + the agent container runtime. Powers the **free tier** (capped usage, the
  starter Apps).
- **(B) Cloud burst** — when (A) saturates, the scheduler **auto-provisions cloud nodes**
  for inference *and* spawn containers. Same artifacts run unchanged.

**Open-core constraint (architectural):** the **control plane is a single, central,
Spawnery-managed service** (auth, index, catalog, scheduler, rendezvous). The *open /
self-hostable* surface is the **node/runtime side** — node agent, container runtime, model
sidecar, storage adapters — packaged as a `docker compose` stack that runs the same on a
user's box and on cloud-burst nodes. **Self-hosting = running node agent(s) that attach to the
central CP** (your hardware/repos/model). Note this is *not* a standalone stack: a self-hosted
node is **inert without the central CP** (it depends on the CP for enrollment, session tokens,
app/version resolution, and rendezvous) — running your **own** CP is post-MVP and loses hosted
web-UI access. The CP relays only E2E ciphertext and never sees *conversation* content; it does
hold scoped storage tokens to your connected repo (see §5). Beyond the CP, cloud-only:
managed-inference gateway (billed), private-App execution, marketplace payments/rev-share, and
the burst orchestrator. See [E0 contracts](2026-05-26-spawnery-e0-contracts-design.md) §1.

---

## 2. Domain model

> **⚠️ Lifecycle made explicit by [Spawn Lifecycle](2026-05-31-spawn-lifecycle-design.md):** the
> **Spawn** below is a durable, resumable instance with an explicit **active ↔ suspended** state
> machine (create · attach/detach · suspend · resume · delete). That spec is **authoritative for
> the spawn state machine**; the "instance binding …" description here is the data it carries.

- **App ("coach")** — a *definition repo* containing **`spawneryapp.yml`** (manifest) plus
  persona, skills/instructions, and the declared bundled toolset. Visibility is **open**
  (public, inspectable, runs anywhere incl. self-host) or **private** (closed, cloud-only).
- **Spawn** — a private instance binding `App@version + data repo + model config +
  personalization + (optional) conversation state`.
- **Two repo types, never confused:**
  - **Definition repo** (creator's) — the App; referenced by id + git tag.
  - **Data repo** (user's, per spawn) — the user's content, plus the spawn's own metadata.

### Source-of-truth split

- **`spawneryapp.yml`** (in the definition repo) — the App definition.
- **`spawn.yml`** (in the **data repo**) — the spawn's metadata and the **authoritative
  source of truth** for its configuration: App id + version (and pin / auto-upgrade state),
  model + provider selection, personalization values, storage schema version, and a
  conversation-history pointer. **Secrets are excluded** — BYO keys are never written here;
  they are injected at start via the end-to-end client-delivered path (§7).
- **Control plane** holds only a **thin index/pointer**: `owner → spawns → repo location +
  storage-provider binding + status/last-used`. Just enough to find and route to a spawn.

**Consequence (and its limits):** your **content** is fully portable — plain files in your own
git repo, leave anytime. The **spawn configuration** (`spawn.yml`) is reconstructable, but
re-spawning is not unconditionally portable: (a) there is currently **one central CP**, so
"re-spawn elsewhere" means re-spawn within Spawnery's managed control plane (own-CP is post-MVP);
and (b) `spawn.yml` references a *creator's* definition repo by `creator/app@<sha>` — if that App
is deleted or delisted, the spawn can't be reconstructed unless the definition was snapshotted.
So: **content = truly portable; the running spawn = Spawnery-hosted.** (Mitigation — snapshot the
App definition into the user's repo at spawn time — is post-MVP.)

---

## 3. Runtime & execution

> **⚠️ "Ephemeral, scale-to-zero" is the spawn lifecycle —
> [Spawn Lifecycle](2026-05-31-spawn-lifecycle-design.md) is authoritative:** "woken per session,
> torn down on idle" = **resume / suspend**. That spec defines the state machine
> (`starting·active·suspending·suspended·error·deleted`), two-stage per-node idle timeouts,
> **data-only** suspend/resume via a `git` WIP ref (uncommitted work preserved), single-session
> takeover, and crash→`suspended` reconciliation. **Requires a persistent storage backend (E3)** —
> `resume` is meaningless on `Scratch`.

- **No custom agent loop, no plugin registry (MVP).** A spawn runs an **existing agent** with
  its tools **bundled into the OCI image**. Spawnery drives the agent over **ACP — the Agent
  Client Protocol** (Zed's client↔agent JSON-RPC). Spawnery builds an *ACP orchestration
  layer*, not an agent.
- **An App is a configuration of an existing agent:** persona + skills (from the definition
  repo) + bundled toolset + mounted data repo + model contract + permissions.
- **Ephemeral, scale-to-zero.** The container is stateless compute: woken per session, torn
  down on idle. All durable state lives in the data repo + the CP index.
- **Isolation = pluggable backend** behind one orchestration interface
  (`start(image, mounts, token, limits) → handle` / `exec` / `stop`). Concrete backends are
  chosen per environment, **deferred**:
  - local dev → Docker/Podman
  - self-host / home machine → gVisor-class
  - cloud burst → microVM, or a VM-per-App with containers-per-spawn pooled on it
- **Model sidecar** per spawn — OpenAI-compatible (LiteLLM-style translation). The in-container
  agent talks to it over localhost; the sidecar routes to local DeepSeek / managed gateway /
  BYO provider. The sidecar is also the **metering point** for managed usage and the **audit
  point** for Spawnery-operated execution.

> Because in-container code is Spawnery's runtime + bundled (vetted) tools — **no arbitrary
> creator code** — the sandbox-escape threat is largely engineered away. Isolation is
> defense-in-depth, not the primary trust mechanism.

---

## 4. Hosting topology

- **One central control plane + node agents that dial out to it.** Node agents (home,
  self-host, burst) hold a persistent **outbound gRPC stream** to the central CP — NAT-agnostic,
  no inbound reach required. The scheduler places spawns **local-first** and **auto-bursts to
  cloud** when the home machine saturates: it provisions a cloud VM running the same node agent;
  the identical OCI image runs unchanged.
- **Placement is decided at spawn start** (so the sidecar knows whether to audit). Mid-session
  migration is out of scope.
- **Connection = CP rendezvous + E2E relay** (LAN-direct when reachable). See [E0](2026-05-26-spawnery-e0-contracts-design.md) §10.
- **Resilience seams (roast `sp-jf7`/`sp-9um`).** The single CP and single home machine are
  single points of failure. Full design: the **relay is a stateless, horizontally-scaled tier**
  (separate from control logic) with reconnect-without-re-auth; **CP state is backed up /
  replicated** (PITR) and provider secrets live in **KMS**; the home machine has **dead-node →
  cloud failover** (not just overload → burst). **Demo MVP** accepts the singletons behind a
  **capped/waitlisted beta + status page**, but still requires **CP-state backup** so a box loss
  isn't unrecoverable. See [Demo MVP Scope](2026-05-28-spawnery-demo-mvp-scope.md) §6.

```
   Browser / mobile (ACP client)
        │  1) session token + rendezvous (HTTP)   2) ACP, E2E-encrypted to the NODE
        ▼                                            (node decrypts, forwards to agent on loopback)
   ┌──────────────┐    persistent outbound   ┌───────────────────────────────────┐
   │ Central CP   │    gRPC + E2E relay       │  Node agent  ── spawn pod (ephemeral)
   │ - auth       │◀─────────────────────────│   - ACP-bridge (ACP↔agent on loopback)
   │ - catalog    │   (relays opaque bytes)  │   - existing agent (ACP/stdio)     │
   │ - scheduler  │──────────────────────────▶   - common toolset + /data mount (E3)
   │ - CP index   │      startSpawn           │   - model sidecar (localhost)──────┼─▶ DeepSeek (home)
   │ - rendezvous │                          └───────────────────────────────────┘   / central gw
   └──────────────┘     node: home / self-host / burst                                / BYO provider
   (single, managed)
   NOTE: the per-session E2E channel terminates at the NODE (not the bridge). DEMO MVP uses plain
   TLS client↔CP (no E2E channel) — everything Spawnery-operated + audited. See demo-mvp-scope.
```

---

## 5. Data & storage

> **⚠️ Amended by [Per-Mount Data Backends](2026-05-29-data-mounts-design.md):** the substrate is a
> **set of named data mounts inside `/app`** (`/app/<path>`, cwd=`/app`), each independently seeded
> and **independently backed** (scratch / git / blob / managed) — not a single `/data` repo. Bindings
> are per-mount, by name, in `spawn.yml`. The git-repo-of-files description below applies *per
> persistent mount*.

- **Universal substrate = a git repo of files** (per persistent mount). Each App declares named data
  folders inside `/app`; each is a working tree. History, diffing, and "clone it anywhere" come free.
  - *Deferred:* a high-write / large-binary App would need a DB-backed storage provider; the
    abstraction allows adding one. Not built now.
- **Provider adapters** do two things: *materialize* the repo into `/data` at session start,
  and *persist* it at checkpoints / session end. Two families:
  - **Git-native** — **GitHub** via a fine-grained **GitHub App** (per-repo grant only;
    never broad `repo` scope). Data lives literally in the user's GitHub. (GitLab/Gitea/
    self-hosted later.)
  - **Blob/file** — Drive / OneDrive / iCloud. **MVP persists a single `git bundle`**
    (`git bundle create --all`) — git-native, integrity-checked, clonable; better than
    tarring `.git`. *Deferred:* human-readable mirror; bidirectional sync; folder-as-git-remote.
- **Storage is per-App optional** (zork barely needs it).
- **Concurrency:** spawns are ephemeral + single-session → assume **single-writer** with
  non-fast-forward / last-write-wins conflict detection, not real merge.
- **Conversation history** is **optionally** committed to the data repo so it travels with
  the user (pointer recorded in `spawn.yml`).
- **Honest custody note (roast `sp-gl2`):** with the one-click GitHub-App grant, the **CP holds
  the signer and can mint scoped read/write tokens to your connected repo** server-side. "Your
  data is yours" means *your content lives in your account in open formats you can take and
  delete* — **not** that Spawnery is technically unable to touch it. A **user-visible
  storage-access log** (every token mint + push, with reason) is the honesty mechanism; ship it
  with storage. User-held / E2E-sealed storage creds (so the CP genuinely can't) are post-MVP.

---

## 6. Model layer

- **Per-spawn model-gateway sidecar** (OpenAI-compatible), configured per spawn via
  env/secrets: provider, model, base URL, key.
- **Managed inference** — provider keys live **only in the cloud central gateway** (also the
  metering point). The sidecar holds a short-lived Spawnery token and routes
  `sidecar → central gateway → provider`; the gateway selects local DeepSeek (home) or cloud
  models (burst). Keys never enter spawn containers.
- **BYO inference** — `sidecar → provider` directly (no central gateway). Keys delivered
  end-to-end (§7). No central path, audited only if the spawn runs on Spawnery-operated cloud.
- **Model contract** — each App declares required capabilities (tool-use, min context window,
  vision, structured output) + a recommended default; the catalog filters to compatible
  models so "model-agnostic" never silently breaks an App.

---

## 7. Identity, auth & secrets

- **Auth (default; revisitable):** OAuth login (**Google + GitHub**; GitHub doubles as the
  storage connection) **plus a separate, server-blind vault passphrase** that roots BYO-secret
  encryption. One account type; "creator" is a role. *Deferred:* passkeys + WebAuthn-PRF.
- **BYO secret delivery (cloud):** the key is encrypted client-side with the vault passphrase;
  the control plane stores **only ciphertext it cannot read** (or it lives in client local
  storage). On connect, the **client decrypts locally and sends the key over the per-session E2E
  channel to the node, which injects it into the sidecar** (the CP relays only opaque bytes; it
  never holds plaintext). See [E2 §2](2026-05-27-spawnery-e2-model-layer-design.md) /
  [E0 §10](2026-05-26-spawnery-e0-contracts-design.md). *Honesty caveat:* on **Spawnery-operated
  cloud** nodes, the node decrypts the BYO key in memory and content is **audited** — so "we never
  see your key/content" holds for **self-host only**, not the cloud/free path.
- **Self-host:** secrets are local config; the CP is not in the loop.
- *Demo MVP:* no BYOK, no vault — see [Demo MVP Scope](2026-05-28-spawnery-demo-mvp-scope.md).

---

## 8. App lifecycle & marketplace

- **Manifest (`spawneryapp.yml`):** identity (id, semver, creator, title, icon, tags),
  persona + skill files, **agent + bundled toolset**, **model contract**, **storage schema +
  seed**, **permissions** (storage scope + egress allowlist), **personalization fields** (the
  typed "what" the user fills at spawn).
- **Versioning:** immutable, content-addressed versions = git tags. **Auto-upgrade to the
  latest reviewed tag, opt-out pin**, with guardrails:
  - **Permission escalation breaks auto-upgrade** → explicit **re-consent** required.
  - **Pre-upgrade git snapshot** (tag) for rollback before any migration touches `/data`.
  - Changelog notice surfaced even on silent upgrades.
- **Publishing:**
  - **Open Apps → open registry** — publish instantly; trust via inspectable public repo +
    ratings / flagging / reactive takedowns.
  - **Private Apps → human review** before listing/sale (cloud-only, opaque).
- **Catalog** = a Spawnery-hosted index over published definition repos; the self-host edition
  reads the open-App index.

---

## 9. Trust, safety & audit

- **Permission/consent at spawn — POST-MVP (roast `sp-ba5`).** The *target* design has the user
  consent to the App's declared storage scope + egress domains, with the runtime **enforcing an
  egress allowlist** (owner = E1, at the pod network layer; note the allowlist is per-spawn-dynamic
  because a BYO `baseUrl` varies). **This consent screen + egress enforcement are NOT in MVP** —
  E8's audit + scanner are the MVP safety net in their absence. Don't present consent as shipped.
- **Audit by environment:** **Spawnery operates the box → conversation content is audited for
  abuse** (at the sidecar, disclosed) — this covers the home machine (incl. free tier) **and**
  cloud burst. **User self-hosts → no audit.**
  - Edge case: a self-hoster using the managed gateway is *metered* (token counts) but **not
    content-audited**, since the audit point is the sidecar on their own box.
- **Trust topology:** open = inspectable (self-policing); private = reviewed + cloud-only +
  audited. No arbitrary creator code (§3) keeps the attack surface small.

---

## 10. Client / connection

*(Refined in [E0](2026-05-26-spawnery-e0-contracts-design.md) §10 — CP rendezvous + E2E relay.)*

- **CP is the rendezvous.** Client gets a signed session token from the central CP, which also
  triggers **wake-from-zero** (`startSpawn` over the node's outbound gRPC stream).
- **Data path:** **LAN-direct when reachable**, else an **E2E-encrypted relay through the CP**
  over the node's outbound stream — the CP pipes **opaque ciphertext**. The **per-session E2E
  channel terminates at the NODE** (node-static + client-ephemeral key agreement; the client pins
  the CP-vended node key), and the node forwards ACP to the in-container bridge over **loopback**.
  (The bridge does *not* terminate a separate client-facing TLS — superseded; see
  [E0 §10](2026-05-26-spawnery-e0-contracts-design.md).)
- **ACP over the authenticated channel**; web + mobile are identical clients. P2P deferred;
  BYO-ingress (Tailscale/Cloudflare Tunnel) bypasses the relay.
- *Demo MVP:* no E2E channel — **plain TLS client↔CP**, everything Spawnery-operated + audited.

---

## 11. The flagship grounded: LLM Wiki

- Data repo = Markdown pages + links. The **existing agent reads / greps / edits files and
  commits** — exactly how a coding agent works a codebase.
- **MVP retrieval = file navigation + full-text search** (bundled tool, e.g. `qmd`).
  *Deferred:* embeddings / vector index.
- **Spawn flow:** connect GitHub (or a managed repo) → pick a model → seed an empty wiki →
  chat to grow it. This is both the demo and the demand magnet.

---

## 12. Decomposition & build order

Each sub-project gets its own spec → plan → build cycle.

**Sub-projects:**
1. **Runtime & orchestration core** — control plane, node agent, container lifecycle
   (ephemeral/scale-to-zero), ACP orchestration, isolation abstraction.
2. **Model layer** — per-spawn sidecar, central gateway, BYO e2e delivery, metering.
3. **Storage layer** — uniform `/data` mount + adapters (GitHub App; blob/`git bundle`).
4. **Identity & secrets** — OAuth + vault passphrase + client-side secret delivery.
5. **App packaging & catalog** — manifest spec, definition-repo format, versioning, open
   registry + private review, marketplace UI.
6. **Web client** — browse, spawn wizard, chat over the direct ACP channel.
7. **Launch coach repos** — zork, llm-wiki, habit/goal coach, system-design-interview coach.
8. **Trust / safety / audit.**
9. **Cloud burst** (autoscaler/provisioner) — later.
10. **Billing / payments / rev-share** — later.

**Recommended order:**
- **Vertical slice first:** core runtime (#1) + model sidecar on local DeepSeek (#2 minimal) +
  minimal web client (#6) + **zork** (#7, no storage) → proves the whole spawn→chat loop
  end-to-end with the fewest moving parts.
- Then **storage (#3) + GitHub** → ship the **LLM Wiki** flagship (the wedge).
- Then the other two coaches, identity/secrets hardening (#4), catalog/marketplace (#5),
  audit (#8).
- **Burst (#9) and monetization (#10) last.**

---

## 13. Monetization (MVP stance)

Free tier now (home machine + local DeepSeek, capped, the 4 starter Apps) + BYOK. Build the
**metering + account seams** so premium models/burst, private Apps, and creator rev-share can
switch on later. **No payments/payouts built in MVP** — validate usage before monetizing.

---

## 14. Explicitly deferred (designed-for, not built now)

Plugin/extension registry (likely MCP) · embeddings/vector retrieval · blob-provider
readable mirror & bidirectional sync · folder-as-git-remote · DB-backed storage provider ·
concrete isolation backends · cloud-burst autoscaler · payments / rev-share / payouts ·
passkeys + WebAuthn-PRF.

---

## Appendix A — Decision log

> **Supersession note (roast `sp-izq`):** this log records the original brainstorming decisions.
> Several were refined by the later epic designs and the demo cut — read it with these overrides:
> **#2/#18** "tools bundled in the image" → **per-agent base image + mounted App definition**
> (E1 §2); **#10** "direct client→container via edge activator" → **CP rendezvous + per-session
> E2E relay, terminating at the node** (E0 §10); **#13** "direct-in-container BYO key" → **delivered
> over the E2E channel; node injects** (E2 §2). And the **demo MVP** ships none of the E2E/BYO/
> gateway/vault machinery (see [Demo MVP Scope](2026-05-28-spawnery-demo-mvp-scope.md)).

| # | Decision | Choice |
|---|---|---|
| 1 | Scope/altitude | Platform arch, MVP-first |
| 2 | App capability model | Spawnery-owned runtime; tools bundled in the image (no plugin registry yet) |
| 3 | Runtime ownership | Spawnery owns the runtime; creators configure existing agents |
| 4 | Container lifecycle | Ephemeral, scale-to-zero |
| 5 | Build vs partner (compute) | Self-hosted day one + automatic cloud burst |
| 6 | Isolation tech | Pluggable backend; concrete impls deferred |
| 7 | Data ownership | BYO storage: GitHub (native git) + blob providers (Drive/OneDrive/iCloud) |
| 8 | Blob persistence | `git bundle` only (MVP) |
| 9 | Versioning | Auto-upgrade + opt-out pin, with escalation/snapshot/changelog guardrails |
| 10 | Connection topology | Direct client→container via edge activator |
| 11 | Conversation state | In the user's data repo (optional) |
| 12 | Model gateway | Per-spawn sidecar; managed via cloud central gateway |
| 13 | BYO key path | Direct-in-container, end-to-end client-delivered |
| 14 | Managed key location | Cloud central gateway only |
| 15 | Audit scope | All Spawnery-operated cloud (home + burst); self-host exempt |
| 16 | Auth | OAuth + separate server-blind vault passphrase (default) |
| 17 | Open-core line | Core open; private Apps / managed gateway / payments / burst cloud-only |
| 18 | Plugin interface | Tools bundled in the image (MVP); registry deferred |
| 19 | Agent runtime | ACP + existing agents (no custom loop) |
| 20 | Publish/review | Open registry for open Apps; human review for private |
| 21 | Monetization | Free tier now; paid + rev-share later |
| 22 | Manifest / spawn metadata | `spawneryapp.yml` (definition repo); `spawn.yml` (data repo, source of truth, secrets excluded) |
