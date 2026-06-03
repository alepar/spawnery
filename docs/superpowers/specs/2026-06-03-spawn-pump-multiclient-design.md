# Long-Lived Per-Spawn Pump + Multi-Client Session Design

**Date:** 2026-06-03
**Status:** Approved (brainstorming)
**Bead:** file at plan time. **Subsumes:** sp-r7t (node owns one ACP session). **Replaces:** the
sp-39u standalone readiness probe (folded into the pump's startup handshake).

## Problem

The chat session transport binds the **stateful relay to the client WebSocket lifecycle**. Today
`node/attach.go:openSession` runs, per client connection, a *fresh* `mgr.Attach` to goose's single
stdio plus a new `brokerEndpoint` pump and relay goroutine; `closeSession` tears them down. The
long-lived `transcript.Recorder` (history/queue/turn-state) is shared, but the *pump that drives it*
is per-connection.

Under the WS auto-reconnect feature (sp-yry), reconnects produce rapid, overlapping Open/Close for
one spawn. These race: `router.DetachClient` (keyed only by `spawnId`) clears a live client on a
stale detach, and `node/openSession` overwrites `a.sessions[spawnId]` without cancelling the old
relay (leaking it) while `closeSession` deletes the inbox. Result observed live (2026-06-03): multiple
relay goroutines attached to one goose stdio (a probe client received every `initialize`/`session/new`
result **twice**), the inbound prompt path dropped, goose never received a coherent `session/prompt`
(0 model calls), and the transcript broker stuck "busy" → **both spawns frozen on "working…"**. ~500
session connect/disconnect telemetry events confirmed the churn.

Root cause: the goose attach + broker pump should be **per-spawn and long-lived** (the agent is
long-lived), with client connections as detachable subscribers — not the drivers of the relay.

## Decisions

- **Long-lived per-spawn pump (node).** One goose attach + one broker for the spawn's whole life;
  created at `startSpawn`, removed at `stopSpawn`; **survives zero clients** (keeps draining goose so
  a turn finishes and lands in history even if the tab closed mid-response).
- **Multi-client fan-out.** N simultaneous clients per spawn, each an independent subscriber. A
  reconnect is just a new client joining; the old client's late detach removes only the old entry —
  the race dissolves.
- **The pump owns the single goose ACP session** (this is *required* by multi-client: N clients
  sharing one conversation must share one `sessionId`). The pump does `initialize` + `session/new`
  **once**; clients are thin (send `prompt{text}`, subscribe to frames). **Subsumes sp-r7t** (goose
  gets exactly one `initialize`; no repeated-initialize hack) and **replaces sp-39u's probe** (the
  pump's startup `initialize` is the readiness gate).
- **Append-only frame log + resumable cursor.** Per spawn, one numbered append-only log of outbound
  *conversation* frames. A client carries its last-applied `seq`: reconnect resumes (server sends only
  `> cursor`), a fresh client replays from 0, a client trimmed past the window gets a `reset` + replay
  from the window start. Same apply-path live and replay.
- **`clientId` in the node↔CP protocol.** `SessionOpen`/`SessionClose`/`Frame` gain `client_id`;
  `SessionOpen` gains `cursor`. The CP router holds a per-spawn *set* of clients.
- **Clean client↔pump frame vocabulary.** The pump is the ACP boundary: it speaks ACP to goose and a
  small clean protocol to clients (no raw ACP on the wire to the browser). The web client stops being
  an ACP speaker.
- **Scope: the Docker lane** (node pump), where the bug bites and the user runs. The CRI in-pod
  adapter keeps its own in-pod history/replay; multi-client for it is a **follow-up**.

## Architecture

### The pump (`internal/node`, per spawn)

State, all behind one mutex:
- `agent` — the single `runtime.AttachedStream` to goose (owned for the spawn's life).
- `sessionId` — the goose session the pump created.
- `log []Frame` — append-only, numbered (`seq`), trimmed to a cap.
- `clients map[clientId]*client` where `client{ cursor int64, notify chan struct{} (buffered 1), send func([]byte) error }`.
- `pendingPerms map[requestId]*perm` — unresolved permission requests (+ a deadline).
- broker turn/queue state (reused from `transcript.Recorder`: `busy`, `queue`, in-flight id).

Goroutines:
- **agent-writer** — the sole writer to goose stdin; drains an internal `toAgent chan []byte` (broker-
  forwarded prompts + drained queued prompts + the pump's own startup handshake).
- **agent-reader** — reads goose stdout line-by-line; for each line runs the **ACP→frame translation +
  broker**: `session/update` → `agent`/`thought`/`tool` frames; the `session/prompt` result →
  turn-end (broker clears busy, drains the queue → more `toAgent` writes + a `turn` frame);
  `session/request_permission` → a transient `permission_request` (broadcast, see below). Conversation
  frames are appended to `log` (seq++), then **all clients notified**. Handshake responses
  (`initialize`/`session/new`) and permission requests are **not** logged.
- **per-client goroutine** — blocks on `client.notify`; on wake copies `log[cursor-base:]` under the
  lock, sends frames to its client (CP `Frame{spawnId, clientId, data}`), advances `cursor`. Coalescing
  notify = "there is more, catch up."

### Frame vocabulary (client↔pump)

**Client → pump:** `prompt{text}` (pump wraps as ACP `session/prompt{sessionId,…}`);
`permission_response{requestId, allow}`.

**Pump → client, logged (each carries `seq`):** `user{text}` (the prompt, echoed so all clients +
replay see it — injected by the broker when it forwards/queues a prompt); `agent{text}` (message
delta); `thought{text}`; `tool{id, title, status}`; `turn{state, queued}`.

**Pump → client, transient (no `seq`, not logged):** `permission_request{requestId, …}`;
`reset{fromSeq}` (clear transcript; subsequent logged frames replay from `fromSeq`).

### Permissions (multi-client)

A `session/request_permission` from goose → the pump records it in `pendingPerms` and **broadcasts** a
`permission_request` to all attached clients. It is **re-sent to any client that attaches** while
pending (reconnect mid-permission still sees it). The **first** `permission_response` for a `requestId`
wins: the pump forwards the chosen option to goose and removes the pending entry; later responses for
the same id are dropped. A per-request **timeout auto-denies** (covers zero clients / abandonment) so
the agent never hangs.

### Lifecycle

- `startSpawn` (node): `mgr.Create` → create pump → `pump.start()`: attach goose, start agent-reader/
  writer, send `initialize`, **await the response (the readiness gate)** — timeout → `mgr.Stop` +
  `ERROR`; on success send `session/new`, store `sessionId` → report **ACTIVE**.
- Runs until `stopSpawn`: `pump.stop()` cancels goroutines, closes the attach, drops all clients,
  removes the pump entry.
- **Survives zero clients** and **CP restarts** — the pump (history + session) lives on the node, so a
  CP reconnect just re-attaches clients (with their cursors → resume); history is preserved.

### Log trimming

Cap the log by frame count/bytes (like today's `transcript.MaxItems`). Trimming advances a `base` seq;
a client whose `cursor < base` gets a `reset{fromSeq: base}` + replay from `base` (a truncation marker
frame at the head, matching current behavior). Disconnected clients don't pin the log; a reconnect
supplies its own cursor and is reset if it's below `base`.

### Node↔CP protocol (`nodev1`)

Add `string client_id` to `SessionOpen`, `SessionClose`, and `Frame` (both `CPMessage_Frame` and
`NodeMessage_Frame`); add `int64 cursor` to `SessionOpen`. Buf regen (plugins from `$(go env
GOPATH)/bin`).

### CP router (`internal/cp/router`)

`route.client` → `clients map[string]ClientSender`. `AttachClient(spawnID, clientID, c)` adds + sends
`Open{spawnId, clientId, cursor}`; `DetachClient(spawnID, clientID)` removes + sends `Close{spawnId,
clientId}`; `FromNode(spawnID, clientID, data)` routes to `clients[clientID]`; `FromClient(spawnID,
clientID, data)` tags `clientID`. `Drop`/`DropNode` close all clients' `done` (route-level, on stop/
evict).

### CP WS handler (`internal/cp/ws.go`)

The bind frame becomes `{spawnId, clientId, token, cursor}`. `HandleWS` validates as today, calls
`AttachClient(spawnID, clientID, …)` and forwards `cursor` on Open; on close calls
`DetachClient(spawnID, clientID)`; relays `Frame` tagged with `clientID`.

### Web client (`web/src`)

- Generate a per-tab `clientId` (stable across that tab's reconnects).
- Track `lastSeq` (the cursor) in memory; **keep it across reconnects** (resume, not reset).
- Bind `{spawnId, clientId, token, cursor: lastSeq}`.
- Send `prompt{text}` instead of ACP `initialize`/`session/new`/`session/prompt`; the `Client`/`Conn`
  ACP machinery is replaced by a thin frame codec (apply `user`/`agent`/`thought`/`tool`/`turn`,
  handle `permission_request` → existing permission UI → `permission_response`, handle `reset`).
- On `reset{fromSeq}`: clear the transcript, set `lastSeq = fromSeq`, apply subsequent frames.
- **User messages render from the server `user` frame, not optimistically.** Dropping the local
  optimistic add keeps every client (and replay) ordered and consistent; the echo round-trip is
  negligible locally, and pending/queued state still comes from `turn{queued}` (`reconcilePending`).
- The `ReconnectingSocket` (sp-yry) is unchanged transport; only what flows over it changes.

## Testing

- **Pump unit tests** (hermetic; fake goose via in-memory pipes, fake client senders): two clients both
  receive logged frames in order; a third attaches mid-stream and catches up from `cursor`; resume
  (`cursor=N`) sends only `>N`; **reconnect-overlap** (attach B before detaching A) leaks nothing and
  both stay correct; detach-one doesn't disturb others; prompt-while-busy queues and drains on turn-end;
  trim → `reset` path; zero-clients turn still completes into the log; permission broadcast +
  first-response-wins + re-send-on-attach-while-pending + timeout-deny.
- **CP router unit tests**: multi-client attach/detach/fan-out + per-client routing; stale detach of a
  departed clientId is a no-op for others.
- **Web unit tests**: frame codec apply (each kind), cursor tracking, resume vs `reset`, permission
  round-trip.
- **e2e**: existing suite stays green (single client, happy path); the existing reconnect test now
  exercises resume (the new turn after reconnect must still echo).
- **Host (goose), manual**: two spawns; send prompts; force reconnects (kill/restore the node or
  flap the WS) → turns complete, no stuck "working…"; a second browser tab mirrors the first live and
  either tab can answer a permission prompt.

## Non-goals / follow-ups

- **CRI in-pod adapter** multi-client/fan-out (separate component; keeps its in-pod history for now).
- **History persistence across a node restart** (the pump's log is in-memory; a node crash loses it —
  same as today's recorder; `transcript.Recorder`'s existing TODO).
- Per-client permission *policy* beyond first-wins/timeout-deny (e.g. roles, "ask the owner only").
- Pagination / on-demand history loading for very long conversations (existing sp-suc).
