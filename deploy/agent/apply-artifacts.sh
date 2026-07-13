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
# A missing manifest.json exits 0 and writes no report (nothing was staged, so nothing to
# verify — the node only awaits a report when it staged artifacts). The old-image guard and a
# no-emitter runnable, however, DO now write an explicit error report before exiting 1: without
# one, the node's AwaitApplyReport would sit out the full 2-minute timeout and only then report a
# cryptic "unknown" verdict — this way a bundle spawn fails immediately with a legible reason.
# Once agentinstall runs and writes its own report, $REPORT_FILE is written by the CLI itself
# (atomically, via --report) — this script no longer parses its stdout.
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

# write_error_report writes an outcome=error apply-report envelope so the node's
# AwaitApplyReport sees an immediate, legible failure instead of timing out. Best-effort: a
# failure to write must not crash this script. $1 is the error message.
write_error_report() {
  msg="$1"
  mkdir -p "${ARTIFACTS_DIR}/report" 2>/dev/null
  # Strip JSON-breaking characters (quote, backslash, newline) from the interpolated values —
  # a typo'd/hostile runnable ID or message must not be able to produce invalid JSON.
  safe_runnable=$(printf '%s' "$RUNNABLE" | tr -d '"\\\n')
  safe_msg=$(printf '%s' "$msg" | tr -d '"\\\n')
  tmp="${ARTIFACTS_DIR}/report/apply-report.json.tmp.$$"
  if printf '{"schema":1,"agent":"","runnable":"%s","outcome":"error","error":"%s","reports":[]}\n' \
      "$safe_runnable" "$safe_msg" >"$tmp" 2>/dev/null; then
    mv -f "$tmp" "$REPORT_FILE" 2>/dev/null ||
      printf 'apply-artifacts: failed to move error report into place\n' >&2
  else
    printf 'apply-artifacts: failed to write error report\n' >&2
    rm -f "$tmp" 2>/dev/null
  fi
}

# Old-image guard: if agentinstall is not in PATH, warn, write an error report, and exit 1.
if ! command -v agentinstall >/dev/null 2>&1; then
  printf 'apply-artifacts: agentinstall not found in PATH (old image?) — skipping artifact application for %s\n' "$RUNNABLE" >&2
  write_error_report "agentinstall missing from image (old agent image?) — cannot install staged artifacts for runnable ${RUNNABLE}"
  exit 1
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
rc=$?

# The CLI itself exits 0 without writing a report when --runnable resolves to no emitter, or to
# an emitter agentinstall.NewRegistry doesn't (yet) register (see cmd/agentinstall/main.go's
# `apply --runnable` handling). Catch both post-hoc: any FUTURE silent exit-0 path the CLI grows
# is covered by the same rule, not just today's two.
if [ "$rc" -eq 0 ] && [ ! -f "$REPORT_FILE" ]; then
  write_error_report "no agentinstall emitter for runnable ${RUNNABLE} — staged artifacts cannot be installed"
  exit 1
fi

exit "$rc"
