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
vm_ssh "$IP" 'mkdir -p ~/incoming/bin ~/incoming/config'
vm_scp "$STAGE/bin/." "$IP" 'incoming/bin/'
[ -d "$STAGE/config" ] && vm_scp "$STAGE/config/." "$IP" 'incoming/config/'

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

# 4. atomic swap + restart the stack (order matters: AS -> CP -> node -> caddy)
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
# (c) node re-registered with the CP over enforced mTLS — poll a CP-side node list via spawnctl on the guest
for i in $(seq 1 60); do
  if vm_ssh "$IP" 'spawnctl -cp http://127.0.0.1:8080 node-list 2>/dev/null | grep -q .'; then break; fi
  [ "$i" = 60 ] && warn "node never appeared in CP node-list (mTLS registration?) — continuing, tests will surface it"
  sleep 2
done
# (d) Caddy TLS + web serving
wait_tcp "$IP" 443 60 || die "Caddy :443 not listening"
curl -fsS --resolve "$HOST:443:$IP" "https://$HOST/" >/dev/null 2>&1 || warn "web root not 200 yet (CA trust / static build?)"

log "app-ready on https://$HOST"
