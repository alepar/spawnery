# Multi-session per spawn: spawn-view tab bar (sp-npxq)

**Status:** Design approved (brainstorming, Mode A) · 2026-06-08
**Epic:** sp-npxq · **Children:** sp-npxq.1–.5
**Related:** sp-jpn (URL history — landed), sp-9xr.18 (status dots), sp-9xr.4 (runnable selector), sp-9xr.12 (shared attach)

## Context

Today a spawn runs exactly **one** agent. `entrypoint.sh` dispatches a single runnable; the
node keys one Pump (ACP) or one tmuxRelay (mosh terminal) per spawn (`attacher.pumps[spawn_id]`),
and CP keeps one route per spawn. The web UI shows that single surface (native `ChatView` for ACP,
xterm `TerminalView` for mosh). There is no concept of a "session" — the proto's
`SessionOpen`/`SessionClose` are about *multiple clients sharing one agent*, not multiple agents.

We want one spawn to host **multiple concurrent sessions inside its single container** (shared
filesystem + sidecar), surfaced as a **tab bar** atop the spawn view. Tab 1 is the existing primary
surface; a `+` button opens additional sessions — any runnable the spawn image offers, plus a
built-in shell — each in its own tab with its own live socket. This is a real backend feature, not
just UI: it introduces a `(spawn_id, session_id)` dimension everywhere that is currently `spawn_id`.

This spec covers the **whole epic**: the session model + proto, the reusable launcher, node-side
multi-session lifecycle, and the web tab bar. Per-tab URL deep-linking is deliberately **deferred to
a closing phase** after merge (§9).

## Core model

A **session** is the unit a tab binds to:

```
session = { id, transport: "mosh" | "acp", runnable, status, endpoint }
```

- **`transport`** decides relay + surface, nothing else:
  - `"acp"` → node **ACP Pump** → native web **ChatView**. `endpoint` = ACP port.
  - `"mosh"` → node **mosh PTY relay** → xterm **TerminalView**. `endpoint` = tmux session name.
- **`runnable`** decides *what runs* — `goose-acp`, `opencode-served`, `claude-tui`, `goose-tui`,
  `opencode-tui`, or the built-in **`shell`** (`bash`). The runnable catalog declares each runnable's
  transport (e.g. `opencode-served`→acp, `opencode-tui`→mosh). `transport` and `runnable` are
  orthogonal in the model; the launcher (§4) maps `runnable → (transport, env, tmux command)`.
- **Session #0** is the spawn's existing primary agent, auto-registered at spawn start with a
  well-known id and **pinned** (not closeable — stopping it means stopping the spawn). Its ACP port
  stays `7000`. In the data model it is just "session zero," no special-casing.

The murk of an earlier `kind` enum (which conflated transport with which-runnable) is gone:
`agent-tui` vs `shell` were the same transport and surface; they are now both `transport:"mosh"`
differing only by `runnable`.

## 1 — Proto / RPC: in-stream, node-authoritative, CP mirrors (sp-npxq.1)

Extend the existing `Attach` bidi stream (`proto/node/v1/node.proto`) — keep a single ordered
control channel rather than a second out-of-stream RPC surface:

- **CP → node (new in-stream messages):**
  `CreateSession{spawn_id, transport, runnable}`, `CloseSession{spawn_id, session_id}`.
- **node → CP (new):** `SessionRoster` / `SessionStatus` reporting the live session set and
  per-session status (the node owns lifecycle truth).
- **Existing messages gain `session_id`:** `Frame`, `SessionOpen`, `SessionClose`. Frame routing
  becomes `(spawn_id, session_id, client_id)`.
- **`ListSessions`** (client-facing) is answered from **CP's mirrored roster** — no extra node
  round-trip. CP mirrors what the node reports and serves it to web clients via the existing CP
  client API.

CP side: `internal/cp/router/router.go` (`route` gains a session map). Node side:
`internal/node/attach.go` (`attacher` re-keyed by `(spawn_id, session_id)`).

## 2 — Reusable launcher (sp-npxq.2)

