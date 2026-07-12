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

offline_runtime='(/var/lib/spawnery-offline|root-key\.pem|service-intermediate-key\.pem|cloud-intermediate-key\.pem|auth-signing-intermediate-key\.pem)'
if rg -n "$offline_runtime" "$HERE/env"/*.env; then
  echo "offline ceremony key material is exposed through a service environment" >&2
  exit 1
fi

if rg -n '^NODE_(ID_DIR|ROOT_CA|CERTIFICATE_REVOCATION_(ISSUERS|CRLS))=/etc/spawnery/pki' "$common"; then
  echo "node runtime still references shared PKI staging" >&2
  exit 1
fi

as_match="$(awk '$1 == "@as" && $2 == "path" { print; exit }' "$HERE/provision.sh")"
read -r -a as_tokens <<< "$as_match"
public_enrollment_found=0
for token in "${as_tokens[@]:2}"; do
  if [[ "$token" == "/enrollment-tokens" ]]; then
    public_enrollment_found=1
  elif [[ "$token" == /enroll* ]]; then
    echo "Caddy exposes internal enrollment path token: $token" >&2
    exit 1
  fi
done
if [[ "$public_enrollment_found" != 1 ]]; then
  echo "Caddy does not proxy public enrollment-token issuance" >&2
  exit 1
fi

echo "auth signing runtime topology is root-anchored and leaf-only"
