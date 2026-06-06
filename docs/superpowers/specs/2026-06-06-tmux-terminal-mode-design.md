# Tmux Terminal Mode + Agent Container Capability Taxonomy — Design

**Date:** 2026-06-06
**Status:** Approved (design); implementation pending plan
**Related:** `2026-06-05-opencode-swap-and-terminal-design.md`, `docs/RUN_OPENCODE_TMUX.md`, `docs/research/2026-06-04-tmux-attach-transport-research-prompt.md`, research report `~/spawnery-web-terminal.md`

## Problem

Spawnery today drives every agent as an **ACP** process over container stdio: the
agent speaks ACP, the node-side `Pump` parses frames into a replayable log and
fans them out to N web clients (`internal/node/pump.go`). The web UI is
ACP-specific (`ChatView`), and a terminal lane already exists for opencode
(`spawnctl attach` → mosh → in-container `tmux new-session -A … opencode attach`,
see `internal/spawnlet/terminal.go`).

This model cannot serve agents that **only have a TUI and don't speak ACP** —
Claude Code CLI, codex, aider, gemini-cli, and "most CLIs." It also can't give a
*shared* terminal (multiple web + native clients on one live TUI). The proven fix
(see research report) is to run the agent's TUI inside **tmux** in the container
and let tmux handle multi-client fan-out, with a browser terminal (xterm.js) and
`spawnctl` both attaching to the same session.

We want this **tmux mode alongside** the existing ACP and opencode-served modes —
not replacing them — and spawnery should pick the right web rendering and
`spawnctl` behavior automatically from what the agent container can do.

## Goals

- Introduce an explicit **capability taxonomy** for agent containers so spawnery
  can decide, per spawn, how to run the agent and how every client surface
  behaves — with no per-surface guessing.
- Add a **`tmux` run mode**: agent TUI in tmux, browser xterm.js + `spawnctl`
  attach to one shared session, for *any* TUI agent.
- Keep the existing **`acp`** and **`served`** modes working unchanged.
- Make adding an image of a *known* agent a database-row change; adding a *new*
  agent type a (bounded) code change.

## Non-goals (v1)

- App-level mode constraints (the manifest `agents.requiresAcp` field stays
  reserved/declarative; it is dead metadata today — parsed and stored, never
  enforced).
- Multi-user collaboration on one spawn (input arbitration, read-only roles,
  grouped sessions).
- Direct browser→node transport (everything relays through CP in v1).
- Native opencode web protocol (web stays ACP-via-ocadapter for `served`).
- ghostty-web / WASM renderer (xterm.js only; ghostty-web is a future drop-in).
- An ACP-TUI client (the `acp`-mode `spawnctl` gap stays open in v1).

## The capability taxonomy

Three levels, separating **who owns what**:

1. **Image** — a Postgres `agent_images` row: fully-qualified image name +
   author-declared list of **binaries** it ships. The author only states "I
   installed `goose` and `opencode`."
2. **Binary** — a value from a **controlled vocabulary the registry knows**
   (`goose`, `opencode`, `claude-code`, …). Declaring an unknown binary is a
   registration error, because spawnery can't derive runnables for it.
3. **Runnable** — *derived* by spawnery via a hardcoded, shared (CP + node)
   registry: `binary → [runnable]`. Each runnable is
   `{ id, mode, launch argv, resume argv, relay path, label }` and maps **1:1 to
   a mode** (the launch command fixes the interface).

An image's offered runnables = the union of runnables across its binaries.

Initial registry:

```
goose       → [ goose-acp    (mode=acp,    argv "goose acp",        relay=pump)
                goose-tui    (mode=tmux,   argv "goose",           relay=raw-pty) ]
opencode    → [ opencode-served (mode=served, argv "opencode serve", relay=ocadapter) ]
claude-code → [ claude-tui   (mode=tmux,   argv "claude",          relay=raw-pty) ]
```

Payoff of the indirection: when *we* later teach spawnery a new way to drive an
existing binary (e.g. an ACP-TUI client gives `goose-acp` a `spawnctl` surface,
or we add an `opencode-tui` runnable), every image already declaring that binary
gains the new runnable with **zero database churn**.

### Capability ownership & selection

- **Capabilities live on the image** (via its binaries + the registry), not on
  the app manifest.
- **The app does not constrain the mode in v1.** (The one genuine future case is
  non-interactive / single-shot apps, which are meaningless in `tmux` mode; and a
  weaker hardened-permission case. Neither is enforced today; revisit when such an
  app appears.)
