#!/usr/bin/env bash
# Pre-sign forbidden-value scanner wrapper.
# Invokes the TypeScript scanner (web/build/forbidden-scan.ts) against dist/.
# Called by the web-release.yml pipeline before cosign signing.
#
# Usage:
#   ./forbidden-scan.sh [dist-dir]
#
# Exits 0 if clean, 1 if violations found.

set -euo pipefail

DIST="${1:-$(git rev-parse --show-toplevel 2>/dev/null || echo .)/web/dist}"

if [[ ! -d "$DIST" ]]; then
  echo "forbidden-scan: dist directory not found: $DIST" >&2
  exit 1
fi

# Run the TS scanner via tsx (available in the web dev environment).
REPO_ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "$REPO_ROOT/web"
npx tsx build/forbidden-scan.ts "$DIST"
