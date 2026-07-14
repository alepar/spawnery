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

runtime_files=("$HERE/env"/*.env "$REPO/scripts/e2e-vm/run.sh" "$HERE/provision.sh")
forbidden_client_shortcuts='CP_DEV_AS_KEY|CP_AS_SESSION_PUBKEYS|NODE_AS_PUBKEYS|/node-token|SPAWNERY_INTENT_KEY_PKCS8_B64|SPAWNERY_NODE_ACCESS_TOKEN'
if rg -n "$forbidden_client_shortcuts" "${runtime_files[@]}"; then
  echo "production VM wiring contains a forbidden client authorization shortcut" >&2
  exit 1
fi

runner="$REPO/scripts/e2e-vm/run.sh"
for required in \
  VITE_ROOT_CA_PEM \
  VITE_NODE_CRL_BUNDLE_JSON \
  VITE_TRUST_DOMAIN \
  VITE_CLOUD_ACCOUNT_ID \
  ACC_ROOT_CA_PEM \
  ACC_TRUST_DOMAIN \
  ACC_CLOUD_ACCOUNT_ID \
  ACC_CRL_STATE \
  ACC_CRL_ISSUERS \
  ACC_CRLS
do
  rg -q "$required" "$runner" || {
    echo "missing public web/CLI trust input ${required} from run.sh" >&2
    exit 1
  }
done

for destructive_binding in \
  'install -o root -g root -m 0600 /dev/stdin /run/spawnery-e2e-runid' \
  'export ACC_E2E_VM_RUNID="$E2E_RUNID"'
do
  rg -Fq "$destructive_binding" "$runner" || {
    echo "run.sh lacks disposable VM identity binding: ${destructive_binding}" >&2
    exit 1
  }
done

for public_binding in \
  '--rawfile cloud_issuer "$public_dir/cloud-intermediate.pem"' \
  '--rawfile cloud_crl "$public_dir/cloud-node.crl.pem"' \
  '--rawfile self_hosted_issuer "$public_dir/self-hosted-intermediate.pem"' \
  '--rawfile self_hosted_crl "$public_dir/self-hosted-node.crl.pem"'
do
  rg -Fq -- "$public_binding" "$runner" || {
    echo "run.sh does not construct the web CRL bundle from ${public_binding}" >&2
    exit 1
  }
done
rg -Fq "jq -n" "$runner" || {
  echo "run.sh does not construct the web CRL bundle as structured JSON" >&2
  exit 1
}
for node_class in cloud self-hosted; do
  rg -Fq "class: \"${node_class}\"" "$runner" || {
    echo "run.sh CRL bundle is missing ${node_class} issuer topology" >&2
    exit 1
  }
done

runtime_crl_fallback='VITE_NODE_CRL_(URL|ENDPOINT)|NODE_CRL_URL|NODE_CERTIFICATE_REVOCATION_URLS|fetch\([^)]*[Cc][Rr][Ll]|curl[^\n]*[Cc][Rr][Ll]'
if rg -n "$runtime_crl_fallback" "$runner" "$REPO/web/src/auth/crl.ts"; then
  echo "web node-certificate revocation has a forbidden runtime URL/fetch fallback" >&2
  exit 1
fi
if rg -n 'VITE_NODE_CRL_BUNDLE_JSON[^\n]*(key|private)' "$runner"; then
  echo "web CRL bundle wiring includes private key material" >&2
  exit 1
fi

rg -Fq 'provision/reconcile-gitea-env.sh" "$STAGE/provision/' "$runner" || {
  echo "run.sh does not stage the versioned Gitea environment reconciler" >&2
  exit 1
}
rg -Fq 'reconcile-gitea-env.sh /etc/spawnery/env.d/gitea.env' "$REPO/scripts/e2e-vm/roll.sh" || {
  echo "roll.sh does not reconcile generated Gitea state before restarting the node" >&2
  exit 1
}
for caddy_cert in wildcard github; do
  rg -Fq "/etc/spawnery/pki/${caddy_cert}.crt /etc/spawnery/caddy/${caddy_cert}.crt" "$REPO/scripts/e2e-vm/roll.sh" || {
    echo "roll.sh does not preserve the golden ${caddy_cert} certificate before rotating internal PKI" >&2
    exit 1
  }
  rg -Fq "/etc/spawnery/pki/${caddy_cert}.key /etc/spawnery/caddy/${caddy_cert}.key" "$REPO/scripts/e2e-vm/roll.sh" || {
    echo "roll.sh does not preserve the golden ${caddy_cert} key before rotating internal PKI" >&2
    exit 1
  }
done

rg -q 'install -d -m0700 "\$CLIENT_STATE"' "$runner" || {
  echo "run.sh does not create a private mutable client-state directory" >&2
  exit 1
}
rg -q 'ACC_CRL_STATE="\$CLIENT_STATE/crl-state.json"' "$runner" || {
  echo "run.sh does not keep mutable CRL state under the private client-state directory" >&2
  exit 1
}
rg -q 'ACC_SEED_SKILL_APP_ID=spawnery/secret-app' "$runner" || {
  echo "run.sh does not wire the production skill-injection scenario to a seeded app" >&2
  exit 1
}
rg -q 'ACC_AGENT_INFERENCE_AVAILABLE=0' "$runner" || {
  echo "fake VM profile does not explicitly declare inference unavailable" >&2
  exit 1
}
if rg -q 'export .*ACC_AGENT_(APP_ID|MODEL)=' "$runner"; then
  echo "fake VM profile falsely advertises live @agent inputs" >&2
  exit 1
fi
rg -q 'ACC_TEST_MODEL' "$REPO/acceptance/tests/customization/injection.spec.ts" || {
  echo "non-LLM customization injection still depends on @agent inference inputs" >&2
  exit 1
}

check_device_rate_limit_topology() {
  local env_dir="$1"
  local fake_profile="$env_dir/profile.fake.env"
  local -a assignments=()

  mapfile -t assignments < <(rg -n '^AS_DEVICE_PER_MIN=' "$env_dir"/*.env || true)
  if (( ${#assignments[@]} != 1 )); then
    printf '%s\n' "${assignments[@]}" >&2
    echo "AS_DEVICE_PER_MIN must occur exactly once across VM profiles" >&2
    return 1
  fi
  local assignment="${assignments[0]}"
  if [[ "${assignment%%:*}" != "$fake_profile" || "${assignment##*:}" != "AS_DEVICE_PER_MIN=100" ]]; then
    printf '%s\n' "$assignment" >&2
    echo "AS_DEVICE_PER_MIN must be 100 and scoped only to profile.fake.env" >&2
    return 1
  fi
}

check_device_rate_limit_topology "$HERE/env"
if (
  fixture="$(mktemp -d)"
  trap 'rm -rf "$fixture"' EXIT
  cp -f "$HERE/env"/*.env "$fixture/"
  printf '%s\n' 'AS_DEVICE_PER_MIN=100' >"$fixture/profile.extra.env"
  check_device_rate_limit_topology "$fixture"
) >/dev/null 2>&1; then
  echo "device-flow rate-limit topology guard accepted an override outside profile.fake.env" >&2
  exit 1
fi

common="$HERE/env/common.env"
if rg -n '^NODE_TERMINAL_ADDR=' "$common"; then
  echo "enforced VM node exposes a direct terminal listener" >&2
  exit 1
fi
spawnlet_prod="$REPO/config/spawnlet.prod.yaml"
rg -Uq '^node:\n  terminal_addr: ""$' "$spawnlet_prod" || {
  echo "production spawnlet config does not explicitly disable the inherited direct terminal listener" >&2
  exit 1
}
if rg -n 'ACC_NODE_(ADDR|TERMINAL_ADDR)' "$REPO/scripts/e2e-vm/up.sh" "$REPO/acceptance/.env.example"; then
  echo "acceptance topology still advertises a direct node endpoint" >&2
  exit 1
fi
rg -q '^AGENT_IMAGE=spawnery/agent:dev$' "$common" || {
  echo "VM node does not run the staged unified agent image" >&2
  exit 1
}
rg -q '^AGENT_BINARIES=opencode,goose,claude-code,codex,hermes,pi$' "$common" || {
  echo "VM node does not advertise the installed production agent binaries exactly" >&2
  exit 1
}

gitea_reconcile="$HERE/reconcile-gitea-env.sh"
[[ -x "$gitea_reconcile" ]] || {
  echo "missing executable Gitea environment reconciler" >&2
  exit 1
}
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT
mkdir -p "$tmp/bin"
cat >"$tmp/bin/systemctl" <<'EOF'
#!/usr/bin/env bash
printf '%s\n' "$*" >>"$SYSTEMCTL_LOG"
if [[ "$*" == "start spawnery-gitea-bootstrap.service" ]]; then
  cat >"$GITEA_ENV_FILE" <<'ENVEOF'
GITHUB_API_BASE_URL=https://github.com/api/v1
GITHUB_HOST=github.com
GITHUB_ALLOW_INSECURE_HOST=0
GITHUB_STATIC_TOKEN=fresh-minted-token
ENVEOF
fi
EOF
cat >"$tmp/bin/curl" <<'EOF'
#!/usr/bin/env bash
case "$*" in
  *'/api/healthz'*) exit 0 ;;
  *'/api/v1/user'*) [[ "$*" == *'Authorization: token fresh-minted-token'* ]] ;;
  *) exit 1 ;;
esac
EOF
chmod +x "$tmp/bin/curl"
chmod +x "$tmp/bin/systemctl"
cat >"$tmp/app.ini" <<'EOF'
APP_NAME = Stale Golden Gitea
[server]
PROTOCOL = https
HTTP_ADDR = 0.0.0.0
HTTP_PORT = 443
DOMAIN = github.com
ROOT_URL = https://github.com/
DISABLE_SSH = true
[database]
DB_TYPE = sqlite3
PATH = /var/lib/gitea/data/gitea.db
[security]
SECRET_KEY = preserve-me
INTERNAL_TOKEN = preserve-me-too
EOF
chmod 0640 "$tmp/app.ini"
cat >"$tmp/gitea.env" <<'EOF'
GITHUB_API_BASE_URL=https://github.com/api/v1
GITHUB_HOST=github.com
GITHUB_ALLOW_INSECURE_HOST=0
GITHUB_STATIC_TOKEN=pre-restart-token
EOF
GITEA_ENV_FILE="$tmp/gitea.env" SYSTEMCTL_LOG="$tmp/systemctl.log" PATH="$tmp/bin:$PATH" \
  "$gitea_reconcile" "$tmp/gitea.env" "$tmp/app.ini"
expected_app_ini="$tmp/expected-app.ini"
cat >"$expected_app_ini" <<'EOF'
APP_NAME = Stale Golden Gitea
[server]
PROTOCOL = http
HTTP_ADDR = 127.0.0.1
HTTP_PORT = 3000
DOMAIN = 127.0.0.1
ROOT_URL = http://127.0.0.1:3000/
DISABLE_SSH = true
[database]
DB_TYPE = sqlite3
PATH = /var/lib/gitea/data/gitea.db
[security]
SECRET_KEY = preserve-me
INTERNAL_TOKEN = preserve-me-too
EOF
cmp -s "$expected_app_ini" "$tmp/app.ini" || {
  echo "Gitea reconciler did not preserve secrets while restoring the HEAD-owned server topology" >&2
  exit 1
}
[[ "$(stat -c %a "$tmp/app.ini")" == 640 ]] || {
  echo "Gitea reconciler changed the app.ini access mode" >&2
  exit 1
}
printf '%s\n' \
  'stop spawnery-gitea-bootstrap.service' \
  'restart gitea' \
  'start spawnery-gitea-bootstrap.service' >"$tmp/expected-systemctl.log"
cmp -s "$tmp/expected-systemctl.log" "$tmp/systemctl.log" || {
  echo "Gitea reconciler did not serialize Gitea restart and bootstrap" >&2
  exit 1
}
expected_gitea_env="$tmp/expected-gitea.env"
cat >"$expected_gitea_env" <<'EOF'
GITHUB_API_BASE_URL=http://127.0.0.1:3000/api/v1
GITHUB_HOST=127.0.0.1:3000
GITHUB_ALLOW_INSECURE_HOST=1
GITHUB_STATIC_TOKEN=fresh-minted-token
EOF
cmp -s "$expected_gitea_env" "$tmp/gitea.env" || {
  echo "Gitea environment reconciler did not restore the HEAD-owned local topology" >&2
  exit 1
}
[[ "$(stat -c %a "$tmp/gitea.env")" == 600 ]] || {
  echo "Gitea environment reconciler did not keep the static token private" >&2
  exit 1
}
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
