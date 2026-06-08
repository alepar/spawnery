#!/bin/sh
# Dispatcher entrypoint for the stub fixture image's session #0 (sp-npxq.5 e2e), mirroring the real
# agent image's deploy/agent/entrypoint.sh: the node passes the runnable id as $1 (image Cmd =
# [runnableID] — internal/spawnlet/manager.go). The only runnable this fixture ships is stub-acp, so
# we default to it when no selection is made. Session #0 keeps ACP port 7000 and tmux session "spawn".
set -e
exec launcher --runnable "${1:-stub-acp}" --acp-port 7000 --tmux-session spawn --keepalive
