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
fake_profile="$HERE/env/profile.fake.env"
for public_github_setting in \
  'GITHUB_API_BASE_URL=https://github.com/api/v1' \
  'GITHUB_HOST=github.com'
do
  [[ "$(rg -n "^${public_github_setting}$" "$fake_profile" | wc -l)" == 1 ]] || {
    echo "fake profile must contain exactly ${public_github_setting}" >&2
    exit 1
  }
done
if rg -n '^(GITHUB_STATIC_TOKEN|GITHUB_STATIC_TOKEN_FILE|GITHUB_ALLOW_INSECURE_HOST|CP_GITHUB_LINK_PREFLIGHT_DISABLED)=' "$fake_profile"; then
  echo "fake profile still selects the static GitHub lane or disables CP preflight" >&2
  exit 1
fi

fake_bootstrap_branch="$(sed -n '/^if \[\[ "\$PROFILE" == fake \]\]; then$/,/^fi$/p' "$runner")"
[[ "$(printf '%s\n' "$fake_bootstrap_branch" | rg -c '^  export ACC_BOOTSTRAP_FAKE_GITHUB_LINKS=1$')" == 1 ]] || {
  echo "run.sh does not enable fake GitHub link bootstrap only in the fake profile branch" >&2
  exit 1
}
printf '%s\n' "$fake_bootstrap_branch" | rg -q '^  unset ACC_BOOTSTRAP_FAKE_GITHUB_LINKS$' || {
  echo "run.sh does not clear fake GitHub link bootstrap for non-fake profiles" >&2
  exit 1
}
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
GITHUB_STATIC_TOKEN=fresh-minted-token
AS_FAKE_GITHUB_TOKEN=fresh-minted-token
ENVEOF
fi
EOF
cat >"$tmp/bin/curl" <<'EOF'
#!/usr/bin/env bash
case "$*" in
  *'http://127.0.0.1:3000/api/healthz'*) exit 0 ;;
  *'http://127.0.0.1:3000/api/v1/user'*) [[ "$*" == *'Authorization: token fresh-minted-token'* ]] ;;
  *) exit 1 ;;
esac
EOF
cat >"$tmp/bin/chown" <<'EOF'
#!/usr/bin/env bash
printf '%s\n' "$*" >>"$RECONCILE_CHOWN_LOG"
EOF
chmod +x "$tmp/bin/curl"
chmod +x "$tmp/bin/chown"
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
AS_FAKE_GITHUB_TOKEN=pre-restart-token
EOF
GITEA_ENV_FILE="$tmp/gitea.env" SYSTEMCTL_LOG="$tmp/systemctl.log" \
RECONCILE_CHOWN_LOG="$tmp/reconcile-chown.log" PATH="$tmp/bin:$PATH" \
  "$gitea_reconcile" "$tmp/gitea.env" "$tmp/app.ini"
