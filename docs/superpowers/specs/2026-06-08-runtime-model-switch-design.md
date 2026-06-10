# Runtime Model Switching — Design

**Date:** 2026-06-08
**Status:** Approved (collaborative brainstorm)

## Goal

Let an operator change the inference model an **already-running** spawn uses,
without recreating the container, and have the change take effect **seamlessly**
— the in-progress conversation continues with full history, and the very next
inference request uses the new model.

### Decisions locked during brainstorming

- **Continuity:** seamless, same session (no restart, no context loss).
- **Surfaces:** web UI **and** `spawnctl` CLI.
- **Model set:** any OpenRouter id, free-form (mirrors spawn creation).
- **Persistence:** persist as the new default in the spawn record.

## Current state (what makes this possible)

- The model is chosen at spawn-creation (`CreateSpawnRequest.model`, an
  OpenRouter id) → stored in `spawns.model` → propagated to the node as
  `StartSpawn.model` → injected into the agent container as `SPAWN_MODEL`.
- `deploy/agent/launch` reads `SPAWN_MODEL` **once at launch** and bakes it into
  each runnable's config (`GOOSE_MODEL`, opencode's `opencode.json`,
  `ANTHROPIC_MODEL`). Nothing re-reads it afterward.
- **Every** inference call funnels through the per-pod **sidecar**
  (`internal/sidecar`), a dumb reverse proxy on `127.0.0.1:8080` in the pod's
  shared netns that injects the OpenRouter key and forwards whatever `model` the
  agent put in the request body. The agent already controls the model field;
  the sidecar never enforced it.
- The **node never sees inference traffic** (pod→sidecar→OpenRouter bypasses the
  node), so the sidecar is the *only* place a running model can be intercepted.

## Approach

**Sidecar model-override (force-rewrite at the chokepoint).** The sidecar holds
an atomic override model; when set, it rewrites the `model` field in each
outbound request before forwarding. A token-gated control endpoint lets the node
set it. The DB (`spawns.model`) is the persistent source of truth; the live
override is how an already-running pod reflects it without a restart.

Rejected alternatives:

- **Agent-native switching** (goose `/model`, opencode picker): per-runnable,
  inconsistent, not uniformly driveable from web+CLI, awkward to persist.
- **Restart the agent in-place** with a new `SPAWN_MODEL`: not seamless — drops
  the in-session conversation.

## Control path

Client → CP (persist) → node (over the existing `Attach` stream) → sidecar.

### 1. CP unary RPC (web + spawnctl), modeled on `RenameSpawn`

```proto
// cp.proto / SpawnService
rpc SetSpawnModel(SetSpawnModelRequest) returns (SetSpawnModelResponse);
message SetSpawnModelRequest  { string spawn_id = 1; string model = 2; }
message SetSpawnModelResponse { string model = 1; bool applied = 2; }
```

Handler: authorize (spawn owner) → validate non-empty model → in one DB
transaction write `spawns.model` and set `model_applied = false` → inline
best-effort push to the node → return current state immediately (does not block
on retries).

### 2. CP→node message on the existing `Attach` bidi (no new RPC)

```proto
// node.proto
message CPMessage   { oneof msg { ...; SetModel set_model = 9; } }
message NodeMessage { oneof msg { ...; SetModelResult set_model_result = 8; } }
message SetModel       { string spawn_id = 1; uint64 generation = 2; string model = 3; }
message SetModelResult { string spawn_id = 1; bool ok = 2; string detail = 3; }
```

Generation-fenced like `StopSpawn`/`Suspend`: the node ignores a `SetModel`
whose generation doesn't match the pod it currently holds.

### 3. node → sidecar (plain HTTP, not protobuf)

The node POSTs to `PodIP:<controlPort>/control/model` with
`Authorization: Bearer <token>`, body `{"model":"<openrouter-id>"}`. The token
is a per-pod secret the manager generates and passes via `SidecarEnv`
(`SIDECAR_CONTROL_TOKEN`) — unreadable by the agent's separate container even
though they share the netns. Binding the control port to the **pod IP** (not
loopback) is what lets the node reach it.

The CP responds to the client only after the node acks (or determines there is
no live pod), so success means the live pod is actually switched, not just the DB.

## Sidecar override mechanism

- **State:** an `atomic.Pointer[string]` holding the override (empty/nil =
  passthrough). Lock-free; read on every proxied request.
- **Control listener:** a second `http.Server` bound to `PodIP:<controlPort>`,
  separate from the `127.0.0.1:8080` inference listener. `POST /control/model`
  checks the bearer token (constant-time compare against
  `SIDECAR_CONTROL_TOKEN`), stores the model atomically, returns 200.
  `GET /control/model` returns the current override (debug/reconciliation).
