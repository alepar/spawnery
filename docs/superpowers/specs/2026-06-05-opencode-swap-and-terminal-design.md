# Design: Switch goose → opencode, with concurrent TUI + web (ACP as canonical protocol)

Date: 2026-06-05
Status: Draft (pending review)
Related: epic `sp-wsu` (tmux attach), research `docs/research/2026-06-04-tmux-attach-transport-research-prompt.md`,
results at `~/opencode-serve.md`, lifecycle `sp-gd9`.

## Motivation

We want one live agent session that a containerized TUI (in tmux) and the Spawnery web UI
can drive interchangeably. opencode offers this out of the box: `opencode serve` is a
headless, server-authoritative HTTP+SSE server; the TUI (`opencode attach <url>`) and any
API client share the same session and state. goose's model has no equivalent multi-client
story, so we replace goose with opencode.

Strategically, this is also the first step toward **onboarding many agents**. Each agent
ships its own agent-client↔agent-server protocol (opencode rolled its own HTTP/SSE API;
goose speaks ACP; future agents will differ again). Rather than couple Spawnery to any one
of them, we standardize on **ACP (Agent Client Protocol, https://agentclientprotocol.com/)
as the canonical protocol** spoken across web ⇄ CP ⇄ node. Each agent is integrated via a
per-agent **adapter** that normalizes its native protocol to canonical ACP. ACP is the
anti-corruption layer; vendor API churn is quarantined inside adapters.

## ACP as the canonical agent protocol

- **Canon = neutral spec ACP**, not "whatever the current agent emits." web/CP/node speak
  only canonical ACP and must not assume agent-specific dialects.
- **The node already speaks real spec-ACP.** `internal/acp/client.go` implements the actual
  ACP wire (`initialize`, `session/new`, `session/prompt`, `session/update` with
  `agent_message_chunk`/`agent_thought_chunk`, `session/request_permission` →
  `{outcome:{outcome:"selected", optionId}}`). `pump.go`'s `pickPermOption` selects on the
  ACP-standard option `kind` (`allow_*`/`reject_*`). The `goose` references in `pump.go` are
  **misleading comments, not protocol coupling.**
- **Per-agent adapter pattern:**
  - **opencode** → a *normalizing adapter* (new): opencode HTTP/SSE ⇄ canonical ACP.
  - **goose** → a *near pass-through* (already ACP); kept working as the conformance witness
    that the node is genuinely agent-neutral.
  - **future agents** → their own adapters onto the same canon.

### Node neutralization (small — corrects an earlier over-estimate)
1. Rename `goose`-flavored comments/field docs in `internal/node/pump.go` to agent-neutral
   (`agentID` = "the ACP request id", etc.). No behavior change.
2. Verify `pickPermOption` against the exact ACP option `kind`s the opencode adapter emits
   (it must emit `allow_once`/`allow_always`/`reject_once`/`reject_always`).
3. Confirm the node makes no other agent-specific assumption (update variants, titles,
   protocolVersion). Add a tiny conformance test fixture of canonical-ACP frames.

## Locked decisions

1. **Translation lives in a per-agent in-pod adapter.** The opencode adapter normalizes to
   canonical ACP; node/CP/web speak only ACP. The node may receive *small, agent-neutral*
   cleanups (above) but no agent-specific logic.
2. **Unified transport.** Both runc (docker) and runsc (CRI) lanes use the same **TCP ACP
   socket**. The docker-API stdio-attach path is **removed**.
3. **Adapter = ACP-*agent* server backed by opencode**, speaking canonical ACP to the node.
4. **One session per spawn, discover-or-create.** On container start the adapter `GET
   /session`; if a persisted session exists (e.g. after resume) it **reuses** it, otherwise
   it creates one. The node's ACP `session/new` maps onto that single session. (Naive
   "always create" would orphan restored history on resume — see Suspend/resume.)
5. **TUI runs inside the container.** mosh-server runs on the node; its child execs into the
   spawn container (docker exec / CRI exec) and runs `tmux new-session -A` → `opencode
   attach http://127.0.0.1:4096`. The agent image ships `opencode` + `tmux`.
