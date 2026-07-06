#!/usr/bin/env bash
# provision.sh — runs INSIDE the build VM (over ssh) to turn a stock Fedora cloud image into the
# golden spawnery-prod-stack image. build-base.sh scp's this + the payload in, runs it, then shuts
# down; the resulting qcow2 is the golden backing image for per-run overlays.
#
# Self-contained: writes every host config (runsc/containerd/CNI/Caddy/systemd/PKI) via heredocs.
# Pinned versions come from the exploration of PROVISIONING.md + the runsc notes.
#
# This fold bakes in the findings from a full live reconcile of the stack in a VM (see
# provision/RECONCILE-NOTES.md): binaries+examples install before PKI generation (spawnery-ca must
# be on PATH), Postgres switched to scram-sha-256 local auth, cp.prod.yaml's ${sops:} store DSN
# patched to the throwaway local Postgres, the captured env (env/common.env + env/profile.*.env)
# installed as per-boot-rendered templates, and systemd units pointed at /opt/spawnery with no
# separate spawnery-web unit (Caddy already serves the web root + reverse-proxies CP/AS).
set -euo pipefail

# ---- pinned versions (verify/bump on the host; these mirror the runsc-node-provisioning notes) ----
CONTAINERD_VER="${CONTAINERD_VER:-2.2.3}"
RUNSC_RELEASE="${RUNSC_RELEASE:-20260525.0}"
CNI_PLUGINS_VER="${CNI_PLUGINS_VER:-1.5.1}"
RUNC_VER="${RUNC_VER:-1.2.4}"
CRICTL_VER="${CRICTL_VER:-v1.32.0}"            # cri-tools; the runsc/CRI lane's `spawnctl exec` shells out to crictl
POD_CIDR="${POD_CIDR:-10.234.0.0/16}"          # avoid Podman's 10.88.0.0/16
POD_DNS="${POD_DNS:-1.1.1.1,8.8.8.8}"          # systemd-resolved's 127.0.0.53 is unreachable in-pod
WILDCARD_DOMAIN="${WILDCARD_DOMAIN:-e2e.test}" # cert covers *.e2e.test

PAYLOAD="${PAYLOAD:-/home/build/payload}"      # scp'd by build-base.sh: bin/ images.tar config/ examples/ env/ web-dist/ spawnery-ca
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

# ---- crictl (cri-tools) — the runsc/CRI lane's non-interactive `spawnctl exec` shells out to
# `crictl exec` (internal/spawnlet/terminal.go). Without it the node NACKs exec ("crictl: not found
# in $PATH"). /etc/crictl.yaml points it at containerd's CRI socket (crictl exec carries no endpoint
# flag). Installed to /usr/local/bin (on the systemd default PATH so the node unit finds it). ----
log "installing crictl ${CRICTL_VER}…"
curl -fsSL "https://github.com/kubernetes-sigs/cri-tools/releases/download/${CRICTL_VER}/crictl-${CRICTL_VER}-linux-amd64.tar.gz" \
  | sudo tar -C /usr/local/bin -xz
sudo tee /etc/crictl.yaml >/dev/null <<'EOF'
runtime-endpoint: unix:///run/containerd/containerd.sock
image-endpoint: unix:///run/containerd/containerd.sock
timeout: 10
EOF

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

log "switching Postgres local TCP auth to scram-sha-256…"
PGHBA=$(sudo -u postgres psql -tAc 'SHOW hba_file' 2>/dev/null || echo /var/lib/pgsql/data/pg_hba.conf)
sudo sed -i -E 's#^(host\s+all\s+all\s+127\.0\.0\.1/32\s+).*#\1scram-sha-256#' "$PGHBA"
sudo sed -i -E 's#^(host\s+all\s+all\s+::1/128\s+).*#\1scram-sha-256#' "$PGHBA"
sudo -u postgres psql -c "ALTER SYSTEM SET password_encryption='scram-sha-256';" || true
sudo systemctl restart postgresql
sudo -u postgres psql -c "CREATE USER spawnery WITH PASSWORD 'spawnery';" 2>/dev/null \
  || sudo -u postgres psql -c "ALTER USER spawnery WITH PASSWORD 'spawnery';"
sudo -u postgres psql -c "CREATE DATABASE spawnery OWNER spawnery;" 2>/dev/null || true
sudo systemctl reload postgresql || sudo systemctl restart postgresql

