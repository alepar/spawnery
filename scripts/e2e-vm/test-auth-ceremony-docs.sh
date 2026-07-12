#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"

for doc in "$ROOT/deploy/authsvc/README.md" "$ROOT/PROVISIONING.md"; do
  rg -q 'private-key/leaf match' "$doc"
  rg -q "maximum artifact lifetime plus the verifier's allowed clock" "$doc"
  rg -q 'Switch issuance by promoting next to current' "$doc"
  rg -q 'delete the retired private key' "$doc"
  rg -q 'Retain the retired public chain' "$doc"
  rg -q 'offline (auth-signing intermediate|issuer)' "$doc"
  rg -q 'higher-generation revocation statement' "$doc"
  rg -q 'generation convergence' "$doc"
done

echo "auth signer ceremony documentation is complete"
