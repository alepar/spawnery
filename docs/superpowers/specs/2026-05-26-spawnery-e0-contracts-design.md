# Spawnery E0 — Cross-component APIs & Contracts (Design)

**Bead:** `sp-9zo`
**Status:** Draft v2 (interview + first review corrections; pending re-review)
**Date:** 2026-05-26
**Parent:** [System design](2026-05-26-spawnery-system-design.md)

This epic defines the seams every other epic consumes. **Design-first**: components are built
against these contracts. Altitude = "contract spec" — purpose, shape, key fields/methods, and
the decisions behind them — not full IDL (each component epic refines its own).

> **Scope note (review v2):** storage details are owned by **E3** (only referenced here, not
> specified). **Personalization** and **permissions/consent/egress** are **post-MVP** (see
> `TODO.md`) and are intentionally absent from the MVP contracts below.

---

## 1. Service topology & communication

**One central managed control plane + per-host node agents.**

| Unit | Cardinality / process | Contains | Where it runs |
|---|---|---|---|
| **Control plane (CP)** | **single, central, Spawnery-managed** | auth, CP index, catalog/registry, scheduler, **rendezvous relay** | Spawnery cloud only |
| **Node agent** | one per host | container orchestration, storage adapters, node end of the relay tunnel | home machine · cloud-burst nodes · **self-host** |
| **Central gateway** | one service | managed-key custody, inference routing, metering | Spawnery cloud only |
| **Spawn** (per session) | container | existing agent (ACP/stdio) + **ACP-bridge** + **model sidecar** | on a node |
| **Web/mobile client** | client | ACP client UI | — |

**Centralized control, distributed execution.** Self-hosting means running **node agents that
attach to the central CP**: your hardware runs the spawns, your repos hold the data, your model
serves inference — the CP only does discovery/auth/scheduling and relays **E2E ciphertext** (it
never sees content). This amends the open-core line: the *open / self-hostable* surface is the
**node/runtime side** (node agent, container runtime, sidecar, storage adapters); the **CP is a
managed service**, and self-host **reads the central open-App catalog** over the network.

