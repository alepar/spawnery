# Web ACP Client (demo) — Design

**Status:** Approved in brainstorming — pending written-spec review
**Date:** 2026-05-30
**Context:** First web client. Talks **directly to the spawnlet** (no CP yet) — the browser
equivalent of `spawnctl`. Seeds the production web client (E6
[web client](2026-05-27-spawnery-e6-web-client-design.md)) but scopes *this* iteration to a
hardcoded spawn + an ACP chat surface. Relies on the spawnlet runtime + the transparent ACP relay
already built (see [spawnlet slice](2026-05-29-spawnlet-slice-design.md)).

---

## 1. Goal & boundary

A **React + TypeScript** SPA that, on load: issues a **hardcoded `CreateSpawn` for `secret-app`**,
opens a **WebSocket** session to the spawn, runs the ACP handshake, and renders a **chat UI** to
interact with the agent (Goose). **The browser is the ACP client** — it parses ACP, renders the
streamed agent activity, and answers permission requests.

```
React SPA ──fetch (Connect JSON unary)──▶ spawnlet   CreateSpawn / StopSpawn
          ──WebSocket (raw ACP bytes)────▶ spawnlet   /ws/session ──Relay──▶ Goose stdio
```

This is the **chat + ACP-rendering layer** of E6. Out of scope here: CP, auth, catalog, spawn
wizard, storage picker (see §9).

---

## 2. ACP client scope (demo-rich)

The client implements the **client/host half** of ACP at "demo-rich" fidelity:

