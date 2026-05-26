# Spawnery E0 — Cross-component APIs & Contracts (Design)

**Bead:** `sp-9zo`
**Status:** Draft v1 (interview complete; pending user review)
**Date:** 2026-05-26
**Parent:** [System design](2026-05-26-spawnery-system-design.md)

This epic defines the seams every other epic consumes. It is **design-first**: components
are built against these contracts. Altitude is "contract spec" — purpose, shape, key fields/
methods, and the decisions behind them — not full IDL (each component epic refines its own).

---

## 1. Service topology & communication

**Modular-monolith control plane + per-host node agent.**

| Unit | Process | Contains | Deploy |
|---|---|---|---|
| **Control plane (CP)** | one process (modular) | auth, CP index, catalog/registry, scheduler | compose + cloud |
| **Node agent** | one per host | container orchestration, storage adapters, edge activation, rendezvous relay endpoint | every node |
| **Central gateway** | one service | managed-key custody, inference routing, metering | **cloud-only** |
| **Spawn** (per session) | container | existing agent (ACP/stdio) + **ACP-bridge** + **model sidecar** | on a node |
| **Web/mobile client** | client | ACP client UI | — |

**Seams:**
- **Network:** HTTP/JSON (service↔service, client↔CP), JSON-RPC/ACP (client↔agent),
  OpenAI-compatible HTTP (agent↔sidecar), HTTP (sidecar↔central gateway).
- **In-process (inside node agent):** container-orchestration interface, storage-adapter
  interface.

**Format conventions:** **JSON Schema** for data contracts (`spawneryapp.yml`, `spawn.yml`,
permission/egress), **OpenAPI** for service HTTP APIs, **JSON-RPC** for ACP, **OpenAI-compatible
HTTP** for the sidecar. **Service-to-service auth = signed service tokens.**

---

## 2. Identity & addressing (threads through every contract)

- **App:** human handle **`creator/app`** (1:1 with the definition repo), backed by an
  immutable internal **UUID** (survives renames/transfers).
- **App version:** **semver git tag** (`v1.4.0`) resolving to an immutable **commit SHA**.
  Stored/referenced as **`creator/app@<sha>`**, displayed as `@v1.4.0`. Auto-upgrade tracks
  the latest reviewed tag's SHA; a pin stores a SHA.
- **Spawn / User:** server-generated **UUIDs**.
- **Data repo:** **provider-scheme URI** — `github:owner/repo`, `gdrive:<fileId>`,
  `onedrive:<id>`, `icloud:<id>`.
- **Node:** UUID + advertised reachability.

---

## 3. `spawneryapp.yml` — App manifest (definition repo root)

JSON-Schema-validated. Baked into the assembled `App@<sha>` image.

```yaml
apiVersion: spawnery/v1
kind: App
id: alice/llm-wiki                 # must match the definition repo
title: LLM Wiki
description: A personal knowledge base your agent grows with you.
icon: ./icon.png
tags: [knowledge, notes]
visibility: open                   # open | private (private => cloud-only)

agent:                             # the existing agent the runtime drives over ACP
  ref: <agent-id>
  version: <semver>
tools:                             # bundled into the image (no registry in MVP)
  - <tool-ref>                     # e.g. qmd
persona: ./persona.md              # system prompt
skills:                            # instruction files
  - ./skills/*.md

model:                             # capability contract; catalog filters compatible models
  requires: { toolUse: true, minContextTokens: 32000, vision: false, structuredOutput: false }
  recommendedDefault: <model-id>

storage:
  required: true                   # false for e.g. zork
  schema: ./storage-schema.md      # expected /data layout
  seed: ./seed/                    # scaffold for a fresh spawn

permissions:                       # declared; user consents at spawn
  storageScope: [read, write]      # within the spawn's data repo
  egress: [api.github.com, <provider-host>]   # domain allowlist

personalization:                   # the typed "what" the user fills at spawn (JSON-Schema-ish)
  - { key: displayName, type: string, label: "Your name", required: false }
  - { key: topics,      type: string[], label: "Focus topics" }

runtime: { baseVersion: ">=1.0" }
```

---

## 4. `spawn.yml` — spawn metadata (in the user's data repo; source of truth)

Secrets **never** appear here. CP holds only a thin pointer/index to this.