expected_app_ini="$tmp/expected-app.ini"
cat >"$expected_app_ini" <<'EOF'
APP_NAME = Stale Golden Gitea
[server]
PROTOCOL = http
HTTP_ADDR = 127.0.0.1
HTTP_PORT = 3000
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
AS_FAKE_GITHUB_TOKEN=fresh-minted-token
EOF
cmp -s "$expected_gitea_env" "$tmp/gitea.env" || {
  echo "Gitea environment reconciler did not publish the AS-only token" >&2
  exit 1
}
[[ "$(stat -c %a "$tmp/gitea.env")" == 600 ]] || {
  echo "Gitea environment reconciler did not keep the AS token private" >&2
  exit 1
}
[[ "$(tail -n1 "$tmp/reconcile-chown.log")" == 'root:root '* ]] || {
  echo "Gitea environment reconciler did not assign root ownership" >&2
  exit 1
}
provision="$HERE/provision.sh"
mkdir -p "$tmp/bootstrap-bin"
cat >"$tmp/bootstrap-bin/chown" <<'EOF'
#!/usr/bin/env bash
printf '%s\n' "$*" >>"$BOOTSTRAP_CHOWN_LOG"
EOF
cat >"$tmp/bootstrap-bin/mktemp" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
[[ "$(umask)" == 0077 ]]
exec /usr/bin/mktemp "$@"
EOF
cat >"$tmp/bootstrap-bin/mv" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
src="${@: -2:1}"
dst="${@: -1}"
[[ "$(dirname "$src")" == "$(dirname "$dst")" ]]
[[ "$src" == "${dst}.tmp."* ]]
[[ "$(stat -c %a "$src")" == 600 ]]
[[ "$(cat "$dst")" == sentinel-before-publication ]]
printf '%s -> %s\n' "$src" "$dst" >>"$BOOTSTRAP_MV_LOG"
/bin/mv "$@"
EOF
chmod +x "$tmp/bootstrap-bin/chown" "$tmp/bootstrap-bin/mktemp" "$tmp/bootstrap-bin/mv"
bootstrap_env="$tmp/fresh-gitea.env"
printf '%s\n' sentinel-before-publication >"$bootstrap_env"
chmod 0644 "$bootstrap_env"
bootstrap_publication="$tmp/generated-bootstrap-publication.sh"
cat >"$bootstrap_publication" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
TOKEN=fresh-minted-token
EOF
sed -n '/^# BEGIN GITEA_ENV_PUBLICATION$/,/^# END GITEA_ENV_PUBLICATION$/p' "$provision" \
  | sed 's/\\\$/\$/g' >>"$bootstrap_publication"
chmod +x "$bootstrap_publication"
GITEA_ENV_FILE="$bootstrap_env" \
BOOTSTRAP_CHOWN_LOG="$tmp/bootstrap-chown.log" \
BOOTSTRAP_MV_LOG="$tmp/bootstrap-mv.log" \
PATH="$tmp/bootstrap-bin:$PATH" \
  "$bootstrap_publication"
cmp -s "$expected_gitea_env" "$bootstrap_env" || {
  echo "fresh Gitea bootstrap did not publish the exact secure environment" >&2
  exit 1
}
[[ "$(stat -c %a "$bootstrap_env")" == 600 ]] || {
  echo "fresh Gitea bootstrap did not keep the AS token private" >&2
  exit 1
}
[[ "$(cat "$tmp/bootstrap-chown.log")" == 'root:root '* ]] || {
  echo "fresh Gitea bootstrap did not assign root ownership" >&2
  exit 1
}
[[ "$(wc -l <"$tmp/bootstrap-mv.log")" == 1 ]] || {
  echo "fresh Gitea bootstrap did not use one atomic replacement" >&2
  exit 1
}
for secure_gitea_binding in \
  'DOMAIN = github.com' \
  'ROOT_URL = https://github.com/' \
  'AS_FAKE_GITHUB_TOKEN=\$TOKEN'
do
  rg -Fq "$secure_gitea_binding" "$provision" || {
    echo "fresh provisioning lacks secure Gitea binding: ${secure_gitea_binding}" >&2
    exit 1
  }
done
if rg -n '^[[:space:]]*(GITHUB_API_BASE_URL|GITHUB_HOST|GITHUB_STATIC_TOKEN|GITHUB_STATIC_TOKEN_FILE|GITHUB_ALLOW_INSECURE_HOST)=' "$provision"; then
  echo "fresh Gitea publication emits node GitHub settings or a static credential" >&2
  exit 1
fi

authsvc_unit="$(sed -n '/tee \/etc\/systemd\/system\/spawnery-authsvc.service/,/^EOF$/p' "$provision")"
cp_unit="$(sed -n '/tee \/etc\/systemd\/system\/spawnery-cp.service/,/^EOF$/p' "$provision")"
node_unit="$(sed -n '/tee \/etc\/systemd\/system\/spawnery-node.service/,/^EOF$/p' "$provision")"