- **Request rewrite — both paths already pass through the sidecar:**
  - *OpenAI passthrough* (`NewHandler`, goose/opencode): currently a pure
    `ReverseProxy` that never touches the body. **Only when an override is set**,
    buffer the request body, JSON-patch the top-level `"model"`, re-marshal, fix
    `Content-Length`. Override unset → byte-identical to today (zero overhead).
    Request bodies are complete JSON (only *responses* stream), so buffering is
    safe.
  - *Anthropic messages* (`NewMessagesHandler`, Claude): already parses and
    rebuilds the request for Anthropic→OpenAI conversion; substitute the
    override before the conversion.
- **Override value = the raw OpenRouter id.** That is exactly what every runnable
  puts in the request body's `model` field (opencode's `spawnery/<id>` provider
  prefix is stripped before the HTTP call), so one string replacement works
  uniformly across goose/opencode/claude.

Effect: the next request the agent makes uses the new model, mid-conversation,
full history intact. The agent's own config still *names* the old model
(cosmetic); the bytes on the wire carry the new one.

## Persistence, `model_applied`, and reconciliation

- **New column** `spawns.model_applied bool` — "the running pod's effective model
  matches `spawns.model`." Existing rows and freshly-created spawns start `true`.
  Optional companion `model_apply_detail string` holds the last failure reason.
- **On `SetSpawnModel`** (one transaction): write `spawns.model`, set
  `model_applied = false`. Inline best-effort push for snappiness; the RPC
  returns the current state without blocking on retries.
- **Reconciler (the real guarantee)** — a background CP loop (~5s tick, tunable)
  scanning spawns where `model_applied = false`:
  - *Live pod on a connected node* → push `SetModel` (generation-fenced); on
    `ok` set `model_applied = true`, clear detail. Also covers node reconnects
    (re-pushed next tick); the inline push is only an optimization.
  - *Suspended / no live pod* → set `model_applied = true` immediately (nothing
    running to diverge; resume bakes the DB model into the fresh pod).
  - *Push keeps failing* → retry with backoff for a bounded window (~2 min,
    tunable), then **give up**: leave `model_applied = false`, record reason in
    `model_apply_detail`. UI shows a "pending" badge. Not lost — any later
    recreate/resume resolves it.
- **Recreate/resume stays clean:** when the manager provisions a fresh pod, the
  started model *is* `spawns.model`, so the CP sets `model_applied = true` at
  provision time and the sidecar override starts empty (passthrough). Live
  override and fresh-pod config converge on the same model from opposite
  directions.

## Surfaces

### spawnctl

```
spawnctl set-model <spawn-id> <openrouter-model-id>
```

Calls `SetSpawnModel`; prints the active model and whether it applied live
(`model set to <id> (applied)` vs `... (saved; pending — agent not yet switched)`).

### Web UI

A model control near the goose-acp ChatView:

- Shows the **current model from the spawn record** (authoritative), not from the
  agent.
- Free-form text input (any OpenRouter id) with confirm. No catalog fetch in v1.
- Calls `SetSpawnModel` via the existing `web/src/api` connect transport;
  optimistically reflects the new model; shows a **"pending"** badge while
  `model_applied = false`, clearing it once applied.
- `model` + `model_applied` reach the client via the existing spawn status/list
  path the ChatView already consumes (`ListSpawns`/status) — no new channel.

## Testing

- **Sidecar (unit, hermetic):** override-unset → byte-identical forward;
  override-set → OpenAI path rewrites top-level `model`, preserves other fields,
  fixes `Content-Length` (stub echo upstream); Anthropic path substitutes before
  conversion; control endpoint token auth (200 / 401, `GET` returns override);
  streaming responses still pass through after a rewritten request.
- **CP handler (unit/integration, fake node):** writes `model` + `applied=false`
  then flips to `true` on ack; suspended/no-pod → `true` immediately; node push
  fails → stays `false` with detail, reconciler eventually flips it, give-up
  leaves `false`; generation fencing; non-owner rejected.
- **Node:** receiving `SetModel` issues the correct token-authenticated POST to a
  stub sidecar; generation mismatch dropped.
- **End-to-end (extends the containerized goose path, skips without Docker):**
  start spawn → `SetSpawnModel` → assert the next request leaving the sidecar
  carries the new model id. The one true seamless-switch proof.
- **Web:** component test — renders record model, submit calls the RPC, "pending"
  badge reflects `model_applied`.
