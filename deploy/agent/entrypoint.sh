#!/bin/sh
# Dispatcher entrypoint — the node passes the runnable id as $1; the image owns how each
# runnable launches (serve+adapter wiring, tmux wrapping, env setup, etc.).
#
# opencode reaches the model provider through the sidecar's OpenAI-compatible endpoint
# (127.0.0.1:8080), which injects the real OpenRouter key. We register a custom "spawnery"
# provider pointing at the sidecar and add the spawn's model to its model map, so any
# SPAWN_MODEL (an OpenRouter id, e.g. openai/gpt-4o-mini) works without baking a static
# catalog. The adapter then prompts opencode with model "spawnery/<SPAWN_MODEL>".
set -e

# Dispatcher: the node passes the runnable id as $1; the image owns how each runnable launches.
# Empty (legacy) or opencode-served -> the default opencode serve + adapter path below.
case "${1:-}" in
  ""|opencode-served)
    : # fall through to the default opencode path below
    ;;
  opencode-tui)
    export TERM="${TERM:-xterm-256color}"
    exec spawn-tmux opencode
    ;;
  goose-tui)
    export TERM="${TERM:-xterm-256color}"
    # goose's openai provider env vars:
    #   GOOSE_PROVIDER=openai      — select the OpenAI-compatible provider
    #   GOOSE_MODEL=<id>           — model id (passed from SPAWN_MODEL by the node)
    #   OPENAI_API_KEY             — API key (unused; sidecar injects the real key)
    #   OPENAI_BASE_URL            — already set by the node to the sidecar (http://<sidecar>:8080/v1)
    #   GOOSE_TELEMETRY_OFF=1      — skip the interactive analytics-consent prompt on first run
    export GOOSE_PROVIDER="${GOOSE_PROVIDER:-openai}"
    export GOOSE_MODEL="${GOOSE_MODEL:-${SPAWN_MODEL:-openai/gpt-4o-mini}}"
    export OPENAI_API_KEY="${OPENAI_API_KEY:-sk-unused-sidecar-injects-real-key}"
    export GOOSE_TELEMETRY_OFF="${GOOSE_TELEMETRY_OFF:-1}"
    # OPENAI_BASE_URL is already set by the node to the sidecar endpoint; goose's openai provider
    # reads it directly (same variable name as the OpenAI SDK convention).
    # Use "goose session" (not bare "goose") — the bare command opens the configure wizard.
    exec spawn-tmux goose session
    ;;
  goose-acp)
    echo "goose-acp not wired yet (needs stdio-ACP->TCP bridge; see sp-9xr.16)" >&2; exit 1 ;;
  claude-tui)
    export TERM="${TERM:-xterm-256color}"
    # Claude Code speaks the Anthropic Messages API; the sidecar's /v1/messages converter
    # translates it to OpenAI Chat Completions against OpenRouter (and injects the real key).
    # The node sets OPENAI_BASE_URL=http://<sidecar>:8080/v1 — strip the trailing /v1 for
    # ANTHROPIC_BASE_URL (Claude Code appends /v1/messages itself).
    SIDECAR_OAI="${OPENAI_BASE_URL:-http://127.0.0.1:8080/v1}"
    export ANTHROPIC_BASE_URL="${ANTHROPIC_BASE_URL:-${SIDECAR_OAI%/v1}}"
    export ANTHROPIC_API_KEY="${ANTHROPIC_API_KEY:-sk-unused-sidecar-injects-real-key}"
    # SPAWN_MODEL is an OpenRouter id (e.g. openai/gpt-4o-mini); expose it as Claude Code's
    # custom model option so it is offered/selected in the TUI.
    export ANTHROPIC_CUSTOM_MODEL_OPTION="${SPAWN_MODEL:-openai/gpt-4o-mini}"
    export ANTHROPIC_CUSTOM_MODEL_OPTION_NAME="Spawnery"
    export ANTHROPIC_MODEL="${ANTHROPIC_MODEL:-${SPAWN_MODEL:-openai/gpt-4o-mini}}"
    export ENABLE_TOOL_SEARCH="${ENABLE_TOOL_SEARCH:-true}"
    export DISABLE_AUTOUPDATER="${DISABLE_AUTOUPDATER:-1}"
    export DISABLE_TELEMETRY="${DISABLE_TELEMETRY:-1}"
    export CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC="${CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC:-1}"
    export CLAUDE_CODE_DISABLE_TERMINAL_TITLE="${CLAUDE_CODE_DISABLE_TERMINAL_TITLE:-1}"
    exec spawn-tmux claude ;;
  nori*|nori)
    echo "nori not wired yet (ACP-TUI client; see sp-9xr.12)" >&2; exit 1 ;;
  *)
    echo "unknown runnable: $1" >&2; exit 1 ;;
esac

# --- default opencode serve + adapter (runnable "" / opencode-served) ---
SIDECAR_BASE="${SIDECAR_BASE:-http://127.0.0.1:8080/v1}"
MODEL_ID="${SPAWN_MODEL:-openai/gpt-4o-mini}"

# Generate the opencode config (custom OpenAI-compatible provider via the sidecar).
mkdir -p /etc/opencode
cat > /etc/opencode/opencode.json <<EOF
{
  "\$schema": "https://opencode.ai/config.json",
  "model": "spawnery/${MODEL_ID}",
  "provider": {
    "spawnery": {
      "npm": "@ai-sdk/openai-compatible",
      "name": "Spawnery Sidecar",
      "options": { "baseURL": "${SIDECAR_BASE}", "apiKey": "sk-unused-sidecar-injects-real-key" },
      "models": { "${MODEL_ID}": { "name": "${MODEL_ID}" } }
    }
  }
}
EOF
export OPENCODE_CONFIG=/etc/opencode/opencode.json

# The adapter forwards this as opencode's "providerID/modelID".
export SPAWN_MODEL="spawnery/${MODEL_ID}"

# opencode listens on loopback; both clients (the adapter and the in-pod TUI) are
# in-pod, so 127.0.0.1 is sufficient (no 0.0.0.0/password needed).
opencode serve --port 4096 --hostname 127.0.0.1 &

# The adapter listens for the node (ACP_LISTEN, set by the node per lane) and
# bridges to opencode at 127.0.0.1:4096.
export OPENCODE_BASE_URL="${OPENCODE_BASE_URL:-http://127.0.0.1:4096}"
exec /usr/local/bin/acpadapter
