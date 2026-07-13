#!/usr/bin/env bash
# run.sh — the one command. Concurrency-safe end to end (run it from many branches at once):
#   0. build fresh binaries + images + stage config         (in the dev-spawnery distrobox)
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
STAGE="$RD/stage"; mkdir -p "$STAGE/bin" "$STAGE/config" "$STAGE/provision/env"
CLIENT_STATE="$RD/client-state"; install -d -m0700 "$CLIENT_STATE"
log "=== e2e-vm run  runid=$E2E_RUNID  profile=$PROFILE ==="

# teardown on ANY exit (success, failure, Ctrl-C) unless --keep
cleanup() { local rc=$?; if [ "$KEEP" = 1 ]; then warn "--keep set; not tearing down"; else
  E2E_RUNID="$E2E_RUNID" "$E2E_DIR/down.sh" || true; fi; exit $rc; }
trap cleanup EXIT INT TERM

DBOX_BIN="${E2E_DISTROBOX_BIN:-distrobox}"
DOCKER_BIN="${E2E_DOCKER_BIN:-docker}"
# Go's build cache action IDs are shared across worktrees of the same module. Keep it run-local so
# a concurrent branch cannot hand this lane a cached binary built from different source/VCS state.
DBOX_GOCACHE="$RD/go-cache"
dbox() { "$DBOX_BIN" enter --root dev-spawnery -- bash -lc "export GOCACHE='$DBOX_GOCACHE'; cd '$REPO_ROOT' && $*"; }

build_web() {
  local public_dir="$1" root_b64
  root_b64="$(base64 -w0 "$public_dir/root.pem")"
  dbox "cd web && npm ci && VITE_CP_ORIGIN='$WEB_ORIGIN' VITE_AS_ORIGIN='$WEB_ORIGIN' VITE_ROOT_CA_PEM=\"\$(printf '%s' '$root_b64' | base64 -d)\" VITE_TRUST_DOMAIN='prod.spawnery.internal' VITE_CLOUD_ACCOUNT_ID='spawnery-system' npm run build && PATH='$REPO_ROOT/sdk/ts/node_modules/.bin':\"\$PATH\" ../deploy/web/forbidden-scan.sh dist && rm -rf '$STAGE/web-dist' && cp -rf dist '$STAGE/web-dist'"
  test -d "$STAGE/web-dist"
  test -f "$STAGE/web-dist/index.html"
}

# ---- 0. build fresh code (per-run staging so concurrent branches never clobber each other) ----
if [ "$BUILD" = 1 ]; then
  log "0/3 building fresh binaries + images …"
  dbox "make build bin/spawnery_cp && cp -f bin/spawnery_cp bin/authsvc bin/spawnlet bin/spawnctl bin/spawnery-ca '$STAGE/bin/'"
  for binary in spawnery_cp authsvc spawnlet spawnctl spawnery-ca; do
    test -x "$STAGE/bin/$binary"
  done
  dbox "make -B images DOCKER='distrobox-host-exec docker'"
  "$DOCKER_BIN" save spawnery/sidecar:dev spawnery/agent:dev -o "$STAGE/images.tar"
  test -s "$STAGE/images.tar"
  dbox "cd acceptance && npm ci"
  cp -rf "$REPO_ROOT/config/." "$STAGE/config/"
else
  log "0/3 --no-build: staging existing $REPO_ROOT/bin"
  cp -f "$REPO_ROOT"/bin/{spawnery_cp,authsvc,spawnlet,spawnctl,spawnery-ca} "$STAGE/bin/" 2>/dev/null || die "no prebuilt bin/ — drop --no-build"
fi
cp -f "$REPO_ROOT/scripts/e2e-vm/provision/gen-pki.sh" "$STAGE/provision/"
cp -f "$REPO_ROOT/scripts/e2e-vm/provision/reconcile-gitea-env.sh" "$STAGE/provision/"
cp -f "$REPO_ROOT/scripts/e2e-vm/provision/env/"*.env "$STAGE/provision/env/"