6. **CP control plane, direct UDP data plane.** `spawnctl tmux <spawn>` → CP (owner-only
   authz, locate node, auto-resume if suspended) → node starts mosh-server + exec → returns
   `{node addr, UDP port, mosh key}` → spawnctl runs mosh-client straight to the node. Only
   the UDP data plane bypasses CP. (Future: tunnel it through the CP↔node link for
   airgapped/NAT'd deployments.)
7. **Two phases, one spec.**
8. **Idle: only opencode-observable activity counts** (see Idle semantics for the exact,
   honest mechanism and its accepted UX cost).

## Architecture

```
┌──────────────────────────── spawn container ────────────────────────────┐
│  opencode serve  ───────────►  HTTP + SSE on 127.0.0.1:4096              │
│  (headless, long-lived)            ▲          ▲                          │
│  tmux ─► opencode attach ──────────┘          │ HTTP/SSE (in-pod)        │
│          127.0.0.1:4096 (TUI)                 │                          │
│  acpadapter (opencode → canonical ACP) ───────┘                         │
│    • discover-or-create ONE opencode session                             │
│    • speaks canonical ACP on TCP  ◄── listens for the node               │
└───────────────────────────────────────────┬──────────────────────────────┘
                       TCP canonical ACP (both lanes: runc shared-netns / runsc CRI)
                                             │
                                    ┌────────▼─────────┐
                                    │  node (Pump)     │  speaks ONLY canonical ACP
                                    │  parses ACP      │  (small neutral cleanups)
                                    └────────┬─────────┘
                                             │ spawnery frames (UNCHANGED)
                                    ┌────────▼─────────┐
                                    │  CP ⇄ web UI     │  UNCHANGED
                                    └──────────────────┘

Why 127.0.0.1 is safe: every opencode client (the adapter, and the TUI via exec-into-
container) is IN-POD, so opencode never needs 0.0.0.0/password despite the netns warning in
the research. The node talks to the ADAPTER over TCP, not to opencode directly.

Terminal path (Phase 2):
  spawnctl tmux <spawn> ─(CP: authz, locate, auto-resume)─► node
     node: start mosh-server; child = exec-into-container `tmux new -A` + opencode attach
     node ─► CP ─► spawnctl: {node addr, UDP port, mosh key}
  spawnctl ── mosh UDP (direct to node, around CP) ──► node ─exec PTY─► tmux/TUI in container
```

## Phase 1 — opencode swap (web-only, fully verifiable)

### Agent image
- Replace goose with `opencode`; also install `tmux` (used in Phase 2, harmless in Phase 1).
- **Pin the opencode version** and record it (`sst`→`anomalyco` rebrand, near-daily releases).
- Provider/model config: point opencode at the existing sidecar's OpenAI-compatible endpoint
  (`127.0.0.1:8080`) + injected key, replacing goose's env wiring in `entrypoint.sh`. Sidecar
  unchanged.
- **Process supervision:** the container runs two long-lived processes (opencode serve +
  adapter). Use a minimal supervisor (tini/sh) as PID1. If opencode crashes, the adapter
  must fail its ACP connection so the node sees the agent as down (not a silent zombie); the
  supervisor restarts the pair per the lane's restart policy.

### opencode adapter (`deploy/agent/acpadapter`, rewritten)
ACP-agent server backed by opencode. Listens on TCP (both lanes), keeps the at-most-one-node
connection + gap-buffer for reconnects, and normalizes to **canonical ACP**:

| Canonical ACP (node drives) | opencode (adapter performs) |
|---|---|
| `initialize` handshake / readiness | `GET /global/health`; discover-or-create the session |
| `session/new` | map onto the single discovered/created session |
| `session/prompt` + streamed `session/update` (`agent_message_chunk`/`agent_thought_chunk`/tool) | `POST /session/:id/prompt_async` + `GET /event` SSE (`message.part.updated` deltas) |
| `session/request_permission` (options w/ ACP `kind`s) → `{outcome:selected,optionId}` | `permission.asked`/`updated` SSE → `POST /session/:id/permissions/:permissionID` |
| turn busy / idle | `session.status` / `session.idle` |
| cancel | `POST /session/:id/abort` |

- The adapter subscribes to `/event` once and fans opencode events into ACP `session/update`
  — **including TUI-originated events** — so the web UI reflects TUI activity automatically.
- SSE has no auto-reconnect in the Go SDK; reconnect with backoff and re-bootstrap via `GET
  /session/:id/message`.
- Permission options must be emitted with standard ACP `kind`s so the node's neutral
  `pickPermOption` works unchanged.
- Where the Go SDK lags the server (`prompt_async`, newer events), call REST directly using
  `/doc` as the contract.

### Transport
- Node dials TCP for both lanes. Remove the docker-API stdio-attach path and the abstract-UDS
  default; standardize on the TCP listen spec already used by the runsc lane.

### Episode-end signal (was implicit in goose)
Today `acpadapter` ends the spawn when goose exits ("the spawn is over when the agent exits").
`opencode serve` is long-lived and does **not** exit per-session, so that signal is gone.
Redefine episode end explicitly: the spawn lifetime is now driven by **CP stop/suspend and
the idle reaper**, not agent-process exit. The adapter reports agent health (opencode
up/down) but a healthy idle server is a *running, idle* spawn, not a finished one. Verify the
node's lifecycle FSM no longer depends on agent-exit to mark a spawn done.

### Phase 1 acceptance
- Web UI drives a conversation end-to-end against opencode with **no CP/web** code changes
  and only the small neutral node cleanups.
- Permissions, streaming, busy/idle, cancel surface in the web UI as before.
- **New opencode e2e tests.** The existing `*goose*` e2e suites are agent-specific; keep a
  goose pass-through suite as the neutrality witness and add opencode equivalents — do not
  claim the goose suites "pass" against opencode.

## Phase 2 — terminal (new capability)

### spawnctl
- `spawnctl tmux <spawn-id>`: calls CP, receives the mosh bootstrap, execs mosh-client to the
  node. Reattaches transparently if a session exists (mosh roam + `tmux -A`).

### CP
- Owner-only authz; locate the spawn's node; auto-resume if suspended; new CP↔node control
  message to start the session and carry back `{node addr, UDP port, mosh key}`.

### Node (additive; does not touch the mediation path)
- mosh-server management: child command = exec-PTY into the spawn container (docker exec / CRI
  exec) running `tmux new-session -A <name>` → `opencode attach http://127.0.0.1:4096`.
- Track the per-spawn tmux/mosh session for reattach; manage/clean up UDP ports.

### Idle semantics (honest mechanism)
- **Policy:** only opencode-observable activity (prompts, tool runs, etc., seen by the
  adapter on SSE and forwarded as ACP frames) resets the idle clock. Decision 8.
- **Consequence (accepted):** terminal keystrokes flow `mosh → node → exec-PTY → tmux` and
  do **not** pass through the adapter/SSE, so reading, scrolling, or pane-navigating in the
  TUI is invisible to the idle clock. A connected-but-not-submitting operator **can** be
  idle-suspended, which tears down the container and drops their tmux/mosh; reattach
  auto-resumes. This is the chosen trade (resource protection over attach-pinning).
- **Optional later:** if this proves too aggressive, have the Phase-2 node exec-PTY feed
  raw keystroke bytes into `lastActivity` (a node change), or add a max-attach keepalive TTL.

### Phase 2 acceptance
- `spawnctl tmux <spawn>` attaches a TUI in the container; typing in the TUI shows in the web
  UI and vice versa (shared session). Attaching to a suspended spawn auto-resumes first. A
  second `spawnctl tmux` reattaches.

## Suspend / resume

- opencode persists session state in **SQLite** in the container filesystem. Two requirements
  the old design glossed over:
  1. **State dir must be inside a captured mount.** Verify opencode's data dir (it may
     default to `$HOME/.local/share/opencode` or a project dir) is within the dirty-tree-
     captured mounts; if not, relocate it (env/config) so resume restores it.
  2. **Quiesce before snapshot.** SQLite + WAL is written continuously; a naive snapshot can
     capture a torn/un-checkpointed DB. On suspend, stop/quiesce opencode (or checkpoint the
     WAL) before the dirty-tree capture so the restored DB is consistent.
