# Control Plane (mediation slice) — Design

**Status:** Approved in brainstorming — pending written-spec review
**Date:** 2026-05-30
**Bead:** E1 runtime core (`sp-ei4`); demo build-order item 1 (vertical slice)
**Depends on:** [E1 runtime core](2026-05-27-spawnery-e1-runtime-core-design.md),
[spawnlet slice](2026-05-29-spawnlet-slice-design.md), [web ACP client](2026-05-30-web-acp-client-design.md)
**Scope authority:** [Demo MVP Scope](2026-05-28-spawnery-demo-mvp-scope.md)

> **Scope bound.** "The CP" eventually owns auth (E4 OAuth), the catalog/marketplace + publish +
> trust tiers (E5/E8), node orchestration, the session relay, telemetry, and CP-state backup — several
> specs. **This spec is the *mediation CP* only:** insert the CP between client and node — node
> registry + heartbeat, spawn routing, session relay, content-free lifecycle telemetry — with
> **stubbed auth**. Catalog/publish/trust-tiers (build-order items 3–4), real OAuth (E4), and
> CP-state backup (`sp-jf7`) are deferred to their own specs.

---

## 1. Goal & boundary

Today clients (`spawnctl`, web) talk **directly** to the spawnlet (node) over `spawn.v1.SpawnService`.
This spec inserts a **control plane** so the topology becomes the real two-tier system and the demo
gets honest session telemetry:

```
                    plain TLS (ConnectRPC + WS)            persistent outbound
   web client ─────────────────────────────────▶  CP  ◀═══ bidi stream ═══  node (spawnlet)
   (ACP client)  cp.v1: CreateSpawn/Session/Stop      node.v1.Attach:            │ /app, /data
                  (+ dev auth token, app_id)           {register, heartbeat,      ▼
                                                        spawnStatus, frames}    pod: agent + sidecar
```

**Both the node and the CP are transparent byte relays** — ACP smarts stay in the client and agent,
exactly as today. The CP adds, on top of the relay: a **node registry**, **spawn routing**, a
**session mux** (many client sessions over one node stream), **stubbed auth**, **app_id resolution**,
and **content-free telemetry**.

**Demo collapses** (per scope doc §3): no per-session E2E channel (plain TLS client↔CP, CP→node
internal), no user-facing node enrollment (nodes are Spawnery-operated), single CP acceptable for a
capped beta (`sp-9um` deferred).

**In scope:** the `cmd/cp` binary, the `node.v1.Attach` rendezvous, the node's CP-attached mode, the
client repoint, telemetry. **Out of scope:** catalog/publish/trust-tiers, OAuth, CP-state backup,
multi-CP/HA, the E2E channel, placement beyond "any node with capacity."

---

## 2. Node ↔ CP rendezvous protocol

One new gRPC bidi method the **node dials** and holds open; everything muxes over it, keyed by the
**CP-assigned `spawn_id`** (one pod has at most one active relay → `spawn_id` is a sufficient mux key,
no separate session id):

```proto
// proto/node/v1/node.proto
service NodeService {
  rpc Attach(stream NodeMessage) returns (stream CPMessage);   // node → CP, persistent
}

message NodeMessage {            // node → CP
  oneof msg {
    Register    register = 1;    // { node_id, max_spawns, agent_images[] }  (once, first)
    Heartbeat   heartbeat = 2;   // { active_spawns, free_slots, cpu_pct, gpu_pct }  (periodic)
    SpawnStatus status = 3;      // { spawn_id, phase, detail }
    Frame       frame = 4;       // { spawn_id, data }   agent → client bytes
  }
}
message CPMessage {              // CP → node
  oneof msg {
    StartSpawn   start = 1;      // { spawn_id, app_ref, data_ref, model }
    StopSpawn    stop = 2;       // { spawn_id }
    SessionOpen  open = 3;       // { spawn_id }  — a client attached; attach the relay
    SessionClose close = 4;      // { spawn_id }  — client detached; detach (pod stays)
    Frame        frame = 5;      // { spawn_id, data }   client → agent bytes
  }
}

enum SpawnPhase { STARTING = 0; ACTIVE = 1; STOPPING = 2; STOPPED = 3; ERROR = 4; }
message Register   { string node_id = 1; uint32 max_spawns = 2; repeated string agent_images = 3; }
message Heartbeat  { uint32 active_spawns = 1; uint32 free_slots = 2; uint32 cpu_pct = 3; uint32 gpu_pct = 4; }
message SpawnStatus{ string spawn_id = 1; SpawnPhase phase = 2; string detail = 3; }
message StartSpawn { string spawn_id = 1; string app_ref = 2; string data_ref = 3; string model = 4; }
message StopSpawn  { string spawn_id = 1; }
message SessionOpen  { string spawn_id = 1; }
message SessionClose { string spawn_id = 1; }
message Frame      { string spawn_id = 1; bytes data = 2; }
```

