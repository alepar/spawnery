#!/usr/bin/env bash
set -euo pipefail

env_file="${1:-/etc/spawnery/env.d/gitea.env}"
origin="${GITEA_ORIGIN:-http://127.0.0.1:3000}"
api_base="$origin/api/v1"

[[ -f "$env_file" ]] || {
  echo "missing generated Gitea environment: $env_file" >&2
  exit 1
}
token="$(sed -n 's/^GITHUB_STATIC_TOKEN=//p' "$env_file" | tail -n1)"
[[ -n "$token" && "$token" != *[[:space:]]* ]] || {
  echo "missing or malformed GITHUB_STATIC_TOKEN in $env_file" >&2
  exit 1
}

# The golden image may predate this branch. Prove its token belongs to the local Gitea before
# publishing the branch-owned endpoint values to the node environment.
curl -fsS "$origin/api/healthz" >/dev/null
curl -fsS -H "Authorization: token $token" "$api_base/user" >/dev/null

umask 077
tmp="$(mktemp "${env_file}.tmp.XXXXXX")"
trap 'rm -f "$tmp"' EXIT
printf '%s\n' \
  "GITHUB_API_BASE_URL=$api_base" \
  "GITHUB_HOST=${origin#http://}" \
  'GITHUB_ALLOW_INSECURE_HOST=1' \
  "GITHUB_STATIC_TOKEN=$token" >"$tmp"
chmod 0600 "$tmp"
mv -f "$tmp" "$env_file"
trap - EXIT