# The node process needs host-loopback resolution for clone-in, but containerd must inherit the
# VM-global hosts file without those aliases so pod DNS can route GitHub traffic to the CNI gateway.
routing_failures=0
if rg -n '^[[:space:]]*printf .*github\.com.*tee -a /etc/hosts' "$provision"; then
  echo "fresh provisioning contaminates the VM-global hosts file with pod-loopback aliases" >&2
  routing_failures=$((routing_failures + 1))
fi
for private_hosts_binding in \
  'NODE_HOSTS=/etc/spawnery/node-hosts' \
  'sudo install -o root -g root -m0644 /etc/hosts "$NODE_HOSTS"' \
  'printf '\''127.0.0.1 github.com codeload.github.com\n'\'' | sudo tee -a "$NODE_HOSTS"'
do
  if ! rg -Fq "$private_hosts_binding" "$provision"; then
    echo "fresh provisioning lacks node-private hosts binding: ${private_hosts_binding}" >&2
    routing_failures=$((routing_failures + 1))
  fi
done
if [[ "$(printf '%s\n' "$node_unit" | rg -Fc 'BindReadOnlyPaths=/etc/spawnery/node-hosts:/etc/hosts')" != 1 ]]; then
  echo "fresh spawnlet unit does not receive exactly one read-only node-private hosts bind" >&2
  routing_failures=$((routing_failures + 1))
fi
printf '%s\n' "$node_unit" | rg -q '^ReadOnlyPaths=/etc/spawnery/node /etc/spawnery/node-hosts$' || {
  echo "fresh spawnlet unit does not protect the node-private hosts bind source from writes" >&2
  routing_failures=$((routing_failures + 1))
}
for global_hosts_unit in "$authsvc_unit" "$cp_unit"; do
  if printf '%s\n' "$global_hosts_unit" | rg -q '^BindReadOnlyPaths=.*:/etc/hosts$'; then
    echo "fresh non-node service unexpectedly receives the node-private hosts view" >&2
    routing_failures=$((routing_failures + 1))
  fi
done
for dnsmasq_record in \
  'host-record=github.com,${GITHUB_DNS_ADDR}' \
  'host-record=codeload.github.com,${GITHUB_DNS_ADDR}'
do
  if [[ "$(rg -Fc "$dnsmasq_record" "$provision")" != 1 ]]; then
    echo "fresh provisioning lacks exact dnsmasq gateway record: ${dnsmasq_record}" >&2
    routing_failures=$((routing_failures + 1))
  fi
done

roll="$REPO/scripts/e2e-vm/roll.sh"
provision_pod_dns_logic="$tmp/provision-pod-dns-reconciliation.sh"
roll_pod_dns_logic="$tmp/roll-pod-dns-reconciliation.sh"
sed -n '/^# BEGIN POD_DNS_RECONCILIATION$/,/^# END POD_DNS_RECONCILIATION$/p' \
  "$provision" >"$provision_pod_dns_logic"
sed -n '/^# BEGIN POD_DNS_RECONCILIATION$/,/^# END POD_DNS_RECONCILIATION$/p' \
  "$roll" >"$roll_pod_dns_logic"
pod_dns_logic_is_shared=1
if [[ ! -s "$provision_pod_dns_logic" || ! -s "$roll_pod_dns_logic" ]] \
  || ! cmp -s "$provision_pod_dns_logic" "$roll_pod_dns_logic"; then
  echo "fresh provision and live roll lack one asserted-identical POD_DNS reconciler" >&2
  pod_dns_logic_is_shared=0
