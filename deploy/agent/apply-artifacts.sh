#!/bin/sh
# apply-artifacts.sh — invoke `agentinstall apply --runnable <id> --report <path> ...`,
# propagating ITS exit code. The runnable->emitter mapping (and the no-op decision for
# runnables with no agentinstall emitter) lives in Go now — see
# internal/agentcaps/emitter.go and cmd/agentinstall/main.go's `apply --runnable` handling
# (sp-mwco.2.6). This script carries no agent names.
#
# Called from launcher's apply_artifacts() wrapper AFTER per-runnable base-config gen and BEFORE
# start_tmux/exec. INVARIANT (sp-mwco.2.12): every exit path from this script leaves
# apply-report.json written, or shouts loudly on stderr — absence of a signal is a BUG, not a
# state. The node's AwaitApplyReport polls $REPORT_FILE after containers_ready; a script that
# exits without writing one used to make a catastrophic failure indistinguishable from
# success-with-no-report, forcing the node to burn its entire wait budget before reporting a
# cryptic "unknown" verdict. That fail-open is gone: an `ensure_report_or_shout` trap fires on
# EVERY exit (normal return, `exit N`, or a signal that kills this script) and, if no report
# landed, synthesizes an outcome=error one from whatever exit code/signal caused the exit. The
# launcher's own `|| true` around the call is ALSO gone (see deploy/agent/launch's
# apply_artifacts() wrapper) — a report-less failure now aborts the launcher instead of being
# silently swallowed, because at that point the only remaining channel to the node is the
# container dying.
#
# A missing manifest.json exits 0 and writes no report (nothing was staged, so nothing to
# verify — the node only awaits a report when it staged artifacts); the trap is armed AFTER this
# check specifically so that legitimate no-op stays report-less.
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
# AwaitApplyReport sees an immediate, legible failure instead of timing out. $1 is the error
# message. On a failure to write the report itself, this is LOUD (a grep-able
# SPAWNERY-APPLY-FATAL: marker on stderr) rather than swallowed — an unwritable report dir must
# not look identical to success.
write_error_report() {
  msg="$1"
  if ! mkdir -p "${ARTIFACTS_DIR}/report" 2>/dev/null; then
    printf 'SPAWNERY-APPLY-FATAL: could not write apply-report.json (mkdir %s/report failed)\n' "$ARTIFACTS_DIR" >&2
    return 1
  fi
  # Strip JSON-breaking characters (quote, backslash, newline) from the interpolated values —
  # a typo'd/hostile runnable ID or message must not be able to produce invalid JSON.
  safe_runnable=$(printf '%s' "$RUNNABLE" | tr -d '"\\\n')
  safe_msg=$(printf '%s' "$msg" | tr -d '"\\\n')
  tmp="${ARTIFACTS_DIR}/report/apply-report.json.tmp.$$"
  if ! printf '{"schema":1,"agent":"","runnable":"%s","outcome":"error","error":"%s","reports":[]}\n' \
      "$safe_runnable" "$safe_msg" >"$tmp" 2>/dev/null; then
    printf 'SPAWNERY-APPLY-FATAL: could not write apply-report.json (write to tmp file failed)\n' >&2
    rm -f "$tmp" 2>/dev/null
    return 1
  fi
  if ! mv -f "$tmp" "$REPORT_FILE" 2>/dev/null; then
    printf 'SPAWNERY-APPLY-FATAL: could not write apply-report.json (mv into place failed)\n' >&2
    rm -f "$tmp" 2>/dev/null
    return 1
  fi
  return 0
}

# ensure_report_or_shout is the EXIT/INT/TERM/HUP trap handler (armed after the manifest check):
# if $REPORT_FILE is still absent when this script is about to exit — for ANY reason, including a
# crash or a signal — synthesize an error report naming the exit status. A no-op when a report
# already exists (the normal-success / already-handled-error paths), so the trap on a clean exit 0
# costs one stat(). Disarms the traps first and re-exits explicitly: a POSIX INT/TERM/HUP trap
# REPLACES the signal's default (process-terminating) action, so without an explicit exit here a
# signal delivered to this script itself (as opposed to a child it exec'd) would run the handler
# and then resume the script instead of actually terminating.
ensure_report_or_shout() {
  rc=$?
  trap '' EXIT INT TERM HUP
  if [ ! -f "$REPORT_FILE" ]; then
    write_error_report "apply-artifacts exited rc=${rc} without writing a report for runnable ${RUNNABLE}"
  fi
  exit "$rc"
}

# Old-image guard: if agentinstall is not in PATH, warn, write an error report, and exit 1.
if ! command -v agentinstall >/dev/null 2>&1; then
  printf 'apply-artifacts: agentinstall not found in PATH (old image?) — skipping artifact application for %s\n' "$RUNNABLE" >&2
  write_error_report "agentinstall missing from image (old agent image?) — cannot install staged artifacts for runnable ${RUNNABLE}"
  exit 1
fi

# No manifest.json → nothing to install (empty staging dir is valid). The trap is armed AFTER
# this early-exit so this legitimate no-op stays report-less, as before.
if [ ! -f "${ARTIFACTS_DIR}/manifest.json" ]; then
  exit 0
fi

trap 'ensure_report_or_shout' EXIT INT TERM HUP

agentinstall apply \
  --runnable "$RUNNABLE" \
  --artifacts "$ARTIFACTS_DIR" \
  --secrets "$SECRETS_DIR" \
  --secret-wait-timeout "$SECRET_WAIT_TIMEOUT" \
  --report "$REPORT_FILE"
rc=$?

# The CLI itself exits 0 without writing a report when --runnable resolves to no emitter, or to
# an emitter agentinstall.NewRegistry doesn't (yet) register (see cmd/agentinstall/main.go's
# `apply --runnable` handling). The EXIT trap above catches this (and any other silent-no-report
# exit, zero or non-zero) uniformly — no rc==0 condition needed here anymore.
if [ "$rc" -eq 0 ] && [ ! -f "$REPORT_FILE" ]; then
  write_error_report "no agentinstall emitter for runnable ${RUNNABLE} — staged artifacts cannot be installed"
  exit 1
fi

exit "$rc"
