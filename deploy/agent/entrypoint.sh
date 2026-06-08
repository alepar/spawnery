#!/bin/sh
# Dispatcher entrypoint for session #0. The node passes the runnable id as $1 and sets the
# image Cmd to [runnableID] (internal/spawnlet/manager.go). All per-runnable launch/env wiring
# now lives in the reusable launcher (deploy/agent/launch), shared with the additional sessions
# the node exec-launches in sp-npxq.3 — so every session gets byte-identical config.
#
# Session #0 keeps ACP port 7000 (the node also sets ACP_LISTEN=:7000) and the tmux session
# name "spawn" (the node attaches via `tmux attach -t spawn`). --keepalive runs the
# keep-PID-1-alive loop for mosh runnables (acp/served exec a foreground server instead).
set -e
exec launcher --runnable "${1:-}" --acp-port 7000 --tmux-session spawn --keepalive