```yaml
apiVersion: spawnery/v1
kind: Spawn
spawnId: 3f2a...                   # UUID
owner: 9c1e...                     # user UUID
app:
  ref: alice/llm-wiki@<sha>
  display: v1.4.0
  versionPolicy: auto              # auto | pinned
  pinnedSha: null
model:
  mode: managed                    # managed | byo
  provider: deepseek
  model: deepseek-v4-flash
  baseUrl: null                    # set for byo/self-host; key is NEVER here
personalization: { displayName: "Alice", topics: ["ml","systems"] }
storage:
  binding: github:alice/my-wiki
  schemaVersion: 1
conversation: { persisted: true, path: .spawnery/threads/ }
permissions:                       # snapshot of what the user consented to
  storageScope: [read, write]
  egress: [api.github.com, api.deepseek.com]
createdAt: 2026-05-26T12:00:00Z
```

> **Portability payoff:** a spawn is reconstructable from `spawn.yml` alone — carry the data
> repo to any Spawnery (self-host or cloud) and re-spawn the identical App.

---

## 5. Control-plane HTTP APIs (OpenAPI)

### 5a. CP index API
Holds `owner → spawns → {data-repo binding, storage provider, status, last-used, node assignment}`.
- `POST /spawns` — create (writes `spawn.yml` into the new/initialized data repo; records pointer)
- `GET /spawns` / `GET /spawns/{id}` — list / resolve
- `POST /spawns/{id}/session` — issue a **signed session token** + rendezvous endpoint (§9, §11)
- `PATCH /spawns/{id}` — status; `DELETE /spawns/{id}` — clean exit (drops pointer; data stays in repo)

### 5b. Catalog/registry API
- `GET /apps` — browse/search (visibility-scoped: open public; private requires entitlement)
- `GET /apps/{creator}/{app}` — listing + versions
- `GET /apps/{creator}/{app}/resolve?ref=v1.4.0` → `{ sha, manifest }`
- `POST /apps` — publish: **open = instant** (after automated checks); **private = review queue**
- ratings / flags / takedown endpoints
- Self-host reads the **open** index read-only.

---

## 6. CP ↔ node-agent protocol (node dials out)

Each node opens a **persistent outbound stream** to the CP (gRPC stream / WebSocket JSON-RPC)
and authenticates with a service token. NAT-agnostic; uniform for home, self-host, and burst nodes.

- **node → CP:** `register`, `heartbeat{capacity, health}` (feeds local-first placement + burst
  trigger), `spawnStatus{spawnId, state}`, **relay data frames** (for the rendezvous, §9).
- **CP → node:** `startSpawn{imageRef, mounts, modelConfig, consent+egressPolicy, sessionTokenPubkey}`,
  `stopSpawn{spawnId}`, **relay data frames**.

---

## 7. ACP orchestration contract (client ↔ agent)

Spawnery drives an **existing agent** over **ACP (Agent Client Protocol, JSON-RPC)**. The
in-container **ACP-bridge** wraps the stdio agent and exposes ACP over an **authenticated
WebSocket** (TLS terminates in the container; §9).

- **Bake vs inject:** the image **is** `App@<sha>` with persona + skills + manifest + bundled
  tools baked in. **Injected at session start:** personalization config, model config (points
  the agent at the sidecar), the `/data` mount as `cwd`, the consented permission set, the
  session token.
- **ACP surface used:** `initialize` (capability negotiation), `session/new` (cwd=`/data`,
  injected config), `session/prompt`, `session/update` (streamed output/thoughts/tool-calls),
  `session/request_permission` (→ the interactive consent layer, §10), `session/cancel`.
- **Agent requirements:** speaks ACP; honors `cwd`; accepts a configurable model endpoint;
  surfaces tool calls + permission requests.

---

## 8. In-process interfaces (inside the node agent)

### 8a. Container orchestration (isolation backend — pluggable)
```
start(imageRef, mounts, env/secrets, egressPolicy, limits) -> handle
attach/exec(handle, ...) ; status(handle) ; stop(handle)
```
Backends: Docker/Podman (local) · gVisor-class (self-host/home) · microVM or
VM-per-App+containers-per-spawn (cloud burst). Chosen per environment (concrete impls deferred).