# Test-only stop point used by the fail-closed build harness. It exercises the same pinned web
# build function with inert public values, without starting a VM. Normal runs never set this.
if [ "${E2E_RUN_BUILD_ONLY:-0}" = 1 ]; then
  TEST_PUBLIC="$RD/test-public"; mkdir -p "$TEST_PUBLIC"
  printf '%s\n' 'test-public-root' > "$TEST_PUBLIC/root.pem"
  build_web "$TEST_PUBLIC"
  exit 0
fi

# ---- 1. start the VM ----
log "1/3 starting VM …"
E2E_RUNID="$E2E_RUNID" GOLDEN_IMAGE="$GOLDEN_IMAGE" "$E2E_DIR/up.sh" --profile "$PROFILE"
# shellcheck disable=SC1091
source "$RD/acc.env"
if ! getent ahosts "$E2E_VM_HOST" >/dev/null 2>&1; then
  [ "$E2E_HOSTS_MODE" = nss ] || die "system resolver cannot resolve $E2E_VM_HOST in $E2E_HOSTS_MODE mode"
  warn "nss-libvirt cannot resolve $E2E_VM_HOST; falling back to the concurrency-safe /etc/hosts entry"
  export E2E_HOSTS_MODE=hosts
  host_resolve_add "$E2E_VM_HOST" "$E2E_VM_IP"
fi
getent ahosts "$E2E_VM_HOST" >/dev/null || die "system resolver cannot resolve $E2E_VM_HOST"

# ---- 2. copy fresh code in + wait ready ----
log "2/3 rolling fresh code + waiting ready …"
E2E_RUNID="$E2E_RUNID" STAGE="$STAGE" "$E2E_DIR/roll.sh"

# roll.sh has now generated the per-run PKI. Copy only public verification material out, stamp the
# immutable web bundle with that exact root, then publish it over the stale golden-image bundle.
PUBLIC="$RD/public-pki"; mkdir -p "$PUBLIC"
vm_ssh "$E2E_VM_IP" 'rm -rf ~/public-pki && mkdir -m0700 ~/public-pki && sudo install -m0644 /etc/spawnery/node/root.pem /etc/spawnery/node/service-intermediate.pem /etc/spawnery/node/cloud-intermediate.pem /etc/spawnery/node/self-hosted-intermediate.pem /etc/spawnery/node/service.crl.pem /etc/spawnery/node/cloud-node.crl.pem /etc/spawnery/node/self-hosted-node.crl.pem ~/public-pki/'
scp -q -o BatchMode=yes -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -i "$E2E_SSH_KEY" \
  "$E2E_SSH_USER@$E2E_VM_IP:public-pki/"'*' "$PUBLIC/"
vm_ssh "$E2E_VM_IP" 'rm -rf ~/public-pki'
build_web "$PUBLIC"
vm_ssh "$E2E_VM_IP" 'rm -rf ~/incoming/web && mkdir -p ~/incoming/web'
vm_scp "$STAGE/web-dist/." "$E2E_VM_IP" 'incoming/web/'
vm_ssh "$E2E_VM_IP" 'sudo rsync -a --delete ~/incoming/web/ /var/www/spawnery/'

