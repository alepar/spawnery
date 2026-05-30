#!/bin/sh
# Configure Goose to use the inference-proxy sidecar as its OpenAI-compatible
# endpoint, then launch it in ACP/stdio mode. The sidecar (shared netns) listens
# on 127.0.0.1:8080 and injects the real OpenRouter key, so the dummy key below
# is never used for auth — Goose just requires the var to be set.
set -e

# OPENAI_HOST is host-only (no path). OPENAI_BASE_PATH defaults to
# v1/chat/completions, which the sidecar forwards to OpenRouter's /api/v1/*.
export GOOSE_PROVIDER="${GOOSE_PROVIDER:-openai}"
export GOOSE_MODEL="${GOOSE_MODEL:-${SPAWN_MODEL}}"
export OPENAI_HOST="${OPENAI_HOST:-http://127.0.0.1:8080}"
export OPENAI_BASE_PATH="${OPENAI_BASE_PATH:-v1/chat/completions}"
export OPENAI_API_KEY="${OPENAI_API_KEY:-sk-unused-sidecar-injects-real-key}"

# Disable Goose's interactive keyring/secret prompts; values come from env.
export GOOSE_DISABLE_KEYRING="${GOOSE_DISABLE_KEYRING:-1}"

# Wire the app's instructions (from the ro /app mount) into Goose's working dir
# so the harness picks them up. cwd is /data (the rw mount). Goose reads project
# instructions from AGENTS.md in cwd (with .goosehints as a fallback name).
if [ -f /app/AGENTS.md ]; then
  cp /app/AGENTS.md /data/AGENTS.md || true
fi

exec goose acp
