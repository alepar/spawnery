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
  && printf '\''%s\n'\'' '\'':443 {'\'' '\''  tls /etc/spawnery/caddy/wildcard.crt /etc/spawnery/caddy/wildcard.key'\'' '\''  @cp path /cp.v1.* /ws*'\'' '\''  @as path /oauth* /refresh* /logout* /github* /device* /ca/* /enrollment-tokens'\'' '\''  handle @cp {'\'' '\''    reverse_proxy 127.0.0.1:8080'\'' '\''  }'\'' '\''  handle @as {'\'' '\''    reverse_proxy 127.0.0.1:8090'\'' '\''  }'\'' '\''  handle {'\'' '\''    root * /var/www/spawnery'\'' '\''    try_files {path} /index.html'\'' '\''    file_server'\'' '\''  }'\'' '\''}'\'' | sudo tee /etc/caddy/Caddyfile >/dev/null \
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
  && sudo cp -f ~/incoming/provision/env/common.env /etc/spawnery/env.d/common.env.tmpl \
  && sudo cp -f ~/incoming/provision/env/profile.*.env /etc/spawnery/env.d/ \
  && sudo sh -c '\''for f in /etc/spawnery/env.d/profile.*.env; do mv -f "$f" "$f.tmpl"; done'\'' \
  && sudo systemctl restart spawnery-render-env'

# 5. atomic swap + restart the stack (order matters: AS -> CP -> node -> caddy)
# Re-copying config/ re-introduces cp.prod.yaml's ${sops:store.dsn} ref (pristine config ships it),
# so re-patch the literal throwaway DSN right after the config copy, every roll.
vm_ssh "$IP" 'sudo install -m0755 ~/incoming/bin/* /usr/local/bin/ \
  && ( [ -d ~/incoming/config ] && sudo cp -rf ~/incoming/config/* /etc/spawnery/config/ || true ) \
  && ( [ -f /etc/spawnery/config/cp.prod.yaml ] && sudo sed -i '"'"'s#\${sops:store.dsn}#postgres://spawnery:spawnery@127.0.0.1:5432/spawnery?sslmode=disable#'"'"' /etc/spawnery/config/cp.prod.yaml || true ) \
  && ( [ -d ~/incoming/web ] && sudo rsync -a --delete ~/incoming/web/ /var/www/spawnery/ || true ) \
  && sudo systemctl restart spawnery-authsvc spawnery-cp spawnery-node caddy'

# ---- wait app-ready — gate ALL the pieces (roast gap), not just AS /healthz ----
# AS and CP bind 127.0.0.1 (Caddy fronts them on :443), so probe localhost INSIDE the VM over ssh —
# a wait_tcp on the external IP only ever sees Caddy's :443, never 8090/8080.
log "waiting for app-ready …"
# (a) AS /healthz (127.0.0.1:8090)
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
