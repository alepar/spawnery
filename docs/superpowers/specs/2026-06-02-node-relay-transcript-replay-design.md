# Node-Relay Transcript Recording + Replay Design

**Date:** 2026-06-02
**Status:** Approved (user chose "server-side node-relay recording")
**Bead:** sp-c5a

## Problem

The `spawn/history` replay built in slice 2 (sp-02f) lives in the in-pod `acpadapter`,
which is only in the path for the **CRI/runsc lane** (`ACP_ADAPTER=1`). In the
**Docker/self-hosted lane** that `just dev` uses, the node attaches directly to the
agent's stdio (no adapter), so on a browser **reload** the client-side transcript
buffer is gone and nothing replays — history disappears.

## Goal

History survives a browser reload (and is cross-device) in the Docker lane, by
recording + replaying the transcript at the **node relay** — which is in both lanes'
path and outlives client reloads. Proven by an e2e test.

## Decisions

1. **Recording lives at the node relay.** The node sees the full ACP ndjson stream in
   both lanes (it relays ws↔agent bytes regardless of Docker-attach vs UDS+setns
   transport). The node process outlives client reloads and CP reconnects, so a
   per-spawn recorder there persists the transcript for the agent's whole life.
2. **Lane-aware to avoid double-replay.** The in-pod adapter still records+replays in
   the CRI lane. So the node records+replays **only in the Docker lane**
   (`cfg.InPodAdapter == false`). In the CRI lane the node just relays (adapter handles
   history). This matches the user's chosen option.
3. **The recorder is shared code.** Extract slice-2's recorder (`deploy/agent/acpadapter/record.go`)
   into `internal/transcript` (exported `Recorder`, `Item`, `New`, `ObserveAgentLine`,
   `ObserveClientLine`, `HistoryFrame`). The adapter imports it (behavior-preserving);
   the node imports it.
4. **Web is unchanged.** `Client.onHistory` + `historyToItems` (slices 2/3) consume the
   `spawn/history` frame regardless of which layer produced it. No web change except
   the e2e.

## Architecture

### Shared recorder — `internal/transcript`

Move the recorder verbatim (renamed exported): `Recorder` with `ObserveAgentLine(line)`
(parses `session/update` → agent/thought/tool items, coalesced), `ObserveClientLine(line)`
(parses `session/prompt` → a user item), `HistoryFrame() []byte` (newline-terminated
`{"jsonrpc":"2.0","method":"spawn/history","params":{"items":[…]}}`, or nil if empty),
500-item cap + truncation marker. `Item{Role,Text,Title,Status}` with omitempty tags.
All methods mutex-guarded. (This is the slice-2 recorder, relocated.)

### Node relay — `internal/node`

- **Recorder registry** (`recorders`): a long-lived `map[spawnID]*transcript.Recorder`
  (mutex-guarded). Created in `node.Run` (so it survives `runOnce`/CP reconnects); nil
  when `cfg.InPodAdapter` (CRI lane → adapter handles history). `getOrCreate(spawnID)`
  on first session; `remove(spawnID)` on `stopSpawn`. NOT removed on `closeSession`
  (reloads reuse the recorder).
- **`node.Config.InPodAdapter bool`** — set by `cmd/spawnlet` to
  `CONTAINER_RUNTIME == "runsc"`. The node records only when false (Docker lane).
- **`lineBuffer`** — accumulates relay byte chunks and emits complete `\n`-terminated
  lines (the relay forwards chunks; the recorder needs ndjson lines).
- **`recordingEndpoint(ep, rec)`** — wraps `spawnlet.StreamEndpoint` so `Recv` tees
  client→agent bytes through a client `lineBuffer` → `rec.ObserveClientLine`, and `Send`
  tees agent→client bytes through an agent `lineBuffer` → `rec.ObserveAgentLine`. The
  original chunks are forwarded byte-for-byte (recording is a tee, not a transform).
- **`openSession`** (Docker lane): get-or-create the recorder; send
  `rec.HistoryFrame()` via the **raw** `ep.Send` FIRST (before the relay starts, so the
  reconnecting client gets history before live bytes; `a.send` is serialized, so order
  holds); then run `Relay(rctx, recordingEndpoint(ep, rec), agentIO)`.
- **`stopSpawn`**: `recorders.remove(spawnID)`.

### Lifecycle

The recorder is created on the first `openSession` and accumulates across every
reconnect (reload) for that spawn. On each `openSession` the current transcript is
replayed. The agent (goose/stub) keeps running across reloads, so the transcript is the
cumulative visual history. Removed only when the spawn is stopped.

## Testing

- **`internal/transcript`** — the slice-2 recorder tests, relocated (coalescing, ignore
  non-ACP / empty→nil, cap+truncation, frame shape).
- **`internal/node`** — hermetic unit tests for `lineBuffer` (chunk splitting incl. a
  line split across chunks) and `recordingEndpoint` (a scripted client prompt + agent
  update flow through the wrapped ep populates the recorder; `HistoryFrame` then carries
  the user + agent items; byte-for-byte forwarding preserved).
- **e2e (`web/e2e`)** — the acceptance gate: spawn the stub app, chat ("say one" →
  "ECHO: say one"), **`page.reload()`**, re-open the spawn from the sidebar, and assert
  the prior transcript is restored (now from the node replay, surviving the reload).
  Run with real containers (Docker lane).

## Non-goals

- Agent memory (the agent still resets its ACP session on reconnect — visual transcript
  only, same as the adapter approach).
- Surviving suspend (the recorder is in-node-memory; a stopped/suspended spawn's
  recorder is removed) — durable cross-suspend history stays sp-3nb.
- Pagination for very long transcripts (sp-suc); the 500-item cap is retained.
