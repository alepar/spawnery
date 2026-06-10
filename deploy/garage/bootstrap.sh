#!/usr/bin/env bash
# Bootstrap a single-node dev Garage for Spawnery's transient-tier journal store:
#   1. wait for the daemon,
#   2. apply the one-time cluster layout (Garage refuses bucket/key ops until a
#      layout is applied — even for a single node),
#   3. mint a default dev bucket + access key via the admin API and print the
#      S3 env the journaler / garage_e2e test consume.
#
# Idempotent: re-running skips the layout if already applied and reuses the bucket.
# Invoked by `just garage`. DEV ONLY (well-known secrets in garage.toml).
set -euo pipefail

DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
COMPOSE=(docker compose -f "$DIR/docker-compose.yml")

# Admin endpoint + token (must match garage.toml).
ADMIN_ENDPOINT="${GARAGE_ADMIN_ENDPOINT:-http://127.0.0.1:3903}"
ADMIN_TOKEN="${GARAGE_ADMIN_TOKEN:-$(grep -E '^admin_token' "$DIR/garage.toml" | cut -d'"' -f2)}"
S3_ENDPOINT="${GARAGE_S3_ENDPOINT:-127.0.0.1:3900}"
DEV_BUCKET="${GARAGE_DEV_BUCKET:-spawnery-journal}"

garage() { "${COMPOSE[@]}" exec -T garage /garage "$@"; }

echo ">> waiting for garage daemon..."
for _ in $(seq 1 60); do
  if garage status >/dev/null 2>&1; then break; fi
  sleep 1
done
garage status >/dev/null 2>&1 || { echo "garage did not come up"; exit 1; }

NODE_ID="$(garage node id -q | cut -d'@' -f1)"
echo ">> node id: $NODE_ID"

if garage layout show 2>/dev/null | grep -q "$NODE_ID"; then
  echo ">> cluster layout already applied"
else
  echo ">> assigning + applying single-node layout"
  garage layout assign -z dc1 -c 1G "$NODE_ID"
  # Garage prints the exact `layout apply --version N` to run; use it verbatim.
  APPLY_ARGS="$(garage layout show | grep -oE 'apply --version [0-9]+' | head -1)"
  # shellcheck disable=SC2086
  garage layout $APPLY_ARGS
fi

# --- mint a default dev bucket + access key via the admin API ---
admin() {
  curl -fsS -H "Authorization: Bearer ${ADMIN_TOKEN}" -H "Content-Type: application/json" "$@"
}

echo ">> ensuring dev bucket '${DEV_BUCKET}' + access key"
KEY_JSON="$(admin -X POST "${ADMIN_ENDPOINT}/v1/key" -d "{\"name\":\"${DEV_BUCKET}-key\"}")"
ACCESS_KEY_ID="$(printf '%s' "$KEY_JSON" | grep -oE '"accessKeyId":"[^"]+"' | cut -d'"' -f4)"
SECRET_KEY="$(printf '%s' "$KEY_JSON" | grep -oE '"secretAccessKey":"[^"]+"' | cut -d'"' -f4)"

# Create the bucket (ignore "alias already exists"); fetch its id either way.
admin -X POST "${ADMIN_ENDPOINT}/v1/bucket" -d "{\"globalAlias\":\"${DEV_BUCKET}\"}" >/dev/null 2>&1 || true
BUCKET_JSON="$(admin "${ADMIN_ENDPOINT}/v1/bucket?globalAlias=${DEV_BUCKET}")"
BUCKET_ID="$(printf '%s' "$BUCKET_JSON" | grep -oE '"id":"[^"]+"' | head -1 | cut -d'"' -f4)"

admin -X POST "${ADMIN_ENDPOINT}/v1/bucket/allow" \
  -d "{\"bucketId\":\"${BUCKET_ID}\",\"accessKeyId\":\"${ACCESS_KEY_ID}\",\"permissions\":{\"read\":true,\"write\":true,\"owner\":true}}" >/dev/null

cat <<EOF

>> Garage ready. Dev S3 credentials for the journaler:

   export GARAGE_S3_ENDPOINT=${S3_ENDPOINT}
   export GARAGE_ADMIN_ENDPOINT=${ADMIN_ENDPOINT}
   export GARAGE_ADMIN_TOKEN=${ADMIN_TOKEN}
   export GARAGE_BUCKET=${DEV_BUCKET}
   export GARAGE_ACCESS_KEY_ID=${ACCESS_KEY_ID}
   export GARAGE_SECRET_ACCESS_KEY=${SECRET_KEY}

>> Run the live S3 round-trip test:

   GARAGE_S3_ENDPOINT=${S3_ENDPOINT} GARAGE_ADMIN_ENDPOINT=${ADMIN_ENDPOINT} GARAGE_ADMIN_TOKEN=${ADMIN_TOKEN} \\
     go test -tags garage_e2e -run TestS3BackendRoundTripGarage -v ./internal/storage/journal/
EOF
