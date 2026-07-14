#!/usr/bin/env bash
# roll.sh — copy the FRESH branch code into a booted VM, restart services, wait app-ready.
# "Fresh code" is ALL first-party artifacts the branch can change (roast finding): the host Go
# binaries AND the sidecar/agent container images AND the web bundle AND the config/ tree —
# not just bin/. Assumes STAGE holds them (populated by run.sh step 0).
#
# Usage: E2E_RUNID=... STAGE=/path/to/staging ./roll.sh
set -euo pipefail
source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"
: "${STAGE:?STAGE (staging dir with bin/ images/ web-dist/ config/) required}"
_need_runid
RD="$(run_dir)"; [ -f "$RD/acc.env" ] || die "no $RD/acc.env — run up.sh first"
# shellcheck disable=SC1091
source "$RD/acc.env"
IP="$E2E_VM_IP"; HOST="$E2E_VM_HOST"

log "rolling fresh code into $IP …"

# 1. host Go binaries -> /usr/local/bin (staging area first, then atomic move via ssh)
vm_ssh "$IP" 'mkdir -p ~/incoming/bin ~/incoming/config ~/incoming/provision/env'
vm_scp "$STAGE/bin/." "$IP" 'incoming/bin/'
[ -d "$STAGE/config" ] && vm_scp "$STAGE/config/." "$IP" 'incoming/config/'
[ -d "$STAGE/provision" ] && vm_scp "$STAGE/provision/." "$IP" 'incoming/provision/'

# 2. fresh sidecar/agent images -> containerd k8s.io namespace (product code lives inside pods)
if [ -f "$STAGE/images.tar" ]; then
  vm_scp "$STAGE/images.tar" "$IP" 'incoming/images.tar'
  vm_ssh "$IP" 'sudo ctr -n k8s.io images import ~/incoming/images.tar'
fi

# 3. fresh web bundle -> Caddy web root
if [ -d "$STAGE/web-dist" ]; then
  vm_ssh "$IP" 'mkdir -p ~/incoming/web'
  vm_scp "$STAGE/web-dist/." "$IP" 'incoming/web/'
fi

