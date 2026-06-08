#!/bin/sh
# Inner command of the opencode-tui transparent-tmux session: launch the opencode TUI attached
# to THIS spawn's shared opencode session. The launcher (deploy/agent/launch) runs this via
# `tmux new-session -- spawn-tui`, so all clients mirror ONE opencode TUI process (sp-npxq §3).
#
# (Historical note: this script used to run WITHOUT an inner tmux on the theory that an extra
# terminal layer half-rendered the full-screen UI. That half-render is now attributed to the
# fixed LANG/LC_ALL + the transparent tmux.conf, so opencode is wrapped like every other mosh
# runnable for true multi-client mirroring + scrollback persistence.)
#
# The acpadapter writes the spawn's opencode session id to SESSION_FILE; we pin the TUI to it
# with `-s` (opencode attach -c does NOT reliably select it). TERM must be set or the TUI
# half-renders.
export TERM="${TERM:-xterm-256color}"
URL="${OPENCODE_BASE_URL:-http://127.0.0.1:4096}"
SESSION_FILE="${SPAWNERY_SESSION_FILE:-/tmp/spawnery-opencode-session}"
S="$(cat "$SESSION_FILE" 2>/dev/null || true)"
if [ -n "$S" ]; then
  exec opencode attach "$URL" -s "$S"
else
  exec opencode attach "$URL" -c
fi