fi
if ! (
  sudo() { "$@"; }
  if (( pod_dns_logic_is_shared )); then
    # shellcheck disable=SC1090
    source "$provision_pod_dns_logic"
  else
    # Model the current permissive rewrite so the rejection fixtures still execute during RED.
    reconcile_pod_dns_template() {
      local template="$1" gateway="$2"
      sed -i "s#^POD_DNS=.*#POD_DNS=${gateway}#" "$template"
    }
  fi

  fixture_failures=0
  printf '%s\n' 'AGENT_IMAGE=spawnery/agent:dev' >"$tmp/pod-dns-missing.env"
  if reconcile_pod_dns_template "$tmp/pod-dns-missing.env" 10.234.0.1 >/dev/null 2>&1; then
    echo "POD_DNS reconciler accepted a template with no POD_DNS assignment" >&2
    fixture_failures=$((fixture_failures + 1))
  fi

  printf '%s\n' 'POD_DNS=1.1.1.1' 'POD_DNS=8.8.8.8' >"$tmp/pod-dns-duplicate.env"
  if reconcile_pod_dns_template "$tmp/pod-dns-duplicate.env" 10.234.0.1 >/dev/null 2>&1; then
    echo "POD_DNS reconciler accepted duplicate POD_DNS assignments" >&2
    fixture_failures=$((fixture_failures + 1))
  fi

  printf '%s\n' 'POD_DNS=1.1.1.1,8.8.8.8' >"$tmp/pod-dns-valid.env"
  reconcile_pod_dns_template "$tmp/pod-dns-valid.env" 10.234.0.1
  [[ "$(grep -Fxc 'POD_DNS=10.234.0.1' "$tmp/pod-dns-valid.env" || true)" == 1 ]] || {
    echo "POD_DNS reconciler did not publish exactly the requested gateway" >&2
    fixture_failures=$((fixture_failures + 1))
  }

  (( fixture_failures == 0 && pod_dns_logic_is_shared == 1 ))
); then
  routing_failures=$((routing_failures + 1))
fi

provision_gateway_logic="$tmp/provision-ipv4-network-gateway.sh"
roll_gateway_logic="$tmp/roll-ipv4-network-gateway.sh"
sed -n '/^# BEGIN IPV4_NETWORK_GATEWAY$/,/^# END IPV4_NETWORK_GATEWAY$/p' \
  "$provision" >"$provision_gateway_logic"
sed -n '/^# BEGIN IPV4_NETWORK_GATEWAY$/,/^# END IPV4_NETWORK_GATEWAY$/p' \
  "$roll" >"$roll_gateway_logic"
gateway_logic_is_shared=1
if [[ ! -s "$provision_gateway_logic" || ! -s "$roll_gateway_logic" ]] \
  || ! cmp -s "$provision_gateway_logic" "$roll_gateway_logic"; then
  echo "fresh provision and live roll lack one asserted-identical IPv4 network+1 helper" >&2
  gateway_logic_is_shared=0
fi
if ! (
  if (( gateway_logic_is_shared )); then
    # shellcheck disable=SC1090
    source "$provision_gateway_logic"
  else
    ipv4_network_gateway() {
      printf '%s' "$1" | cut -d/ -f1 | awk -F. '{print $1"."$2"."$3".1"}'
    }
  fi

  gateway_fixture_failures=0
  for gateway_case in \
    '192.0.2.128/25=192.0.2.129' \
    '198.51.100.4/30=198.51.100.5'
  do
    cidr="${gateway_case%%=*}"
    expected_gateway="${gateway_case#*=}"
    actual_gateway="$(ipv4_network_gateway "$cidr")" || actual_gateway=
    [[ "$actual_gateway" == "$expected_gateway" ]] || {
      echo "IPv4 gateway helper returned ${actual_gateway:-failure} for $cidr, want $expected_gateway" >&2
      gateway_fixture_failures=$((gateway_fixture_failures + 1))
    }
  done
  for rejected_cidr in '10.0.999.0/24' '192.0.2.0/31'; do
    if ipv4_network_gateway "$rejected_cidr" >/dev/null 2>&1; then
      echo "IPv4 gateway helper accepted invalid/unsupported CIDR $rejected_cidr" >&2
      gateway_fixture_failures=$((gateway_fixture_failures + 1))
    fi
  done
  (( gateway_fixture_failures == 0 && gateway_logic_is_shared == 1 ))
); then
  routing_failures=$((routing_failures + 1))
fi

roll_hosts_logic="$tmp/roll-global-hosts-reconciliation.sh"
sed -n '/^# BEGIN GLOBAL_HOSTS_RECONCILIATION$/,/^# END GLOBAL_HOSTS_RECONCILIATION$/p' \
  "$roll" >"$roll_hosts_logic"