# 4. Reconcile the branch's PKI/env topology, then atomically swap + restart the stack.
# Preserve the golden Caddy certificate in its isolated directory while rotating the internal root
# and rebuilding every workload-specific runtime bundle from the branch's current ceremony tool.
vm_ssh "$IP" 'sudo install -m0755 ~/incoming/bin/spawnery-ca /usr/local/bin/spawnery-ca \
  && sudo install -d -m0750 -o root -g caddy /etc/spawnery/caddy \
  && ( if sudo test -f /etc/spawnery/pki/wildcard.crt && sudo test -f /etc/spawnery/pki/wildcard.key; then sudo install -m0644 -o root -g caddy /etc/spawnery/pki/wildcard.crt /etc/spawnery/caddy/wildcard.crt && sudo install -m0640 -o root -g caddy /etc/spawnery/pki/wildcard.key /etc/spawnery/caddy/wildcard.key; fi ) \
  && ( if sudo test -f /etc/spawnery/pki/github.crt && sudo test -f /etc/spawnery/pki/github.key; then sudo install -m0644 -o root -g caddy /etc/spawnery/pki/github.crt /etc/spawnery/caddy/github.crt && sudo install -m0640 -o root -g caddy /etc/spawnery/pki/github.key /etc/spawnery/caddy/github.key; fi ) \
  && printf '\''%s\n'\'' '\'':443 {'\'' '\''  tls /etc/spawnery/caddy/wildcard.crt /etc/spawnery/caddy/wildcard.key'\'' '\''  @cp path /cp.v1.*'\'' '\''  @ws path /ws*'\'' '\''  @as path /oauth* /refresh* /logout* /github* /device* /ca/* /enrollment-tokens'\'' '\''  handle @cp {'\'' '\''    reverse_proxy h2c://127.0.0.1:8080'\'' '\''  }'\'' '\''  handle @ws {'\'' '\''    reverse_proxy 127.0.0.1:8080'\'' '\''  }'\'' '\''  handle @as {'\'' '\''    reverse_proxy 127.0.0.1:8090'\'' '\''  }'\'' '\''  handle {'\'' '\''    root * /var/www/spawnery'\'' '\''    try_files {path} /index.html'\'' '\''    file_server'\'' '\''  }'\'' '\''}'\'' '\''github.com, codeload.github.com {'\'' '\''  tls /etc/spawnery/caddy/github.crt /etc/spawnery/caddy/github.key'\'' '\''  reverse_proxy 127.0.0.1:3000'\'' '\''}'\'' | sudo tee /etc/caddy/Caddyfile >/dev/null \
  && sudo rm -rf /etc/spawnery/pki /etc/spawnery/authsvc /etc/spawnery/cp /etc/spawnery/node \
  && sudo install -d -m0700 /etc/spawnery/pki /etc/spawnery/authsvc /etc/spawnery/cp /etc/spawnery/node /var/lib/spawnery-offline \
  && sudo env SPAWNERY_OFFLINE_PKI_DIR=/var/lib/spawnery-offline bash ~/incoming/provision/gen-pki.sh /etc/spawnery/pki e2e.test \
  && sudo cp -rf /etc/spawnery/pki/authsvc/. /etc/spawnery/authsvc/ \
  && sudo cp -rf /etc/spawnery/pki/cp/. /etc/spawnery/cp/ \
  && sudo cp -f /etc/spawnery/pki/node-cloud/{cert.pem,chain.pem,key.pem,root.pem} /etc/spawnery/node/ \
  && sudo cp -f /etc/spawnery/pki/{service-intermediate.pem,cloud-intermediate.pem,self-hosted-intermediate.pem,service.crl.pem,cloud-node.crl.pem,self-hosted-node.crl.pem} /etc/spawnery/node/ \
  && sudo find /etc/spawnery/authsvc /etc/spawnery/cp /etc/spawnery/node -type f -exec chmod 0600 {} + \
  && sudo find /etc/spawnery/pki -mindepth 1 -delete \
  && sudo rm -rf /var/lib/spawnery/authsvc-revocations /var/lib/spawnery/cp-revocations /var/lib/spawnery/cp-signer-revocations /var/lib/spawnlet/certificate-revocations /var/lib/spawnlet/signer-revocations /var/lib/spawnlet/user-revocations \
  && sudo install -d -m0700 /var/lib/spawnery/authsvc-revocations /var/lib/spawnery/cp-revocations /var/lib/spawnery/cp-signer-revocations /var/lib/spawnlet/certificate-revocations /var/lib/spawnlet/signer-revocations /var/lib/spawnlet/user-revocations \
  && sudo cp -f /etc/spawnery/authsvc/self-hosted-node.crl.pem /var/lib/spawnery/authsvc-revocations/self-hosted-node.crl.pem \
  && sudo bash ~/incoming/provision/reconcile-gitea-env.sh /etc/spawnery/env.d/gitea.env'

# Older goldens put GitHub aliases in the VM-global hosts file. Stage and validate a clean global
# view plus spawnlet's private loopback view while the old node still resolves locally, then stop the
# node before the atomic global swap. containerd subsequently copies only the clean view into pods.
vm_ssh "$IP" '
set -euo pipefail

# BEGIN POD_DNS_RECONCILIATION
reconcile_pod_dns_template() {
  local template="$1" gateway="$2" assignments
  assignments="$(sudo grep -c '^POD_DNS=' "$template" || true)"
  [[ "$assignments" == 1 ]] || {
    echo "expected exactly one POD_DNS assignment in $template, found ${assignments:-0}" >&2
    return 1
  }
  sudo sed -i "s#^POD_DNS=.*#POD_DNS=${gateway}#" "$template"
  [[ "$(sudo grep -Fxc "POD_DNS=${gateway}" "$template" || true)" == 1 ]] || {
    echo "failed to publish exactly POD_DNS=${gateway} in $template" >&2
    return 1
  }
}
# END POD_DNS_RECONCILIATION

# BEGIN IPV4_NETWORK_GATEWAY
ipv4_network_gateway() {
  local cidr="$1" address prefix o1 o2 o3 o4 address_int mask gateway_int
  [[ "$cidr" =~ ^([0-9]{1,3}\.){3}[0-9]{1,3}/[0-9]{1,2}$ ]] || return 1
  address="${cidr%/*}"
  prefix="${cidr#*/}"
  IFS=. read -r o1 o2 o3 o4 <<<"$address"
  for octet in "$o1" "$o2" "$o3" "$o4"; do
    [[ "$octet" =~ ^[0-9]+$ ]] && (( 10#$octet <= 255 )) || return 1
  done
  (( 10#$prefix >= 1 && 10#$prefix <= 30 )) || return 1
  o1=$((10#$o1)); o2=$((10#$o2)); o3=$((10#$o3)); o4=$((10#$o4)); prefix=$((10#$prefix))
  address_int=$(( (o1 << 24) | (o2 << 16) | (o3 << 8) | o4 ))
  mask=$(( (0xffffffff << (32 - prefix)) & 0xffffffff ))
  gateway_int=$(( (address_int & mask) + 1 ))
  printf '%d.%d.%d.%d\n' \
    "$(( (gateway_int >> 24) & 255 ))" "$(( (gateway_int >> 16) & 255 ))" \
    "$(( (gateway_int >> 8) & 255 ))" "$(( gateway_int & 255 ))"
}
# END IPV4_NETWORK_GATEWAY

# BEGIN GLOBAL_HOSTS_RECONCILIATION
assert_no_global_github_aliases() {
  local path="$1" line active host
  local -a fields
  sudo cat "$path" | while IFS= read -r line || [[ -n "$line" ]]; do
    [[ "$line" =~ ^[[:space:]]*# ]] && continue
    active="${line%%#*}"
    read -r -a fields <<<"$active"
    for host in "${fields[@]:1}"; do
      [[ "$host" != github.com && "$host" != codeload.github.com ]] || exit 1
    done
  done
}

write_clean_global_hosts() {
  local source="$1" destination="$2" line active comment address host removed
  local -a fields kept_hosts
  sudo cat "$source" | while IFS= read -r line || [[ -n "$line" ]]; do
    if [[ "$line" =~ ^[[:space:]]*# ]]; then
      printf "%s\n" "$line"
      continue
    fi
    active="${line%%#*}"
    comment=
    [[ "$line" == *#* ]] && comment="#${line#*#}"
    read -r -a fields <<<"$active"
    if (( ${#fields[@]} == 0 )); then
      printf "%s\n" "$line"
      continue
    fi
    address="${fields[0]}"
    kept_hosts=()
    removed=0
    for host in "${fields[@]:1}"; do
      if [[ "$host" == github.com || "$host" == codeload.github.com ]]; then
        removed=1
      else
        kept_hosts+=("$host")
      fi
    done
    if (( removed == 0 )); then
      printf "%s\n" "$line"
    elif (( ${#kept_hosts[@]} == 0 )); then
      [[ -z "$comment" ]] || printf "%s\n" "$comment"
    else
      printf "%s" "$address"
      printf " %s" "${kept_hosts[@]}"
      [[ -z "$comment" ]] || printf " %s" "$comment"
      printf "\n"
    fi
  done | sudo tee "$destination" >/dev/null
  assert_no_global_github_aliases "$destination"
}
# END GLOBAL_HOSTS_RECONCILIATION

NODE_HOSTS=/etc/spawnery/node-hosts
HOSTS_STAGE="$(sudo mktemp /etc/hosts.spawnery.XXXXXX)"
NODE_HOSTS_STAGE="$(sudo mktemp /etc/spawnery/node-hosts.XXXXXX)"
DNS_RESPONSE_STAGE=
cleanup_routing_staging() {
  [[ -z "$HOSTS_STAGE" ]] || sudo rm -f "$HOSTS_STAGE"
  [[ -z "$NODE_HOSTS_STAGE" ]] || sudo rm -f "$NODE_HOSTS_STAGE"
  [[ -z "$DNS_RESPONSE_STAGE" ]] || rm -f "$DNS_RESPONSE_STAGE"
}
trap cleanup_routing_staging EXIT

write_clean_global_hosts /etc/hosts "$HOSTS_STAGE"
sudo cp --attributes-only --preserve=all /etc/hosts "$HOSTS_STAGE"
sudo cp "$HOSTS_STAGE" "$NODE_HOSTS_STAGE"
printf "127.0.0.1 github.com codeload.github.com\n" | sudo tee -a "$NODE_HOSTS_STAGE" >/dev/null
sudo chown root:root "$NODE_HOSTS_STAGE"
sudo chmod 0644 "$NODE_HOSTS_STAGE"
[[ "$(sudo grep -Fxc "127.0.0.1 github.com codeload.github.com" "$NODE_HOSTS_STAGE" || true)" == 1 ]] || {
  echo "node-private hosts staging lacks the exact GitHub loopback aliases" >&2
  exit 1
}
sudo mv -f "$NODE_HOSTS_STAGE" "$NODE_HOSTS"
NODE_HOSTS_STAGE=

sudo cp -f ~/incoming/provision/env/common.env /etc/spawnery/env.d/common.env.tmpl
sudo cp -f ~/incoming/provision/env/profile.*.env /etc/spawnery/env.d/
sudo sh -c "for f in /etc/spawnery/env.d/profile.*.env; do mv -f \"\$f\" \"\$f.tmpl\"; done"

# Recompute the host-local gateway from the installed CNI subnet rather than carrying a second
# address constant. The bridge is created lazily by CNI; when present, validate its address and make
# a real DNS query. The conflist, dnsmasq config/active service, and exact records are always required.
CNI_SUBNET="$(sudo jq -er ".plugins[] | select(.type == \"bridge\" and .bridge == \"spawnery-cni0\") | .ipam.subnet" /etc/cni/net.d/10-spawnery.conflist)"
CNI_PREFIX="${CNI_SUBNET#*/}"
CNI_GATEWAY="$(ipv4_network_gateway "$CNI_SUBNET")" || {
  echo "invalid or unsupported spawnery CNI subnet: $CNI_SUBNET" >&2
  exit 1
}

DNSMASQ_CONF=/etc/dnsmasq.d/spawnery-github.conf
[[ "$(sudo grep -Fxc "interface=spawnery-cni0" "$DNSMASQ_CONF" || true)" == 1 ]] || {
  echo "spawnery dnsmasq bridge binding is missing or ambiguous" >&2
  exit 1
}
[[ "$(sudo grep -Fxc "host-record=github.com,$CNI_GATEWAY" "$DNSMASQ_CONF" || true)" == 1 ]] || {
  echo "github.com dnsmasq record does not match CNI gateway $CNI_GATEWAY" >&2
  exit 1
}
[[ "$(sudo grep -Fxc "host-record=codeload.github.com,$CNI_GATEWAY" "$DNSMASQ_CONF" || true)" == 1 ]] || {
  echo "codeload.github.com dnsmasq record does not match CNI gateway $CNI_GATEWAY" >&2
  exit 1
}
sudo dnsmasq --test
sudo systemctl restart dnsmasq
sudo systemctl is-active --quiet dnsmasq
if ip link show spawnery-cni0 >/dev/null 2>&1; then
  ip -4 -o addr show dev spawnery-cni0 | awk "{print \$4}" | grep -Fqx "$CNI_GATEWAY/$CNI_PREFIX" || {
    echo "live spawnery CNI bridge does not own $CNI_GATEWAY/$CNI_PREFIX" >&2
    exit 1
  }
  sudo ss -H -lun | awk "{print \$4}" | grep -Fqx "$CNI_GATEWAY:53" || {
    echo "dnsmasq is not listening on $CNI_GATEWAY:53" >&2
    exit 1
  }
  # Older goldens do not carry bind-utils; Bash UDP sockets plus coreutils prove the live answer.
  DNS_RESPONSE_STAGE="$(mktemp)"
  exec 3<>"/dev/udp/${CNI_GATEWAY}/53"
  printf "\x53\x50\x01\x00\x00\x01\x00\x00\x00\x00\x00\x00\x06github\x03com\x00\x00\x01\x00\x01" >&3
  timeout 3 dd bs=512 count=1 <&3 >"$DNS_RESPONSE_STAGE" 2>/dev/null
  exec 3>&- 3<&-
  [[ "$(od -An -tx1 -N2 "$DNS_RESPONSE_STAGE" | tr -d " \n")" == 5350 ]] || {
    echo "dnsmasq returned an invalid transaction ID for github.com" >&2
    exit 1
  }
  read -r DNS_FLAGS1 DNS_FLAGS2 < <(od -An -tu1 -j2 -N2 "$DNS_RESPONSE_STAGE")
  (( (DNS_FLAGS1 & 128) != 0 && (DNS_FLAGS2 & 15) == 0 )) || {
    echo "dnsmasq returned a non-response or error for github.com" >&2
    exit 1
  }
  read -r DNS_A DNS_B DNS_C DNS_D < <(tail -c 4 "$DNS_RESPONSE_STAGE" | od -An -tu1)
  [[ "$DNS_A.$DNS_B.$DNS_C.$DNS_D" == "$CNI_GATEWAY" ]] || {
    echo "dnsmasq did not answer github.com with $CNI_GATEWAY" >&2
    exit 1
  }
  rm -f "$DNS_RESPONSE_STAGE"
  DNS_RESPONSE_STAGE=
fi
reconcile_pod_dns_template /etc/spawnery/env.d/common.env.tmpl "$CNI_GATEWAY"

sudo install -d -m0755 /etc/systemd/system/spawnery-cp.service.d /etc/systemd/system/spawnery-node.service.d
printf "%s\n" "[Service]" "InaccessiblePaths=/etc/spawnery/env.d" | sudo tee /etc/systemd/system/spawnery-cp.service.d/90-gitea-custody-fence.conf >/dev/null
printf "%s\n" "[Service]" "UnsetEnvironment=GITHUB_STATIC_TOKEN GITHUB_STATIC_TOKEN_FILE AS_FAKE_GITHUB_TOKEN" "InaccessiblePaths=/etc/spawnery/env.d" "ReadOnlyPaths=/etc/spawnery/node-hosts" "BindReadOnlyPaths=/etc/spawnery/node-hosts:/etc/hosts" | sudo tee /etc/systemd/system/spawnery-node.service.d/90-github-secret-fence.conf >/dev/null
sudo systemctl daemon-reload
sudo systemctl cat spawnery-node | grep -Fqx "ReadOnlyPaths=/etc/spawnery/node-hosts"
sudo systemctl cat spawnery-node | grep -Fqx "BindReadOnlyPaths=/etc/spawnery/node-hosts:/etc/hosts"
sudo systemctl restart spawnery-render-env
sudo systemctl stop spawnery-node
sudo mv -f "$HOSTS_STAGE" /etc/hosts
HOSTS_STAGE=
cleanup_routing_staging
trap - EXIT
'

# 5. Atomic swap + restart the stack. Start node last: every earlier failure leaves the node stopped,
# and there is no fallible command after it joins the clean-global/private-node routing topology.
# Re-copying config/ re-introduces cp.prod.yaml's ${sops:store.dsn} ref (pristine config ships it),
# so re-patch the literal throwaway DSN right after the config copy, every roll.
vm_ssh "$IP" 'sudo install -m0755 ~/incoming/bin/* /usr/local/bin/ \
  && ( [ -d ~/incoming/config ] && sudo cp -rf ~/incoming/config/* /etc/spawnery/config/ || true ) \
  && ( [ -f /etc/spawnery/config/cp.prod.yaml ] && sudo sed -i '"'"'s#\${sops:store.dsn}#postgres://spawnery:spawnery@127.0.0.1:5432/spawnery?sslmode=disable#'"'"' /etc/spawnery/config/cp.prod.yaml || true ) \
  && ( [ -d ~/incoming/web ] && sudo rsync -a --delete ~/incoming/web/ /var/www/spawnery/ || true ) \
  && sudo systemctl restart spawnery-authsvc \
  && sudo systemctl restart spawnery-cp \
  && sudo systemctl restart caddy \
  && sudo systemctl start spawnery-node'

# ---- wait app-ready — gate ALL the pieces (roast gap), not just AS /healthz ----
# AS and CP bind 127.0.0.1 (Caddy fronts them on :443), so probe localhost INSIDE the VM over ssh —
# a wait_tcp on the external IP only ever sees Caddy's :443, never 8090/8080.
log "waiting for app-ready …"
# (a) Public AS /healthz remains loopback HTTP behind Caddy. Internal service and node traffic uses the
# separate mTLS listener on 8091.
for i in $(seq 1 60); do
  vm_ssh "$IP" 'curl -fsS --max-time 3 http://127.0.0.1:8090/healthz >/dev/null 2>&1' && break
  [ "$i" = 60 ] && die "AS /healthz not ready"
  sleep 1
done
# (b) CP listening on 127.0.0.1:8080 (prod auth returns Unauthenticated to anon calls — this is a
#     liveness gate, not a 2xx check; node-list below proves it actually serves).
for i in $(seq 1 60); do
  vm_ssh "$IP" 'ss -ltn 2>/dev/null | grep -q 127.0.0.1:8080' && break
  [ "$i" = 60 ] && die "CP :8080 not listening"
  sleep 1
done
# (c) node re-registered with the CP over enforced mTLS — spawnctl has no node-list verb, so check the
#     CP journal: it logs `msg="node connected" id=node-1 ...` on a successful mTLS registration.
for i in $(seq 1 60); do
  if vm_ssh "$IP" 'sudo journalctl -u spawnery-cp --no-pager 2>/dev/null | grep -q "node connected"'; then break; fi
  [ "$i" = 60 ] && warn "node never registered (no 'node connected' in the CP journal — mTLS?) — tests will surface it"
  sleep 2
done
# (d) Caddy TLS + web serving
wait_tcp "$IP" 443 60 || die "Caddy :443 not listening"
curl -fsS --resolve "$HOST:443:$IP" "https://$HOST/" >/dev/null 2>&1 || warn "web root not 200 yet (CA trust / static build?)"

log "app-ready on https://$HOST"
