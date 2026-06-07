# Deep Research Task: ACP Capability Map for Agent-Adapter Enrichment

> Assembled 2026-06-07 to feed enrichment of spawnery's ACP adapter (`ocadapter`), proxies
> (`acpmux`/pump), and client (`internal/acp`). Scope agreed via brainstorming: capability-level +
> exhaustive; catalog + commonality-weighted prioritized backlog; full ACP surface; 4 agents.

## Your mission

I maintain **spawnery**, a system that runs coding agents in containers and renders their
conversations in a web UI. Agents reach our UI over the **Agent Client Protocol (ACP)** — either
natively, or via an adapter that translates a non-ACP agent's native API into ACP. I want to
**enrich** that layer so the UI surfaces far more of what agents can do. To prioritize that work, I
need a **complete capability map**: which ACP capabilities exist, which agents support/provide them,
and — for non-ACP agents — how their native capabilities map onto ACP.

Your output answers **WHAT to implement, not HOW.** Stay at the **capability level** — be
**exhaustive, miss nothing** — but do **not** drill into wire-level JSON/field shapes (we derive those
at implementation time). Every capability is in scope; precise byte formats are not.

## Background: ACP

The Agent Client Protocol is a JSON-RPC 2.0 protocol (newline-delimited over stdio) by Zed
Industries that lets coding agents talk to clients (editors/UIs).
- Spec/docs: https://agentclientprotocol.com/  (full index: https://agentclientprotocol.com/llms.txt)
- Machine-readable schema + reference libs (Rust + TypeScript): https://github.com/zed-industries/agent-client-protocol

ACP has two distinct halves you MUST cover separately:
1. **Emit/display surface** — what the agent SENDS the client, chiefly the `session/update`
   notification variants (assistant text, reasoning/thoughts, tool calls **and their content blocks
   incl. diffs**, plans, available commands, mode changes, etc.) plus **capability negotiation**
   (`initialize`: agent/client/prompt capabilities).
2. **Interactive client-obligation surface** — what the agent CALLS ON the client and expects it to
   fulfill: `session/request_permission`, `fs/read_text_file`, `fs/write_text_file`,
   `terminal/create|output|release|wait_for_exit|kill`, plus session lifecycle
   (`session/load`, `session/cancel`, `session/set_mode`, `authenticate`/`logout`).

Enumerate the COMPLETE set of ACP capabilities across both halves from the spec + schema. This
enumeration is the backbone of everything else — do not summarize it; itemize it.

## Background: our current baseline (the gap to measure against)

We already decode a minimal slice. Treat anything beyond this list as a **gap** (i.e. an enrichment
candidate). What our stack handles TODAY:
- **Consumed/rendered:** assistant text, reasoning/thoughts, tool calls as **title + status only**
  (no tool content, no diffs, no output/results), turn state (busy/idle/queued), and permission
  request/response (flattened to fixed allow/deny options).
- **Dropped/flattened today:** tool-call *content* (incl. file diffs/patches), token usage & cost,
  per-message model/provider attribution, agent plans / todo lists, available commands/slash-commands,
  session modes, sub-agents/tasks, step markers, snapshots/revert, structured errors, message
  lifecycle (edit/delete), and the entire client-obligation surface (`fs/*`, `terminal/*`,
  `session/load`, `session/cancel`, `session/set_mode`, `authenticate`).
- **Adapter context:** for non-ACP agents we run a translator (today: opencode's HTTP+SSE API →
  ACP). It currently maps only text→assistant and reasoning→thought and drops all richer event/part
  types. Future adapters for other non-ACP agents would follow the same pattern.

So a capability is "missing" for us if it's in ACP and/or an agent's native API but NOT in the
"consumed/rendered" list above.

## Agents to cover (4)

Profile each agent's capabilities and (for non-ACP agents) map their native capabilities to ACP:
1. **opencode** (FIXED) — non-ACP; HTTP + streaming (SSE) API. Our live adapter target. Catalog its
   full native capability surface (event types, message-part types, session metadata, etc.) and map
   each to the nearest ACP capability (or mark "no ACP analog").
2. **goose** (FIXED) — runs as a native ACP agent (`goose acp`). Catalog which ACP capabilities it
   actually supports/emits.
3 & 4. **Two more popular agents of your choice**, chosen to span both lenses:
   - **≥1 that natively speaks ACP** (e.g. Gemini CLI, Zed's own agent, or another) — to show what
     ACP-native agents emit/require.
   - **≥1 rich NON-ACP agent** (e.g. OpenAI Codex CLI, Aider, Claude Code, or another) — to show
     native-API→ACP mapping patterns for future adapters.
   State your selection rationale (popularity, capability breadth, fit to the two lenses).

## Required deliverable (single self-contained markdown report)

1. **ACP capability taxonomy** — the exhaustive itemized list of ACP capabilities across both
   halves (emit/display + client-obligation + lifecycle + capability-negotiation). One row per
   capability with a one-line description and which half it belongs to.
2. **Cross-agent support matrix** — capabilities (rows) × the 4 agents (columns); each cell:
   supported / partial / not-supported / N-A, with a brief note. For non-ACP agents, "supported"
   means "has a native capability that maps to this ACP capability."
3. **Per-agent capability profiles** — for each of the 4 agents, its full capability set. For the
   non-ACP agents (opencode + your non-ACP pick), include their NATIVE capability inventory and an
   explicit **native-capability → ACP-capability mapping** table (incl. "no ACP analog" cases and
   "ACP-feature the agent can't provide" cases).
4. **Prioritized enrichment backlog** — the gap (capabilities not in our current baseline), each
   item **scored on three dimensions**: (a) cross-agent **commonality** (how many of the surveyed
   agents support it), (b) user-visible **value**, (c) **cost/dependency** — explicitly tagging
   whether it's a cheap **decode/render-side** win vs. requires a bigger **client-side capability**
   (e.g. `fs/*`, `terminal/*`). Then a single recommended ordering led by **commonality-weighted
   blend** (commonality first, then value, then cost), with the per-dimension scores shown so the
   ranking can be re-sorted. Note which items unlock multiple agents at once.

## Rules of evidence

- **Cite a primary source for every capability claim** — official docs, the ACP JSON schema, or the
  agent's own documentation/source code. Prefer primary sources over blog posts.
- **Flag uncertainty explicitly.** If a capability's support is unverified, ambiguous, or
  version-dependent, say so and give the version/date you checked. **Do not invent APIs or
  capabilities.** A clearly-marked "unverified" is far more useful than a confident guess.
- Note ACP protocol version and each agent's version/date, since these surfaces evolve.

## Boundary

Stop at the **ACP / agent-capability layer**. You may note that delivering a capability to a web UI
implies downstream work in our rendering pipeline, but do **not** design our internal wire format or
UI — that's ours to handle.
