# Codex CLI support (TUI, switchable model via sidecar)

**Date:** 2026-06-08
**Status:** Approved — ready for implementation

## Summary

Add OpenAI's **Codex CLI** as a selectable agent in Spawnery, in terminal (tmux)
mode only, mirroring the existing `claude-code` integration. The spawn's model
(`SPAWN_MODEL`) is wired in via the in-pod sidecar and is switchable from inside
Codex's own `/model` TUI — the same bar as Claude Code (no curated multi-model
menu).

## Background: how "switchable models like Claude Code" works today

- `internal/agentcaps/agentcaps.go` is the shared registry mapping an agent
  binary → its runnables. `claude-code` ships one runnable, `claude-tui`
  (`ModeTmux`, `RelayRawPTY`).
- `deploy/agent/entrypoint.sh` is the image dispatcher: the node passes the
  runnable id as `$1` and a per-runnable `case` wires env/config and execs the
  agent under `spawn-tmux`. The `claude-tui` case points Claude Code at the
  sidecar (`ANTHROPIC_BASE_URL`) and exposes `SPAWN_MODEL` as a custom model
  option selectable in the TUI.
- The sidecar (`internal/sidecar`, `cmd/sidecar/main.go`) is the single egress
  chokepoint. `mux.Handle("/v1/messages", …)` is an Anthropic→ChatCompletions
  translator for Claude Code; `mux.Handle("/", …)` is a **transparent reverse
  proxy** (`httputil.NewSingleHostReverseProxy`) that forwards every other path
  to `SIDECAR_UPSTREAM` (default `https://openrouter.ai/api`), injecting the real
  `OPENROUTER_API_KEY`.
- The node injects `OPENAI_BASE_URL=http://<sidecar>:8080/v1` into the pod
  (`internal/spawnlet/manager.go`) and advertises its shipped binaries via
  `AGENT_BINARIES` (CSV → `Register.Binaries`, `internal/node/attach.go`).
- The web create flow passes a single hardcoded `MODEL` constant
  (`web/src/App.tsx`, currently `deepseek/deepseek-v4-flash`) as `SPAWN_MODEL`;
  actual model switching happens inside the agent's TUI, not the web UI.

## Key decision: the wire protocol

Codex custom providers **require** `wire_api = "responses"` (the OpenAI Responses
API). The previous `wire_api = "chat"` (Chat Completions) was removed in Feb
2026. This is normally a blocker because the sidecar only *translates*
`/v1/messages`; everything else is a passthrough. Two facts make it a clean
mirror of the Claude Code effort anyway:

1. The sidecar's catch-all forwards **any** path. `POST /v1/responses` →
   `https://openrouter.ai/api/v1/responses`, key injected, body untouched.
2. **OpenRouter ships an OpenAI-compatible Responses API** (Beta), a drop-in for
   OpenAI's Responses endpoint across hundreds of models.

So Codex → sidecar → OpenRouter Responses works with **zero sidecar/proxy/proto
changes**. Codex points at the same `OPENAI_BASE_URL=http://<sidecar>:8080/v1`
the node already injects; `base_url + /responses` resolves correctly because
`SIDECAR_UPSTREAM` is `…/api` (no `/v1`), exactly as it does for the existing
`/v1/chat/completions` traffic.

## Components & changes

All five changes mirror existing patterns. No web, sidecar, or proto code is
touched.

### 1. Registry — `internal/agentcaps/agentcaps.go`

Add a `"codex"` binary with one runnable, mirroring `claude-code`:

```go
"codex": {
    {ID: "codex-tui", Mode: ModeTmux, Launch: []string{"codex"}, Relay: RelayRawPTY, Label: "Codex · terminal"},
},
```

This makes `Known("codex")` true and auto-expands to a selectable runnable in the
web `runnable-select` dropdown via `ListAgentImages` — no web changes.

### 2. Dispatcher — `deploy/agent/entrypoint.sh`

Add a `codex-tui)` case (sibling of `claude-tui)`). Because the model is
per-spawn, generate `~/.codex/config.toml` at runtime (like
`setup_opencode_provider` does for opencode):

```toml
model = "<SPAWN_MODEL>"
model_provider = "spawnery"
approval_policy = "never"            # non-interactive; the pod is the trust boundary
sandbox_mode  = "danger-full-access" # pod (gVisor) IS the sandbox; disable Codex's own Landlock/seccomp
disable_response_storage = true      # OpenRouter Responses Beta is stateless

[model_providers.spawnery]
name = "Spawnery Sidecar"
base_url = "<OPENAI_BASE_URL>"       # already = http://<sidecar>:8080/v1
env_key  = "CODEX_SPAWNERY_KEY"
wire_api = "responses"
```

Then `export CODEX_SPAWNERY_KEY=sk-unused-sidecar-injects-real-key` (the sidecar
injects the real key) and `exec spawn-tmux codex`. The model is selectable inside
Codex's own `/model` picker — any OpenRouter id passes through the sidecar — which
satisfies "switchable models, like Claude Code."

Notes:
- Use `CODEX_HOME` if needed to pin the config location to a writable path;
  default is `~/.codex`.
- Mirror the env-with-default idiom used by the other cases
  (`export TERM="${TERM:-xterm-256color}"`).

### 3. Dockerfile — `deploy/agent/Dockerfile`

Install the Codex native binary (Rust) from `openai/codex` GitHub releases,
pinned to a specific version, with a `codex --version` verification step —
mirroring the goose/nori/opencode tarball approach. This avoids adding Node.js to
the image (Codex is also distributed via `npm @openai/codex`, but the image has
no Node).

### 4. Node binary advertisement — deploy config

Add `codex` to the node's `AGENT_BINARIES` CSV (deploy env) so the node Registers
it and the CP catalog advertises it (`upsertAgentCatalog`). This is a deploy-env
change, not code. The exact deploy manifests live under `deploy/`; update wherever
`AGENT_BINARIES` is set for the agent node.

### 5. Tests — `internal/agentcaps/agentcaps_test.go`

Existing invariants (globally-unique runnable IDs; tmux runnables must declare a
`Launch`) extend to the new entry automatically. Add a focused assertion that
`Lookup("codex", "codex-tui")` resolves with `ModeTmux` / `RelayRawPTY`.

## Out of scope

- No ACP / rich-web (ChatView) runnable for Codex.
- No curated multi-model menu (mirror Claude Code: single wired model, switchable
  in-TUI).
- No sidecar translation, no proxy changes, no web UI changes, no proto changes.

## Risks — verify during implementation (manual e2e in the pod)

1. **OpenRouter Responses Beta is stateless.** Confirm Codex works with
   `disable_response_storage = true` and does not depend on server-side
   `previous_response_id` chaining or `GET /v1/responses/:id`.
2. **Default model over Responses.** The global `MODEL`
   (`deepseek/deepseek-v4-flash`, `web/src/App.tsx`) must be served over
   OpenRouter's *Responses* endpoint; some models may be Chat-Completions-only
   there. May require choosing a Codex-appropriate default for verification (not
   a code change in this spec).
3. **First-run onboarding.** Confirm Codex with an API-key provider skips ChatGPT
   OAuth and any trust-folder prompt when launched under tmux non-interactively.

## Acceptance

- `codex` appears as a selectable runnable ("Codex · terminal") for the agent
  image in the web marketplace detail view.
- Spawning it launches Codex in a tmux terminal, routed through the sidecar to
  OpenRouter's Responses API, with the spawn's model active.
- Switching models from inside Codex's `/model` picker works for OpenRouter ids.
- `go test ./internal/agentcaps/...` passes; `golangci-lint` clean.
