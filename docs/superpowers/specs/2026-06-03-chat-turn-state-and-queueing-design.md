# Per-spawn turn-state + server-side prompt queueing

**Date:** 2026-06-03
**Epic:** E6 Web client (`sp-95v`)
**Status:** Design approved, ready for implementation plan

> **Status (2026-06-09): implemented** — `sp-b2b7` closed as shipped. The architecture evolved
> during the pump rewrite: the session broker lives in **`internal/node/pump.go`** (busy/queue/turn
> frames), not a promoted `internal/transcript` recorder — `internal/transcript` was **deleted**.
> Web side: `web/src/sessions/store.ts` + `PromptInput`. One deliberate delta from this design:
> over-cap prompts are **silently dropped server-side** (the web client gates at `MAX_QUEUED`
> before sending).

## Problem

The chat input's "ready to send" state is **global and fragile**.

1. **Global, not per-spawn.** `App.tsx` holds a single `const [busy, setBusy] = useState(false)`; the send button is disabled when `busy || conn !== "connected"`. There is no per-spawn dimension, so the moment one spawn is mid-turn, *every* spawn's input is disabled. Switching to an idle spawn while another is replying leaves you unable to type.

2. **`busy` can get stuck on permanently.** `onSend` does `setBusy(true)`, `await client.prompt(...)`, then `setBusy(false)` in a `finally`. That `await` only resolves when the `session/prompt` *response* arrives over the WebSocket. But `Conn` (`web/src/acp/conn.ts`) wires up only `onmessage` — it never rejects pending calls on socket close/error. So whenever the socket goes away mid-turn — exactly what switching spawns does (`closeSession → teardown → ws.close()`), and also suspend/network blips — the prompt promise never settles, the `finally` never runs, and `busy` stays `true` forever.

The deeper cause: **the only "agent is ready again" signal is a per-connection promise** that is tied to one WebSocket and never fires on disconnect.

## Constraints discovered

- **One live WebSocket session at a time**, to the active spawn. Switching tears it down. The backend replays *history* on reconnect (`spawn/history`) but exposes **no per-spawn "turn finished / idle" signal** anywhere.
- **ACP has no native queueing and no concurrent prompts.** Per the ACP spec, prompt turns are strictly sequential per session: a turn runs from `session/prompt` until the agent returns a response with a `stopReason` (`end_turn`, `cancelled`, `max_tokens`, `max_turn_requests`, `refusal`). Sending a second `session/prompt` while one is in flight is **undefined behavior**. The "type while it responds" feature in Zed/Claude Code is built **client-side on top of ACP**; for *external* ACP agents Zed only delivers a queued message after the current generation ends. **We must queue ourselves.**
- **Two lanes share one recorder.** The CRI lane uses the in-pod `acpadapter` (`deploy/agent/acpadapter/bridge.go`); the **Docker lane** — the default for local dev (`InPodAdapter` is `true` only when `CONTAINER_RUNTIME == "runsc"`) — uses the **node relay** (`internal/node/record.go`, `attach.go`). Both feed the same `internal/transcript` recorder line-by-line. Logic must live in that **shared recorder** so both lanes get it; putting it only in `acpadapter` would not fix local Docker-lane behavior.

## Approach

Promote `internal/transcript`'s `Recorder` from a passive tee into an active per-spawn **session broker**, owned by the node's long-lived recorder registry (so it survives client reconnects), and consulted by both lanes' relay seams to decide *what to forward in each direction*.

### Responsibilities (one per-spawn object)

- **History** (unchanged): accumulate items, emit `spawn/history`.
- **Turn-state** (`idle | busy`): derived by correlating the in-flight `session/prompt` request id → its response `stopReason`.
- **Queue**: prompts received while busy are recorded as `pending` user items, enqueued, and drained FIFO on turn-end.

The relay in each lane changes from "tee + always forward" to "ask the broker what to forward." This is the one structural change, localized to the two line-split seams that already exist (`internal/node/record.go` recordingEndpoint; `deploy/agent/acpadapter/bridge.go` pump/recordingCopy).

### Turn-state machine & queueing semantics

- **Gate only `session/prompt`.** Permission responses, `session/cancel`, `initialize`, and `session/new` always pass through immediately.
- **Client line is `session/prompt`:**
  - **idle** → record user item (sent), mark **busy**, remember the request id, forward to agent.
  - **busy** → record user item (**pending**), enqueue the raw line, **do not forward**.