hosts_logic_exists=1
if [[ ! -s "$roll_hosts_logic" ]]; then
  echo "roll.sh lacks executable exact-token global hosts reconciliation" >&2
  hosts_logic_exists=0
fi
if ! (
  sudo() { "$@"; }
  if (( hosts_logic_exists )); then
    # shellcheck disable=SC1090
    source "$roll_hosts_logic"
  else
    write_clean_global_hosts() {
      cp "$1" "$2"
      sed -i -E -e "/^[[:space:]]*#/! s/(^|[[:space:]])github\.com([[:space:]]|$)/\\1\\2/g" \
        -e "/^[[:space:]]*#/! s/(^|[[:space:]])codeload\.github\.com([[:space:]]|$)/\\1\\2/g" "$2"
    }
    assert_no_global_github_aliases() { return 0; }
  fi

  cat >"$tmp/hosts-stale" <<'EOF'
127.0.0.1 localhost github.com github.com codeload.github.com # mixed aliases
127.0.0.2 github.com codeload.github.com
127.0.0.3 github.com # alias-only comment
10.0.0.2 git.example notgithub.com codeload.github.com.evil # keep unrelated
# github.com codeload.github.com full-line comment
EOF
  cat >"$tmp/hosts-expected" <<'EOF'
127.0.0.1 localhost # mixed aliases
# alias-only comment
10.0.0.2 git.example notgithub.com codeload.github.com.evil # keep unrelated
# github.com codeload.github.com full-line comment
EOF
  write_clean_global_hosts "$tmp/hosts-stale" "$tmp/hosts-clean"
  hosts_fixture_failures=0
  cmp -s "$tmp/hosts-expected" "$tmp/hosts-clean" || {
    echo "global hosts reconciliation did not preserve comments/unrelated hosts or remove every exact alias" >&2
    hosts_fixture_failures=$((hosts_fixture_failures + 1))
  }
  assert_no_global_github_aliases "$tmp/hosts-clean" || {
    echo "global hosts reconciliation postcondition rejected its cleaned output" >&2
    hosts_fixture_failures=$((hosts_fixture_failures + 1))
  }
  printf '%s\n' '127.0.0.1 github.com' >"$tmp/hosts-still-stale"
  if assert_no_global_github_aliases "$tmp/hosts-still-stale" >/dev/null 2>&1; then
    echo "global hosts postcondition accepted an active github.com hostname field" >&2
    hosts_fixture_failures=$((hosts_fixture_failures + 1))
  fi
  (( hosts_fixture_failures == 0 && hosts_logic_exists == 1 ))
); then
  routing_failures=$((routing_failures + 1))
fi

fresh_common_copy_line="$(rg -n 'cp -f .*common\.env /etc/spawnery/env.d/common\.env\.tmpl' "$provision" | head -n1 | cut -d: -f1 || true)"
fresh_pod_dns_line="$(rg -n 'reconcile_pod_dns_template /etc/spawnery/env.d/common\.env\.tmpl "\$GITHUB_DNS_ADDR"' "$provision" | head -n1 | cut -d: -f1 || true)"
if [[ -z "$fresh_common_copy_line" || -z "$fresh_pod_dns_line" || "$fresh_common_copy_line" -ge "$fresh_pod_dns_line" ]]; then
  echo "fresh provisioning does not patch POD_DNS to the dnsmasq gateway after copying common.env" >&2
  routing_failures=$((routing_failures + 1))
fi
fresh_dnsmasq_test_line="$(rg -n 'dnsmasq --test' "$provision" | head -n1 | cut -d: -f1 || true)"
fresh_dnsmasq_active_line="$(rg -n 'systemctl is-active --quiet dnsmasq' "$provision" | head -n1 | cut -d: -f1 || true)"
if [[ -z "$fresh_dnsmasq_test_line" || -z "$fresh_dnsmasq_active_line" \
   || "$fresh_dnsmasq_test_line" -ge "$fresh_pod_dns_line" || "$fresh_dnsmasq_active_line" -ge "$fresh_pod_dns_line" ]]; then
  echo "fresh provisioning does not validate active dnsmasq before publishing POD_DNS" >&2
  routing_failures=$((routing_failures + 1))