# ---- spawnery binaries + config + examples (BEFORE PKI: spawnery-ca must be on PATH for gen-pki.sh) ----
log "installing spawnery binaries + config + examples…"
sudo install -m0755 "$PAYLOAD"/bin/* /usr/local/bin/
sudo mkdir -p /etc/spawnery/config /opt/spawnery /var/www/spawnery /var/lib/spawnlet
sudo cp -rf "$PAYLOAD"/config/* /etc/spawnery/config/ 2>/dev/null || true
[ -d "$PAYLOAD/examples" ] && sudo cp -rf "$PAYLOAD/examples" /opt/spawnery/
[ -d "$PAYLOAD/web-dist" ] && sudo rsync -a "$PAYLOAD/web-dist/" /var/www/spawnery/

# ---- PKI: throwaway CA + AS session key + node/CP mTLS + the *.e2e.test wildcard cert ----
log "generating throwaway PKI + wildcard cert…"
sudo mkdir -p /etc/spawnery/pki
sudo bash "$PAYLOAD/gen-pki.sh" /etc/spawnery/pki "$WILDCARD_DOMAIN"   # writes root.pem/ca.crt, session-key/pub, node/cp certs, wildcard.{crt,key}
sudo chmod 644 /etc/spawnery/pki/wildcard.crt /etc/spawnery/pki/wildcard.key   # caddy runs as user 'caddy'
sudo cp /etc/spawnery/pki/ca.crt /home/build/ca.crt   # build-base.sh pulls this out for host trust

# ---- cp.prod.yaml: patch the ${sops:} store DSN to the throwaway local Postgres (baseline; roll.sh
#      re-applies this after every config re-copy, since a fresh config/ ships the sops ref again) ----
sudo sed -i 's#\${sops:store.dsn}#postgres://spawnery:spawnery@127.0.0.1:5432/spawnery?sslmode=disable#' \
  /etc/spawnery/config/cp.prod.yaml || true

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

# ---- spawnery env: install the reconciled env as per-boot-rendered TEMPLATES ----
# env/common.env + env/profile.*.env (captured verbatim off the live reconcile) carry @@HOST@@/@@IP@@
# placeholders; spawnery-render-env.sh renders them into /etc/spawnery/env.d/{common,profile}.env on
# every boot, once the per-run hostname/profile are known (cloud-init has already written
# /etc/spawnery-e2e/{profile,web_origin} and set the VM hostname by the time this unit runs).
log "installing env templates + per-boot render unit…"
sudo mkdir -p /etc/spawnery/env.d /var/lib/spawnery /var/www/spawnery /var/lib/spawnlet
sudo cp -f "$PAYLOAD"/env/common.env /etc/spawnery/env.d/common.env.tmpl
for p in "$PAYLOAD"/env/profile.*.env; do [ -f "$p" ] && sudo cp -f "$p" "/etc/spawnery/env.d/$(basename "$p").tmpl"; done

sudo tee /usr/local/bin/spawnery-render-env.sh >/dev/null <<'RENDER_EOF'
#!/usr/bin/env bash
set -euo pipefail
HOST="$(hostname -f 2>/dev/null || hostname)"
IP="$(ip -4 route get 1.1.1.1 2>/dev/null | awk '{for(i=1;i<=NF;i++) if($i=="src"){print $(i+1); exit}}')"
[ -n "${IP:-}" ] || IP="$(hostname -I | awk '{print $1}')"
PROFILE="$(cat /etc/spawnery-e2e/profile 2>/dev/null || echo fake)"
sed -e "s#@@HOST@@#$HOST#g" -e "s#@@IP@@#$IP#g" /etc/spawnery/env.d/common.env.tmpl > /etc/spawnery/env.d/common.env
TMPL="/etc/spawnery/env.d/profile.$PROFILE.env.tmpl"
[ -f "$TMPL" ] || { echo "no env template for profile=$PROFILE ($TMPL)" >&2; exit 1; }
sed -e "s#@@HOST@@#$HOST#g" -e "s#@@IP@@#$IP#g" "$TMPL" > /etc/spawnery/env.d/profile.env
RENDER_EOF
sudo chmod 0755 /usr/local/bin/spawnery-render-env.sh

sudo tee /etc/systemd/system/spawnery-render-env.service >/dev/null <<'EOF'
[Unit]
Description=render per-boot spawnery env (HOST/IP/profile substitution)
After=cloud-init.service network-online.target
Wants=network-online.target
[Service]
Type=oneshot
RemainAfterExit=yes
ExecStart=/usr/local/bin/spawnery-render-env.sh
[Install]
WantedBy=multi-user.target
EOF

# ---- spawnery systemd units (authsvc/cp/node) — no separate spawnery-web: Caddy already serves the
#      web root and reverse-proxies the CP/AS routes (see Caddyfile above) ----
log "writing spawnery systemd units…"
sudo tee /etc/systemd/system/spawnery-authsvc.service >/dev/null <<'EOF'
[Unit]
Description=spawnery auth service
After=spawnery-render-env.service postgresql.service network-online.target
Requires=spawnery-render-env.service
[Service]
EnvironmentFile=/etc/spawnery/env.d/common.env
EnvironmentFile=-/etc/spawnery/env.d/profile.env
WorkingDirectory=/opt/spawnery
ExecStart=/usr/local/bin/authsvc
Restart=on-failure
[Install]
WantedBy=multi-user.target
EOF

sudo tee /etc/systemd/system/spawnery-cp.service >/dev/null <<'EOF'
[Unit]
Description=spawnery control plane
After=spawnery-render-env.service spawnery-authsvc.service containerd.service postgresql.service
Requires=spawnery-render-env.service
[Service]
EnvironmentFile=/etc/spawnery/env.d/common.env
EnvironmentFile=-/etc/spawnery/env.d/profile.env
WorkingDirectory=/opt/spawnery
ExecStart=/usr/local/bin/spawnery_cp
Restart=on-failure
[Install]
WantedBy=multi-user.target
EOF

sudo tee /etc/systemd/system/spawnery-node.service >/dev/null <<'EOF'
[Unit]
Description=spawnery node (spawnlet)
After=spawnery-render-env.service spawnery-cp.service containerd.service
Requires=spawnery-render-env.service
[Service]
EnvironmentFile=/etc/spawnery/env.d/common.env
EnvironmentFile=-/etc/spawnery/env.d/profile.env
WorkingDirectory=/opt/spawnery
ExecStart=/usr/local/bin/spawnlet
Restart=on-failure
[Install]
WantedBy=multi-user.target
EOF

sudo systemctl daemon-reload
sudo systemctl enable spawnery-render-env spawnery-authsvc spawnery-cp spawnery-node caddy

# ---- self-check (best-effort) + clean shutdown handled by build-base.sh ----
log "provision complete. runsc: $(runsc --version | head -1). containerd: $(/usr/local/bin/containerd --version)"
log "REMINDER: env templates installed at /etc/spawnery/env.d/*.tmpl; spawnery-render-env renders them (HOST/IP/profile) on every boot before the spawnery services start."