- **Selection at spawn-create:** user picks `(image, runnable_id)`; if an image
  has exactly one runnable, the UI auto-selects it; otherwise the registry's
  declared default is the fallback.

## Modes → surfaces

A run mode is `(protocol, multiclient-broker)`. The chosen mode deterministically
dictates the web renderer, the `spawnctl` behavior, and suspend/resume — spawnery
never makes a separate "how do I display this" decision.

| Mode | Broker | Web | `spawnctl` attach | Suspend / resume |
|---|---|---|---|---|
| `acp` | node `Pump` (frame-log fan-out) | `ChatView` (rich: diffs, permission buttons) | none in v1 (future ACP-TUI client) | existing path |
| `tmux` | tmux (raw PTY fan-out) | xterm.js `TerminalView` | mosh → `tmux attach` | fresh tmux + agent `resume argv`; **scrollback lost** |
| `served` | the agent (opencode client-server) | `ChatView` via `ocadapter` | mosh → `opencode attach` | existing path |

Notes:
- opencode is **not special-cased** — it is simply the only current agent whose
  capability set includes `served`. No tmux, no xterm.js for it; web stays rich,
  `spawnctl` gets the native TUI.
- goose's limitation is structural: `{goose-acp, goose-tui}` are **mutually
  exclusive per spawn**. goose can give rich-web-without-terminal-spawnctl
  (`acp`) **or** terminal-everywhere-without-rich-web (`tmux`), never both.
- The long tail (claude-code, codex, aider, …) is `tmux`-only and scales for free.

## Architecture: what is new vs reused

The research report implies a large build; in reality most of the machinery
already exists. The **only genuinely new transport** is one node websocket→PTY
bridge.

**Reused unchanged:**
- node `Pump` (`internal/node/pump.go`) for `acp`.
- `ocadapter` (`internal/ocadapter`) for `served`.
- The mosh → `docker exec -it` / `crictl exec -it` → in-container-command lane,
  including `StartTerminal`'s `cmd` override and `ExecPrefixFor`
  (`internal/spawnlet/terminal.go`).
- In-container tmux wrapping (`spawn-tui` already runs
  `tmux new-session -A -s … -- opencode attach`).

**Generalized:**
- `spawn-tui` (`deploy/agent/spawn-tui.sh`) becomes **runnable-parameterized** —
  it wraps the runnable's argv instead of hardcoded `opencode attach`.
- `spawnctl` `tmux`-mode attach = the existing mosh lane with the inner command
  swapped to the runnable's `tmux attach` invocation. Essentially free.

