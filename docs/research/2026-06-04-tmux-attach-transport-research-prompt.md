# Research task: transport/multiplexing design for "tmux attach to a running spawn"

## Your objective
Produce a **design recommendation** (a written markdown design doc) for how to carry a
second, raw, interactive terminal (PTY/tmux) byte channel into a RUNNING spawn's
container, **concurrently with the existing ACP chat session**.

Scope yourself HARD to ONE question: **the transport & multiplexing design** — how the
raw-PTY channel coexists with the ACP path end-to-end (interactive client → CP → node →
exec-into-container), given that today's transport assumes a single channel per spawn.

Everything else (PTY/exec mechanics, tmux session lifecycle, idle-timer/suspend
integration, permissioning, audit) is OUT of deep scope: note implications in a few
sentences and list them as deferred follow-up concerns. Do NOT design them.

Deliverable = a design doc with: (1) problem recap, (2) per-layer options with tradeoffs,
(3) a single recommended end-to-end design with a minimal proto/connHub change sketch,
(4) the prior-art patterns that informed it (with sources), (5) risks/open questions,
(6) deferred concerns to file as issues.

## Grounding: READ THESE FILES FIRST (do not assume — verify against the code)
- `deploy/agent/acpadapter/main.go` — adapter entrypoint, transport selection (ACP_LISTEN
  tcp:// vs ACP_SOCKET abstract UDS @spawnlet-acp).
- `deploy/agent/acpadapter/bridge.go` — `connHub`: AT-MOST-ONE attached node connection +
  1 MiB ndjson gap buffer with whole-line eviction. This single-conn assumption is central.
- `deploy/agent/entrypoint.sh` — how goose is launched (acpadapter goose acp vs goose acp).
- `internal/spawnlet/relay.go` — `Relay()` / `StreamEndpoint` / `AgentIO` byte copy.
- `internal/node/attach.go` — node↔CP bidi stream lifecycle, per-spawn Pump.
- `internal/node/pump.go` + `internal/node/frame.go` — the node PARSES ACP and emits
  STRUCTURED conversation frames (user/agent/thought/tool/turn/perm); clients do NOT get
  raw ACP. (The PTY channel is the opposite: opaque, unparsed, bidirectional.)
- `proto/node/v1/node.proto` — `NodeMessage`/`CPMessage` oneofs; `Frame{spawn_id, data,
  client_id}` opaque bytes multiplexed by spawn_id + client_id; lifecycle messages.
- `internal/spawnlet/manager.go` — how the node execs/attaches to the container per lane.
- Beads: run `bd show sp-wsu` (this epic) and `bd show sp-gd9` (lifecycle dependency).

## Current architecture (verify, then build on)
- Agent container runs goose in ACP/stdio mode behind `acpadapter`. The adapter bridges
  goose stdio to ONE attached node connection (connHub), buffering stdout when detached.
- The node dials the adapter over an abstract UDS (docker/shared-netns lane) or TCP (runsc/
  CRI lane, because gVisor isolates the abstract socket namespace).
- The node's Pump parses ACP and emits structured frames upstream.
- Node↔CP is a single Connect bidi stream (`Attach`); opaque `Frame` bytes are multiplexed
  per (spawn_id, client_id) alongside lifecycle messages.
- CP relays frames between interactive clients and the owning node.

## The core problem
A raw terminal is a SECOND concurrent, bidirectional, OPAQUE byte stream per spawn — with
no ndjson line semantics, with out-of-band control events (window resize), and (leaning)
opened by `node → exec PTY into the existing agent container`, NOT through the acpadapter
(which is goose-stdio-specific and at-most-one). The ACP session and the terminal must run
AT THE SAME TIME without interfering. Resolve how this channel is carried at each layer:

1. **In-pod / adapter layer:** Does the terminal channel touch acpadapter/connHub at all,
   or is it an independent `node → container exec` path that sidesteps the adapter? Justify.
2. **Node↔CP layer:** Reuse proto `Frame` with an added channel/stream discriminator, vs.
   add a new `oneof` message type (e.g. `TermFrame`/`TermOpen`/`TermClose`/`TermResize`),
   vs. a separate Connect RPC/stream. Weigh ordering, backpressure, head-of-line blocking
   between channels, and the existing client_id multiplexing.
3. **CP↔client layer:** How an interactive client opens and carries the raw channel
   alongside its structured ACP subscription.
4. **Framing:** How ACP vs PTY bytes are distinguished on the wire; how resize/control
   events interleave with data; whether the gap-buffer/eviction model (which assumes whole
   ndjson lines) applies or needs a different backpressure policy for raw bytes.

## Constraints to honor
- **Lean: exec-into-the-existing-agent-container.** A sidecar shell container sharing
  netns/mounts is the alternative — only recommend it if you can justify deviating.
- **Lane-agnostic:** the design must work for BOTH the docker shared-netns lane and the
  runsc/CRI TCP lane, mirroring how ACP transport already abstracts UDS vs TCP.
- Must NOT break the single-ACP-session + structured-pump path.
- Prefer the smallest change to the existing proto/relay that cleanly supports two channels.

## Prior art to research (web) — extract framing/multiplexing patterns, then map to our proto
- **Kubernetes `kubectl exec`** streaming protocol (SPDY & the WebSocket
  channel-byte-prefix scheme: stdin/stdout/stderr/resize as numbered channels). Highly
  relevant to channel-prefixed framing + resize-as-control.
- **SSH** connection protocol: multiplexed channels over one transport (the canonical model).
- **Docker exec/attach** stream multiplexing (the 8-byte stdcopy header).
- **ttyd / gotty / wetty:** PTY over WebSocket, resize message framing.
- **Coder / code-server / VS Code Remote:** terminal + structured (LSP) channels over one tunnel.
- **gRPC bidi** patterns for terminal multiplexing.

For each, capture: how channels are identified/framed, how control (resize) interleaves with
data, and how backpressure/buffering is handled. Cite sources.

## Rigor
- Verify every claim about our code by reading the file, not by inference.
- Be adversarial about your own recommendation: state the strongest case against it.
- End with a minimal concrete change sketch (proto diff + connHub/relay touch points) and
  an explicit list of deferred concerns to file as beads follow-ups.