fi

for roll_hosts_binding in \
  'NODE_HOSTS=/etc/spawnery/node-hosts' \
  'BindReadOnlyPaths=/etc/spawnery/node-hosts:/etc/hosts' \
  'ReadOnlyPaths=/etc/spawnery/node-hosts'
do
  if ! rg -Fq "$roll_hosts_binding" "$roll"; then
    echo "roll.sh lacks node-private hosts reconciliation: ${roll_hosts_binding}" >&2
    routing_failures=$((routing_failures + 1))
  fi
done
if ! rg -q 'printf [^|]*127\.0\.0\.1 github\.com codeload\.github\.com\\n[^|]* \| sudo tee -a "\$NODE_HOSTS_STAGE"' "$roll"; then
  echo "roll.sh does not publish both loopback aliases to the node-private hosts view" >&2
  routing_failures=$((routing_failures + 1))
fi
for gateway_contract in \
  '/etc/cni/net.d/10-spawnery.conflist' \
  '/etc/dnsmasq.d/spawnery-github.conf' \
  'reconcile_pod_dns_template /etc/spawnery/env.d/common.env.tmpl "$CNI_GATEWAY"'
do
  if ! rg -Fq "$gateway_contract" "$roll"; then
    echo "roll.sh lacks fail-closed CNI/dnsmasq gateway reconciliation: ${gateway_contract}" >&2
    routing_failures=$((routing_failures + 1))
  fi
done
roll_common_copy_line="$(rg -n 'cp -f .*common\.env /etc/spawnery/env.d/common\.env\.tmpl' "$roll" | head -n1 | cut -d: -f1 || true)"
roll_pod_dns_line="$(rg -n 'reconcile_pod_dns_template /etc/spawnery/env.d/common\.env\.tmpl "\$CNI_GATEWAY"' "$roll" | head -n1 | cut -d: -f1 || true)"
roll_render_line="$(rg -n 'systemctl restart spawnery-render-env' "$roll" | head -n1 | cut -d: -f1 || true)"
if [[ -z "$roll_common_copy_line" || -z "$roll_pod_dns_line" || -z "$roll_render_line" \
   || "$roll_common_copy_line" -ge "$roll_pod_dns_line" || "$roll_pod_dns_line" -ge "$roll_render_line" ]]; then
  echo "roll.sh does not patch POD_DNS after template copy and before rendering" >&2
  routing_failures=$((routing_failures + 1))
fi

roll_node_hosts_line="$(rg -n 'mv -f "\$NODE_HOSTS_STAGE" "\$NODE_HOSTS"' "$roll" | head -n1 | cut -d: -f1 || true)"
roll_dropin_line="$(rg -n 'spawnery-node.service.d/90-github-secret-fence.conf' "$roll" | head -n1 | cut -d: -f1 || true)"
roll_daemon_reload_line="$(rg -n 'systemctl daemon-reload' "$roll" | head -n1 | cut -d: -f1 || true)"
roll_dropin_validate_line="$(rg -n 'systemctl cat spawnery-node' "$roll" | tail -n1 | cut -d: -f1 || true)"
roll_dnsmasq_test_line="$(rg -n 'dnsmasq --test' "$roll" | head -n1 | cut -d: -f1 || true)"
roll_dnsmasq_restart_line="$(rg -n 'systemctl restart dnsmasq' "$roll" | head -n1 | cut -d: -f1 || true)"
roll_dnsmasq_active_line="$(rg -n 'systemctl is-active --quiet dnsmasq' "$roll" | head -n1 | cut -d: -f1 || true)"
roll_node_stop_line="$(rg -n 'systemctl stop spawnery-node' "$roll" | head -n1 | cut -d: -f1 || true)"
roll_global_hosts_line="$(rg -n 'mv -f "\$HOSTS_STAGE" /etc/hosts' "$roll" | head -n1 | cut -d: -f1 || true)"
for staged_line in "$roll_node_hosts_line" "$roll_dropin_line" "$roll_daemon_reload_line" "$roll_dropin_validate_line" \
  "$roll_dnsmasq_test_line" "$roll_dnsmasq_restart_line" "$roll_dnsmasq_active_line" \
  "$roll_pod_dns_line" "$roll_render_line"