Factor `deploy/agent/entrypoint.sh`'s per-runnable `case` into **one in-image launcher script**
callable by **both** the entrypoint (session #0) and the node via `docker exec` (sessions 1..N), so
an exec-launched session gets byte-identical config:

```
launcher --runnable <id> [--acp-port <N>] [--tmux-session <NAME>] [--keepalive]
```

It owns the per-runnable env/wiring that lives in the entrypoint today: opencode sidecar
`opencode.json` + `OPENCODE_CONFIG`/`OPENCODE_BASE_URL`, goose OpenAI-sidecar env, claude
`apiKeyHelper` + `ANTHROPIC_BASE_URL`, `acpmux` vs `acpexec` selection for acp runnables, and tmux
wrapping for mosh runnables. Builds on sp-9xr.18's `setup_opencode_provider` factoring.

## 3 — Transparent tmux for all `mosh` sessions (sp-npxq.2/.3)

**Every `mosh`-transport session runs inside tmux — no exemptions, opencode included.** A single
`tmux.conf` baked into the image makes tmux ~invisible:

```sh
set  -g  status off                    # no status bar
set  -g  prefix None                   # don't intercept C-b — all keys reach the app
unbind C-b
set -sg  escape-time 0                 # no ESC delay; TUIs feel native
set  -g  mouse off                     # app owns the mouse
set  -g  default-terminal "tmux-256color"
set -ga  terminal-overrides ",*:Tc"    # truecolor passthrough
set  -g  allow-passthrough on          # raw DCS (graphics/img protocols) passthrough
set  -g  set-clipboard on              # OSC 52 clipboard passthrough
set  -g  set-titles on                 # forward the app's terminal title
set  -g  visual-activity off
set  -g  visual-bell off
set  -g  monitor-activity off
set  -g  detach-on-destroy on          # agent exits → session ends (matches spawn-tmux loop)
```

Rationale and consequences:
- **Uniform launcher path:** `spawn-tui.sh`'s bare `opencode attach -s <id>` becomes the *inner
  command* of a transparent tmux session (`tmux new-session -d -s <name> -- spawn-tui.sh`). No
  per-runnable "tmux or not" branch.
- **True shared TUI:** every client `tmux attach`es to the one tmux session running the one TUI
  process, so all viewers mirror pixel-identical render — not N independent `opencode attach`
  processes. This is the multi-client win and the reason opencode is no longer exempt.
- **Persistence for free:** all mosh sessions (incl. shell) survive transient disconnects and
  restore scrollback on re-attach.
- The historical opencode half-render is attributed to the now-fixed `LANG`/`LC_ALL`; the
  `default-terminal`/`terminal-overrides`/`allow-passthrough` lines are belt-and-suspenders.

**Known caveats (deliberate):**
- **Shell wheel-scroll:** a full-screen TUI (claude/goose/opencode) uses the alternate screen so
  tmux is fully invisible; a **bash shell** uses the normal screen where tmux mediates scrollback,
  and with `mouse off` the wheel won't drive copy-mode — so a shell is ~95% transparent (scroll-history
  UX is the only tell).
- **Resize clamping:** tmux sizes a session to its *smallest* attached client; surfaces only when
  >1 client shares a session (shared-attach), and is definitional for true mirroring.

## 4 — Node: launch & reap sessions (sp-npxq.3)

`attacher` manages **N Pumps and N relays per spawn**, keyed by `(spawn_id, session_id)`.
`CreateSession` dispatch by transport:

- **`mosh` (any tui runnable or `shell`):** `docker exec launcher --runnable <id> --tmux-session
  <unique>` → creates a transparent-tmux session whose command is that runnable (agent or `bash`).
  Node attaches a mosh PTY relay via `tmux attach -t <unique>`. The node has no per-runnable logic —
  it only ever "attaches a PTY to a named tmux session."
