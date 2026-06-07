# ACP Enrichment — Decode/Render-Side Wins (sp-ufz)

**Date:** 2026-06-07
**Epic:** sp-ufz — Enrich ACP adapter/proxy/client beyond the minimal slice
**Driven by:** `docs/superpowers/research/2026-06-07-acp-capability-map-prompt.md` (capability map) and the research result `~/acp-capabilities.md`

## Problem

Our ACP pipeline decodes only a minimal slice of what agents emit. Today we surface:
text chunks, thought chunks, tool **title + status**, turn busy/idle, and a **binary**
permission prompt. Everything richer — tool-call content/results, file diffs, agent
plans/todos, per-turn token usage/cost, slash commands, session modes, structured stop
reasons, real permission option kinds, raw tool I/O, and turn cancellation — is dropped
on the floor.

The loss happens across four layers:

- **ocadapter** (`internal/ocadapter`, opencode→ACP): translates only `text`, `reasoning`,
  `permission.asked` (4 hardcoded options), and `session.idle`→`end_turn`. Tool parts,
  diffs, todos, usage, commands, modes, and real stop/error semantics are dropped at the
  source.
- **pump** (`internal/node/pump.go`, ACP→Frame): `updateToFrame` maps only
  `user`/`agent`/`thought`/`tool`(title+status). All other `session/update` variants are
  dropped.
- **Frame protocol** (`internal/node/frame.go` + `web/src/acp/frames.ts`, node↔web): a flat
  sparse struct of scalars with no fields for tool content, diffs, plans, usage, modes, etc.
- **web ChatView** (`web/src/views/ChatView.tsx`): renders text, thought, a tool chip
  (`🔧 title — status`), and a binary allow/deny modal. Nothing richer can be displayed.

## Scope

**In scope** — 10 categories, all decode/render-side or light control-plane, **no
client-obligation surface** (`fs/*`, `terminal/*`, embedded terminal, auth/logout proxying
are explicitly out: in our remote ACP topology the "client" is a thin node/web relay with
no access to the filesystem where the agent runs, so those methods are meaningless). The one
client-obligation we keep is `session/request_permission` — already wired; we only enrich
its option *kinds*.

| | Category | Notes |
|---|---|---|
| A | Tool-call **content** blocks (text/results) | Biggest single information-loss fix. |
| B | Tool-call **diff** blocks (path/oldText/newText) | The marquee coding-agent artifact. |
| C | Agent **plan / todos** | opencode `todo.updated`; replace-in-place. goose won't emit. |
| D | Per-turn **token usage + cost** | UNSTABLE in ACP — guard behind presence. goose won't emit. |
| E | **Slash commands** (`available_commands_update`) | First item touching the **input** UI. |
| F | **Session modes** (`set_mode` + `current_mode_update`) | Control-plane; capability negotiation. |
| G | Structured **StopReason + errors** | Reuses the `turn` frame. |
| H | Permission **option kinds** (un-flatten) | Enriches the existing perm path. |
| I | **rawInput/rawOutput** | Rides on A. (`locations` dropped — see below.) |
| J | **session/cancel** wiring | Control-plane; revisits the acpmux no-op. |

**Out of scope / deferred (follow-ups filed):**

- All `fs/*`, `terminal/*`, embedded-terminal display, auth/logout — client-obligation tier,
  meaningless in our topology. Not filed; reconsider only if a use case appears.
- `locations` (follow-along file:line) — low value without an editor client; belongs with the
  IDE/local-app ACP client work (**sp-881**).
- `session/load` + cross-restart history replay — our Frame seq-log already replays the live
  session to reconnecting clients; ACP `session/load` only adds cold-start/cross-restart
  durability, a bigger lifecycle project. Filed as **sp-ufz.2**.
- `session_info_update` (agent-supplied conversation title) — commonality 2, UNSTABLE; sp-jpn
  sets `document.title` from the spawn name and does not need it. Filed as **sp-ufz.3**;
  **sp-jpn depends on sp-ufz.3** so title work is done once against the richer source.

### Per-agent support (verifying agent = opencode)

Relevant agents are **opencode** (primary live target, non-ACP via ocadapter) and **goose**
(native ACP, shared-attach target), with **Claude Code** / **Gemini** as future native-ACP
drivers. opencode supports A,B,C,D,E,F,G,H,I,J. goose supports A(◑),G,H,J and lacks
C,D,E,F. Each task verifies against opencode in the live demo; native-ACP rows stay
best-effort and must degrade gracefully when a field is absent.

