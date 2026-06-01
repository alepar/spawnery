# Node-Class Propagation to CP (sp-2as) — Design

**Bead:** `sp-2as` · builds on the egress floor (`sp-rpa`, merged `1435585`)
**Status:** Draft v1 — build proceeding per "keep going" unless flagged
**Date:** 2026-06-01

## 0. Context

`sp-rpa` introduced **node class** (`cloud` | `self-hosted`) on the spawnlet — cloud nodes always
enforce the egress floor, self-hosted opt in. Today that class lives only on the node; the CP has no
idea which class a node is. This slice **propagates the class up to the CP** at registration, records
it on the in-memory node registry, and **stamps it on the `spawn_create` telemetry event** so we can
observe which class a spawn ran on. This is the foundation for later policy (e.g. routing untrusted
apps only to enforcing nodes) — that policy is **out of scope** here (no spawnable-unverified path
yet).

## 1. Scope

**In (pure-CP + node contract, hermetically testable):**
1. **Contract:** add `node_class` to the `Register` node→CP message (`node.proto`).
2. **Node:** `node.Config` gains `NodeClass`; the `Register` it sends includes it; `cmd/spawnlet`
   sets it from `NODE_CLASS` (default `cloud`, same env the egress floor reads).
3. **CP registry:** `registry.Node` gains `Class`; the CP's `Register` handler sets it.
4. **Telemetry (the consumer):** `telemetry.Event` gains `NodeClass`; the `spawn_create` emit stamps
   the registering node's class.

**Out:** scheduler routing/restriction by class · persisting nodes to the durable store (registry
stays in-memory) · surfacing class in a client API/UI · `self-hosted` trust tiers.

## 2. Changes

### 2.1 Contract — `proto/node/v1/node.proto`
`Register` gains `string node_class = 5;` (values `"cloud"` | `"self-hosted"`; empty tolerated →
treated as unknown/unset by the CP, defaults to `"cloud"` for safety on the CP side — an
unidentified node is assumed restricted).

### 2.2 Node — `internal/node/attach.go`, `cmd/spawnlet/main.go`
- `node.Config` gains `NodeClass string`.
- The `Register{...}` send includes `NodeClass: cfg.NodeClass`.
- `cmd/spawnlet` (CP-attached branch) sets `NodeClass: env("NODE_CLASS", "cloud")` on `node.Config`
  (reuses the same env the spawnlet `ManagerConfig` already reads — single source).

### 2.3 CP registry — `internal/cp/registry/registry.go`, `internal/cp/server.go`
- `registry.Node` gains `Class string`.
- The `Register` handler (`server.go`) sets `Class: nodeClassOrDefault(m.Register.NodeClass)` where
  an empty class defaults to `"cloud"` (safe default).

### 2.4 Telemetry — `internal/cp/telemetry/telemetry.go`, `internal/cp/server.go`
- `telemetry.Event` gains `NodeClass string `json:"node_class"``.
- In the node-stream handler, capture the node's class alongside `nodeID` (both come from the same
  `Register`), and set `NodeClass` on the `spawn_create` `Emit`.

## 3. Testing (hermetic)

- **Registry:** after the CP processes a `Register{node_class:"self-hosted"}` over a fake node
  stream, the registry's node has `Class=="self-hosted"`; a `Register` with empty class → `"cloud"`.
- **Telemetry:** drive a spawn to active on a registered node and assert the captured `spawn_create`
  event has the node's `NodeClass`. (Reuse the existing `newTestServer` + `capSender` + fake
  telemetry sink pattern; if telemetry isn't already captured in tests, assert via the registry +
  a direct emit unit instead — implementer's call, but prefer the end-to-end telemetry assertion if
  the harness supports it.)
- Existing tests: the `Register` messages they send omit `node_class` (zero value) → defaults to
  `"cloud"`; assert nothing breaks.

## 4. Decision log

| # | Decision | Choice |
|---|---|---|
| N.1 | Where class is recorded | in-memory `registry.Node.Class` (nodes aren't in the durable store) |
| N.2 | Empty/unknown class | CP defaults to `"cloud"` (safe — unidentified node assumed restricted) |
| N.3 | Consumer this slice | telemetry `spawn_create.node_class`; scheduler routing deferred |
| N.4 | Env source | `NODE_CLASS` (same var the egress floor reads), default `cloud` |
