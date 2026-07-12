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
# 20260601.0 is the first release containing the gVisor containerd-shim pause/resume fix (commit
# 55b3fd17, gVisor #12647/#13305, 2026-05-28). Earlier builds (incl. the prior 20260525.0 pin) have a
# regression where `task pause` corrupts the shim/ttrpc Status → the task goes PID 0/UNKNOWN, resume
# fails "no running task found", and sandbox teardown wedges — which broke suspend/resume + fork.
RUNSC_RELEASE="${RUNSC_RELEASE:-20260601.0}"
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
  qemu-guest-agent cloud-init rsync jq openssl containernetworking-plugins git dnsmasq || true
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

# ---- Garage (S3) — the transient-tier journal store (Kopia). WITHOUT this the journaler is OFF and
# suspend/resume + fork cannot move a spawn's workspace. Native single-node binary + well-known DEV
# secrets (mirrors deploy/garage/); a per-boot oneshot mints the bucket+key into journal.env. ----
GARAGE_VER="${GARAGE_VER:-v1.0.1}"
log "installing garage ${GARAGE_VER} (journal store)…"
curl -fsSL -o /tmp/garage "https://garagehq.deuxfleurs.fr/_releases/${GARAGE_VER}/x86_64-unknown-linux-musl/garage"
sudo install -m0755 /tmp/garage /usr/local/bin/garage
sudo mkdir -p /var/lib/garage/meta /var/lib/garage/data /var/lib/spawnlet/journal
sudo tee /etc/garage.toml >/dev/null <<'EOF'
metadata_dir = "/var/lib/garage/meta"
data_dir = "/var/lib/garage/data"
db_engine = "sqlite"
replication_factor = 1
rpc_bind_addr = "[::]:3901"
rpc_public_addr = "127.0.0.1:3901"
rpc_secret = "7642aaf5cbe9ae49eec221853099829d9e05f82f8e1dff2b36f5e4a7b1d63e3c"
[s3_api]
s3_region = "garage"
api_bind_addr = "127.0.0.1:3900"
root_domain = ".s3.garage.localhost"
[admin]
api_bind_addr = "127.0.0.1:3903"
admin_token = "e8a6bb74d9331b884614a992c640234faa408f543fd31ec9"
EOF
sudo tee /etc/systemd/system/garage.service >/dev/null <<'EOF'
[Unit]
Description=Garage S3 (spawnery journal store)
After=network.target
[Service]
ExecStart=/usr/local/bin/garage -c /etc/garage.toml server
Restart=on-failure
[Install]
WantedBy=multi-user.target
EOF
# per-boot bootstrap: apply the single-node layout, mint bucket + access key, write journal.env
# (adapted from deploy/garage/bootstrap.sh; native garage CLI + admin API).
sudo tee /usr/local/bin/spawnery-garage-bootstrap.sh >/dev/null <<'BOOT'
#!/usr/bin/env bash
set -euo pipefail
ADMIN=http://127.0.0.1:3903; TOKEN=e8a6bb74d9331b884614a992c640234faa408f543fd31ec9; BUCKET=spawnery-journal
G(){ garage -c /etc/garage.toml "$@"; }
for _ in $(seq 1 60); do G status >/dev/null 2>&1 && break; sleep 1; done
NID="$(G node id -q | cut -d@ -f1)"; SID="${NID:0:16}"
if ! G layout show 2>/dev/null | grep -q "$SID"; then
  G layout assign -z dc1 -c 1G "$NID"
  CUR="$(G layout show 2>/dev/null | grep -oE 'version: [0-9]+' | grep -oE '[0-9]+' | tail -1)"
  G layout apply --version "$(( ${CUR:-0} + 1 ))"
