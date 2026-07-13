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

for phase in binaries images image-save web staging acceptance-npm; do
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

echo "e2e-vm fresh build failures are fail-closed"