do
  if [[ -z "$staged_line" || -z "$roll_node_stop_line" || "$staged_line" -ge "$roll_node_stop_line" ]]; then
    echo "roll.sh does not finish all routing validation/staging before stopping spawnery-node" >&2
    routing_failures=$((routing_failures + 1))
    break
  fi
done
if [[ -z "$roll_global_hosts_line" || -z "$roll_node_stop_line" \
   || "$roll_node_stop_line" -ge "$roll_global_hosts_line" ]]; then
  echo "roll.sh does not stop spawnery-node immediately before atomically installing clean global hosts" >&2
  routing_failures=$((routing_failures + 1))
fi
for live_dns_contract in \
  'ip link show spawnery-cni0' \
  'ss -H -lun' \
  '/dev/udp/${CNI_GATEWAY}/53' \
  'tail -c 4'
do
  rg -Fq "$live_dns_contract" "$roll" || {
    echo "roll.sh lacks live dnsmasq bridge proof: ${live_dns_contract}" >&2
    routing_failures=$((routing_failures + 1))
  }
done
post_stop_block="$(sed -n "${roll_node_stop_line:-1},${roll_global_hosts_line:-1}p" "$roll")"
if printf '%s\n' "$post_stop_block" | rg -q 'systemctl (start|restart) spawnery-node'; then
  echo "roll.sh restarts spawnery-node from the post-stop failure path" >&2
  routing_failures=$((routing_failures + 1))
fi
rg -Fq "&& sudo systemctl start spawnery-node'" "$roll" || {
  echo "roll.sh does not make spawnery-node the final fallible restart step" >&2
  routing_failures=$((routing_failures + 1))
}
if (( routing_failures != 0 )); then
  echo "GitHub proxy routing topology has ${routing_failures} contract violation(s)" >&2
  exit 1
fi

[[ "$(printf '%s\n' "$authsvc_unit" | rg -c '^EnvironmentFile=-/etc/spawnery/env.d/gitea.env$')" == 1 ]] || {
  echo "fresh authsvc unit does not consume the private Gitea environment exactly once" >&2
  exit 1
}
if printf '%s\n' "$node_unit" | rg -q '^EnvironmentFile=.*gitea.env$'; then
  echo "fresh spawnlet unit still consumes the private Gitea environment" >&2
  exit 1
fi
printf '%s\n' "$node_unit" | rg -q '^UnsetEnvironment=GITHUB_STATIC_TOKEN GITHUB_STATIC_TOKEN_FILE AS_FAKE_GITHUB_TOKEN$' || {
  echo "fresh spawnlet unit lacks the GitHub secret environment fence" >&2
  exit 1
}
inaccessible_path_tokens() {
  local unit_text="$1" line rhs token
  local -a tokens=()
  while IFS= read -r line; do
    [[ "$line" == InaccessiblePaths=* ]] || continue
    rhs="${line#InaccessiblePaths=}"
    read -r -a tokens <<<"$rhs"
    for token in "${tokens[@]}"; do
      printf '%s\n' "$token"
    done
  done <<<"$unit_text"
}

unit_has_inaccessible_path() {
  inaccessible_path_tokens "$1" | rg -Fqx -- "$2"
}

for first_fence_token in \
  /etc/spawnery/env.d \
  -/etc/spawnery/env.d \
  /etc/spawnery/env.d/gitea.env \
  -/etc/spawnery/env.d/gitea.env
do
  first_token_fixture="InaccessiblePaths=${first_fence_token} /unrelated"
  unit_has_inaccessible_path "$first_token_fixture" "$first_fence_token" || {
    echo "InaccessiblePaths test matcher misses first RHS token ${first_fence_token}" >&2
    exit 1
  }
