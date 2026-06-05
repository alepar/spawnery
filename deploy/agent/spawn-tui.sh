#!/bin/sh
# Launch the opencode TUI in tmux, attached to THIS spawn's shared opencode session.
#
# The acpadapter writes the spawn's opencode session id to the file below. We pin the TUI to it with
# `-s` so the TUI and the web UI share ONE session — `opencode attach -c` does NOT reliably select
# it (it opens the home screen / a fresh session). TERM must be set or the full-screen TUI renders
# only partially over the mosh PTY. `tmux new-session -A` attaches if the session exists (reattach).
export TERM="${TERM:-xterm-256color}"
URL="${OPENCODE_BASE_URL:-http://127.0.0.1:4096}"
SESSION_FILE="${SPAWNERY_SESSION_FILE:-/tmp/spawnery-opencode-session}"
S="$(cat "$SESSION_FILE" 2>/dev/null || true)"
if [ -n "$S" ]; then
  exec tmux new-session -A -s opencode opencode attach "$URL" -s "$S"
else
  # Fallback: continue the last session (best-effort if the id file isn't present yet).
  exec tmux new-session -A -s opencode opencode attach "$URL" -c
fi
