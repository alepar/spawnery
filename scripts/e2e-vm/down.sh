#!/usr/bin/env bash
# down.sh — collect artifacts, then destroy ONE e2e VM instance and its overlay.
# Collect BEFORE destroy (roast gap): a disposable VM's logs are the only evidence a failed run leaves.
#
# Usage: E2E_RUNID=... ./down.sh [--keep]   (--keep leaves the domain running for debugging)
set -euo pipefail
source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"
KEEP=0; [ "${1:-}" = "--keep" ] && KEEP=1
_need_runid
DOM="$(vm_domain)"; RD="$(run_dir)"; ART="$RD/artifacts"
mkdir -p "$ART"

# ---- artifact capture (best-effort; never fail teardown on a collection error) ----
if [ -f "$RD/acc.env" ]; then
  # shellcheck disable=SC1091
  source "$RD/acc.env"
  IP="${E2E_VM_IP:-}"
  if [ -n "$IP" ] && vm_ssh "$IP" true 2>/dev/null; then
    log "collecting artifacts -> $ART"
    vm_ssh "$IP" 'sudo journalctl -u spawnery-authsvc -u spawnery-cp -u spawnery-node -u spawnery-web -u caddy --no-pager' >"$ART/journal.log" 2>/dev/null || true
    vm_ssh "$IP" 'sudo journalctl -t containerd -t runsc --no-pager' >"$ART/containerd-runsc.log" 2>/dev/null || true
    vm_ssh "$IP" 'sudo -u postgres psql -c "select id,status,app_id from spawns order by created_at desc limit 50" 2>/dev/null' >"$ART/spawns.txt" 2>/dev/null || true
  fi
fi

if [ "$KEEP" = 1 ]; then
  warn "--keep: leaving domain $DOM running (IP=${IP:-?}); remember to './down.sh' later"
  exit 0
fi

# ---- destroy + cleanup ----
virsh_ destroy "$DOM" 2>/dev/null || true
virsh_ undefine "$DOM" --nvram 2>/dev/null || true
host_resolve_del
rm -f "$(overlay_path)" "$RD/seed.iso" 2>/dev/null || true
log "destroyed $DOM (artifacts kept in $ART)"