## Architecture — Frame envelope (typed payload, hybrid)

Decision: **typed payload envelope (hybrid)** — chosen over (a) flat field extension and
(b) ACP pass-through to the web.

- *Flat field extension* (add ~12 optional scalars) was rejected: the struct balloons and
  nested data (content blocks, diffs, plan lists, option lists) sits awkwardly as flat fields,
  with kind↔field validity left implicit.
- *ACP pass-through* (forward ACP `session/update` to the web, decode in TS) was rejected:
  it couples the web to a **semi-synthetic** ACP shape (ocadapter *synthesizes* ACP from
  opencode; goose has quirks and UNSTABLE variants) and erodes Frame as a stable contract.
  The Frame layer is precisely where opencode's `todo.updated` and a native agent's `plan`
  must both normalize to one web vocabulary; pass-through would push both dialects onto the web.

Keep `Seq` + `Kind` as the envelope and keep the existing flat scalar fields for the current
simple kinds (`user`/`agent`/`thought`/`turn`/`perm_request`/`reset`/`prompt`/`perm_response`).
Add **optional nested payload pointers** for the rich kinds — self-documenting, no
double-decode, and a clean TS discriminated union keyed on `kind`.

```go
type Frame struct {
    // envelope (unchanged)
    Seq int64; Kind string
    // existing scalars (unchanged): Text, ToolID, Title, Status, State, Queued, ReqID, FromSeq

    // enriched / new payloads (all optional):
    Tool   *ToolPayload   // enriches kind="tool"
    Plan   []PlanEntry    // kind="plan"        (replace-in-place: latest supersedes)
    Usage  *Usage         // rides kind="turn"  (per-turn breakdown; guard nil)
    Reason string         // kind="turn"        (end_turn|max_tokens|max_turn_requests|refusal|cancelled)
    Error  *ErrorInfo     // kind="turn"        (code+message; from opencode NamedError union)
    Cmds   []Command      // kind="commands"    (advertised slash commands; replace-in-place)
    Mode   *ModePayload   // kind="mode"        (current + availableModes)
    Options []PermOption  // kind="perm_request" (replaces binary; optionId+name+kind)
    OptionID string       // kind="perm_response" (replaces Allow bool)
}

type ToolPayload struct {
    Content   []ContentBlock  // text/result blocks (cat A)
    Diff      *Diff           // path/oldText/newText (cat B)
    RawInput  json.RawMessage // cat I
    RawOutput json.RawMessage // cat I
}
type ContentBlock struct{ Type, Text string } // extend as needed (image/resource later)
type Diff struct{ Path, OldText, NewText string }
type PlanEntry struct{ Content, Priority, Status string }
type Usage struct{ Input, Output, Cached, Thought, Total int; Cost *float64 }
type ErrorInfo struct{ Code int; Message string }
type Command struct{ Name, Description, InputHint string }
type PermOption struct{ OptionID, Name, Kind string } // allow_once|allow_always|reject_once|reject_always|...
type Mode struct{ ID, Name string }
type ModePayload struct{ Current string; Available []Mode }
```

Two new **client→node** control frames mirror the existing `prompt`/`perm_response` upward
frames:

- `{kind:"cancel"}` — interrupt the active turn (cat J).
- `{kind:"set_mode", modeId}` — switch session mode (cat F).

Properties:

- **Backward-compatible** — existing kinds and scalar fields are untouched, so the
  seq-log/replay/`reset` machinery and current rendering keep working unchanged.
- **Frame stays the normalization point** — the web never sees the two ACP dialects.
- **One breaking change to an existing frame:** `perm_response` `Allow bool` → `OptionID
  string`, carried as part of cat H with both ends updated together and a migration test.

## Work decomposition

Sliced **vertically by category** (each task spans ocadapter → pump → Frame → ChatView and
delivers a demoable win) on top of one shared foundation task. All land under **sp-ufz**.

