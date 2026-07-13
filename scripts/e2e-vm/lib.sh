#!/usr/bin/env bash
# Shared helpers for the prod-stack-in-a-VM e2e harness (epic sp-te3y).
# Sourced by up.sh / roll.sh / down.sh / run.sh. Concurrency-safe: every derived
# name/path/port is namespaced by a per-run E2E_RUNID so many branches can run at once.
#
# Design: docs/superpowers/specs/2026-07-03-prod-stack-vm-e2e-harness-design.md
# STATUS: first draft — validate on a real KVM host (libvirtd + /dev/kvm). Not sandbox-testable.

set -euo pipefail

# ---- config (env-overridable) ----
: "${LIBVIRT_URI:=qemu:///system}"
: "${E2E_NET:=spawnery-e2e}"                 # libvirt NAT network (routable per-VM IP)
: "${E2E_DOMAIN_SUFFIX:=e2e.test}"           # per-run hostname = <runid>.<suffix>; wildcard cert *.<suffix>
: "${E2E_VM_MEM_MB:=6144}"
: "${E2E_VM_VCPUS:=4}"
: "${E2E_HOSTS_MODE:=nss}"                    # nss (nss-libvirt, no sudo) | hosts (/etc/hosts + flock + sudo)
: "${E2E_STATE_ROOT:=${XDG_STATE_HOME:-$HOME/.local/state}/spawnery-e2e}"
# Under qemu:///system, disk images + seed ISOs the qemu process reads/writes must live in a
# libvirt-accessible pool path (NOT $HOME). Logs/artifacts/staging stay under E2E_STATE_ROOT.
: "${E2E_IMG_ROOT:=/var/lib/libvirt/images/spawnery-e2e}"
: "${E2E_SSH_USER:=spawnery}"                # cloud-init user baked into the golden image
: "${E2E_SSH_KEY:=$HOME/.ssh/spawnery_e2e}"  # private key whose pubkey the golden image trusts
: "${GOLDEN_IMAGE:=}"                         # REQUIRED: path to the golden qcow2 (built by build-base.sh)

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd -P)"
E2E_DIR="$REPO_ROOT/scripts/e2e-vm"

log()  { printf '\033[36m[e2e-vm %(%H:%M:%S)T]\033[0m %s\n' -1 "$*" >&2; }
warn() { printf '\033[33m[e2e-vm %(%H:%M:%S)T WARN]\033[0m %s\n' -1 "$*" >&2; }
die()  { printf '\033[31m[e2e-vm %(%H:%M:%S)T ERR]\033[0m %s\n' -1 "$*" >&2; exit 1; }

# ---- run identity ----
# Derive a short, DNS/libvirt-safe run id from the branch + short sha + a nonce, so concurrent
# runs (even from the same branch) never collide.
gen_runid() {
  local branch sha rnd
  branch="$(git -C "$REPO_ROOT" rev-parse --abbrev-ref HEAD 2>/dev/null || echo detached)"
  branch="$(printf '%s' "$branch" | tr '[:upper:]/_.' '[:lower:]---' | tr -cd 'a-z0-9-')"
  branch="${branch: -12}"                     # last 12 chars keeps hostnames short + valid
  sha="$(git -C "$REPO_ROOT" rev-parse --short HEAD 2>/dev/null || echo nogit)"
  rnd="$(printf '%04x' "$RANDOM")$(printf '%03x' $((RANDOM % 4096)))"   # unique per run
  # collapse dashes and strip leading/trailing (trailing '-' is an invalid hostname label)
  printf '%s-%s-%s' "$branch" "$sha" "$rnd" | sed 's/-\+/-/g; s/^-//; s/-$//'
}

# ---- per-run derived names (require E2E_RUNID set) ----
_need_runid() { [ -n "${E2E_RUNID:-}" ] || die "E2E_RUNID unset (call gen_runid / set it first)"; }
vm_domain()   { _need_runid; printf 'spawnery-e2e-%s' "$E2E_RUNID"; }
vm_hostname() { _need_runid; printf '%s.%s' "$E2E_RUNID" "$E2E_DOMAIN_SUFFIX"; }
run_dir()     { _need_runid; printf '%s/%s' "$E2E_STATE_ROOT" "$E2E_RUNID"; }   # $HOME: logs/artifacts/stage
img_dir()     { _need_runid; printf '%s/%s' "$E2E_IMG_ROOT" "$E2E_RUNID"; }     # pool: qemu-read/write disks
overlay_path(){ printf '%s/overlay.qcow2' "$(img_dir)"; }

# build a cloud-init NoCloud seed ISO (label MUST be cidata) with whatever builder is present
iso_make() {
  local out="$1"; shift
  if   command -v xorrisofs  >/dev/null; then xorrisofs  -quiet -o "$out" -V cidata -J -r "$@"
  elif command -v mkisofs    >/dev/null; then mkisofs    -quiet -o "$out" -V cidata -J -r "$@"
  elif command -v genisoimage>/dev/null; then genisoimage -quiet -o "$out" -V cidata -J -r "$@"
  else die "no ISO builder found (need xorriso/genisoimage)"; fi
}

virsh_() { virsh -c "$LIBVIRT_URI" "$@"; }