fi
A(){ curl -fsS -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' "$@"; }
KJ=""; for _ in $(seq 1 20); do KJ="$(A -X POST "$ADMIN/v1/key" -d "{\"name\":\"${BUCKET}-key\"}" 2>/dev/null)" && [ -n "$KJ" ] && break; sleep 1; done
AK="$(printf '%s' "$KJ" | grep -oE '"accessKeyId": *"[^"]+"' | head -1 | cut -d'"' -f4)"
SK="$(printf '%s' "$KJ" | grep -oE '"secretAccessKey": *"[^"]+"' | head -1 | cut -d'"' -f4)"
A -X POST "$ADMIN/v1/bucket" -d "{\"globalAlias\":\"$BUCKET\"}" >/dev/null 2>&1 || true
BID="$(A "$ADMIN/v1/bucket?globalAlias=$BUCKET" | grep -oE '"id": *"[^"]+"' | head -1 | cut -d'"' -f4)"
A -X POST "$ADMIN/v1/bucket/allow" -d "{\"bucketId\":\"$BID\",\"accessKeyId\":\"$AK\",\"permissions\":{\"read\":true,\"write\":true,\"owner\":true}}" >/dev/null
mkdir -p /etc/spawnery/env.d
printf 'JOURNAL_S3_ACCESS_KEY=%s\nJOURNAL_S3_SECRET_KEY=%s\n' "$AK" "$SK" > /etc/spawnery/env.d/journal.env
BOOT
sudo chmod 0755 /usr/local/bin/spawnery-garage-bootstrap.sh
sudo tee /etc/systemd/system/spawnery-garage-bootstrap.service >/dev/null <<'EOF'
[Unit]
Description=bootstrap garage layout+bucket+key -> /etc/spawnery/env.d/journal.env
After=garage.service
Requires=garage.service
[Service]
Type=oneshot
RemainAfterExit=yes
ExecStart=/usr/local/bin/spawnery-garage-bootstrap.sh
[Install]
WantedBy=multi-user.target
EOF

# ---- Gitea (local GitHub-compatible git host) — backs the github: storage-mount lane so the
# acceptance suite can prove a `github:` mount survives suspend/resume WITHOUT reaching github.com.
# Native single-node binary + sqlite; a per-boot oneshot creates an admin + access token + seed repo
# and writes the node github-override env into /etc/spawnery/env.d/gitea.env (mirrors the garage
# bootstrap). The node clones over http from 127.0.0.1 under GITHUB_ALLOW_INSECURE_HOST. ----
GITEA_VER="${GITEA_VER:-1.22.6}"
GITEA_PORT="${GITEA_PORT:-3000}"
GITEA_ADMIN_USER="${GITEA_ADMIN_USER:-spawnery}"
GITEA_ADMIN_PASS="${GITEA_ADMIN_PASS:-spawnery-e2e-pass}"
log "installing gitea ${GITEA_VER} (local git host)…"
curl -fsSL -o /tmp/gitea "https://dl.gitea.com/gitea/${GITEA_VER}/gitea-${GITEA_VER}-linux-amd64"
sudo install -m0755 /tmp/gitea /usr/local/bin/gitea
id gitea >/dev/null 2>&1 || sudo useradd --system --shell /bin/bash --home-dir /var/lib/gitea --create-home gitea
sudo mkdir -p /var/lib/gitea/custom /var/lib/gitea/data /var/lib/gitea/log /etc/gitea
# Generate the two required secrets with the gitea binary (INSTALL_LOCK=true skips the web installer).
GITEA_SECRET_KEY="$(gitea generate secret SECRET_KEY)"
GITEA_INTERNAL_TOKEN="$(gitea generate secret INTERNAL_TOKEN)"
sudo tee /etc/gitea/app.ini >/dev/null <<EOF
APP_NAME = Spawnery E2E Gitea
RUN_USER = gitea
RUN_MODE = prod
WORK_PATH = /var/lib/gitea
[server]
PROTOCOL = http
HTTP_ADDR = 127.0.0.1
HTTP_PORT = ${GITEA_PORT}
DOMAIN = 127.0.0.1
# github.com, not the loopback: sp-wwtc.1 fronts Gitea with a github.com-SAN cert (Caddy
# reverse-proxies it), so Gitea's OWN generated URLs (clone_url, etc.) must say github.com too —
# Gitea itself stays plain HTTP on loopback and never learns TLS; Caddy is what terminates it.
ROOT_URL = https://github.com/
DISABLE_SSH = true
OFFLINE_MODE = true
[database]
DB_TYPE = sqlite3
PATH = /var/lib/gitea/data/gitea.db
[security]
INSTALL_LOCK = true
SECRET_KEY = ${GITEA_SECRET_KEY}
INTERNAL_TOKEN = ${GITEA_INTERNAL_TOKEN}
[service]
DISABLE_REGISTRATION = true
[repository]
DEFAULT_BRANCH = main
[log]
ROOT_PATH = /var/lib/gitea/log
LEVEL = warn
EOF
sudo chown -R gitea:gitea /var/lib/gitea /etc/gitea
sudo chmod 640 /etc/gitea/app.ini
sudo tee /etc/systemd/system/gitea.service >/dev/null <<'EOF'
[Unit]
Description=Gitea (spawnery e2e local git host)
After=network.target
[Service]
User=gitea
Group=gitea
WorkingDirectory=/var/lib/gitea
Environment=GITEA_WORK_DIR=/var/lib/gitea
ExecStart=/usr/local/bin/gitea web --config /etc/gitea/app.ini
Restart=on-failure
[Install]
WantedBy=multi-user.target
EOF
# per-boot bootstrap: create the admin (idempotent), mint a fresh access token + seed repo, and write
# the node github-override env fragment. The token name is unique per boot to avoid a name clash on
# reboot; the fragment is what wires GITHUB_STATIC_TOKEN etc. into the node unit below.
#
# sp-wwtc.4: the SAME token is ALSO published as AS_FAKE_GITHUB_TOKEN, consumed by authsvc (below).
# The sidecar's GitHub MITM proxy injects `Authorization: Basic base64("x-access-token:"+token)` at
# github.com using whatever token the AS's fake-GitHub OAuth flow hands out — a real, non-optional
# production code path, not a stub to bypass — so for Gitea to actually ACCEPT that injected
# credential, the fake's minted token has to equal a Gitea PAT Gitea will accept. Gitea mints its PAT
# itself and won't let a caller choose its value, so the token has to flow gitea -> AS, not the other
# way: this bootstrap mints the real Gitea PAT first, and authsvc's AS_FAKE_GITHUB_TOKEN (below) makes
# the fake hand back that SAME string.
sudo tee /usr/local/bin/spawnery-gitea-bootstrap.sh >/dev/null <<BOOT
#!/usr/bin/env bash
set -euo pipefail
CFG=/etc/gitea/app.ini; U='${GITEA_ADMIN_USER}'; P='${GITEA_ADMIN_PASS}'; PORT='${GITEA_PORT}'
G(){ sudo -u gitea GITEA_WORK_DIR=/var/lib/gitea gitea --config "\$CFG" "\$@"; }
for _ in \$(seq 1 60); do curl -fsS "http://127.0.0.1:\$PORT/api/healthz" >/dev/null 2>&1 && break; sleep 1; done
G admin user create --username "\$U" --password "\$P" --email "\$U@e2e.test" --admin --must-change-password=false 2>/dev/null || true
TOKEN="\$(G admin user generate-access-token -u "\$U" --scopes 'write:repository,write:user' -t "e2e-\$(date +%s%N)" --raw 2>/dev/null | tail -1)"
# Seed a repo so a manual github:${GITEA_ADMIN_USER}/seed bind works without create; the acceptance
# suite instead creates unique per-run repos via create_if_missing.
curl -fsS -X POST -H "Authorization: token \$TOKEN" -H 'Content-Type: application/json' \
  "http://127.0.0.1:\$PORT/api/v1/user/repos" -d '{"name":"seed","auto_init":true,"private":true}' >/dev/null 2>&1 || true
mkdir -p /etc/spawnery/env.d
cat > /etc/spawnery/env.d/gitea.env <<ENV
GITHUB_API_BASE_URL=http://127.0.0.1:\$PORT/api/v1
GITHUB_HOST=127.0.0.1:\$PORT
GITHUB_ALLOW_INSECURE_HOST=1
GITHUB_STATIC_TOKEN=\$TOKEN
AS_FAKE_GITHUB_TOKEN=\$TOKEN
ENV
BOOT
sudo chmod 0755 /usr/local/bin/spawnery-gitea-bootstrap.sh
sudo tee /etc/systemd/system/spawnery-gitea-bootstrap.service >/dev/null <<'EOF'
[Unit]
Description=bootstrap gitea admin+token+seed repo -> /etc/spawnery/env.d/gitea.env
After=gitea.service
Requires=gitea.service
[Service]
Type=oneshot
RemainAfterExit=yes
ExecStart=/usr/local/bin/spawnery-gitea-bootstrap.sh
[Install]
WantedBy=multi-user.target
EOF

# ---- spawnery binaries + config + examples (BEFORE PKI: spawnery-ca must be on PATH for gen-pki.sh) ----
log "installing spawnery binaries + config + examples…"
sudo install -m0755 "$PAYLOAD"/bin/* /usr/local/bin/
sudo mkdir -p /etc/spawnery/config /opt/spawnery /var/www/spawnery /var/lib/spawnlet
sudo cp -rf "$PAYLOAD"/config/* /etc/spawnery/config/ 2>/dev/null || true
[ -d "$PAYLOAD/examples" ] && sudo cp -rf "$PAYLOAD/examples" /opt/spawnery/
[ -d "$PAYLOAD/web-dist" ] && sudo rsync -a "$PAYLOAD/web-dist/" /var/www/spawnery/

# ---- PKI: throwaway CA + AS session key + node/CP mTLS + the *.e2e.test wildcard cert + the
#      github.com/codeload.github.com cert (sp-wwtc.1) ----
log "generating throwaway PKI + wildcard cert…"
sudo mkdir -p /etc/spawnery/pki
sudo bash "$PAYLOAD/gen-pki.sh" /etc/spawnery/pki "$WILDCARD_DOMAIN"   # writes root.pem/ca.crt, session-key/pub, node/cp certs, wildcard.{crt,key}, github.{crt,key}
sudo chmod 644 /etc/spawnery/pki/wildcard.crt /etc/spawnery/pki/wildcard.key   # caddy runs as user 'caddy'
sudo chmod 644 /etc/spawnery/pki/github.crt /etc/spawnery/pki/github.key      # caddy runs as user 'caddy'
sudo cp /etc/spawnery/pki/ca.crt /home/build/ca.crt   # build-base.sh pulls this out for host trust

# ---- sidecar upstream CA trust bundle (sp-wwtc.3): system roots + the golden CA, MERGED (SSL_CERT_FILE
# REPLACES rather than appends — see cmd/spawnlet SIDECAR_CA_BUNDLE_FILE doc), in its OWN directory
# (never /etc/spawnery/pki itself — that dir also holds host PKI private keys, and this bundle is
# bind-mounted into the sidecar container; internal/spawnlet.SidecarCABundleMountPath). e2e/VM profile
# ONLY (profile.fake.env); the production sidecar image never sees this file. ----
log "building sidecar upstream CA trust bundle (system roots + golden CA)…"
sudo mkdir -p /etc/spawnery/sidecar-ca-bundle
SYS_CA_BUNDLE=""
for p in /etc/pki/tls/certs/ca-bundle.crt /etc/ssl/certs/ca-certificates.crt /etc/ssl/cert.pem; do
  [ -f "$p" ] && SYS_CA_BUNDLE="$p" && break
done
[ -n "$SYS_CA_BUNDLE" ] || { echo "ERR: no system CA bundle found (tried the usual Fedora/Debian paths) for the sidecar trust merge" >&2; exit 1; }
sudo bash -c "cat '$SYS_CA_BUNDLE' /etc/spawnery/pki/root.pem > /etc/spawnery/sidecar-ca-bundle/ca-bundle.crt"
sudo chmod 644 /etc/spawnery/sidecar-ca-bundle/ca-bundle.crt

# ---- cp.prod.yaml: patch the ${sops:} store DSN to the throwaway local Postgres (baseline; roll.sh
#      re-applies this after every config re-copy, since a fresh config/ ships the sops ref again) ----
sudo sed -i 's#\${sops:store.dsn}#postgres://spawnery:spawnery@127.0.0.1:5432/spawnery?sslmode=disable#' \
  /etc/spawnery/config/cp.prod.yaml || true

# ---- Caddy: TLS :443 wildcard cert, route to web/CP/AS; a HOST-MATCHED site block for
#      github.com/codeload.github.com (sp-wwtc.1) fronts Gitea over real TLS so the sidecar's GitHub
#      MITM proxy intercepts it exactly as it does in production. Gitea itself stays plain HTTP on
#      loopback (127.0.0.1:3000) — this reverse_proxy is what terminates TLS for it. ----
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

github.com, codeload.github.com {
  tls /etc/spawnery/pki/github.crt /etc/spawnery/pki/github.key
  reverse_proxy 127.0.0.1:3000
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

# ---- github.com DNS, from inside the pod netns (sp-wwtc.2) ----
# The CNI bridge/host-local IPAM combo (the "spawnery" conflist above) assigns the bridge's gateway
# address deterministically as POD_CIDR's first host address (host-local's default when no explicit
# "gateway" is set: network address + 1) — e.g. 10.234.0.1 for the default 10.234.0.0/16. That address
# is on THIS host (the VM), reachable from every pod via its default route, and is what Caddy's :443
# already listens on (Caddy binds all interfaces) — so it doubles as "the VM's IP" for in-pod DNS.
GITHUB_DNS_ADDR="$(printf '%s' "$POD_CIDR" | cut -d/ -f1 | awk -F. '{print $1"."$2"."$3".1"}')"
log "installing dnsmasq (github.com/codeload.github.com -> ${GITHUB_DNS_ADDR}, else forward to ${POD_DNS})…"
sudo mkdir -p /etc/dnsmasq.d
sudo tee /etc/dnsmasq.d/spawnery-github.conf >/dev/null <<EOF
# Answers ONLY github.com/codeload.github.com (exact names, no subdomain matching — host-record is
# exact-match, unlike address=/domain/) with the VM's address; everything else is forwarded to the
# same public resolvers POD_DNS used before this bead (no-resolv: never fall back to the host's own
# /etc/resolv.conf / systemd-resolved stub).
port=53
bind-dynamic
interface=spawnery-cni0
no-resolv
no-hosts
host-record=github.com,${GITHUB_DNS_ADDR}
host-record=codeload.github.com,${GITHUB_DNS_ADDR}
$(printf '%s' "$POD_DNS" | tr ',' '\n' | sed 's/^/server=/')
EOF
sudo systemctl enable dnsmasq

# Point the node's PodSpec.DNSServers (internal/runtime/cri/backend.go) at dnsmasq instead of the
# public resolvers directly, so github.com/codeload.github.com resolve in-pod without touching
# anything else's resolution (dnsmasq forwards everything else to the SAME POD_DNS servers). The
# template was just installed above (common.env.tmpl), so patch it in place; the per-boot render
# (spawnery-render-env.sh) copies this value through verbatim like every other non-@@…@@ line.
sudo sed -i "s#^POD_DNS=.*#POD_DNS=${GITHUB_DNS_ADDR}#" /etc/spawnery/env.d/common.env.tmpl

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
After=spawnery-render-env.service postgresql.service network-online.target spawnery-gitea-bootstrap.service
Requires=spawnery-render-env.service
# Gitea is a test-only git host: order after its bootstrap and consume its AS_FAKE_GITHUB_TOKEN
# fragment (sp-wwtc.4) via the optional EnvironmentFile below, but Wants= not Requires= so a Gitea
# failure degrades to AS's normal random-token fake GitHub rather than blocking AS entirely.
Wants=spawnery-gitea-bootstrap.service
[Service]
EnvironmentFile=/etc/spawnery/env.d/common.env
EnvironmentFile=-/etc/spawnery/env.d/profile.env
EnvironmentFile=-/etc/spawnery/env.d/gitea.env
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
After=spawnery-render-env.service spawnery-cp.service containerd.service spawnery-garage-bootstrap.service spawnery-gitea-bootstrap.service
Requires=spawnery-render-env.service spawnery-garage-bootstrap.service
# Gitea is a test-only git host: order after its bootstrap and consume its env fragment (optional
# EnvironmentFile below), but Wants= not Requires= so a Gitea failure only breaks git-mount tests,
# not the whole node (suspend/fork/etc. must stay up).
Wants=spawnery-gitea-bootstrap.service
[Service]
EnvironmentFile=/etc/spawnery/env.d/common.env
EnvironmentFile=-/etc/spawnery/env.d/profile.env
EnvironmentFile=-/etc/spawnery/env.d/journal.env
EnvironmentFile=-/etc/spawnery/env.d/gitea.env
WorkingDirectory=/opt/spawnery
ExecStart=/usr/local/bin/spawnlet
Restart=on-failure
[Install]
WantedBy=multi-user.target
EOF

sudo systemctl daemon-reload
sudo systemctl enable garage spawnery-garage-bootstrap gitea spawnery-gitea-bootstrap spawnery-render-env spawnery-authsvc spawnery-cp spawnery-node caddy

# ---- self-check (best-effort) + clean shutdown handled by build-base.sh ----
log "provision complete. runsc: $(runsc --version | head -1). containerd: $(/usr/local/bin/containerd --version)"
log "REMINDER: env templates installed at /etc/spawnery/env.d/*.tmpl; spawnery-render-env renders them (HOST/IP/profile) on every boot before the spawnery services start."
