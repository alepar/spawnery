#!/usr/bin/env bash
set -euo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO="$(cd "$HERE/../../.." && pwd)"
FILES=(
  "$REPO/Justfile"
  "$REPO/config"
  "$REPO/deploy"
  "$REPO/PROVISIONING.md"
  "$HERE"
)

legacy='CP_AS_SESSION_PUBKEYS|NODE_AS_PUBKEYS|AS_SESSION_KEY_PEM|AS_SESSION_KEY_NEXT_PEM|CP_DEV_AS_KEY|as_session_pubkeys|as_pubkeys|session-(key|pub)\.pem'
if rg -n --glob '!test-auth-topology.sh' "$legacy" "${FILES[@]}"; then
  echo "legacy raw authorization-signing configuration remains" >&2
  exit 1
fi

common="$HERE/env/common.env"
for expected in \
  AS_AUTH_SIGNING_CURRENT_KEY_PEM \
  AS_AUTH_SIGNING_CURRENT_CHAIN_PEM \
  AS_AUTH_SIGNING_NEXT_KEY_PEM \
  AS_AUTH_SIGNING_NEXT_CHAIN_PEM \
  CP_AUTH_ROOT_CA \
  CP_AUTH_SIGNER_REVOCATION_STATE \
  NODE_ROOT_CA \
  NODE_SIGNER_REVOCATION_STATE
do
  rg -q "^${expected}=" "$common" || {
    echo "missing ${expected} from VM common.env" >&2
    exit 1
  }
done

if rg -n 'auth-signing-intermediate-key' "$common" "$HERE/env"/*.env "$HERE/provision.sh"; then
  echo "offline auth-signing intermediate key is exposed to runtime provisioning" >&2
  exit 1
fi

echo "auth signing runtime topology is root-anchored and leaf-only"