**Genuinely new:**
- A node **websocket → PTY bridge** that runs
  `docker exec -it … tmux attach -t <session>` (or `crictl exec` on runsc) and
  pipes raw bytes both ways. Bytes are **relayed through CP**, keyed by
  `client_id`, alongside the existing Frame routing (a new raw-byte frame kind on
  the `Session` stream — one auth/routing path, consistent with "relay
  everything").
- A web **`TerminalView`** (xterm.js + WebGL renderer with canvas/DOM fallback +
  fit/attach/unicode11/clipboard addons), selected when the spawn's mode is
  `tmux`.

### Byte flow (tmux mode, web client)

```
browser xterm.js
  ⇅ websocket (raw PTY bytes + resize control msg)
  ⇅ CP  (Session stream, raw-byte frame kind, keyed by client_id)
  ⇅ node  (per-client PTY: docker/crictl exec -it … tmux attach -t spawn)
  ⇅ tmux session in agent container  ← the shared state; merges input, mirrors output
  ⇅ agent TUI (goose / claude / …)
```

There is **no `Pump` and no frame-log in `tmux` mode** — tmux *is* the shared
state. Each client connection is an independent `tmux attach` PTY; tmux fans out.
Late joiners get a full redraw of the current screen (not history); scrollback is
via tmux copy-mode.

## Lifecycle

- **Create:** `CreateSpawn(image, runnable_id)` → CP validates `runnable ∈ image`,
  resolves the mode via the registry, **persists the mode on the spawn** for
  later client-render decisions.
- **Agent start (tmux mode):** the agent starts **at spawn-create**, not lazily —
  the node execs `tmux new-session -d -s spawn -- <launch argv>` right after
  `StartAgent`, so the agent is alive/working before anyone attaches (consistent
  with ACP). Client attaches use `tmux attach` / `tmux new-session -A`.
- **Attach (web):** web client connects → CP reports the spawn's mode → web
  renders `ChatView` (acp/served) or `TerminalView` (tmux).
- **Attach (spawnctl):** mosh lane; inner command derived from the runnable
  (`tmux attach` for tmux, `opencode attach` for served, error/none for acp).
- **Suspend/resume (tmux):** suspend stops the container (in-tmux process +
  scrollback are lost); resume starts a fresh tmux and relaunches the agent with
  its **`resume argv`**, restoring the conversation from the persisted data mount.
  Terminal scrollback is not restored. Requires the registry to carry each
  runnable's resume invocation; runnables without one cannot resume a conversation
  (files on the mount still persist).

## Multi-client model (settled: one user, many surfaces)

The "multiple clients" are **one user switching between surfaces** (laptop web,
phone, native `spawnctl`), rarely typing in two at once. Therefore tmux is
configured:

- `window-size latest` — the window follows whoever last typed.
- All attached clients are writable; **no input arbitration, no read-only roles,
  no grouped sessions.**

This matches how the `Pump` already behaves in ACP mode (fans out to whoever
attaches, no per-user roles), keeping the modes consistent. Real multi-user
collaboration (input arbitration, simultaneous different-sized views) is
explicitly deferred.

## Flow control (tmux)

xterm.js parses at ~5–35 MB/s and caps its **unprocessed write backlog** at
~50 MB; the backlog only grows when arrival rate sustainably exceeds parse rate,
so it is naturally rate-limited by relay throughput. The plan:

- **Backpressure end-to-end, never drop.** Stop reading at the browser →
  websocket stops draining → CP stops pulling from the node → node stops reading
  the PTY → the PTY pipe fills → tmux blocks on write and buffers server-side.
  Dropping is unsafe: the VT byte stream is stateful, so dropping mid-sequence
  garbles a curses/alt-screen TUI until the next full redraw.
- **Ship a metric** counting backlog-threshold crossings / write-callback latency
  spikes. Confirm with real data post-launch whether shell/build bursts actually
  hurt before building any credit-based scheme.

## Future / node connection capability (reserved, out of scope v1)

Nodes will advertise a **reachability capability** at `Register` time
(`relay-only` vs `direct-capable` + public address). CP then chooses, per node,
whether to hand the browser a direct websocket (cloud nodes, carefully
firewalled) or relay through CP (self-hosted nodes, or a future purpose-built
relay service). v1 relays everything; the `Register` field is cheap to reserve
now.

## Implementation phasing (for the plan, not a scope cut)

- **Phase 1 — taxonomy plumbing, no new transport.** `agent_images` table; the
  shared registry; `CreateSpawn(image, runnable_id)`; persist mode on the spawn;
  `ListAgentImages` API (images + derived runnables: `id, label, mode`);
  two-dropdown web selector (image → runnable, auto-select singletons). Routed
  through the *existing* `acp`/`served` modes — a clean refactor that ships value
  on its own.
- **Phase 2 — tmux mode end-to-end.** Runnable-parameterized `spawn-tui`; node
  websocket→PTY bridge relayed via CP; web `TerminalView`; the `goose-tui`
  runnable; `spawnctl` `tmux attach`; suspend/resume via resume-argv; backpressure
  + metric.
- **Phase 3 — future.** ACP-TUI client (closes the `acp` `spawnctl` gap); node
  direct-connection capability; more runnables (claude-code, codex, aider);
  multi-user/collaboration.

## Risks

- **Flow control under shell/build bursts** — mitigated by backpressure; metric
  gates any further work.
- **tmux resize tension** — inherent; `window-size latest` is the chosen tradeoff
  for the single-user/multi-surface case.
- **Image honesty** — the DB asserts which binaries an image contains; a lying
  image fails at runtime. Acceptable for curated/registered images in v1; an image
  probe/validation step is a possible later addition.
- **Prefix/Ctrl-key collisions** — tmux's prefix can steal keys a TUI agent wants
  (a documented Claude Code pain point). Pick a prefix / pass-through binding that
  doesn't collide; use plain tmux, never `-CC` control mode (it breaks fullscreen
  TUIs).
- **Per-agent resume invocations** — `tmux`-mode resume depends on each runnable's
  `resume argv` and the agent persisting its session to the mount; agents without
  reliable resume lose the conversation on suspend.