- **`acp`:** node allocates the lowest-free port in the pool **`32668–32767`** (100 contiguous
  ports, the highest block *below* the `32768` Linux ephemeral boundary, so in-container listeners
  can't collide with kernel-assigned ephemeral source ports). `docker exec launcher --runnable <id>
  --acp-port <N>` starts that runnable's existing ACP setup (`acpmux` for goose-acp, `acpexec` for
  opencode-served) bound to port N. Node opens an Nth Pump pointed at N. **Reject `CreateSession`
  if the pool is exhausted.** Session #0 keeps port `7000`.

`CloseSession` reaps **only** that session: acp → kill its Pump + in-container endpoint, free its
port; mosh → kill its tmux session. A **transient WS disconnect does not reap** — tmux sessions and
node-side Pumps stay alive, so reconnect (or reload + `ListSessions`) re-attaches.

Exec/port constants live near `internal/spawnlet/terminal.go` (`ExecPrefixFor`, `execArgv`) and the
ACP port constant currently in `internal/runtime/docker_pod.go`.

## 5 — Web: tab bar + per-session surfaces (sp-npxq.4)

**State model — small Zustand store keyed by `session_id`:**
`{ socket, items, turn, conn, transport, runnable, status, label }` per session. This lifts
`App.tsx`'s single `wsRef`/`activeId` (which only supports one live ACP socket) into per-session
state living *above* the tab switch — a prerequisite for keeping N sockets alive. One `Conn`/socket
per session.

**Keep-alive — DIY, no library** (per the deep-research reconciliation):
- All session panels are **mounted simultaneously and kept mounted**; Radix `Tabs` is used for the
  **tab strip only** (free ARIA `tablist`/arrow-nav/focus), never as the content host (its
  `TabsContent` unmounts inactive panels — use `forceMount` + CSS hide).
- **ChatView / WebSocket panels:** hide inactive via `display:none` — the socket has no DOM
  dependency, so it is untouched.
- **TerminalView / xterm panels:** hide inactive, and call `fitAddon.fit()` on **re-activation**
  (xterm's `FitAddon` reads 0×0 under `display:none` → throws / `Infinity` resize loop, so never fit
  while hidden). `TerminalView` already owns a `ResizeObserver` + `fit()`; wire a refit-on-show.
  React `StrictMode` is off, so no double-mount concern.

**Tab UX:**
- **`+` menu = one runnable picker** — the spawn image's runnables (reuse the sp-9xr.4 selector /
  `ListAgentImages`) plus the built-in **`shell`** entry. Selecting one calls `CreateSession`
  (transport from the runnable's catalog metadata) and opens the right surface.
- **Tab `×`** on sessions 1..N → `CloseSession`. Session #0 is **pinned** (no `×`).
- **Per-tab status dot** reuses the `onConn` → `ConnStatus` pattern (sp-9xr.18), now per session.
- Tabs are derived from `ListSessions`; the active tab is in-memory in the store (no URL yet — §9).

Touched: `web/src/App.tsx`, `web/src/shell/AppShell.tsx`, `web/src/views/ChatView.tsx`,
`web/src/views/TerminalView.tsx`, new store under `web/src/` , `web/src/shell/ConnStatus.tsx`.

## 6 — Lifecycle & known MVP limitations

- Immediate reap on **explicit close only**; reconnect/reload re-attaches via `ListSessions`.
- **Suspend/resume restores session #0 only** — additional exec-launched sessions are not persisted
  across a spawn suspend (deliberate MVP cut; revisit if needed).
- Carries forward existing deferrals: **no** cross-session turn/permission arbitration, **no** LRU
  eviction (target is ~2–6 sessions), **no** keep-alive library.

## 7 — Testing (sp-npxq.5)

- **e2e (live):** create a spawn → via the tab bar add (1) a `shell` tab and (2) a second agent
  session of a *different* runnable → assert all run concurrently in the **same container** (shared
  fs visible across tabs), each surface works (chat responds / terminal echoes), and closing a tab
  reaps **only** that session. Covers acp-agent (2nd Pump), tui-agent (2nd tmux session), shell.
- **Unit:** port allocator (lowest-free, exhaustion rejection, free-on-close), session registry
  CRUD + roster mirroring, web store reducers, keep-alive refit-on-show.

## 8 — Sequencing

1. Build the epic with tabs as in-memory store state (keyed by stable `session_id`; `ListSessions`
   rebuilds on load), **URL untouched**.
2. Merge to main.
3. **Closing phase (§9):** per-tab URL deep-linking.

## 9 — Closing phase (post-merge): per-tab URL deep-links

The wouter/`Nav` framework from sp-jpn already landed on master (`web/src/nav/nav.ts`
`pathToNav`/`navToPath`, `web/src/nav/useNav.ts`, Router in `main.tsx`). Extend the `Nav` type to
encode the active session per spawn (e.g. `/spawn/<id>/session/<sid>`) through the established
`pathToNav`/`navToPath`/`useNav` path, with back/forward + reload restore rebuilding tabs from
`ListSessions`. Separate from the epic; done after merge.

## Open implementation notes (not blocking)

- Exact `endpoint` representation for mosh (tmux session name) vs acp (port) — opaque per-transport
  handle in the registry.
- Whether `SessionStatus` reuses the `SpawnStatus` enum or a session-scoped subset.
- Built-in `shell` runnable: synthetic catalog entry (transport `mosh`, command `bash`), always
  offered regardless of image.
