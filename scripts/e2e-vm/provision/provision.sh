#!/usr/bin/env bash
# provision.sh — runs INSIDE the build VM (over ssh) to turn a stock Fedora cloud image into the
# golden spawnery-prod-stack image. build-base.sh scp's this + the payload in, runs it, then shuts
# down; the resulting qcow2 is the golden backing image for per-run overlays.
#
# Self-contained: writes every host config (runsc/containerd/CNI/Caddy/systemd/PKI) via heredocs.
# Pinned versions come from the exploration of PROVISIONING.md + the runsc notes.
#
# STATUS: first draft — the SYSTEM provisioning (containerd/runsc/CNI/pg/caddy/PKI) is concrete;
# the spawnery SYSTEMD ENV must be reconciled against the Justfile *-github/*-enforced recipes
# (the single source) on the host — search for "RECONCILE" below.
set -euo pipefail

# ---- pinned versions (verify/bump on the host; these mirror the runsc-node-provisioning notes) ----
CONTAINERD_VER="${CONTAINERD_VER:-2.2.3}"
RUNSC_RELEASE="${RUNSC_RELEASE:-20260525.0}"
CNI_PLUGINS_VER="${CNI_PLUGINS_VER:-1.5.1}"
RUNC_VER="${RUNC_VER:-1.2.4}"
POD_CIDR="${POD_CIDR:-10.234.0.0/16}"          # avoid Podman's 10.88.0.0/16
POD_DNS="${POD_DNS:-1.1.1.1,8.8.8.8}"          # systemd-resolved's 127.0.0.53 is unreachable in-pod
WILDCARD_DOMAIN="${WILDCARD_DOMAIN:-e2e.test}" # cert covers *.e2e.test

PAYLOAD="${PAYLOAD:-/home/build/payload}"      # scp'd by build-base.sh: bin/ images.tar config/ web-dist/ spawnery-ca
log(){ printf '\033[36m[provision]\033[0m %s\n' "$*"; }

log "installing base packages…"
sudo dnf -y install curl tar iptables-legacy postgresql-server postgresql caddy chrony \
  qemu-guest-agent cloud-init rsync jq openssl containernetworking-plugins || true
sudo systemctl enable --now chronyd qemu-guest-agent

# ---- containerd (pinned) ----
log "installing containerd ${CONTAINERD_VER} + runc ${RUNC_VER}…"
curl -fsSL "https://github.com/containerd/containerd/releases/download/v${CONTAINERD_VER}/containerd-${CONTAINERD_VER}-linux-amd64.tar.gz" \
  | sudo tar -C /usr/local -xz
curl -fsSL -o /tmp/runc "https://github.com/opencontainers/runc/releases/download/v${RUNC_VER}/runc.amd64"
sudo install -m0755 /tmp/runc /usr/local/sbin/runc

# ---- runsc + shim (pinned release) ----
log "installing runsc release-${RUNSC_RELEASE} (systrap)…"
base="https://storage.googleapis.com/gvisor/releases/release/${RUNSC_RELEASE}/x86_64"
curl -fsSL -o /tmp/runsc "$base/runsc"
curl -fsSL -o /tmp/shim  "$base/containerd-shim-runsc-v1"
sudo install -m0755 /tmp/runsc /usr/local/bin/runsc
sudo install -m0755 /tmp/shim  /usr/local/bin/containerd-shim-runsc-v1

# ---- CNI plugins (pinned) into /opt/cni/bin ----
log "installing CNI plugins ${CNI_PLUGINS_VER}…"
sudo mkdir -p /opt/cni/bin
curl -fsSL "https://github.com/containernetworking/plugins/releases/download/v${CNI_PLUGINS_VER}/cni-plugins-linux-amd64-v${CNI_PLUGINS_VER}.tgz" \
  | sudo tar -C /opt/cni/bin -xz

# ---- /etc/runsc/runsc.toml — overlay2=none (delta capture) + systrap (no /dev/kvm, VM-safe) ----
sudo mkdir -p /etc/runsc
sudo tee /etc/runsc/runsc.toml >/dev/null <<'EOF'
[runsc_config]
platform = "systrap"
network = "sandbox"
overlay2 = "none"
EOF

