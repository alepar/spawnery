#!/bin/sh
# apply-artifacts.sh — invoke `agentinstall apply --runnable <id> --report <path> ...`,
# propagating ITS exit code. The runnable->emitter mapping (and the no-op decision for
# runnables with no agentinstall emitter) lives in Go now — see
# internal/agentcaps/emitter.go and cmd/agentinstall/main.go's `apply --runnable` handling
# (sp-mwco.2.6). This script carries no agent names.
#
# Called from launcher AFTER per-runnable base-config gen and BEFORE start_tmux/exec:
#   apply-artifacts "$RUNNABLE" || true
# The launcher's `|| true` stays (sp-mwco.2.3, sp-mwco.2.7 design note): killing the launcher's
# set -e entrypoint on a partial install would lose the per-skill detail this script's --report
# just captured, and would race the node's own read of it. The all-or-nothing bundle contract is
# enforced by the NODE (spawnlet) polling $REPORT_FILE after containers_ready, not by this
# script's own exit code — this script's exit code exists for humans/CI (apply-artifacts_test.go)
# and so a caller who does NOT want the `|| true` swallow can still see it.
#
# No-op runnables, the old-image guard, and a missing manifest.json all exit 0 and write no
# report (nothing to install, so nothing to verify). Once agentinstall runs, $REPORT_FILE is
# written by the CLI itself (atomically, via --report) — this script no longer parses its stdout.
#
# Environment:
#   SPAWNERY_ARTIFACTS_DIR  staging dir bind-mounted by the node (default /run/spawnery/artifacts)
#   SPAWNERY_SECRETS_DIR    secrets dir bind-mounted by the node (default /run/spawnery/secrets)
#   SECRET_WAIT_TIMEOUT     duration passed to --secret-wait-timeout (default 30s)

RUNNABLE="${1:-}"
ARTIFACTS_DIR="${SPAWNERY_ARTIFACTS_DIR:-/run/spawnery/artifacts}"
SECRETS_DIR="${SPAWNERY_SECRETS_DIR:-/run/spawnery/secrets}"
SECRET_WAIT_TIMEOUT="${SECRET_WAIT_TIMEOUT:-30s}"
REPORT_FILE="${ARTIFACTS_DIR}/report/apply-report.json"

# Old-image guard: if agentinstall is not in PATH, warn and exit 0.
if ! command -v agentinstall >/dev/null 2>&1; then
  printf 'apply-artifacts: agentinstall not found in PATH (old image?) — skipping artifact application for %s\n' "$RUNNABLE" >&2
  exit 0
fi

# No manifest.json → nothing to install (empty staging dir is valid).
if [ ! -f "${ARTIFACTS_DIR}/manifest.json" ]; then
  exit 0
fi

agentinstall apply \
  --runnable "$RUNNABLE" \
  --artifacts "$ARTIFACTS_DIR" \
  --secrets "$SECRETS_DIR" \
  --secret-wait-timeout "$SECRET_WAIT_TIMEOUT" \
  --report "$REPORT_FILE"
