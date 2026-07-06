#!/usr/bin/env bash
# run.sh — the one command. Concurrency-safe end to end (run it from many branches at once):
#   0. build fresh binaries + images + web + stage config   (in the dev-spawnery distrobox)
#   1. start a fresh VM off the golden image                (up.sh)
#   2. copy the fresh code in + restart + wait ready        (roll.sh)
#   3. run the real e2e acceptance suite against the VM     (acceptance/)
# Always tears the VM down at the end (unless --keep). Everything is namespaced by E2E_RUNID.
#
# Usage:
#   GOLDEN_IMAGE=/var/lib/libvirt/images/spawnery-golden.qcow2 \
#     scripts/e2e-vm/run.sh [--profile fake|github] [--grep <pattern>] [--keep] [--no-build]
#
# Prereqs (one-time host setup — see README.md): libvirtd + /dev/kvm, the '$E2E_NET' libvirt
# network, nss-libvirt (or E2E_HOSTS_MODE=hosts), the golden CA in host trust, an ssh keypair
# at $E2E_SSH_KEY trusted by the golden image, and a built golden image.
set -euo pipefail
source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"

PROFILE=fake; GREP=""; KEEP=0; BUILD=1
while [ $# -gt 0 ]; do case "$1" in
  --profile) PROFILE="$2"; shift 2;;
  --grep)    GREP="$2"; shift 2;;
  --keep)    KEEP=1; shift;;
  --no-build) BUILD=0; shift;;
  *) die "unknown arg: $1";;
esac; done

export E2E_RUNID="$(gen_runid)"
WEB_ORIGIN="https://$(vm_hostname)"    # prod web build pins CSP/connect-src to this origin
RD="$(run_dir)"; mkdir -p "$RD"
STAGE="$RD/stage"; mkdir -p "$STAGE/bin" "$STAGE/config"
log "=== e2e-vm run  runid=$E2E_RUNID  profile=$PROFILE ==="

# teardown on ANY exit (success, failure, Ctrl-C) unless --keep
cleanup() { local rc=$?; if [ "$KEEP" = 1 ]; then warn "--keep set; not tearing down"; else
  E2E_RUNID="$E2E_RUNID" "$E2E_DIR/down.sh" || true; fi; exit $rc; }
trap cleanup EXIT INT TERM

dbox() { distrobox enter --root dev-spawnery -- bash -lc "cd '$REPO_ROOT' && $*"; }

# ---- 0. build fresh code (per-run staging so concurrent branches never clobber each other) ----
if [ "$BUILD" = 1 ]; then
  log "0/3 building fresh binaries + images + web …"
  dbox "make build bin/spawnery_cp && cp -f bin/spawnery_cp bin/authsvc bin/spawnlet bin/spawnctl '$STAGE/bin/'"
  # docker runs on the HOST, not the distrobox (socket group 985 unreachable inside) — build+save here.
  command -v make >/dev/null 2>&1 && make images || warn "no host make — using pre-built spawnery/{sidecar,agent} images"
  docker save spawnery/sidecar:dev spawnery/agent:dev -o "$STAGE/images.tar" \
    || warn "docker save failed — sidecar/agent will be STALE (baked) this run"
  dbox "cd web && npm ci && VITE_CP_ORIGIN='$WEB_ORIGIN' VITE_AS_ORIGIN='$WEB_ORIGIN' npm run build && rm -rf '$STAGE/web-dist' && cp -rf dist '$STAGE/web-dist'" \
    || warn "web build failed — SPA will be STALE this run"
  cp -rf "$REPO_ROOT/config/." "$STAGE/config/" 2>/dev/null || true
else
  log "0/3 --no-build: staging existing $REPO_ROOT/bin"
  cp -f "$REPO_ROOT"/bin/{spawnery_cp,authsvc,spawnlet,spawnctl} "$STAGE/bin/" 2>/dev/null || die "no prebuilt bin/ — drop --no-build"
fi

# ---- 1. start the VM ----
log "1/3 starting VM …"
E2E_RUNID="$E2E_RUNID" GOLDEN_IMAGE="$GOLDEN_IMAGE" "$E2E_DIR/up.sh" --profile "$PROFILE"

# ---- 2. copy fresh code in + wait ready ----
log "2/3 rolling fresh code + waiting ready …"
E2E_RUNID="$E2E_RUNID" STAGE="$STAGE" "$E2E_DIR/roll.sh"

# ---- 3. run the real e2e suite against the VM ----
log "3/3 running acceptance suite …"
# shellcheck disable=SC1091
source "$RD/acc.env"
# these mirror the validated live invocation (see provision/RECONCILE-NOTES.md): the acceptance
# suite needs dev-token auth wired to the fake-GitHub identities, the demo app id, model ids, and a
# longer spawn-active timeout (VM boot is slower than local dev) to actually run against the VM.
export ACC_AUTH_MODE=dev-token
export ACC_IDENTITY_POOL="devtoken1=acc-owner-1,devtoken2=acc-owner-2,devtoken3=acc-owner-3"
# version-pin refs (preflight requires them). target==build here (the VM runs the code we just
# rolled), so pin both to this run id so the check is meaningful and trivially satisfied.
export ACC_TARGET_REF="$E2E_RUNID" ACC_BUILD_REF="$E2E_RUNID"
export ACC_TEST_APP_ID=spawnery/secret-app ACC_LIFECYCLE_APP=spawnery/secret-app ACC_AGENT_APP_ID=spawnery/secret-app
export ACC_APP_ID=spawnery/secret-app   # tenancy specs' generic app id
export ACC_TEST_MODEL=openai/gpt-4o-mini ACC_AGENT_MODEL=openai/gpt-4o-mini
export ACC_SPAWN_ACTIVE_TIMEOUT_MS=240000
[ -f "${GOLDEN_IMAGE%.qcow2}-ca.crt" ] && export NODE_EXTRA_CA_CERTS="${GOLDEN_IMAGE%.qcow2}-ca.crt"
export ACC_SPAWNCTL_BIN="$STAGE/bin/spawnctl"     # cliDriver shells out to the fresh spawnctl
export PLAYWRIGHT_HTML_REPORT="$RD/artifacts/pw-report"   # per-run output — concurrency-safe
export PLAYWRIGHT_OUTPUT_DIR="$RD/artifacts/pw-results"
GREP_ARGS=(); [ -n "$GREP" ] && GREP_ARGS=(-g "$GREP")
( cd "$REPO_ROOT/acceptance" && npm ci >/dev/null 2>&1 || true
  npm run test:accept -- "${GREP_ARGS[@]}" )
rc=$?
log "acceptance suite exit=$rc  (report: $RD/artifacts/pw-report)"
exit $rc
