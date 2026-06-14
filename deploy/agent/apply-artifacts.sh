#!/bin/sh
# apply-artifacts.sh — map $RUNNABLE to an agentinstall emitter name and invoke
# `agentinstall apply --agent <emitter> ...` in a set -e-safe subshell.
#
# Called from launcher AFTER per-runnable base-config gen and BEFORE start_tmux/exec:
#   apply-artifacts "$RUNNABLE" || true
#
# Always exits 0 so a failure/skip never kills the launcher's set -e entrypoint.
# Writes a JSON report to $REPORT_FILE and warns on stderr for failed/skipped entries.
#
# Environment:
#   SPAWNERY_ARTIFACTS_DIR  staging dir bind-mounted by the node (default /run/spawnery/artifacts)
#   SPAWNERY_SECRETS_DIR    secrets dir bind-mounted by the node (default /run/spawnery/secrets)
#   SECRET_WAIT_TIMEOUT     duration passed to --secret-wait-timeout (default 30s)

RUNNABLE="${1:-}"
ARTIFACTS_DIR="${SPAWNERY_ARTIFACTS_DIR:-/run/spawnery/artifacts}"
SECRETS_DIR="${SPAWNERY_SECRETS_DIR:-/run/spawnery/secrets}"
SECRET_WAIT_TIMEOUT="${SECRET_WAIT_TIMEOUT:-30s}"
REPORT_FILE="${ARTIFACTS_DIR}/apply-report.json"

# Map runnable → emitter name, or empty for no-op runnables.
case "$RUNNABLE" in
  claude-tui)      EMITTER=claude ;;
  codex-tui)       EMITTER=codex ;;
  opencode-served|opencode-tui) EMITTER=opencode ;;
  # All other runnables (goose-*, shell, stub-acp, nori, "") are no-ops.
  *)               EMITTER="" ;;
esac

# No-op runnables: nothing to install.
if [ -z "$EMITTER" ]; then
  exit 0
fi

# Old-image guard: if agentinstall is not in PATH, warn and exit 0.
if ! command -v agentinstall >/dev/null 2>&1; then
  printf 'apply-artifacts: agentinstall not found in PATH (old image?) — skipping artifact application for %s\n' "$RUNNABLE" >&2
  exit 0
fi

# No manifest.json → nothing to install (empty staging dir is valid).
if [ ! -f "${ARTIFACTS_DIR}/manifest.json" ]; then
  exit 0
fi

# Invoke agentinstall in a subshell so any failure is captured without propagating.
APPLY_OUT="$(agentinstall apply \
  --agent "$EMITTER" \
  --artifacts "$ARTIFACTS_DIR" \
  --secrets "$SECRETS_DIR" \
  --secret-wait-timeout "$SECRET_WAIT_TIMEOUT" \
  2>&1)" || true

# Persist the report JSON from stdout (last line of APPLY_OUT that looks like JSON).
REPORT_JSON="$(printf '%s\n' "$APPLY_OUT" | grep '^{' | tail -1)"
if [ -n "$REPORT_JSON" ]; then
  printf '%s\n' "$REPORT_JSON" > "$REPORT_FILE"
  # Warn on failed or skipped entries so they surface in the spawn log.
  if printf '%s\n' "$REPORT_JSON" | grep -q '"status":"failed"\|"status":"skipped"'; then
    printf 'apply-artifacts: WARNING — some artifacts were not applied for %s (see %s):\n' "$RUNNABLE" "$REPORT_FILE" >&2
    # Best-effort human-readable summary on stderr; $REPORT_FILE holds the authoritative JSON
    # (do not parse it programmatically — use $REPORT_FILE instead).
    printf '%s\n' "$REPORT_JSON" | grep -o '"name":"[^"]*","[^}]*"status":"[^"]*"' >&2 || true
  fi
else
  # agentinstall produced non-JSON output (error before JSON marshal).
  if [ -n "$APPLY_OUT" ]; then
    printf 'apply-artifacts: agentinstall error for %s: %s\n' "$RUNNABLE" "$APPLY_OUT" >&2
  fi
fi

exit 0