- **Agent line is a response matching the in-flight prompt id** (has `result`/`error`, carries `stopReason`) → mark **idle**; if the queue is non-empty, pop the next prompt, mark busy with its id, forward it to the agent, flip its item `pending → sent`.
- `stopReason: cancelled` and error responses are also turn-ends. Agent stdout **EOF** (process died) resets to idle and clears the queue.
- The queue is **bounded** (cap ~50). Over the cap, reject the prompt with an error notification to the client rather than grow unbounded.

### Wire protocol additions

- **`spawn/turn`** notification (agent→client, broker-synthesized):
  ```json
  { "jsonrpc": "2.0", "method": "spawn/turn", "params": { "state": "busy" | "idle", "queued": 2 } }
  ```
  Emitted on every state transition and whenever queue depth changes.
- **`spawn/history` gains turn-state.** The frame already sent on (re)connect adds `turn: { state, queued }`, and queued user items carry `pending: true`. So on switch-back the client immediately gets the correct working-state *and* the queued messages.

### Frontend (`web/`)

- **Remove the Send button entirely.** `PromptInput` becomes just the textarea: **Enter sends, Shift+Enter newlines**. The `data-testid="prompt-send"` element is gone; tests that target it move to the textarea / Enter keypress.
- **Stop deriving `busy` from the prompt promise.** Delete the global `busy` state and the `finally { setBusy(false) }` pattern. `onSend` becomes fire-and-forget over the ws.
- **Turn-state comes from the backend.** Handle the `spawn/turn` notification and the `turn` field in `spawn/history`, stored per active spawn.
- **Enter is allowed whenever `conn === "connected"`** — sends always queue server-side. While not connected (starting / error / none), Enter is a no-op. Typing into a not-yet-live spawn and auto-flushing on connect is explicitly **deferred** (see below).
- **Working affordance:** a **transcript footer typing-indicator** — an animated row of pulsing dots pinned at the end of the message list (rendered via Virtuoso's `Footer` so it sticks with `followOutput`), shown when `state === "busy"`. A muted-foreground label beside it reads `working…` and, when `queued > 0`, `working… · N queued`. It disappears on turn-end.
- **Pending rendering:** queued user bubbles render **dimmed with a small "queued" tag**. Reconcile against the `queued` count from `spawn/turn` — the oldest pending bubble un-pends as the queue drains.

## Edge cases & error handling

- **The stuck-busy bug is gone by construction.** The frontend no longer waits on a per-connection promise that never settles. On disconnect/switch mid-turn, the broker keeps running server-side, the turn completes, and reconnect replays correct state.
- **Switch away & back mid-turn** → `spawn/history` reports `busy` + the transcript-so-far + any pending queue. Correct indicator even for a turn that finished while you were away (the broker observed the `stopReason` server-side).
- **Agent error / refusal / cancel** → treated as turn-end; queue drains normally.
- **Agent process dies (stdout EOF)** → turn-state resets to idle, queue cleared; spawn status transitions are handled by existing readiness/poll machinery.
- **Known interaction with `sp-r7t`.** On reconnect the browser re-sends `initialize`/`session/new` with a reset JSON-RPC id counter, which can collide with a still-running turn's request id (both sides share the id namespace on the transparent relay). This is a **pre-existing hazard**; `sp-r7t` (node owns the session and filters redundant handshakes on reconnect) is the proper fix. Flagged here as a dependency/risk; not solved in this work.

## Testing

- **Broker unit tests (Go):** idle→busy→idle correlation by request id; enqueue while busy; FIFO drain on turn-end; `cancelled`/error/EOF turn-ends; `spawn/history` includes `turn` + `pending` items; queue cap rejection.
- **Both lanes wired:** shared broker → one core suite + thin per-lane wiring tests (Docker relay; CRI adapter).
- **Frontend:** Enter sends / Shift+Enter inserts newline; no Send button present; working footer appears/disappears on `spawn/turn`; pending bubbles render dimmed and reconcile with `queued`; reconnect replays turn-state + queued messages.

## Out of scope (deferred)

- **Background-spawn "working" dots in the sidebar.** Tracked as a new beads issue under E6. Needs turn-state plumbed beyond the single live WS — candidate approaches to weigh there: (a) fast-track busy propagation over WS, (b) retain WS connections across spawn switches with a timeout, (c) full plumb-through CP + DB.
- **Typing into a not-yet-connected spawn** and auto-flushing on connect.
- **"Send Now" / cancel-then-send** (interrupt the running turn instead of queueing) — a possible later affordance, not the default.