- Suspend tears down the container, killing any live tmux/mosh child; resume restores the DB,
  the adapter **discovers** the existing session (decision 4), and the operator reattaches.

## Permissions

- **Web/node is the authoritative permission surface.** opencode bug #21154: a TUI attaching
  to an existing session does not fetch pending permission/question prompts, so we must not
  depend on the TUI to clear asks; the adapter `GET`s pending perms on connect and forwards
  them as ACP `session/request_permission`.
- **Dual-answer policy (decision needed → default chosen):** both the web (via
  node→adapter→`POST permissions`) and the TUI can answer the *same* permission, a documented
  race risk. Default: **accept last-write-wins** at the opencode server (it is the single
  serializer) and surface the resolved outcome to both surfaces via SSE; do **not** attempt
  to disable TUI answering in Phase 2. Revisit if the race causes real confusion.

## Concurrency (busy is serialized, not merged)

opencode returns a busy error on a concurrent submit. The node's `Pump` only enters `busy`
when *it* sends a prompt, and canonical ACP has no "another client started a turn" notion.
So the adapter must **synthesize ACP turn state for TUI-originated turns**: when the TUI
submits, the adapter emits the user frame + a turn-busy `session/update` to the node so the
node's busy/queue stays correct and a queued web prompt isn't fired into a busy server; it
maps opencode busy errors back to ACP turn state. This is real adapter work, not a footnote.

## Audit / E8 (corrected — the TUI *is* a mutation path)

The opencode TUI is **not** merely a viewer: opencode exposes `POST /session/:id/shell` and
bash/edit tools, so an attached operator can run arbitrary commands and mutate `/app`,
bypassing the sidecar audit point exactly as a raw shell would. We **own this as accepted
risk** for now: owner-only (enforced at CP), disclosed per E8 trust/safety, and out of scope
to fully audit here. We do **not** claim the terminal is audit-neutral.

## Open risks

1. **Adapter fidelity** — opencode SSE → canonical ACP (events, permission kinds, turn
   state, busy synthesis) is the core Phase-1 engineering risk; keep it bug-for-bug verified
   against the goose pass-through as the neutrality reference.
2. **Single-session landing** — `opencode attach` must reliably land on the one
   discovered/created session on the pinned version (`--session` is unreliable, #5445/#7149).
3. **Suspend consistency** — SQLite quiesce + correct state-dir capture must be proven, or
   resume loses/corrupts history.
4. **mosh bootstrap security** — AES key over the CP↔node control message + UDP port mgmt;
   the key is the only data-plane auth around CP.
5. **opencode version churn** — pin, record, treat `/doc` as the contract over the Go SDK.

## Out of scope

- Multiple sessions per spawn / session picker.
- A full audit story for terminal-originated mutations (E8 follow-up).
- Tunneling the mosh data plane through the CP↔node link (future airgapped/NAT support).
- Keystroke-based idle keepalive / max-attach TTL (optional Phase-2+ refinement).
