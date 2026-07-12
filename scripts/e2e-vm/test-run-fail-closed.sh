#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

cat > "$TMP/distrobox" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
cmd="${*: -1}"
case "$cmd" in
  *"make build bin/spawnery_cp"*)
    [ "${FAIL_PHASE:-}" = binaries ] && exit 41
    stage="$(printf '%s' "$cmd" | sed -n "s#.*'\([^']*\)/bin/'.*#\1#p")"
    mkdir -p "$stage/bin"
    for name in spawnery_cp authsvc spawnlet spawnctl spawnery-ca; do
      printf '#!/bin/sh\n' > "$stage/bin/$name"
      chmod +x "$stage/bin/$name"
    done
    ;;
  *"make -B images"*)
    [ "${FAIL_PHASE:-}" = images ] && exit 42
    ;;
  *"cd web && npm ci"*)
    [ "${FAIL_PHASE:-}" = web ] && exit 43
    [ "${FAIL_PHASE:-}" = staging ] && exit 0
    stage="$(printf '%s' "$cmd" | sed -n "s#.*rm -rf '\([^']*\)/web-dist'.*#\1#p")"
    mkdir -p "$stage/web-dist"
    : > "$stage/web-dist/index.html"
    ;;
  *"cd acceptance && npm ci"*)
    [ "${FAIL_PHASE:-}" = acceptance-npm ] && exit 44
    ;;
esac
EOF

cat > "$TMP/docker" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
[ "${FAIL_PHASE:-}" = image-save ] && exit 45
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

echo "e2e-vm fresh build failures are fail-closed"