# ---- 3. run the real e2e suite against the VM ----
log "3/3 running acceptance suite …"
# shellcheck disable=SC1091
source "$RD/acc.env"
# these mirror the validated live invocation (see provision/RECONCILE-NOTES.md): the acceptance
# suite needs real OAuth wired to the fake-GitHub identity, the demo app id, model ids, and a
# longer spawn-active timeout (VM boot is slower than local dev) to actually run against the VM.
export ACC_AUTH_MODE=oauth-pop
export ACC_IDENTITY_POOL="acc-owner-1=acc-owner-1,acc-owner-2=acc-owner-2"
export ACC_DESTRUCTIVE_DEV_TOKEN=devtoken1
export ACC_ROOT_CA_PEM="$PUBLIC/root.pem"
export ACC_TRUST_DOMAIN=prod.spawnery.internal
export ACC_CLOUD_ACCOUNT_ID=spawnery-system
export ACC_CRL_STATE="$CLIENT_STATE/crl-state.json"
export ACC_CRL_ISSUERS="$PUBLIC/service-intermediate.pem,$PUBLIC/cloud-intermediate.pem,$PUBLIC/self-hosted-intermediate.pem"
export ACC_CRLS="$PUBLIC/service.crl.pem,$PUBLIC/cloud-node.crl.pem,$PUBLIC/self-hosted-node.crl.pem"
# version-pin refs (preflight requires them). target==build here (the VM runs the code we just
# rolled), so pin both to this run id so the check is meaningful and trivially satisfied.
export ACC_TARGET_REF="$E2E_RUNID" ACC_BUILD_REF="$E2E_RUNID"
export ACC_TEST_APP_ID=spawnery/secret-app ACC_LIFECYCLE_APP=spawnery/secret-app ACC_AGENT_APP_ID=spawnery/secret-app
export ACC_SEED_SKILL_APP_ID=spawnery/secret-app
export ACC_APP_ID=spawnery/secret-app   # tenancy specs' generic app id
export ACC_TEST_MODEL=openai/gpt-4o-mini ACC_AGENT_MODEL=openai/gpt-4o-mini
export ACC_SPAWN_ACTIVE_TIMEOUT_MS=240000
[ -f "${GOLDEN_IMAGE%.qcow2}-ca.crt" ] && export NODE_EXTRA_CA_CERTS="${GOLDEN_IMAGE%.qcow2}-ca.crt"
export ACC_SPAWNCTL_BIN="$STAGE/bin/spawnctl"     # cliDriver shells out to the fresh spawnctl
export ACC_E2E_VM_IP="$E2E_VM_IP" ACC_E2E_SSH_KEY="$E2E_SSH_KEY" ACC_E2E_SSH_USER="$E2E_SSH_USER"
# Some hosts lack libvirt's NSS module. Resolve only this disposable VM hostname inside Node test
# processes; the URL hostname and golden CA remain unchanged, so TLS hostname validation still runs.
export NODE_OPTIONS="--require=$E2E_DIR/node-dns-hook.cjs${NODE_OPTIONS:+ $NODE_OPTIONS}"
# Exercise the exact DNS/TLS/runtime path globalSetup uses. A listening :443 can still be Caddy's
# old process during restart; do not hand that transient state to a one-shot acceptance preflight.
for i in $(seq 1 30); do
  if node -e 'fetch(process.env.ACC_WEB_ORIGIN).then((r) => { if (r.status >= 400) throw new Error(`HTTP ${r.status}`); }).catch(() => process.exit(1))'; then
    break
  fi
  [ "$i" = 30 ] && die "Node acceptance path did not reach $ACC_WEB_ORIGIN"
  sleep 1
done
export PLAYWRIGHT_HTML_REPORT="$RD/artifacts/pw-report"   # per-run output — concurrency-safe
export PLAYWRIGHT_OUTPUT_DIR="$RD/artifacts/pw-results"
GREP_ARGS=(); [ -n "$GREP" ] && GREP_ARGS=(-g "$GREP")
# VM lane knobs:
# --retries=0: spawns are per-owner-quota-limited + slow to create, so a retry doesn't mask a flake —
#   it spawns MORE, exhausts the quota, and cascades later @mutating tests to red.
# --workers=<pool size>: each worker needs its own identity from ACC_IDENTITY_POOL; Playwright
#   otherwise defaults to CPU-count workers, and every worker past the pool size (default 3) dies at
#   "identity pool has N entries but worker parallelIndex=… needs one". Cap workers to the pool.
( cd "$REPO_ROOT/acceptance"
  npm run test:accept -- --retries=0 --workers=1 "${GREP_ARGS[@]}" )
rc=$?
log "acceptance suite exit=$rc  (report: $RD/artifacts/pw-report)"
exit $rc