### 8b. Storage adapter
```
materialize(binding, dataDir) -> working tree at /data
persist(binding, dataDir, checkpoint)
capabilities() -> { gitNative | blob, ... }
```
- **Adapters:** GitHub (App, per-repo: clone/pull + commit/push) · blob (Drive/OneDrive/iCloud:
  materialize-from-`git bundle`, persist-as-`git bundle`).
- **Persist policy:** agent makes **semantic commits**; runtime **persists per completed turn**
  + **idle/exit autosave**. One-turn loss window.
- **Conflict:** non-fast-forward detection → last-write-wins + surfaced (no auto-merge in MVP).

---

## 9. Model sidecar + central-gateway protocol

- **Sidecar** exposes an **OpenAI-compatible** HTTP endpoint on localhost to the agent;
  configured per spawn (provider, model, base URL, key source).
- **Managed mode:** `sidecar → central gateway` (bearer = CP-issued short-lived token) `→ provider`.
  Gateway custodies managed keys, routes to **local DeepSeek (home)** or **cloud models (burst)**,
  and emits **metering events** to the CP.
- **BYO mode:** `sidecar → provider` directly. The **plaintext key is delivered by the client**
  over the authenticated channel into the sidecar at session start; at rest it is e2e-encrypted
  with the user's vault passphrase (CP/local stores ciphertext only).
- **Audit hook:** when the spawn runs on **Spawnery-operated** infra (home or burst), the sidecar
  logs conversation content to the audit store (abuse-only, disclosed). Self-host → no audit.

---

## 10. Permission / consent / egress

- **Declared** in the manifest (`permissions.storageScope`, `permissions.egress`).
- **Consented** at spawn → snapshot written to `spawn.yml` + CP pointer.
- **Enforced** two layers:
  - **Hard boundary (network):** node configures a **per-spawn egress proxy/firewall** from the
    allowlist; only model-provider + storage + declared tool domains pass; all else blocked
    regardless of in-container behavior.
  - **Interactive layer (ACP):** `session/request_permission` surfaces sensitive actions and
    dynamic escalation to the client for approval.
- **Upgrade escalation:** a new App version requesting broader scope/egress **breaks
  auto-upgrade → re-consent** (per system-design §8).

---

## 11. Edge-activator session protocol & rendezvous

- CP issues a **signed session token** (claims: `spawnId`, `owner`, `node`, `exp`, **bridge cert
  fingerprint**) via `POST /spawns/{id}/session`.
- **Rendezvous:** CP is always reachable; the node holds an outbound stream (§6). Data path is
  **LAN-direct when reachable**, else **E2E-encrypted relay** over the node's outbound tunnel —
  CP pipes **opaque ciphertext** (TLS terminates at the in-container ACP-bridge; the client pins
  the bridge cert via the token fingerprint). **P2P/hole-punching deferred.** BYO-ingress
  (Tailscale/Cloudflare Tunnel) bypasses the relay.
- **Wake-from-zero:** if the target spawn is cold, the CP instructs the node to `startSpawn`,
  then establishes the path. Web and mobile are identical ACP clients.

---

## 12. Open items deferred out of E0
P2P/hole-punching transport · third-party plugin/extension registry (MCP) · richer conflict
merge · multi-version concurrent sessions on one spawn · catalog federation for self-host.

---

## Appendix — E0 decision log

| # | Decision | Choice |
|---|---|---|
| E0.1 | Service topology | Modular-monolith CP + per-host node agent + cloud-only central gateway |
| E0.2 | App/version identity | `creator/app` handle (UUID-backed) + semver tag → commit SHA |
| E0.3 | Contract formats | JSON Schema / OpenAPI / JSON-RPC(ACP) / OpenAI-compatible; signed service tokens |
| E0.4 | CP↔node control | Node dials out, holds persistent stream; outbound-only |
| E0.5 | Bake vs inject | Definition baked into `App@sha` image; per-spawn config/model/data/perms/token injected |
| E0.6 | ACP transport | In-container ACP-bridge over authenticated WebSocket; client = ACP client |
| E0.7 | Rendezvous | CP rendezvous; LAN-direct else E2E relay; P2P deferred; BYO-ingress bypass |
| E0.8 | Persistence | Semantic commits + persist per completed turn + idle/exit autosave |
| E0.9 | Permission/egress | Network-level per-spawn egress proxy (hard) + ACP request_permission (interactive) |