# ---- VM IP from the libvirt DHCP lease (retries until the guest has booted + leased) ----
vm_ip() {
  local dom="$1" ip="" i
  for i in $(seq 1 60); do
    ip="$(virsh_ -q domifaddr "$dom" 2>/dev/null | awk '/ipv4/{print $4}' | cut -d/ -f1 | head -1)"
    [ -n "$ip" ] && { printf '%s' "$ip"; return 0; }
    sleep 2
  done
  return 1
}

# ---- wait until a tcp host:port accepts a connection ----
wait_tcp() {
  local host="$1" port="$2" timeout="${3:-180}" i
  for i in $(seq 1 "$timeout"); do
    if (exec 3<>"/dev/tcp/$host/$port") 2>/dev/null; then exec 3>&- 3<&-; return 0; fi
    sleep 1
  done
  return 1
}

# ---- ssh/scp into the guest (batch mode; key baked at golden-build time) ----
vm_ssh() { local host="$1"; shift; ssh -o BatchMode=yes -o StrictHostKeyChecking=no \
             -o UserKnownHostsFile=/dev/null -i "$E2E_SSH_KEY" "$E2E_SSH_USER@$host" "$@"; }
vm_scp() { local src="$1" host="$2" dst="$3"; scp -q -o BatchMode=yes -o StrictHostKeyChecking=no \
             -o UserKnownHostsFile=/dev/null -i "$E2E_SSH_KEY" -r "$src" "$E2E_SSH_USER@$host:$dst"; }

# ---- host name resolution for <runid>.<suffix> -> <vm-ip> ----
# nss mode: rely on the one-time-installed nss-libvirt module (nsswitch `hosts: ... libvirt_guest`),
#   which resolves guest names from the libvirt network's leases automatically — no per-run edit.
# hosts mode: append a marker-tagged line to /etc/hosts under flock (needs sudo), removed on teardown.
host_resolve_add() {
  local hostname="$1" ip="$2"
  case "$E2E_HOSTS_MODE" in
    nss)
      # VERIFY, don't assume. nss mode used to just log an expectation and proceed; on a host without the
      # nss-libvirt module the name never resolved and the run died ~2 minutes later inside Playwright's
      # global setup with an opaque "TargetDownError: acceptance target unreachable", pointing at nothing.
      if getent hosts "$hostname" >/dev/null 2>&1; then
        log "host resolution via nss-libvirt ('$hostname' resolves)"
        return 0
      fi
      warn "nss-libvirt cannot resolve '$hostname' — falling back to E2E_HOSTS_MODE=hosts for this run"
      # Diagnose the two independent halves: the MODULE must be installed, and nsswitch must REFERENCE it.
      # Getting these confused wastes real time — on an immutable host the module can be staged (so the
      # package "is installed") yet absent from the running system until a reboot.
      local have_mod=0 have_cfg=0
      compgen -G '/usr/lib*/libnss_libvirt*.so*' >/dev/null 2>&1 && have_mod=1
      grep -qE '^hosts:.*\blibvirt' /etc/nsswitch.conf 2>/dev/null && have_cfg=1
      if [ "$have_mod" = 0 ] && [ "$have_cfg" = 1 ]; then
        warn "  cause: nsswitch.conf references the libvirt module, but the module is NOT on this system."
        warn "         On an immutable OS (rpm-ostree/uCore) a layered package only lands after a REBOOT."
        warn "         fix: reboot, or keep using E2E_HOSTS_MODE=hosts."
      elif [ "$have_mod" = 0 ]; then
        warn "  cause: the nss-libvirt module is not installed."
        warn "         fix: sudo dnf install libvirt-nss   (immutable OS: sudo rpm-ostree install libvirt-nss + reboot)"
        warn "              then add 'libvirt libvirt_guest' after 'files' on the hosts: line of /etc/nsswitch.conf"
      elif [ "$have_cfg" = 0 ]; then
        warn "  cause: the module is installed but /etc/nsswitch.conf does not reference it."
        warn "         fix: add 'libvirt libvirt_guest' after 'files' on the hosts: line of /etc/nsswitch.conf"
      else
        warn "  module and nsswitch both look right — the guest may not hold a libvirt DHCP lease yet."
      fi
      E2E_HOSTS_MODE=hosts
      host_resolve_add "$hostname" "$ip"
      return 0;;
    hosts)
      local marker="# e2e-vm:$E2E_RUNID"
      ( flock 9; printf '%s %s %s\n' "$ip" "$hostname" "$marker" | sudo tee -a /etc/hosts >/dev/null ) 9>/tmp/e2e-vm-hosts.lock
      log "added /etc/hosts: $ip $hostname";;
    *) die "unknown E2E_HOSTS_MODE=$E2E_HOSTS_MODE";;
  esac
}
host_resolve_del() {
  [ "$E2E_HOSTS_MODE" = hosts ] || return 0
  ( flock 9; sudo sed -i "/# e2e-vm:$E2E_RUNID\$/d" /etc/hosts ) 9>/tmp/e2e-vm-hosts.lock || true
}

# ---- free host port (for optional forwards; NAT model mostly uses the routable IP directly) ----
free_port() { python3 - <<'PY'
import socket
s=socket.socket(); s.bind(('127.0.0.1',0)); print(s.getsockname()[1]); s.close()
PY
}
