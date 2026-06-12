#!/usr/bin/env bash
# Reusable cosign verify wrapper for the Spawnery SPA.
# Called by the web-release.yml deploy job and can be run locally against any artifact.
#
# Usage:
#   ./verify.sh <artifact-path> <signature-bundle-path>
#
# Pins the EXACT certificate identity and OIDC issuer — no regexp flags.
# This is the gate: if the signature does not match the expected identity,
# the script exits non-zero and deployment is refused.
#
# [WM21]: The gate is FALSIFIABLE — see web-release.yml for the self-test step
# that asserts this script FAILS for a signature-stripped artifact and a wrong-identity fixture.

set -euo pipefail

ARTIFACT="${1:-}"
BUNDLE="${2:-}"

if [[ -z "$ARTIFACT" || -z "$BUNDLE" ]]; then
  echo "Usage: $0 <artifact-path> <signature-bundle-path>" >&2
  exit 1
fi

# The exact GitHub Actions OIDC workflow identity for the release pipeline.
# These values MUST match the signing identity — any deviation is refused.
# Replace with the real repository path before going to production.
CERT_IDENTITY="${COSIGN_CERT_IDENTITY:-https://github.com/gastownhall/spawnery/.github/workflows/web-release.yml@refs/heads/master}"
OIDC_ISSUER="${COSIGN_OIDC_ISSUER:-https://token.actions.githubusercontent.com}"

echo "Verifying artifact: $ARTIFACT"
echo "Certificate identity: $CERT_IDENTITY"
echo "OIDC issuer: $OIDC_ISSUER"

cosign verify-blob \
  --certificate-identity "$CERT_IDENTITY" \
  --certificate-oidc-issuer "$OIDC_ISSUER" \
  --bundle "$BUNDLE" \
  "$ARTIFACT"

echo "Verification PASSED."
