#!/bin/sh
# Launch the opencode TUI attached to THIS spawn's shared opencode session, over the mosh PTY.
#
# No inner tmux: opencode is server-authoritative (the conversation lives on the in-pod opencode
# server), and mosh already provides roaming/reconnect — so a container-side tmux only adds a third
# terminal-emulation layer (mosh + tmux + TUI) that half-renders the full-screen UI. Reattaching is
# just running `spawnctl tmux` again (a fresh `opencode attach -s <id>` onto the same server session).
#
# The acpadapter writes the spawn's opencode session id to the file below; we pin the TUI to it with
# `-s` (opencode attach -c does NOT reliably select it). TERM must be set or the TUI half-renders.
export TERM="${TERM:-xterm-256color}"
URL="${OPENCODE_BASE_URL:-http://127.0.0.1:4096}"
SESSION_FILE="${SPAWNERY_SESSION_FILE:-/tmp/spawnery-opencode-session}"
S="$(cat "$SESSION_FILE" 2>/dev/null || true)"
if [ -n "$S" ]; then
  exec opencode attach "$URL" -s "$S"
else
  exec opencode attach "$URL" -c
fi
