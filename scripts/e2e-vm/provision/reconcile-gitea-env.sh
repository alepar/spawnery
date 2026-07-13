#!/usr/bin/env bash
set -euo pipefail

env_file="${1:-/etc/spawnery/env.d/gitea.env}"
app_ini="${2:-/etc/gitea/app.ini}"
origin="http://127.0.0.1:3000"
api_base="$origin/api/v1"

[[ -f "$env_file" ]] || {
  echo "missing generated Gitea environment: $env_file" >&2
  exit 1
}
[[ -f "$app_ini" ]] || {
  echo "missing Gitea configuration: $app_ini" >&2
  exit 1
}

# Golden images may predate this branch. Replace only the HEAD-owned server keys, leaving the
# database and generated security secrets intact. Stop the golden-image bootstrap first: restarting
# Gitea otherwise reactivates that oneshot unit asynchronously and lets it overwrite gitea.env
# after this reconciler has returned.
systemctl stop spawnery-gitea-bootstrap.service
app_mode="$(stat -c %a "$app_ini")"
app_uid="$(stat -c %u "$app_ini")"
app_gid="$(stat -c %g "$app_ini")"
app_tmp="$(mktemp "${app_ini}.tmp.XXXXXX")"
trap 'rm -f "$app_tmp"' EXIT
awk '
  {
    section = $0
    gsub(/^[[:space:]]+|[[:space:]]+$/, "", section)
    if (section == "[server]") {
      found_server = 1
      in_server = 1
      print $0
      print "PROTOCOL = http"
      print "HTTP_ADDR = 127.0.0.1"
      print "HTTP_PORT = 3000"
      print "DOMAIN = 127.0.0.1"
      print "ROOT_URL = http://127.0.0.1:3000/"
      next
    }
    if (in_server && section ~ /^\[/) in_server = 0
    if (in_server && $0 ~ /^[[:space:]]*(PROTOCOL|HTTP_ADDR|HTTP_PORT|DOMAIN|ROOT_URL)[[:space:]]*=/) next
    print $0
  }
  END { if (!found_server) exit 42 }
' "$app_ini" >"$app_tmp" || {
  echo "Gitea configuration has no [server] section: $app_ini" >&2
  exit 1
}
chmod "$app_mode" "$app_tmp"
chown "$app_uid:$app_gid" "$app_tmp"
mv -f "$app_tmp" "$app_ini"
trap - EXIT

systemctl restart gitea
healthy=0
for _ in $(seq 1 60); do
  if curl -fsS "$origin/api/healthz" >/dev/null 2>&1; then
    healthy=1
    break
  fi
  sleep 1
done
[[ "$healthy" == 1 ]] || {
  echo "Gitea did not become healthy at $origin" >&2
  exit 1
}

# Bootstrap synchronously against the reconciled server, then consume the token it just minted.
# Golden-image bootstrap scripts may still publish stale endpoint values; the atomic write below
# replaces those values only after proving the new token against the local Gitea API.
systemctl start spawnery-gitea-bootstrap.service
token="$(sed -n 's/^GITHUB_STATIC_TOKEN=//p' "$env_file" | tail -n1)"
[[ -n "$token" && "$token" != *[[:space:]]* ]] || {
  echo "missing or malformed GITHUB_STATIC_TOKEN in $env_file" >&2
  exit 1
}
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
