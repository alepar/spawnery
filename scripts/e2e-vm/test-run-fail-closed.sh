#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

cat > "$TMP/distrobox" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
cmd="${*: -1}"
echo "mock distrobox: $cmd"
[[ "$cmd" == *"forbidden-scan"* ]] && echo "forbidden-scan: mock PASS"
case "$cmd" in
  *"make build bin/spawnery_cp"*)
    if [ "${FAIL_PHASE:-}" = binaries ]; then exit 41; fi
    stage="$(printf '%s' "$cmd" | sed -n "s#.*'\([^']*\)/bin/'.*#\1#p")"
    mkdir -p "$stage/bin"
    for name in spawnery_cp authsvc spawnlet spawnctl spawnery-ca; do
      printf '#!/bin/sh\n' > "$stage/bin/$name"
      chmod +x "$stage/bin/$name"
    done
    ;;
  *"make -B images"*)
    if [ "${FAIL_PHASE:-}" = images ]; then exit 42; fi
    ;;
  *"npm run gen --workspace @spawnery/client"*)
    if [ "${FAIL_PHASE:-}" = sdk ]; then exit 46; fi
    ;;
  *"cd web && npm ci"*)
    if [ "${FAIL_PHASE:-}" = web ]; then exit 43; fi
    if [ "${FAIL_PHASE:-}" = staging ]; then exit 0; fi
    stage="$(printf '%s' "$cmd" | sed -n "s#.*rm -rf '\([^']*\)/web-dist'.*#\1#p")"
    mkdir -p "$stage/web-dist"
    : > "$stage/web-dist/index.html"
    ;;
  *"cd acceptance && npm ci"*)
    if [ "${FAIL_PHASE:-}" = acceptance-npm ]; then exit 44; fi
    ;;
esac
EOF

cat > "$TMP/docker" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
echo "mock docker: $*"
if [ "${FAIL_PHASE:-}" = image-save ]; then exit 45; fi
while [ "$#" -gt 0 ]; do
  if [ "$1" = -o ]; then
    mkdir -p "$(dirname "$2")"
    printf image > "$2"
    exit 0
  fi
  shift
done
exit 46
EOF
chmod +x "$TMP/distrobox" "$TMP/docker"

for phase in binaries images image-save sdk web staging acceptance-npm; do
  out="$TMP/$phase.out"
  if FAIL_PHASE="$phase" E2E_RUN_BUILD_ONLY=1 E2E_DISTROBOX_BIN="$TMP/distrobox" \
      E2E_DOCKER_BIN="$TMP/docker" E2E_STATE_ROOT="$TMP/state-$phase" \
      GOLDEN_IMAGE=/not-used "$ROOT/scripts/e2e-vm/run.sh" --keep >"$out" 2>&1; then
    echo "run.sh accepted injected $phase failure" >&2
    exit 1
  fi
  if rg -q '1/3 starting VM|3/3 running acceptance suite' "$out"; then
    echo "run.sh reached VM/acceptance after injected $phase failure" >&2
    exit 1
  fi
done

if E2E_RUN_BUILD_ONLY=1 E2E_DISTROBOX_BIN="$TMP/distrobox" E2E_DOCKER_BIN="$TMP/docker" \
    E2E_STATE_ROOT="$TMP/state-pins" GOLDEN_IMAGE=/not-used \
    "$ROOT/scripts/e2e-vm/run.sh" --keep >"$TMP/pins.out" 2>&1; then
  :
else
  echo "run.sh build-only happy path failed" >&2
  cat "$TMP/pins.out" >&2
  exit 1
fi
rg -q 'forbidden-scan' "$TMP/pins.out" || {
  echo "run.sh did not execute the web release forbidden scan" >&2
  exit 1
}
rg -q 'sdk/ts/node_modules/.bin' "$TMP/pins.out" || {
  echo "run.sh did not make the workspace tsx available to the release scanner" >&2
  exit 1
}
rg -q 'VITE_NODE_CRL_BUNDLE_JSON' "$TMP/pins.out" || {
  echo "run.sh did not pass the generated issuer/CRL bundle to the fresh web build" >&2
  exit 1
}
for sdk_build_step in \
  'npm run gen --workspace @spawnery/client' \
  'npm run build --workspace @spawnery/client' \
  'test -f sdk/ts/dist/index.js'
do
  rg -Fq -- "$sdk_build_step" "$TMP/pins.out" || {
    echo "run.sh lacks required SDK build step: ${sdk_build_step}" >&2
    exit 1
  }