# ---- /etc/containerd/config.toml — runsc runtime handler ----
sudo mkdir -p /etc/containerd
sudo tee /etc/containerd/config.toml >/dev/null <<EOF
version = 3
[plugins.'io.containerd.cri.v1.runtime'.containerd]
  default_runtime_name = "runc"
  [plugins.'io.containerd.cri.v1.runtime'.containerd.runtimes.runc]
    runtime_type = "io.containerd.runc.v2"
  [plugins.'io.containerd.cri.v1.runtime'.containerd.runtimes.runsc]
    runtime_type = "io.containerd.runsc.v1"
    [plugins.'io.containerd.cri.v1.runtime'.containerd.runtimes.runsc.options]
      TypeUrl = "io.containerd.runsc.v1.options"
      ConfigPath = "/etc/runsc/runsc.toml"
[plugins.'io.containerd.cri.v1.runtime'.cni]
  bin_dir = "/opt/cni/bin"
  conf_dir = "/etc/cni/net.d"
EOF

# ---- CNI bridge/firewall/portmap conflist (NOT hostinet — it bypasses the egress floor) ----
sudo mkdir -p /etc/cni/net.d
sudo tee /etc/cni/net.d/10-spawnery.conflist >/dev/null <<EOF
{
  "cniVersion": "1.0.0", "name": "spawnery",
  "plugins": [
    { "type": "bridge", "bridge": "spawnery-cni0", "isGateway": true, "ipMasq": true,
      "ipam": { "type": "host-local", "subnet": "${POD_CIDR}",
                "routes": [ { "dst": "0.0.0.0/0" } ] } },
    { "type": "firewall" },
    { "type": "portmap", "capabilities": { "portMappings": true } }
  ]
}
EOF

# ---- containerd systemd unit ----
sudo tee /etc/systemd/system/containerd.service >/dev/null <<'EOF'
[Unit]
Description=containerd
After=network.target
[Service]
ExecStart=/usr/local/bin/containerd
Delegate=yes
KillMode=process
LimitNOFILE=1048576
[Install]
WantedBy=multi-user.target
EOF

# ---- import agent/sidecar images into the k8s.io namespace (containerd must be up) ----
sudo systemctl daemon-reload
sudo systemctl enable --now containerd
sleep 3
if [ -f "$PAYLOAD/images.tar" ]; then
  log "importing agent/sidecar images into k8s.io…"
  sudo ctr -n k8s.io images import "$PAYLOAD/images.tar"
fi

# ---- Postgres (throwaway, local) ----
log "initializing Postgres…"
sudo postgresql-setup --initdb || true
sudo systemctl enable --now postgresql
sudo -u postgres psql -c "CREATE USER spawnery WITH PASSWORD 'spawnery';" || true
sudo -u postgres psql -c "CREATE DATABASE spawnery OWNER spawnery;" || true

# ---- PKI: throwaway CA + AS session key + node/CP mTLS + the *.e2e.test wildcard cert ----
log "generating throwaway PKI + wildcard cert…"
sudo mkdir -p /etc/spawnery/pki
sudo bash "$PAYLOAD/gen-pki.sh" /etc/spawnery/pki "$WILDCARD_DOMAIN"   # writes ca.crt, session-key/pub, node/cp certs, wildcard.{crt,key}
sudo cp /etc/spawnery/pki/ca.crt /home/build/ca.crt   # build-base.sh pulls this out for host trust