done
for forbidden_authsvc_fence in \
  /etc/spawnery/env.d \
  -/etc/spawnery/env.d \
  /etc/spawnery/env.d/gitea.env \
  -/etc/spawnery/env.d/gitea.env
do
  if unit_has_inaccessible_path "$authsvc_unit" "$forbidden_authsvc_fence"; then
    echo "fresh authsvc unit cannot read its private fake-provider token" >&2
    exit 1
  fi
done
for private_unit in cp node; do
  case "$private_unit" in
    cp) unit_text="$cp_unit" ;;
    node) unit_text="$node_unit" ;;
  esac
  unit_has_inaccessible_path "$unit_text" /etc/spawnery/env.d || {
    echo "fresh ${private_unit} unit lacks the stable authsvc environment-directory fence" >&2
    exit 1
  }
  for weak_fence in \
    -/etc/spawnery/env.d \
    /etc/spawnery/env.d/gitea.env \
    -/etc/spawnery/env.d/gitea.env
  do
    if unit_has_inaccessible_path "$unit_text" "$weak_fence"; then
      echo "fresh ${private_unit} unit uses an optional or exact-file custody fence" >&2
      exit 1
    fi
  done
  printf '%s\n' "$unit_text" | rg -q '^CapabilityBoundingSet=~CAP_SYS_ADMIN CAP_SYS_PTRACE$' || {
    echo "fresh ${private_unit} unit lost the CAP_SYS_ADMIN denial required by the file sandbox" >&2
    exit 1
  }
done

rg -Fq '/etc/systemd/system/spawnery-node.service.d/90-github-secret-fence.conf' "$roll" || {
  echo "roll.sh does not install the spawnlet GitHub secret fence drop-in" >&2
  exit 1
}
rg -Fq 'UnsetEnvironment=GITHUB_STATIC_TOKEN GITHUB_STATIC_TOKEN_FILE AS_FAKE_GITHUB_TOKEN' "$roll" || {
  echo "roll.sh drop-in does not clear every legacy broad Gitea credential" >&2
  exit 1
}
rg -Fq '/etc/systemd/system/spawnery-cp.service.d/90-gitea-custody-fence.conf' "$roll" || {
  echo "roll.sh does not install the CP Gitea custody fence drop-in" >&2
  exit 1
}
[[ "$(rg -Foc 'InaccessiblePaths=/etc/spawnery/env.d' "$roll")" == 2 ]] || {
  echo "roll.sh must hide the stable environment directory from both CP and spawnlet" >&2
  exit 1
}
if rg -q 'InaccessiblePaths=-/etc/spawnery/env.d|InaccessiblePaths=-?/etc/spawnery/env.d/gitea\.env' "$roll"; then
  echo "roll.sh uses a fail-open optional or exact-file custody fence" >&2
  exit 1
fi
daemon_reload_line="$(rg -n 'systemctl daemon-reload' "$roll" | head -n1 | cut -d: -f1)"
node_start_line="$(rg -n 'systemctl start spawnery-node' "$roll" | head -n1 | cut -d: -f1)"
[[ -n "$daemon_reload_line" && -n "$node_start_line" && "$daemon_reload_line" -lt "$node_start_line" ]] || {
  echo "roll.sh does not daemon-reload the node fence before restarting spawnlet" >&2
  exit 1
}
for custody_dropin in \
  '/etc/systemd/system/spawnery-cp.service.d/90-gitea-custody-fence.conf' \
  '/etc/systemd/system/spawnery-node.service.d/90-github-secret-fence.conf'
do
  dropin_line="$(rg -n -F "$custody_dropin" "$roll" | head -n1 | cut -d: -f1)"
  [[ -n "$dropin_line" && "$dropin_line" -lt "$daemon_reload_line" ]] || {
    echo "roll.sh installs ${custody_dropin} too late for the effective service merge" >&2
    exit 1
  }
done
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
