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

# `layout show` truncates the node id to its short (16-hex) form, so match THAT — comparing the
# full id never hit and re-assigned every run.
SHORT_ID="${NODE_ID:0:16}"
if garage layout show 2>/dev/null | grep -q "$SHORT_ID"; then
  echo ">> cluster layout already applied"
else
  echo ">> assigning + applying single-node layout"
  garage layout assign -z dc1 -c 1G "$NODE_ID"
  # Apply staged version = current + 1 (parsed from "Current cluster layout version: N").
  CUR="$(garage layout show 2>/dev/null | grep -oE 'version: [0-9]+' | grep -oE '[0-9]+' | tail -1)"
  garage layout apply --version "$(( ${CUR:-0} + 1 ))"
fi

# --- mint a default dev bucket + access key via the admin API ---
admin() {
  curl -fsS -H "Authorization: Bearer ${ADMIN_TOKEN}" -H "Content-Type: application/json" "$@"
}

echo ">> ensuring dev bucket '${DEV_BUCKET}' + access key"
# The admin API can 503 briefly right after a layout apply (propagation); retry the first call.
for _ in $(seq 1 15); do
  if KEY_JSON="$(admin -X POST "${ADMIN_ENDPOINT}/v1/key" -d "{\"name\":\"${DEV_BUCKET}-key\"}" 2>/dev/null)"; then break; fi
  sleep 1
done
[ -n "${KEY_JSON:-}" ] || { echo "admin key API not ready"; exit 1; }
# Garage pretty-prints the admin JSON ("key": "value" — note the space after the colon), so the
# field patterns tolerate optional whitespace.
ACCESS_KEY_ID="$(printf '%s' "$KEY_JSON" | grep -oE '"accessKeyId": *"[^"]+"' | head -1 | cut -d'"' -f4)"
SECRET_KEY="$(printf '%s' "$KEY_JSON" | grep -oE '"secretAccessKey": *"[^"]+"' | head -1 | cut -d'"' -f4)"

# Create the bucket (ignore "alias already exists"); fetch its id either way.
admin -X POST "${ADMIN_ENDPOINT}/v1/bucket" -d "{\"globalAlias\":\"${DEV_BUCKET}\"}" >/dev/null 2>&1 || true
BUCKET_JSON="$(admin "${ADMIN_ENDPOINT}/v1/bucket?globalAlias=${DEV_BUCKET}")"
BUCKET_ID="$(printf '%s' "$BUCKET_JSON" | grep -oE '"id": *"[^"]+"' | head -1 | cut -d'"' -f4)"

admin -X POST "${ADMIN_ENDPOINT}/v1/bucket/allow" \
  -d "{\"bucketId\":\"${BUCKET_ID}\",\"accessKeyId\":\"${ACCESS_KEY_ID}\",\"permissions\":{\"read\":true,\"write\":true,\"owner\":true}}" >/dev/null

# Persist the creds where `just node` can pick them up (gitignored; dev only). Uses the
# spawnlet's JOURNAL_* env names directly so the recipe can source the file as-is.
CREDS="$DIR/dev-creds.env"
cat > "$CREDS" <<EOF
JOURNAL_BACKEND=s3
JOURNAL_S3_ENDPOINT=${S3_ENDPOINT}
JOURNAL_S3_BUCKET=${DEV_BUCKET}
JOURNAL_S3_ACCESS_KEY=${ACCESS_KEY_ID}
JOURNAL_S3_SECRET_KEY=${SECRET_KEY}
JOURNAL_S3_REGION=garage
JOURNAL_S3_DISABLE_TLS=true
EOF
chmod 600 "$CREDS"
echo ">> wrote $CREDS (sourced automatically by 'just node')"

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