**Seams:**
- **Service-to-service: gRPC** (CP↔node — the node's persistent outbound stream; CP↔central gateway).
- **Client↔CP:** HTTP/JSON (OpenAPI); gRPC-web optional.
- **Client↔agent:** JSON-RPC / **ACP** (over the authenticated WebSocket from the ACP-bridge).
- **Agent↔sidecar:** OpenAI-compatible HTTP (localhost).

**Format conventions:** **gRPC/protobuf** for s2s · **JSON Schema** for data contracts
(`spawneryapp.yml`, `spawn.yml`) · **OpenAPI/HTTP+JSON** for client-facing CP APIs · **JSON-RPC**
for ACP · **OpenAI-compatible HTTP** for the sidecar. **s2s auth = signed service tokens.**

---

## 2. Identity & addressing

- **App:** human handle **`creator/app`** (1:1 with the definition repo), backed by an immutable
  internal **UUID** (survives renames/transfers).
- **App version:** **semver git tag** (`v1.4.0`) → immutable **commit SHA**. Stored as
  **`creator/app@<sha>`**, displayed `@v1.4.0`. Auto-upgrade tracks the latest reviewed tag's SHA;
  a pin stores a SHA.
- **Spawn / User:** server-generated **UUIDs**.
- **Data repo:** **provider-scheme URI** (`github:owner/repo`, `gdrive:<id>`, …) — owned by **E3**.
- **Node:** UUID + advertised reachability.

---

## 3. `spawneryapp.yml` — App manifest (definition repo root)

JSON-Schema-validated. **Apps are agent-agnostic** — the agent runtime is chosen at spawn time;
the manifest declares only *compatibility*. **Skills are imported by the agent through its own
normal process** (the App ships skill files; the chosen agent loads them natively).

```yaml
apiVersion: spawnery/v1
kind: App
id: alice/llm-wiki                 # must match the definition repo
title: LLM Wiki
description: A personal knowledge base your agent grows with you.
icon: ./icon.png
tags: [knowledge, notes]
visibility: open                   # open | private (private => cloud-only)

agents:                            # agent-agnostic; chosen at spawn time
  support: any                     # any | [list of agent ids]
  exclude: []                      # optionally declared-unsupported agents
  requiresAcp: [prompt, tools]     # required ACP capabilities

tools:                             # bundled into the assembled image (no registry in MVP)
  - qmd
persona: ./persona.md              # system prompt
skills:                            # instruction files; imported via the agent's normal process
  - ./skills/*.md

model:                             # capability contract; catalog filters compatible models
  requires: { toolUse: true, minContextTokens: 32000, vision: false }
  recommendedDefault: deepseek-v4-flash

runtime: { baseVersion: ">=1.0" }

# storage:        -> owned by E3 (see TODO/E3 design)
# personalization -> POST-MVP (TODO.md)
# permissions     -> POST-MVP (TODO.md)
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
agent:                             # chosen at spawn (must satisfy manifest agents.*)
  id: <agent-id>
  version: <semver>
model:
  mode: managed                    # managed | byo
  provider: deepseek
  model: deepseek-v4-flash
  baseUrl: null                    # set for byo/self-host; key is NEVER here
conversation: { persisted: true, path: .spawnery/threads/ }
storage: { binding: github:alice/my-wiki }   # shape owned by E3; optional (e.g. zork: none)
createdAt: 2026-05-26T12:00:00Z
# personalization / permissions -> POST-MVP (TODO.md)
```

> **Portability payoff:** a spawn is reconstructable from `spawn.yml` alone — carry the data repo
> to any Spawnery node and re-spawn the identical App with the same agent + model choices.

---

## 5. Control-plane APIs

### 5a. CP index API (HTTP/OpenAPI, client-facing)
Holds `owner → spawns → {data-repo binding, status, last-used, node assignment}`.
- `POST /spawns` — create (initializes the data repo + writes `spawn.yml`; records pointer)
- `GET /spawns` / `GET /spawns/{id}` — list / resolve
- `POST /spawns/{id}/session` — issue a **signed session token** + rendezvous endpoint (§9, §11)
- `PATCH /spawns/{id}` — status; `DELETE /spawns/{id}` — clean exit (drops pointer; data stays in repo)

### 5b. Catalog/registry API (HTTP/OpenAPI)
- `GET /apps` — browse/search (visibility-scoped; private requires entitlement)
- `GET /apps/{creator}/{app}` — listing + versions
- `GET /apps/{creator}/{app}/resolve?ref=v1.4.0` → `{ sha, manifest }`
- `POST /apps` — publish: **open = instant** (after automated checks); **private = review queue**
- ratings / flags / takedown
- Self-host attaches to the central CP and reads the **open** index read-only.

---

## 6. CP ↔ node-agent protocol (gRPC; node dials out)

Each node opens a **persistent outbound gRPC stream** to the central CP, authenticated with a
service token. NAT-agnostic; uniform for home, self-host, and burst nodes.

- **node → CP:** `register`, `heartbeat{capacity, health}` (feeds local-first placement + burst
  trigger), `spawnStatus{spawnId, state}`, **relay frames** (the node end of the rendezvous, §9).
- **CP → node:** `startSpawn{imageRef, mounts, modelConfig, sessionTokenPubkey}`,
  `stopSpawn{spawnId}`, **relay frames**.

---

## 7. ACP orchestration contract (client ↔ agent)

Spawnery drives a **spawn-time-chosen** existing agent over **ACP (JSON-RPC)**. The in-container
**ACP-bridge** wraps the stdio agent and exposes ACP over an **authenticated WebSocket** (TLS
terminates in the container; §9).

- **Image assembly (agent-agnostic):** the assembled image = **base runtime + the chosen agent +
  the App's bundled tools**, built per `(App@<sha>, agent)` and cached. The App's persona + skills
  are placed where the agent expects them so it **imports skills via its normal process**.
- **Injected at session start:** model config (points the agent at the sidecar), the `/data` mount
  as `cwd` (storage owned by E3), the session token.
- **ACP surface used:** `initialize`, `session/new` (cwd=`/data`), `session/prompt`,
  `session/update` (streamed output/thoughts/tool-calls), `session/cancel`.
  (`session/request_permission` exists in ACP but its consent/egress *enforcement* is **post-MVP**.)
- **Agent requirements:** speaks ACP; honors `cwd`; accepts a configurable model endpoint;
  imports skills via its normal mechanism.

---

## 8. In-process interfaces (inside the node agent)

### 8a. Container orchestration (isolation backend — pluggable)
```
start(imageRef, mounts, env/secrets, limits) -> handle
attach/exec(handle, ...) ; status(handle) ; stop(handle)
```
Backends: Docker/Podman (local) · gVisor-class (self-host/home) · microVM or
VM-per-App+containers-per-spawn (cloud burst). Chosen per environment (concrete impls deferred).

### 8b. Storage adapter
Interface + persistence policy (semantic commits, persist-per-turn, blob `git bundle`, conflict
handling) are **owned by E3**. Referenced here only as a node-agent in-process seam.

---

## 9. Model sidecar + central-gateway protocol

- **Sidecar** exposes an **OpenAI-compatible** HTTP endpoint on localhost; configured per spawn
  (provider, model, base URL, key source).
- **Managed mode:** `sidecar → central gateway` (bearer = CP-issued short-lived token) `→ provider`.
  Gateway custodies managed keys, routes to **local DeepSeek (home)** or **cloud models (burst)**,
  emits **metering events** to the CP.
- **BYO mode:** `sidecar → provider` directly; the **plaintext key is delivered by the client**
  over the authenticated channel at session start; at rest it's e2e-encrypted with the user's vault
  passphrase (CP/local stores ciphertext only).
- **Audit hook:** on **Spawnery-operated** infra (home or burst), the sidecar logs conversation
  content to the audit store (abuse-only, disclosed). Self-host → no audit.

---

## 10. Session protocol & rendezvous

- CP issues a **signed session token** (claims: `spawnId`, `owner`, `node`, `exp`, **bridge cert
  fingerprint**) via `POST /spawns/{id}/session`.
- **Rendezvous lives in the CP.** The CP is always reachable; the node holds an outbound gRPC
  stream (§6). Data path = **LAN-direct when reachable**, else **E2E-encrypted relay through the
  CP** over the node's outbound stream — the CP pipes **opaque ciphertext** (TLS terminates at the
  in-container ACP-bridge; client pins the bridge cert via the token fingerprint). **P2P deferred.**
  BYO-ingress (Tailscale/Cloudflare Tunnel) bypasses the relay.
