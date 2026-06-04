# CRI Thin Byte-Bridge Adapter — Design (sp-j5b)

**Goal:** Make the CRI/runsc lane behave identically to the Docker lane by stripping the in-pod
`acpadapter` down to a transparent byte-bridge, so the long-lived node **pump** does all
brokering, history, and multi-client fan-out. A small in-pod gap buffer preserves agent output
produced while no node is attached (a node↔CP reconnect re-dial).

**Bead:** sp-j5b. **Date:** 2026-06-03.

---

## Problem

The node pump (shipped on the Docker lane, see `2026-06-03-spawn-pump-multiclient-design.md`) owns the
single goose ACP session: it does `initialize` + `session/new` once, brokers turns, keeps an
append-only frame log, and fans out to N clients with resumable cursors.

On the CRI lane the node pump reaches goose through `mgr.Attach → AttachACP →` the in-pod
`acpadapter`. But the adapter *also* has its own broker (`transcript.Recorder`, `agentCh`), its own
single-client history/replay, and emits `spawn/turn` / `spawn/history` frames. The result on runsc is
**two stacked brokers** fighting over one goose stdio — double turn-handling, history frames the node
pump does not understand, and a single-client `connHub` that defeats the pump's multi-client fan-out.

## Solution

The adapter becomes a **transparent byte-bridge** that understands nothing about ACP. It only:
1. keeps goose alive as a subprocess for the adapter's whole life,
2. accepts one node connection at a time over the abstract UDS `@spawnlet-acp`,
3. forwards bytes raw in both directions, and
4. buffers goose stdout while no node is attached, flushing it to the next connection.

The node pump speaks `initialize` / `session/new` / `session/prompt` / `session/update` etc. straight
through, exactly as it does over Docker stdio. CRI and Docker now run the *same* pump code path.

```
node Pump  ──UDS @spawnlet-acp──►  acpadapter  ──stdin──►  goose acp
   ▲                                   │
   └───────── raw ACP ndjson ◄─────────┴──stdout──◄────────
```

## Components

### `connHub` (deploy/agent/acpadapter/bridge.go)

The one piece with logic. A single mutex guards `cur net.Conn` plus a bounded byte ring buffer:

- **`maxBufBytes`** cap (1 MiB). When appending would exceed it, evict **oldest whole lines** so the
  node never receives a torn JSON line.
- **`write(line []byte)`** — called by the stdout pump, one ndjson line at a time. Under the lock: if
  `cur != nil`, write live to `cur`; else append the line to the ring.
- **`attach(c net.Conn) (prev net.Conn)`** — under the lock: set `cur = c`, flush the ring to `c` in
  order, clear the ring, return the displaced conn (if any) for the caller to close.
- **`detach(c net.Conn)`** — under the lock: if `cur == c`, set `cur = nil` (subsequent output now
  buffers) and close `c`.

A single mutex serializes the live-vs-buffer decision against the attach swap+flush, so byte order is
preserved with no interleaving.

**Lock tradeoff (must be commented in the code).** `attach` flushes the whole gap buffer to the new
conn *while holding the lock*. This is what guarantees strict ordering: any concurrent `write` is
forced either before the flush (appended to the ring → flushed) or after it (live to `cur`, strictly
behind the flushed bytes) — no live line slips in front of the buffer and nothing interleaves. The
cost is head-of-line blocking: while the flush runs, the stdout pump cannot take the lock, so it stops
draining goose stdout; a slow reattaching node therefore briefly stalls the agent's stdout. We accept
this because the flush is bounded (≤ `maxBufBytes`) and goes to a local abstract UDS only on
reconnect — tiny and rare. The off-lock alternative (snapshot under lock, write outside it, queue
concurrent writes behind a flushing flag) removes the stall at a real complexity cost not worth it
here. A comment at `connHub.attach` must state this trade.

### stdout pump (deploy/agent/acpadapter/bridge.go)

Single persistent reader of goose stdout. Reads line-by-line (`bufio.ReadBytes('\n')`, 64 KiB buffer)
and calls `hub.write(line)`. No recording, no turn detection, no frame synthesis.

### serve / accept loop (deploy/agent/acpadapter/bridge.go)

```
go pump(gooseStdout, hub)
for {
    conn := ln.Accept()
    if prev := hub.attach(conn); prev != nil { prev.Close() }   // flush gap buffer to the fresh conn
    io.Copy(gooseStdin, conn)                                   // node → goose, returns when node closes
    hub.detach(conn)                                            // start buffering again
}
```

One node conn at a time; goose persists for the adapter's life. `conn` lifetime == node attachment
lifetime.

## What gets deleted

- **`internal/transcript`** — the entire package and its tests. The adapter was its only consumer.
- The adapter's `agentCh` single-writer-to-stdin broker, `recordingCopy`, the `transcript.Recorder`
  wiring, and `spawn/turn` / `spawn/history` frame emission.
- The existing half-close test and half-close behavior: the node pump never half-closes, so conn
  lifetime is the node attachment lifetime. `io.Copy(gooseStdin, conn)` returning means the node
  closed → detach + close.

## What does NOT change

- **No node-side changes.** The pump reaches the CRI lane via the existing `mgr.Attach → AttachACP`;
  it now simply talks to a dumb pipe. `internal/node/attach.go`, `internal/node/pump.go`, the
  manager, and the CRI backend are untouched.
- `deploy/agent/entrypoint.sh` still runs `exec acpadapter goose acp` for the CRI lane.

## Edge cases

| Case | Behavior |
|------|----------|
| Detach gap (node re-dial blip) | Goose stdout buffered, flushed in order to the next node conn. |
| Long gap / absent node | Ring caps at `maxBufBytes`, evicting oldest whole lines — a wedged or absent node cannot OOM the pod. Lossy-but-not-corrupting (whole-line eviction). |
| Node full close | `io.Copy(gooseStdin, conn)` returns → `detach` → buffering resumes. |
| Superseded conn (new attach before detach) | `attach` returns the displaced conn; the accept loop closes it. |

## Out of scope (tracked elsewhere)

- **True/durable history** — agent memory across suspend, cross-device, store-backed transcript:
  sp-3nb (P4, post-demo). The in-pod gap buffer here is a reconnect-blip convenience, not durability.
- **History pagination / item caps** — sp-suc (P4). Deleting the in-pod recorder removes the old
  500-item cap naturally; the node pump's `maxLog=2000` trim governs instead.

## Testing

`deploy/agent/acpadapter/bridge_test.go` (hermetic, real UNIX socket + piped fake agent):

1. **Bridges raw bytes both ways** — a line written to the node conn reaches goose stdin and the echo
   returns unchanged.
2. **Goose survives reconnect** — a second client reaches the same persistent agent subprocess.
3. **Gap buffer flush (new)** — attach, detach, agent emits N lines while detached, reattach: those N
   lines arrive first, in order, before any live traffic.
4. **Ring eviction (new)** — past `maxBufBytes` of buffered output, oldest whole lines are dropped and
   no partial/torn line is ever delivered (every flushed line is valid).

Full `go test ./...` stays green after deleting `internal/transcript`.