| # | Task | Layers | Depends on |
|---|---|---|---|
| 0 | **Frame envelope foundation** — typed-payload hybrid Frame (`frame.go` + `frames.ts`), discriminated-union TS types, all existing kinds green | Frame | — |
| 1 | **A+I — Tool content + raw** — ocadapter `ToolPart`/`tool.execute.after` → ACP tool content + rawInput/rawOutput; pump decode; ChatView expandable tool chip | all 4 | 0 |
| 2 | **B — Diff blocks** — ocadapter `session.diff`/`PartPatchPart` → ACP diff; pump decode; ChatView diff component | all 4 | 0,1 |
| 3 | **G — StopReason + errors** — ocadapter stop semantics + `NamedError` → `turn.Reason`/`turn.Error`; ChatView turn-ending text | pump→Frame→web | 0 |
| 4 | **H — Permission option kinds** — ocadapter real options → `perm_request.Options[]`; `perm_response.OptionID`; PermissionModal N buttons | existing perm path | 0 |
| 5 | **C — Plan / todos** — ocadapter `todo.updated` → ACP `plan`; pump decode; ChatView checklist (replace-in-place) | OC,GEM,CC | 0 |
| 6 | **D — Token usage** — ocadapter `StepFinishPart` usage+cost → `turn.Usage`; ChatView badge; guard nil | OC,GEM,CC | 0,3 |
| 7 | **E — Slash commands** — ocadapter MCP-prompt commands → `available_commands_update` → `commands` frame; PromptInput `/`-autocomplete | OC,GEM,CC | 0 |
| 8 | **F — Session modes** — capability negotiation (`availableModes` at init) + `current_mode_update` + client `set_mode`; ChatView selector; pump/acpmux control path | OC,GEM,CC | 0 |
| 9 | **J — session/cancel** — client `cancel` frame → pump → upstream `session/cancel`; revisit acpmux no-op; ChatView stop button; ties to G's `cancelled` | all 4 | 0,3 |

**Execution order** (research-weighted, deps respected): `0 → 1 → 2 → 3 → 4 → 5 → 6 → 7 → 8 → 9`.

Cross-cutting:

- Tasks **8 & 9** are the only ones touching the **control path** back to the agent
  (`set_mode`, `cancel`) and the **acpmux no-op**. Each carries an explicit sub-decision for
  the shared-attach "whose action wins" question. **v1 answer: any client may cancel or
  set-mode the shared turn** (no arbitration — consistent with the shared-attach v1 scope of
  "one user across devices").
- Tasks **1–7** are pure display/decode and don't touch the upward control path.

## Testing

Mirror the existing test seams; every category gets a fixture-driven path test plus a render
test.

**Go — table-driven, by seam:**

- **ocadapter** (`internal/ocadapter`): fixtures of real opencode SSE events (a `ToolPart`
  lifecycle, a `session.diff`, a `todo.updated`, a `StepFinishPart`, a `permission.asked`
  with options, a stop/`NamedError`) → assert the emitted ACP `session/update` JSON. Most
  enrichment logic lives here, so it gets the most coverage.
- **pump `updateToFrame`** (`internal/node`): fixtures of ACP variants → assert the resulting
  `Frame` (right kind, right payload populated, scalars untouched for existing kinds).
- **Frame round-trip** (task 0): encode→decode every kind incl. new payloads; assert existing
  kinds byte-stable so replay/seq-log is unaffected. The `perm_response` `Allow`→`OptionID`
  change gets an explicit migration test.
- **Control path** (tasks 8–9): `cancel`/`set_mode` client frames reach the upstream ACP
  call; acpmux "any client cancels the shared turn" has a multi-client unit test.

**Web — component + e2e:**

- Component tests per new piece: expandable tool chip (A/I), diff block (B), plan checklist
  with in-place updates (C), permission modal with N option buttons (H), usage badge (D),
  mode selector (F), stop button (J).
- **Playwright** (`e2e/*.spec.ts`): extend the demo flow to assert a tool call expands to show
  output, an edit renders a diff, a plan checklist appears and advances, and Stop interrupts a
  turn. Guard unstable/absent cases (goose has no plan/usage) by asserting graceful *absence*
  — no empty checklist, no zero-token badge.

**Acceptance per task:** the fixture flows green end-to-end *and* the category renders
correctly in the live demo (opencode is the verifying agent; native-ACP rows stay best-effort
and degrade gracefully when a field is absent).

## Execution note

Implementation runs in a **separate git worktree** (per the brainstorming decision), executed
via subagent-driven-development in beads mode over the sp-ufz child tasks.