- **Wake-from-zero:** if the target spawn is cold, CP instructs the node to `startSpawn`, then
  establishes the path. Web + mobile are identical ACP clients.

---

## 11. Deferred out of E0 (MVP)
Storage specifics → **E3** · **personalization** → post-MVP (`TODO.md`) · **permissions / consent /
egress enforcement** → post-MVP (`TODO.md`) · P2P/hole-punching · third-party plugin registry (MCP)
· richer conflict merge · multi-version concurrent sessions on one spawn · **self-hosted CP**
(own control plane; loses hosted web-UI access) → post-MVP.

---

## Appendix — E0 decision log

| # | Decision | Choice |
|---|---|---|
| E0.1 | Service topology | Modular-monolith CP + per-host node agent + cloud-only central gateway |
| E0.2 | App/version identity | `creator/app` handle (UUID-backed) + semver tag → commit SHA |
| E0.3 | Contract formats | gRPC (s2s) · JSON Schema (data) · OpenAPI (client) · JSON-RPC (ACP) · OpenAI-compatible (sidecar) |
| E0.4 | CP↔node control | Node dials out, persistent outbound **gRPC** stream |
| E0.5 | Bake vs inject | Image = base + chosen agent + bundled tools (per App@sha×agent); skills via agent's native import; model/data/token injected |
| E0.6 | ACP transport | In-container ACP-bridge over authenticated WebSocket; client = ACP client |
| E0.7 | Rendezvous | **In the CP**; LAN-direct else E2E relay through CP; P2P deferred; BYO-ingress bypass |
| E0.8 | Persistence | (Owned by E3) semantic commits + persist per turn + idle/exit autosave |
| E0.9 | Permission/egress | **Post-MVP** (TODO.md) |
| E0.10 | CP cardinality | **Single central managed instance**; self-host = node agents attaching to it |
| E0.11 | Rendezvous relay owner | **CP** |
| E0.12 | s2s comms | **gRPC** |
| E0.13 | Agent model | **Agent-agnostic** — chosen at spawn; manifest declares compatibility; skills native-import |
| E0.14 | Scope trim | Storage → E3; personalization + permissions → post-MVP |