# ---- spawnery binaries + config (baseline; roll.sh replaces per run) ----
log "installing spawnery binaries + config…"
sudo install -m0755 "$PAYLOAD"/bin/* /usr/local/bin/
sudo mkdir -p /etc/spawnery/config /var/www/spawnery /var/lib/spawnlet
sudo cp -rf "$PAYLOAD"/config/* /etc/spawnery/config/ 2>/dev/null || true
[ -d "$PAYLOAD/web-dist" ] && sudo rsync -a "$PAYLOAD/web-dist/" /var/www/spawnery/

# ---- Caddy: TLS :443 wildcard cert, route to web/CP/AS ----
sudo tee /etc/caddy/Caddyfile >/dev/null <<EOF
:443 {
  tls /etc/spawnery/pki/wildcard.crt /etc/spawnery/pki/wildcard.key
  @cp   path /cp.v1.* /ws*
  @as   path /oauth* /refresh* /logout* /github* /device* /ca/*
  reverse_proxy @cp 127.0.0.1:8080
  reverse_proxy @as 127.0.0.1:8090
  root * /var/www/spawnery
  file_server
}
EOF
sudo systemctl enable caddy

# ---- spawnery env (RECONCILE with Justfile *-github/*-enforced) + systemd units ----
# The prod delta: SPAWNERY_ENV=prod, but override the ${sops:} store DSN to the local throwaway pg
# (do NOT ship the real secrets.prod.sops.yaml / age key).
log "writing spawnery env + systemd units (RECONCILE env with the Justfile)…"
sudo mkdir -p /etc/spawnery/env.d
sudo tee /etc/spawnery/env.d/common.env >/dev/null <<EOF
SPAWNERY_ENV=prod
SPAWNERY_CONFIG_DIR=/etc/spawnery/config
# store: real prod path (postgres) but throwaway local DSN, overriding the \${sops:} ref
CP_STORE_DRIVER=postgres
CP_STORE_DSN=postgres://spawnery:spawnery@127.0.0.1:5432/spawnery?sslmode=disable
# PKI (throwaway)
AS_SESSION_KEY_PEM=/etc/spawnery/pki/session-key.pem
CP_AS_SESSION_PUBKEYS=/etc/spawnery/pki/session-pub.pem
CP_NODE_ROOT_CA=/etc/spawnery/pki/ca.crt
CP_NODE_TLS_CERT=/etc/spawnery/pki/cp-node.crt
CP_NODE_TLS_KEY=/etc/spawnery/pki/cp-node.key
NODE_AS_PUBKEYS=/etc/spawnery/pki/session-pub.pem
# listeners
AS_LISTEN=127.0.0.1:8090
CP_LISTEN=127.0.0.1:8080
CP_NODE_LISTEN=127.0.0.1:8081
NODE_AUTH_MODE=enforced
NODE_CLASS=cloud
EGRESS_ENFORCE=true
CONTAINER_RUNTIME=runsc
USERNS_MODE=native
POD_DNS=${POD_DNS}
# RECONCILE: AS_ROOT_CA_PEM, AS_INTERMEDIATE_*, AS_GITHUB_TOKEN_ENC_KEY, AS_CP_RPC_SECRET/CP_AS_RPC_SECRET,
# CP_ADDR/CP_NODE_ADDR, NODE_ID_DIR, GITHUB_CLIENT_ID/SECRET (github profile), AS_FAKE_GITHUB* (fake
# profile) — copy the exact set from the Justfile authsvc-github/cp-github/node-github recipes.
EOF

# auth profile (fake|github) is written per-run to /etc/spawnery-e2e/profile by up.sh's cloud-init;
# a drop-in reads it. github profile additionally needs GITHUB_CLIENT_ID/SECRET; fake needs AS_FAKE_GITHUB*.
sudo tee /etc/systemd/system/spawnery-authsvc.service >/dev/null <<'EOF'
[Unit]
Description=spawnery auth service
After=postgresql.service network-online.target
[Service]
EnvironmentFile=/etc/spawnery/env.d/common.env
EnvironmentFile=-/etc/spawnery/env.d/profile.env
ExecStart=/usr/local/bin/authsvc
Restart=on-failure
[Install]
WantedBy=multi-user.target
EOF
for svc in cp node web; do
  bin=spawnery_cp; [ "$svc" = node ] && bin=spawnlet; [ "$svc" = web ] && bin=""
  sudo tee /etc/systemd/system/spawnery-$svc.service >/dev/null <<EOF
[Unit]
Description=spawnery $svc
After=spawnery-authsvc.service containerd.service postgresql.service
[Service]
EnvironmentFile=/etc/spawnery/env.d/common.env
EnvironmentFile=-/etc/spawnery/env.d/profile.env
$( [ "$svc" = web ] && echo 'ExecStart=/usr/bin/caddy file-server --root /var/www/spawnery' || echo "ExecStart=/usr/local/bin/$bin" )
Restart=on-failure
[Install]
WantedBy=multi-user.target
EOF
done
sudo systemctl daemon-reload
sudo systemctl enable spawnery-authsvc spawnery-cp spawnery-node caddy

# ---- self-check (best-effort) + clean shutdown handled by build-base.sh ----
log "provision complete. runsc: $(runsc --version | head -1). containerd: $(/usr/local/bin/containerd --version)"
log "REMINDER: reconcile /etc/spawnery/env.d/common.env with the Justfile before first real run."
