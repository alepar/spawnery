#!/bin/sh
# Launch opencode (headless server) plus the in-pod acpadapter.
#
# opencode reaches the model provider through the sidecar's OpenAI-compatible
# endpoint (127.0.0.1:8080), which injects the real OpenRouter key. We register a
# custom "spawnery" provider pointing at the sidecar and add the spawn's model to
# its model map, so any SPAWN_MODEL (an OpenRouter id, e.g. openai/gpt-4o-mini)
# works without baking a static catalog. The adapter then prompts opencode with
# model "spawnery/<SPAWN_MODEL>".
set -e

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