| Capability | In scope? |
|---|---|
| Session protocol: `initialize` → `session/new(cwd)` → `session/prompt` → `session/update` → `session/cancel` | ✅ required |
| Render `agent_message_chunk` (assistant text) | ✅ required |
| Render `tool_call` / `tool_call_update` (shows the agent reading `data/README.md`) | ✅ |
| Render `agent_thought_chunk` (collapsible) | ✅ |
| Handle `session/request_permission` (visible allow/deny modal) | ✅ |
| `plan`, modes, available-commands updates | ❌ skip |
| **Client fs capability** (agent reads/writes the *client's* files) | ❌ **advertise NONE** — agent is self-contained on container `/app/data` |
| **Client terminal capability** | ❌ none |
| Content types beyond text (images/resources) | ❌ text only |

Advertising **no fs/terminal** is deliberate: it keeps the agent operating on its own container
mounts (the property that made the secret-word test work), not the browser.

> **Spike:** the agent-client-protocol project publishes a TypeScript library. Evaluate it for
> reusable **types** (and a client class) — but it is editor/Node-oriented, so the **transport
> (our WS) and the UI are ours regardless**. Default to a small hand-rolled `acp/` module (mirrors
> our Go `internal/acp`); adopt the lib's types if browser-friendly.

---

## 3. Components (layered so the real client grows into them)

### 3a. `src/api/` — spawnlet client
- `createSpawn({ appPath, model }): Promise<{ spawnId }>`
- `stopSpawn(spawnId): Promise<void>`
- **Transport:** plain `fetch` using **Connect's JSON unary** against the existing handler — POST to
  `/{package}.SpawnService/CreateSpawn` with `Content-Type: application/json`, body in proto-JSON
  (camelCase: `{ appPath, model }`), response `{ spawnId }`. No codegen now; `connect-es` typing is
  a later upgrade as the API surface grows.

### 3b. `src/acp/` — minimal ACP client over WebSocket
- **`Conn`** wraps a `WebSocket` (binary): on receive, **buffer chunks and split on `\n`** (ndjson),
  parse each line as a JSON-RPC 2.0 message; on send, `JSON.stringify(msg) + "\n"` as a binary frame.
  (Mirrors our Go `acp` codec — transparent to the spawnlet relay.)
- **`Client`**: `initialize()` (advertises empty client capabilities — no fs/terminal),
  `newSession(cwd)` (captures `sessionId`), `prompt(text, handlers)`, and a
  `session/request_permission` responder driven by a UI callback.
- **`handlers`**: typed callbacks for `agent_message_chunk`, `tool_call`, `tool_call_update`,
  `agent_thought_chunk`, and `requestPermission` (returns the user's allow/deny).

### 3c. `src/ui/` — React chat
- `App`: on mount → `createSpawn` → open WS → `initialize` → `newSession("/app")` → `ready`.
- `ChatLog`: ordered messages; streams `agent_message_chunk` into the current agent bubble.
- `ToolCallChip`: from `tool_call`/`tool_call_update` (e.g. `🔧 read data/README.md → done`).
- `Thoughts`: collapsible, from `agent_thought_chunk`.
- `PermissionModal`: from `session/request_permission` — allow / deny (resolves the responder).
- `PromptInput`: textarea + send → `prompt(text)`.
- On unmount / tab close → `stopSpawn`.

---

## 4. Spawnlet change (one addition)

Add a **WebSocket session endpoint** that reuses the existing relay:
- `GET /ws/session` (HTTP Upgrade). The **spawn id is the first WS message** (a small JSON
  `{"spawnId": "..."}` text frame), mirroring the ConnectRPC `Session`'s first-frame binding; all
  subsequent frames are raw ACP bytes.
- Handler: upgrade → `manager.Store().Get(spawnId)` → `rt.Attach(agentID)` → build a WS
  `StreamEndpoint{ Recv: read next WS binary msg → bytes; Send: bytes → WS binary msg }` → call the
  **existing `Relay(ctx, ep, AgentIO)`**. So it's the *same transparent byte relay* as the
  ConnectRPC `Session`, just over WS.
- Library: `github.com/coder/websocket` (modern, maintained).
- The existing Connect handler already accepts browser **unary** calls for `CreateSpawn`/`StopSpawn`
  (Connect protocol over HTTP/1.1), so no other server change. (CORS is avoided via the Vite dev
  proxy in §5; a permissive dev CORS on `/ws` may be needed if not proxied.)

---

## 5. Build / serve

- **Vite + React + TS.** Dev: `vite dev` with a **proxy** so the SPA and spawnlet share an origin:
  `/spawn.v1.SpawnService/*` and `/ws/*` → `http://127.0.0.1:9090` (the spawnlet). No CORS.
- Run order for the demo: start the spawnlet (`AGENT_IMAGE=spawnery/goose:dev …` + key), then
  `vite dev`, open the page → it spawns `secret-app` and you chat.
- Production (later): `vite build` → the spawnlet serves the static bundle at `/`. Out of scope now.
- Location: `web/` at the repo root (its own `package.json`; the Go module ignores it).

---

## 6. Data flow (happy path)

load → `CreateSpawn(appPath=examples/secret-app, model=openai/gpt-oss-120b:free)` → open
`/ws/session` (send `spawnId`) → ACP `initialize` → `session/new("/app")` → **ready** → user types
*"What is the secret word?"* → `session/prompt` → stream renders **thought** → **tool-call chip**
(reads `data/README.md`) → **`agent_message_chunk`** "QUOKKA-4417" → turn ends. Close → `StopSpawn`.

---

## 7. Error handling

- `CreateSpawn` failure → error banner (the spawn never started; nothing to stop).
- WS connect failure / mid-session drop → "disconnected" banner + a Reconnect action (re-opens WS;
  the spawn persists until `StopSpawn`).
- Agent process exit → WS closes → "session ended" banner.
- ACP JSON-RPC error responses → surfaced inline (our `acp` client returns the error, per the Go
  pattern).
- Permission requests are **never silently auto-approved** (demo-rich): the modal blocks until the
  user chooses.

---

## 8. Testing

- **`acp/` unit (Vitest):** ndjson framing (split across chunk boundaries), parse each
  `session/update` variant, `prompt` handler dispatch, permission round-trip — against an in-memory
  fake WS (no spawnlet).
- **Spawnlet WS endpoint (Go):** open a WS, drive the **stub agent** through it, assert the relayed
  round-trip (a WS analogue of `TestEndToEndStub`; `//go:build e2e`, Docker-backed, fails loudly).
- **Manual e2e:** the live secret-word flow in the browser end-to-end (Goose recites `QUOKKA-4417`,
  with the tool-call chip visible).

---

## 9. Explicitly out of scope (hardcoded / deferred to E6)

CP connection (the real client talks to the CP, not the spawnlet directly); auth/accounts; catalog
browse; spawn wizard + storage picker; multiple spawns; the per-session E2E channel; production
static serving. `CreateSpawn` is hardcoded to `secret-app` + a fixed model; one spawn at a time.

---

## Appendix — decision log

| # | Decision | Choice |
|---|---|---|
| W.1 | Client scope | **Demo-rich**: chat + tool-calls + thoughts + permission modal; no fs/terminal |
| W.2 | Session transport | **WebSocket** endpoint on the spawnlet, reusing the existing `Relay` (transparent bytes; browser does ndjson framing) |
| W.3 | Stack/intent | **React + TS (Vite)** — seed of the production web client; this iteration = chat + ACP rendering only |
| W.4 | Unary calls | **Plain fetch / Connect-JSON** against the existing handler (connect-es codegen deferred) |
| W.5 | ACP lib | **Hand-rolled minimal `acp/` TS module** (mirrors Go `internal/acp`); spike the official ACP TS lib for types |
| W.6 | Spawn target | **Hardcoded** `examples/secret-app` + a fixed free model; single spawn |
| W.7 | WS lib (server) | `github.com/coder/websocket` |