**Flow for one spawn:**
1. Node connects → `Register` → `Heartbeat` loop. CP adds it to the registry.
2. Client `CreateSpawn(app_id)` → CP picks a node, assigns `spawn_id`, sends `StartSpawn`. Node
   creates the pod (mounts `/app`,`/data`; starts sidecar + agent), replies `SpawnStatus{ACTIVE}`.
   CP returns `spawn_id`.
3. Client opens `Session` (first frame `{spawn_id}`). CP sends `SessionOpen`; node **attaches its
   existing `Relay`** to the pod stdio.
4. Bytes relay both ways as `Frame{spawn_id, data}`, the CP fanning between the right client stream
   and the one node stream. Neither hop parses ACP.
5. Client disconnects → `SessionClose` (pod stays; idle-timeout/`StopSpawn` destroys it).
   `StopSpawn` → over the stream → pod destroyed.

**Ordering/backpressure:** gRPC preserves per-direction order, so control and relay frames interleave
safely; the node demuxes by `oneof`. CP holds bounded per-spawn buffers + relies on gRPC flow control
(the single-CP bandwidth cliff `sp-9um` is the accepted demo limitation). Pod lifecycle
(`StartSpawn`/`StopSpawn`) and relay attach (`SessionOpen`/`SessionClose`) bracket **independently** —
the pod is created on CreateSpawn, not on session attach (matching today's create-then-connect web flow).

---

## 3. CP internals (`internal/cp/*`)

Behind the `cp.v1` client API (`CreateSpawn(app_id, model) → spawn_id`, `Session(stream Frame)`,
`StopSpawn(spawn_id)`), four focused units:

**a. Node registry** (`internal/cp/registry`) — the set of attached nodes + live `Heartbeat`
capacity. On `Attach`: store `{node_id, send-handle, free_slots, …}`; on `Heartbeat`: update; on
stream close: **evict + fail in-flight spawns** (emit `SpawnStatus{ERROR}` to attached clients).
Thread-safe; one writer goroutine per node stream.

**b. Session router / mux** (`internal/cp/router`) — the heart. Two maps: `spawn_id → node` and
`spawn_id → client-stream`. A transparent two-sided relay: client `Frame` → look up node → send over
its stream; node `Frame` → look up client stream → write. `Session` open/close drive
`SessionOpen`/`SessionClose` and register/deregister the client-stream handle. **Never parses ACP.**

**c. Scheduler / placement** (`internal/cp/scheduler`) — demo placement is trivial (`E1.3`): pick any
registered node with `free_slots > 0` (least-loaded). Assign `spawn_id` (uuid), resolve `app_id`, send
`StartSpawn`, await `SpawnStatus{ACTIVE}` (or `ERROR`/timeout). No capacity → `ResourceExhausted`.

**d. Auth stub + app resolution** (`internal/cp/auth`, `internal/cp/apps`)
- **Auth:** a ConnectRPC interceptor reads a **dev bearer token** from the request header → `owner_id`
  via a small config map (`{token → owner}`). No/invalid token → `Unauthenticated`. This is the **only**
  thing E4's OAuth later replaces; the rest of the CP is owner-id-agnostic. Every spawn is tagged with
  its `owner_id` (telemetry + a `StopSpawn` ownership check).
- **App resolution:** `app_id → app_ref` (the definition the node mounts at `/app`) via a static config
  map for the slice (e.g. `zork → examples/zork`, `secret-app → examples/secret-app`). The real catalog
  (E5) replaces the map later; the `StartSpawn.app_ref` contract is stable. The client→CP boundary thus
  swaps the node **filesystem `app_path`** for an opaque **`app_id`** — clients never name node paths.

---

## 4. Telemetry (content-free, scope §7)

The CP owns session lifecycle, so it emits. `internal/cp/telemetry`:

```go
type Sink interface { Emit(ev Event) error }

type Event struct {
    Kind      string    // "spawn_create" | "session_start" | "session_end"
    Owner     string    // owner_id (auth stub)
    AppID     string    // app_id (+ @sha once the catalog ships)
    Tier      string    // static "reviewed" for seed apps this slice
    Storage   string    // "managed" (the only option this slice)
    NodeID    string
    SpawnID   string
    Timestamp time.Time
}
```

- **Call sites:** `spawn_create` on `SpawnStatus{ACTIVE}`; `session_start` on `SessionOpen`;
  `session_end` on `SessionClose` **or** `StopSpawn` (whichever ends the relay).
- **No content, ever.** The event carries only the metadata above; the CP relays opaque bytes and
  **must not inspect or log frame contents** — an explicit invariant test guards this.
- **Sink for the slice:** append-only **JSONL** (`telemetry/events.jsonl`, one event/line); a `nopSink`
  for tests. The interface is the seam so the §7 hourly-Parquet rollup drops in later untouched.
- **Derivable offline** from these three events: # users, sessions/user, W1/W2/W4 return, app
  popularity, tier/storage distribution — no `/data` inspection.

---

## 5. Node & client changes

**Node (spawnlet) — add CP-attached mode, reuse everything else.**
- **Dual mode:** `CP_ADDR` set → **attach mode** (dial the CP, hold `Attach`, no inbound listener);
  else → **standalone mode** (today's inbound `spawn.v1` server — so `just spawnlet`/`spawnctl`/
  direct-web and all existing tests keep working).
- **Reused unchanged:** pod lifecycle (`manager.Create/Stop`) + the transparent `Relay`. New code is
  the **attach loop:** dial → `Register` → heartbeat ticker → handle `CPMessage` (`StartSpawn`→
  `manager.Create`; `SessionOpen`→attach `Relay` with a sink wrapping outbound bytes as
  `NodeMessage.Frame`; `Frame`→client→agent bytes; `SessionClose`→detach; `StopSpawn`→`manager.Stop`).
  Heartbeat capacity = `max_spawns − active` (cpu/gpu fields present but basic this slice).

**CP client-facing transport = the same shape the spawnlet exposes today.** Browsers can't do gRPC
bidi, so the CP mirrors the existing web-client transport: **Connect-JSON unary** for
`CreateSpawn`/`StopSpawn` + a **`/ws/session` WebSocket** relay (first frame `{spawnId, token}`). The
web client barely changes: repoint the API + Vite proxy from `:9090` to the CP, send the **dev token**,
send **`app_id`** instead of `app_path`. `spawnctl` gains a `-cp` address (its gRPC path can hit the
CP's gRPC `Session` directly).

---

## 6. Error handling

- **Node disconnects mid-session** → CP evicts it, fails its spawns' client streams ("session ended"),
  emits `session_end`.
- **No node with capacity** → `CreateSpawn` → `ResourceExhausted` ("at capacity" — capped-beta posture).
- **Unknown `app_id`** → `InvalidArgument`; **bad/missing token** → `Unauthenticated`; **`StopSpawn` by
  non-owner** → `PermissionDenied`.
- **`StartSpawn` timeout or `SpawnStatus{ERROR}`** → `CreateSpawn` fails cleanly (nothing to stop).
- **Client `Session` for an unknown/foreign `spawn_id`** → stream closes with `NotFound`/`PermissionDenied`.

---

## 7. Testing

- **Unit:** registry (add/heartbeat/evict-fails-spawns), router mux (two fake streams; frames route by
  `spawn_id` both ways), scheduler (pick / at-capacity), auth interceptor (token→owner, reject),
  app resolution (id→ref, unknown), telemetry (events at the right call sites) + an explicit
  **"CP never logs/inspects frame content"** invariant test. Docker/key-free, hermetic.
- **e2e (`//go:build e2e`, fails loudly):** stand up the CP, attach a node in CP-mode with the **stub
  agent** image, drive `CreateSpawn`+`Session` through the cp client → assert the `ECHO:` round-trip
  **and** that `telemetry/events.jsonl` recorded `spawn_create`→`session_start`→`session_end`. A web/
  Playwright variant can follow; the Go e2e covers the topology.
- **Tooling:** new `cmd/cp`; `just cp`; `just dev` becomes 3 panes (cp + node-attached + web); the node
  pane sets `CP_ADDR`.

---

## 8. File structure (informs the plan)

```
proto/node/v1/node.proto       NEW  Attach rendezvous (NodeMessage/CPMessage envelopes)
proto/cp/v1/cp.proto           NEW  client-facing SpawnService (CreateSpawn(app_id)/Session/StopSpawn)
cmd/cp/main.go                 NEW  CP binary: serve cp.v1 (+ /ws), serve node.v1.Attach, wire units
internal/cp/registry/          NEW  node registry + heartbeat capacity
internal/cp/router/            NEW  session mux (spawn_id ↔ node / client-stream); transparent relay
internal/cp/scheduler/         NEW  placement (any node w/ capacity) + StartSpawn/await ACTIVE
internal/cp/auth/              NEW  dev-token → owner interceptor
internal/cp/apps/              NEW  app_id → app_ref static map
internal/cp/telemetry/         NEW  Sink interface + JSONL sink + Event
internal/cp/ws/                NEW  /ws/session browser relay (mirrors internal/spawnlet/ws.go)
internal/node/attach.go        NEW  node CP-attached mode: dial, register, heartbeat, handle CPMessage
cmd/spawnlet/main.go           MOD  pick standalone vs attach mode on CP_ADDR
web/src/api/spawnlet.ts        MOD  repoint to CP, send dev token, app_id
web/vite.config.ts             MOD  proxy → CP address
Justfile                       MOD  `just cp`; `just dev` = cp + node(attach) + web
```

---

## Appendix — decision log

| # | Decision | Choice |
|---|---|---|
| C.1 | Scope of this spec | **Mediation CP only** (registry + routing + relay + telemetry); auth stubbed; catalog/OAuth/backup later |
| C.2 | Node↔CP link | **Full outbound-stream rendezvous** — node dials CP, holds one persistent bidi stream; register/heartbeat/control/frames muxed |
| C.3 | Mux key | **`spawn_id`** (one active relay per pod → no separate session id) |
| C.4 | Pod vs session bracketing | `StartSpawn`/`StopSpawn` bracket the pod; `SessionOpen`/`SessionClose` bracket the relay — independent (create-then-connect) |
| C.5 | Placement | **Any registered node with capacity** (least-loaded); no node → `ResourceExhausted` |
| C.6 | Auth | **Dev bearer token → owner_id** via config map; the only piece E4 OAuth replaces |
| C.7 | App resolution | **`app_id` → `app_ref` static map** (catalog E5 later); clients never send node paths |
| C.8 | Telemetry | CP emits 3 content-free events to a **JSONL sink** behind a `Sink` interface (Parquet later) |
| C.9 | Node mode | **Dual:** standalone inbound (today) vs CP-attached outbound; chosen by `CP_ADDR` |
| C.10 | CP client transport | **Same as spawnlet today** — Connect-JSON unary + `/ws/session` WebSocket (browser can't gRPC bidi) |