done
for public_binding in \
  '--rawfile cloud_issuer "$public_dir/cloud-intermediate.pem"' \
  '--rawfile cloud_crl "$public_dir/cloud-node.crl.pem"' \
  '--rawfile self_hosted_issuer "$public_dir/self-hosted-intermediate.pem"' \
  '--rawfile self_hosted_crl "$public_dir/self-hosted-node.crl.pem"'
do
  rg -Fq -- "$public_binding" "$ROOT/scripts/e2e-vm/run.sh" || {
    echo "run.sh lacks structured public CRL bundle binding: ${public_binding}" >&2
    exit 1
  }
done
if rg -n 'VITE_NODE_CRL_(URL|ENDPOINT)|NODE_CRL_URL|fetch\([^)]*[Cc][Rr][Ll]' \
    "$ROOT/scripts/e2e-vm/run.sh" "$ROOT/web/src/auth/crl.ts"; then
  echo "run.sh/web CRL verification contains a runtime refresh fallback" >&2
  exit 1
fi
for required in 'getent ahosts' 'E2E_HOSTS_MODE=hosts'; do
  rg -q "$required" "$ROOT/scripts/e2e-vm/run.sh" || {
    echo "run.sh lacks required production-lane wiring: $required" >&2
    exit 1
  }
done
rg -Fq 'export ACC_PRODUCTION_SPA_BUNDLE="$STAGE/web-dist"' "$ROOT/scripts/e2e-vm/run.sh" || {
  echo "run.sh does not expose the exact production SPA bundle for destructive restoration" >&2
  exit 1
}

expected_acceptance_commands=$'npm run test:accept -- --retries=0 --workers=1 --project=chromium -g "@noderestart" && rc=0 || rc=$?\nnpm run test:accept -- --retries=0 --workers=1 --project=chromium --grep-invert "@noderestart" && arc=0 || arc=$?\nnpm run test:accept -- --retries=0 --workers=1 --project=destructive-root-artifacts --no-deps && drc=0 || drc=$?'
actual_acceptance_commands="$(
  sed -n '/^# Pass 1:/,$p' "$ROOT/scripts/e2e-vm/run.sh" |
    rg '^npm run test:accept -- '
)"
if [ "$actual_acceptance_commands" != "$expected_acceptance_commands" ]; then
  echo "run.sh acceptance command topology mismatch" >&2
  printf 'expected:\n%s\nactual:\n%s\n' \
    "$expected_acceptance_commands" "$actual_acceptance_commands" >&2
  exit 1
fi

PLAYWRIGHT="$ROOT/node_modules/.bin/playwright"
[ -x "$PLAYWRIGHT" ] || {
  echo "repository-local Playwright CLI is unavailable; run npm ci" >&2
  exit 1
}

list_acceptance() {
  env ACC_LIFECYCLE_APP=test-app ACC_TEST_MODEL=test-model \
    "$PLAYWRIGHT" test \
      --config="$ROOT/acceptance/playwright.config.ts" \
      --list --workers=1 "$@"
}

list_acceptance --project=chromium -g '@noderestart' >"$TMP/noderestart.list"
list_acceptance --project=chromium --grep-invert '@noderestart' >"$TMP/ordinary.list"
list_acceptance --project=destructive-root-artifacts --no-deps >"$TMP/destructive.list"

restart_count="$(rg -c '^  \[chromium\].*@noderestart$' "$TMP/noderestart.list" || true)"
if [ "$restart_count" -ne 1 ] ||
    rg -q '\[destructive-root-artifacts\]' "$TMP/noderestart.list"; then
  echo "restart discovery is not exactly one chromium @noderestart test" >&2
  cat "$TMP/noderestart.list" >&2
  exit 1
fi

rg -q '^  \[chromium\] ›' "$TMP/ordinary.list" || {
  echo "ordinary discovery contains no chromium tests" >&2
  cat "$TMP/ordinary.list" >&2
  exit 1
}
if rg -q '@noderestart|\[destructive-root-artifacts\]' "$TMP/ordinary.list"; then
  echo "ordinary discovery replayed restart or destructive coverage" >&2
  cat "$TMP/ordinary.list" >&2
  exit 1
fi

destructive_count="$(
  rg -c '^  \[destructive-root-artifacts\] › auth/root-anchored-artifacts\.spec\.ts:' \
    "$TMP/destructive.list" || true
)"
if [ "$destructive_count" -ne 1 ] ||
    rg -q '\[chromium\]|@noderestart' "$TMP/destructive.list"; then
  echo "destructive discovery replayed chromium dependencies or lost root-artifact coverage" >&2
  cat "$TMP/destructive.list" >&2
  exit 1
fi

echo "e2e-vm fresh build and acceptance topology are fail-closed"
