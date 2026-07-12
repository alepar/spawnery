set dotenv-load := true              # auto-load .env (OPENROUTER_API_KEY) if present

repo         := justfile_directory()
addr         := "127.0.0.1:9090"
addr_cp      := "127.0.0.1:8080"
addr_cp_node := "127.0.0.1:8081"   # CP mTLS node listener (enforced mode)
addr_as      := "127.0.0.1:8090"
addr_as_internal := "127.0.0.1:8091"
# Web origin the BROWSER uses to reach the dev SPA (vite). For LAN/remote access without an SSH
# tunnel this is the box's domain; override via DEV_WEB_ORIGIN. It is the AS's single canonical SPA
# origin and must be a registered GitHub App callback host (login: <origin>/oauth/callback, link:
# <origin>/github/link/callback). Also add the host to vite `allowedHosts` (web/vite.config.ts).
dev_web_origin := env_var_or_default("DEV_WEB_ORIGIN", "https://blacky.dayton:5173")
free         := "openai/gpt-oss-120b:free"
data_root    := repo / ".envs/dev/data"
devca        := repo / ".envs/dev/dev-ca"

# list recipes
default:
    @just --list

# --- run the dev stack ---------------------------------------------------

# spawnlet, foreground. agent = agent (opencode, default) | stub
spawnlet agent="agent": (_images agent)
    @bin=spawnery/{{ if agent == "stub" { "stubagent" } else { "agent" } }}:dev; \
    SPAWNERY_ENV=dev \
    AGENT_IMAGE=$bin SIDECAR_IMAGE=spawnery/sidecar:dev \
    AGENT_BINARIES="{{ if agent == "stub" { "" } else { "opencode,goose,claude-code,codex,hermes,pi" } }}" \
    DATA_ROOT={{data_root}} SPAWNLET_ADDR={{addr}} \
    OPENROUTER_API_KEY="${OPENROUTER_API_KEY:-unused}" \
    {{repo}}/bin/spawnlet

# control plane (foreground)
cp:
    @make bin/spawnery_cp
    SPAWNERY_ENV=dev CP_LISTEN={{addr_cp}} CP_DEV_TOKENS=dev-token=alice CP_TELEMETRY={{repo}}/telemetry/events.jsonl \
    CP_INSECURE_DEV_NODE_ON_PUBLIC=true \
    {{repo}}/bin/spawnery_cp

# auth service (foreground; dev = ephemeral in-memory CA, not for production)
authsvc:
    @make bin/authsvc
    SPAWNERY_ENV=dev AS_DEV=1 AS_LISTEN={{addr_as}} {{repo}}/bin/authsvc

# spawnlet attached to the CP — root-free dev node (self-hosted + egress floor off). `just node stub` = echo agent.
# Sources deploy/garage/dev-creds.env when present (written by `just garage`), enabling the
# transient-tier s3 journal against the dev Garage; without it journaling stays off.
# USERNS_MODE=remap (writable-rootfs, sp-ei4.1) gives the agent the default cap set so apt/useradd/
# chown work in the spawn. REQUIRES the Docker daemon to run with userns-remap — one-time host setup:
#   echo '{"userns-remap":"default"}' | sudo tee /etc/docker/daemon.json && sudo systemctl restart docker
# Without it, spawnlet probes the daemon, logs a warning, and FALLS BACK to cap-drop=ALL (apt fails).
node agent="agent": (_images agent)
    @bin=spawnery/{{ if agent == "stub" { "stubagent" } else { "agent" } }}:dev; \
    set -a; [ -f {{repo}}/.envs/dev/garage-creds.env ] && . {{repo}}/.envs/dev/garage-creds.env; set +a; \
    SPAWNERY_ENV=dev \
    AGENT_IMAGE=$bin SIDECAR_IMAGE=spawnery/sidecar:dev DATA_ROOT={{data_root}} \
    AGENT_BINARIES="{{ if agent == "stub" { "" } else { "opencode,goose,claude-code,codex,hermes,pi" } }}" \
    CP_ADDR=http://{{addr_cp}} NODE_ID=node-1 \
    NODE_CLASS=self-hosted NODE_OWNER=alice EGRESS_ENFORCE=false \
    NODE_ADVERTISE_IP=127.0.0.1 NODE_TERMINAL_ADDR=127.0.0.1:9092 \
    USERNS_MODE=remap DELTA_CAPTURE=1 \
    OPENROUTER_API_KEY="${OPENROUTER_API_KEY:-unused}" \
    {{repo}}/bin/spawnlet

# build only the artifacts the chosen agent needs
_images agent="agent":
    make bin/spawnlet .make/img-sidecar .make/img-{{ if agent == "stub" { "stubagent" } else { "agent" } }}

# web UI (vite, LAN-accessible)
web:
    cd web && npm run dev -- --host

# web for the github lane: auth ENABLED so the SPA uses the AS session for CP calls AND signs spawn
# intents (pollAndSign). The github lane's CP runs the intent flow (CP_DEV_INTENT_ENABLED=1) and
# validates root-anchored AS sessions; the generic `just web` leaves auth off in dev
# (authEnabled()=false), so it would skip intent signing and every spawn would hang in 'starting'.
web-github:
    cd web && VITE_AUTH_ENABLED=1 npm run dev -- --host

# both, in mprocs panes (one Ctrl-C). Depends on `garage` so the transient-tier journal is
# backed before the node starts (compose runs detached + bootstrap writes dev-creds.env, which
# `just node` sources) — without it, journaled mounts silently fail to persist on suspend.
# Full dev stack in mprocs: CP + AS (GitHub OAuth/link) + node (github-mount, MITM proxy) + web +
# garage (journaled mounts). This is the mode where `gh` login/linking and git pushes work
# end-to-end. Generates the dev CA if missing. Requires GITHUB_CLIENT_ID/GITHUB_CLIENT_SECRET in
# .env. For the plain non-auth stack (no GitHub/mTLS), use `just dev-plain`.
dev: garage
    @test -f {{devca}}/root.pem || just gen-dev-ca
    @test -f {{repo}}/.envs/dev/web-tls/cert.pem || just gen-web-tls
    mprocs --config {{repo}}/mprocs-github.yaml

# Self-signed TLS cert for the dev web server (vite). Needed for a SECURE CONTEXT so the WebCrypto
# auth flow (crypto.subtle) works when the SPA is served on a LAN host over HTTPS without an SSH
# tunnel (http://localhost is a secure context; http://<lan-host> is not). SANs: blacky.dayton +
# localhost + 127.0.0.1. Browser shows a one-time "proceed anyway" warning (self-signed).
gen-web-tls:
    @mkdir -p {{repo}}/.envs/dev/web-tls
    openssl req -x509 -newkey rsa:2048 -nodes \
      -keyout {{repo}}/.envs/dev/web-tls/key.pem -out {{repo}}/.envs/dev/web-tls/cert.pem -days 3650 \
      -subj "/CN=blacky.dayton" \
      -addext "subjectAltName=DNS:blacky.dayton,DNS:localhost,IP:127.0.0.1"

# plain non-auth dev stack (no GitHub/mTLS) — the previous `just dev`.
dev-plain: garage
    mprocs

# full A4 signing path in dev: CP + node with intent flow enabled (CP_DEV_INTENT_ENABLED=1).
# Use `spawnctl create` to exercise the two-phase pollAndSign cycle end-to-end.
# NOT the default for `just dev` because the web SPA does not yet implement GetPendingIntent
# /SubmitIntent (A5 scope) — web-initiated spawns would hang at the await until TTL.
# When ready: `just cp-intent` in one pane, `just node` in another, `spawnctl create ...` from CLI.
cp-intent:
    @make bin/spawnery_cp
    SPAWNERY_ENV=dev CP_LISTEN={{addr_cp}} CP_DEV_TOKENS=dev-token=alice CP_TELEMETRY={{repo}}/telemetry/events.jsonl \
    CP_DEV_INTENT_ENABLED=1 \
    CP_INSECURE_DEV_NODE_ON_PUBLIC=true \
    {{repo}}/bin/spawnery_cp

# --- enforced node-auth dev stack (mTLS node<->CP) -----------------------

# generate a LOCAL dev CA: root, self-hosted intermediate, CP node-listener server cert, and a
# pre-provisioned node identity (node-1/alice) into .dev-ca/. NOT for production.
gen-dev-ca:
    @make bin/spawnery-ca
    {{repo}}/bin/spawnery-ca dev {{devca}}

# control plane with enforced node auth: mTLS node listener on a second port; clients still use :8080.
cp-enforced:
    @make bin/spawnery_cp
    @install -d -m700 {{repo}}/.envs/dev/revocations/cp-certificates {{repo}}/.envs/dev/revocations/cp-signers
    SPAWNERY_ENV=dev CP_LISTEN={{addr_cp}} CP_DEV_TOKENS=dev-token=alice CP_TELEMETRY={{repo}}/telemetry/events.jsonl \
    CP_AUTH_ENVIRONMENT=dev CP_AUTH_ROOT_CA={{devca}}/root.pem CP_AUTH_SIGNER_REVOCATION_STATE={{repo}}/.envs/dev/revocations/cp-signers/state.json \
    CP_INTERNAL_LISTEN={{addr_cp_node}} CP_INTERNAL_TRUST_DOMAIN=dev.spawnery.internal \
    CP_INTERNAL_ROOT_CA={{devca}}/root.pem CP_INTERNAL_TLS_CERT={{devca}}/cp-service.pem \
    CP_INTERNAL_TLS_CHAIN={{devca}}/cp-service-chain.pem CP_INTERNAL_TLS_KEY={{devca}}/cp-service-key.pem \
    CP_INTERNAL_SERVER_NAME=authsvc.internal CP_INTERNAL_REVOCATION_STATE={{repo}}/.envs/dev/revocations/cp-certificates/state.json \
    CP_INTERNAL_REVOCATION_ISSUERS={{devca}}/service-intermediate.pem,{{devca}}/cloud-intermediate.pem,{{devca}}/self-hosted-intermediate.pem \
    CP_INTERNAL_REVOCATION_CRLS={{devca}}/service.crl.pem,{{devca}}/cloud-node.crl.pem,{{devca}}/self-hosted-node.crl.pem \
    CP_INTERNAL_REVOCATION_REFRESH=5s \
    {{repo}}/bin/spawnery_cp

# auth service loaded with the persistent dev CA (issues real certs; mints enrollment + session tokens).
authsvc-enforced:
    @make bin/authsvc
    @mkdir -p {{data_root}}
    SPAWNERY_ENV=dev AS_LISTEN={{addr_as}} \
    AS_DB_DSN="file:{{data_root}}/authsvc.db?_pragma=foreign_keys(1)" \
    AS_ROOT_CA_PEM={{devca}}/root.pem \
    AS_INTERMEDIATE_CERT_PEM={{devca}}/self-hosted-intermediate.pem \
    AS_INTERMEDIATE_KEY_PEM={{devca}}/self-hosted-intermediate-key.pem \
    AS_AUTH_SIGNING_ENVIRONMENT=dev AS_AUTH_SIGNING_ROOT_PEM={{devca}}/root.pem \
    AS_AUTH_SIGNING_CURRENT_KEY_PEM={{devca}}/auth-signer-current-key.pem AS_AUTH_SIGNING_CURRENT_CHAIN_PEM={{devca}}/auth-signer-current-chain.pem \
    AS_AUTH_SIGNING_NEXT_KEY_PEM={{devca}}/auth-signer-next-key.pem AS_AUTH_SIGNING_NEXT_CHAIN_PEM={{devca}}/auth-signer-next-chain.pem \
    {{repo}}/bin/authsvc

# node with enforced auth: pre-provisioned identity from .dev-ca/node, mTLS to the CP node listener.
node-enforced agent="agent": (_images agent)
    @bin=spawnery/{{ if agent == "stub" { "stubagent" } else { "agent" } }}:dev; \
    install -d -m700 {{repo}}/.envs/dev/revocations/node-enforced-certificates {{repo}}/.envs/dev/revocations/node-enforced-signers; \
    SPAWNERY_ENV=dev \
    AGENT_IMAGE=$bin SIDECAR_IMAGE=spawnery/sidecar:dev DATA_ROOT={{data_root}} \
    CP_ADDR=http://{{addr_cp}} NODE_AUTH_MODE=enforced \
    CP_NODE_ADDR=https://{{addr_cp_node}} CP_SERVER_NAME=cp.internal NODE_ID=node-1 NODE_ID_DIR={{devca}}/node \
    NODE_ROOT_CA={{devca}}/root.pem NODE_TRUST_DOMAIN=dev.spawnery.internal NODE_AUTH_ENVIRONMENT=dev \
    NODE_SIGNER_REVOCATION_STATE={{repo}}/.envs/dev/revocations/node-enforced-signers/state.json \
    NODE_CERTIFICATE_REVOCATION_STATE={{repo}}/.envs/dev/revocations/node-enforced-certificates/state.json \
    NODE_CERTIFICATE_REVOCATION_ISSUERS={{devca}}/service-intermediate.pem,{{devca}}/cloud-intermediate.pem,{{devca}}/self-hosted-intermediate.pem \
    NODE_CERTIFICATE_REVOCATION_CRLS={{devca}}/service.crl.pem,{{devca}}/cloud-node.crl.pem,{{devca}}/self-hosted-node.crl.pem \
    NODE_CLASS=self-hosted EGRESS_ENFORCE=false \
    NODE_ADVERTISE_IP=127.0.0.1 NODE_TERMINAL_ADDR=127.0.0.1:9092 \
    OPENROUTER_API_KEY="${OPENROUTER_API_KEY:-unused}" \
    {{repo}}/bin/spawnlet

# enforced dev stack in mprocs (generates .dev-ca first if missing). Real mTLS between node and CP.
dev-enforced:
    @test -f {{devca}}/root.pem || just gen-dev-ca
    mprocs --config {{repo}}/mprocs-enforced.yaml

# --- github-mount dev stack (real GitHub App + journaled mounts + internal mTLS) -----------------

# AS for the github lane: provisioned dev CA + real GitHub App link/mint + direct internal mTLS.
authsvc-github:
    @make bin/authsvc
    @mkdir -p {{data_root}}; install -d -m700 {{repo}}/.envs/dev/revocations/authsvc-certificates
    SPAWNERY_ENV=dev AS_DEV=1 AS_LISTEN={{addr_as}} \
    AS_DB_DSN="file:{{data_root}}/authsvc.db?_pragma=foreign_keys(1)" \
    AS_ROOT_CA_PEM={{devca}}/root.pem \
    AS_INTERMEDIATE_CERT_PEM={{devca}}/self-hosted-intermediate.pem \
    AS_INTERMEDIATE_KEY_PEM={{devca}}/self-hosted-intermediate-key.pem \
    AS_INTERNAL_LISTEN={{addr_as_internal}} AS_INTERNAL_TRUST_DOMAIN=dev.spawnery.internal \
    AS_INTERNAL_ROOT_CA={{devca}}/root.pem \
    AS_INTERNAL_CERT={{devca}}/authsvc-service.pem AS_INTERNAL_CHAIN={{devca}}/authsvc-service-chain.pem \
    AS_INTERNAL_KEY={{devca}}/authsvc-service-key.pem AS_INTERNAL_SERVER_NAME=authsvc.internal \
    AS_INTERNAL_REVOCATION_STATE={{repo}}/.envs/dev/revocations/authsvc-certificates/state.json \
    AS_INTERNAL_REVOCATION_ISSUERS={{devca}}/service-intermediate.pem,{{devca}}/cloud-intermediate.pem,{{devca}}/self-hosted-intermediate.pem \
    AS_INTERNAL_REVOCATION_CRLS={{devca}}/service.crl.pem,{{devca}}/cloud-node.crl.pem,{{devca}}/self-hosted-node.crl.pem \
    AS_AUTH_SIGNING_ENVIRONMENT=dev AS_AUTH_SIGNING_ROOT_PEM={{devca}}/root.pem \
    AS_AUTH_SIGNING_CURRENT_KEY_PEM={{devca}}/auth-signer-current-key.pem AS_AUTH_SIGNING_CURRENT_CHAIN_PEM={{devca}}/auth-signer-current-chain.pem \
    AS_AUTH_SIGNING_NEXT_KEY_PEM={{devca}}/auth-signer-next-key.pem AS_AUTH_SIGNING_NEXT_CHAIN_PEM={{devca}}/auth-signer-next-chain.pem \
    AS_GITHUB_TOKEN_ENC_KEY="$(printf %s 'spawnery-dev-github-mount-enck32' | base64)" \
    GITHUB_CLIENT_ID="${GITHUB_CLIENT_ID:?set GITHUB_CLIENT_ID in .env (GitHub App client_id)}" \
    GITHUB_CLIENT_SECRET="${GITHUB_CLIENT_SECRET:?set GITHUB_CLIENT_SECRET in .env}" \
    AS_PUBLIC_URL={{dev_web_origin}} \
    AS_GITHUB_LINK_REDIRECT_URI=${AS_GITHUB_LINK_REDIRECT_URI:-{{dev_web_origin}}/github/link/callback} \
    AS_GITHUB_POST_REDEEM_REDIRECT=${AS_GITHUB_POST_REDEEM_REDIRECT:-{{dev_web_origin}}/settings} \
    AS_REDIRECT_URIS=http://127.0.0.1/cb,{{dev_web_origin}}/callback,http://localhost:5173/callback \
    AS_CP_URL=https://{{addr_cp_node}} AS_CP_SERVER_NAME=cp.internal \
    {{repo}}/bin/authsvc

# CP for the github lane: enforced node mTLS + mount-intent signing (pollAndSign) + AS->CP RPC auth.
# Validates real AS session artifacts under the environment root so the spawn owner == the AS accountID
# the GitHub link is filed under — the gh:<owner> mount credential resolves end-to-end. The
# dev-token shortcut stays available for non-github smoke tests.
cp-github:
    @make bin/spawnery_cp
    @test -f {{devca}}/auth-signer-current-chain.pem || just gen-dev-ca
    @install -d -m700 {{repo}}/.envs/dev/revocations/cp-certificates {{repo}}/.envs/dev/revocations/cp-signers
    SPAWNERY_ENV=dev CP_LISTEN={{addr_cp}} CP_DEV_TOKENS=dev-token=${CP_DEV_OWNER:-alice} CP_TELEMETRY={{repo}}/telemetry/events.jsonl \
    CP_STORE_DSN="file:{{repo}}/.envs/dev/cp.db?_pragma=busy_timeout(5000)" \
    CP_AUTH_ENVIRONMENT=dev CP_AUTH_ROOT_CA={{devca}}/root.pem CP_AUTH_SIGNER_REVOCATION_STATE={{repo}}/.envs/dev/revocations/cp-signers/state.json \
    CP_INTERNAL_LISTEN={{addr_cp_node}} CP_INTERNAL_TRUST_DOMAIN=dev.spawnery.internal \
    CP_INTERNAL_ROOT_CA={{devca}}/root.pem CP_INTERNAL_TLS_CERT={{devca}}/cp-service.pem \
    CP_INTERNAL_TLS_CHAIN={{devca}}/cp-service-chain.pem CP_INTERNAL_TLS_KEY={{devca}}/cp-service-key.pem \
    CP_INTERNAL_SERVER_NAME=authsvc.internal CP_INTERNAL_REVOCATION_STATE={{repo}}/.envs/dev/revocations/cp-certificates/state.json \
    CP_INTERNAL_REVOCATION_ISSUERS={{devca}}/service-intermediate.pem,{{devca}}/cloud-intermediate.pem,{{devca}}/self-hosted-intermediate.pem \
    CP_INTERNAL_REVOCATION_CRLS={{devca}}/service.crl.pem,{{devca}}/cloud-node.crl.pem,{{devca}}/self-hosted-node.crl.pem \
    CP_INTERNAL_REVOCATION_REFRESH=5s \
    CP_DEV_INTENT_ENABLED=1 \
    CP_AS_URL=https://{{addr_as_internal}} \
    CP_ALLOWED_ORIGINS=${CP_ALLOWED_ORIGINS:-{{dev_web_origin}},http://localhost:5173,https://localhost:5173} \
    {{repo}}/bin/spawnery_cp

# Node for the github lane: cloud-class (multi-tenant), one node SVID reused for CP and AS mTLS,
# and journaled mounts. EGRESS_FLOOR_FORCE_OFF remains the rootless local-network compatibility.
node-github agent="agent": (_images agent)
    @bin=spawnery/{{ if agent == "stub" { "stubagent" } else { "agent" } }}:dev; \
    install -d -m700 {{repo}}/.envs/dev/revocations/node-certificates {{repo}}/.envs/dev/revocations/node-signers; \
    set -a; [ -f {{repo}}/.envs/dev/garage-creds.env ] && . {{repo}}/.envs/dev/garage-creds.env; set +a; \
    SPAWNERY_ENV=dev \
    AGENT_IMAGE=$bin SIDECAR_IMAGE=spawnery/sidecar:dev DATA_ROOT={{data_root}} \
    AGENT_BINARIES="{{ if agent == "stub" { "" } else { "opencode,goose,claude-code,codex,hermes,pi" } }}" \
    CP_ADDR=http://{{addr_cp}} NODE_AUTH_MODE=enforced \
    CP_NODE_ADDR=https://{{addr_cp_node}} CP_SERVER_NAME=cp.internal NODE_ID=node-1 NODE_ID_DIR={{devca}}/node-cloud \
    NODE_CLASS=cloud EGRESS_FLOOR_FORCE_OFF=1 \
    NODE_ROOT_CA={{devca}}/root.pem NODE_TRUST_DOMAIN=dev.spawnery.internal NODE_AUTH_ENVIRONMENT=dev \
    NODE_SIGNER_REVOCATION_STATE={{repo}}/.envs/dev/revocations/node-signers/state.json \
    NODE_CERTIFICATE_REVOCATION_STATE={{repo}}/.envs/dev/revocations/node-certificates/state.json \
    NODE_CERTIFICATE_REVOCATION_ISSUERS={{devca}}/service-intermediate.pem,{{devca}}/cloud-intermediate.pem,{{devca}}/self-hosted-intermediate.pem \
    NODE_CERTIFICATE_REVOCATION_CRLS={{devca}}/service.crl.pem,{{devca}}/cloud-node.crl.pem,{{devca}}/self-hosted-node.crl.pem \
    NODE_ADVERTISE_IP=127.0.0.1 NODE_TERMINAL_ADDR=127.0.0.1:9092 \
    USERNS_MODE=remap DELTA_CAPTURE=1 \
    AS_URL=https://{{addr_as_internal}} AS_SERVER_NAME=authsvc.internal \
    OPENROUTER_API_KEY="${OPENROUTER_API_KEY:-unused}" \
    {{repo}}/bin/spawnlet

# alias — the github-mount stack is now the default `just dev`.
dev-github: dev

# one-shot spawnctl against the running spawnlet
spawnctl prompt="What is the secret word?" model=free:
    @make bin/spawnctl
    printf '%s\n' "{{prompt}}" | SPAWNERY_ENV=dev {{repo}}/bin/spawnctl -addr http://{{addr}} -app {{repo}}/examples/secret-app -model {{model}}

# --- tests (actions) -----------------------------------------------------

test:
    go test ./... -count=1

test-web:
    cd web && npm test

test-e2e:
    make images
    go test -tags e2e ./... -count=1 -v

# output the Garage S3 env vars for the journaler from .envs/dev/garage-creds.env.
# Pipe into `export $(just test-garage-env | tr -d " ")` before running e2e tests.
test-garage-env:
    @[ -f {{repo}}/.envs/dev/garage-creds.env ] && cat {{repo}}/.envs/dev/garage-creds.env || true

# suspend/resume lifecycle e2e (sp-u53.7.9): real CP + real node + real Docker pods.
# Sources deploy/garage/dev-creds.env for the S3 journal; FAILS (not skips) if Garage is down.
test-e2e-lifecycle:
    make images
    @set -a; [ -f {{repo}}/.envs/dev/garage-creds.env ] && . {{repo}}/.envs/dev/garage-creds.env; set +a; \
    go test -tags e2e -run TestSuspendResumeLifecycleE2E -v -count=1 -timeout 5m ./internal/cp/

# Real node-mTLS -> AS GitHub mint/refresh leg (sp-v40s.19). The node-identity + refresher wiring
# sub-tests are deterministic and always run; TestGitHubE2E_Rotation needs the throwaway App
# (app_id=4065493) creds and FAILS (does not skip) without them: GITHUB_E2E_REFRESH_TOKEN (single-use
# — the test logs its rotated successor to re-seed), GITHUB_CLIENT_ID, GITHUB_CLIENT_SECRET.
test-github-mint:
    CGO_ENABLED=1 go test -tags github_e2e -run TestGitHubE2E -v -count=1 ./internal/node/

test-web-e2e:
    cd web && npm run test:e2e

# CSP-enforced prod-bundle Playwright suite (W1, sp-2ckv.6).
# Builds once (with pinned origins from .env.production if present), then runs
# the csp-prod.spec.ts suite against vite preview + dist/_headers emulation.
# Host-gated: skipped if PLAYWRIGHT_BROWSERS_UNAVAILABLE is set or browsers are absent.
web-csp:
    cd web && npm run build && npm run test:e2e:csp

# egress-floor enforcement e2e — REAL iptables on a privileged host (needs Docker + iptables + root).
# Compiles as the current user (warm cache), runs the binary as root with iptables/nsenter/docker on PATH.
test-egress:
    docker pull -q curlimages/curl:latest
    go test -tags egress_e2e -c -o /tmp/egress.test ./internal/spawnlet/firewall/
    sudo env "PATH=/sbin:/usr/sbin:/usr/bin:/bin:$(dirname $(command -v nsenter)):$(dirname $(command -v docker))" /tmp/egress.test -test.run TestEgressFloorEnforced -test.v -test.count=1

# CRI/CNI egress-floor enforcement e2e — REAL iptables on a privileged host (needs Docker + iptables + root).
test-cni-egress:
    docker pull -q curlimages/curl:latest
    go test -tags cni_egress_e2e -c -o /tmp/cni-egress.test ./internal/spawnlet/firewall/
    sudo env "PATH=/sbin:/usr/sbin:/usr/bin:/bin:$(dirname $(command -v docker))" /tmp/cni-egress.test -test.run TestCNIEgressFloorEnforced -test.v -test.count=1

# CRI/containerd delta-only export/import e2e (sp-ei4.1.16). Stands up a DEDICATED containerd
# (own root/state/socket — never touches the system daemon), pulls the base image, runs the
# cri_delta_e2e round-trip (Capture→ExportTopLayer→AssembleOnBase→unpack+run, asserting the
# uncompressed delta layer materializes + the whiteout applies), then tears down. Needs root +
# containerd + ctr + runc on the host.
test-cri-delta:
    #!/usr/bin/env bash
    set -euo pipefail
    sock=/run/spawnery-cde2e/c.sock; root=/var/tmp/spawnery-cde2e
    go test -tags cri_delta_e2e -c -o /tmp/cde2e.test ./internal/runtime/cri/
    sudo mkdir -p "$root/root" /run/spawnery-cde2e
    printf 'version = 3\nroot = "%s/root"\nstate = "/run/spawnery-cde2e"\n[grpc]\n  address = "%s"\n' "$root" "$sock" | sudo tee "$root/config.toml" >/dev/null
    cleanup(){ sudo systemctl stop spawnery-cde2e 2>/dev/null || true; sudo rm -rf "$root" /run/spawnery-cde2e; }
    trap cleanup EXIT
    sudo systemctl reset-failed spawnery-cde2e 2>/dev/null || true
    sudo systemd-run --unit=spawnery-cde2e --collect containerd --config "$root/config.toml"
    for i in $(seq 1 30); do sudo ctr --address "$sock" version >/dev/null 2>&1 && break; sleep 0.5; done
    sudo ctr --address "$sock" -n k8s.io images pull --snapshotter overlayfs docker.io/library/debian:stable
    sudo env "PATH=/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin" CONTAINERD_ADDRESS="$sock" \
        BASE_IMAGE=docker.io/library/debian:stable \
        /tmp/cde2e.test -test.run TestCRIDeltaOnlyRoundTrip -test.v -test.count=1

# --- lint (correctness-focused: bugs, not formatting/style) --------------

# run all linters
lint: lint-go lint-web

# backend: golangci-lint (.golangci.yml). The binary MUST be built with go >= go.mod's version (1.26),
# else it refuses to run. Install once:
#   GOTOOLCHAIN=go1.26.0 go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.8.0
lint-go:
    GOTOOLCHAIN=go1.26.0 "$(go env GOPATH)/bin/golangci-lint" run ./...

# frontend: eslint + tsc (no emit)
lint-web:
    cd web && npx eslint . && npx tsc --noEmit

# --- garage (transient-tier journal object store) ------------------------

# bring up a single-node dev Garage + apply cluster layout + mint a dev bucket/key (sp-u53.5; deploy/garage)
garage:
    docker compose -f {{repo}}/deploy/garage/docker-compose.yml up -d
    {{repo}}/deploy/garage/bootstrap.sh

# tear down the dev Garage AND drop its data volumes
garage-down:
    docker compose -f {{repo}}/deploy/garage/docker-compose.yml down -v

# live S3 round-trip against a running `just garage` (build-tagged garage_e2e; needs Docker)
test-garage:
    GARAGE_S3_ENDPOINT=127.0.0.1:3900 \
    GARAGE_ADMIN_ENDPOINT=http://127.0.0.1:3903 \
    GARAGE_ADMIN_TOKEN="$(awk -F'\"' '/^admin_token/{print $2}' {{repo}}/deploy/garage/garage.toml)" \
    go test -tags garage_e2e -run TestS3BackendRoundTripGarage -v -count=1 {{repo}}/internal/storage/journal/

# GitHub backend e2e: create->agent commit->suspend->resume against a local Gitea (sp-u53.1.5).
# Requires a running Gitea: set GITEA_URL, GITEA_TOKEN, GITEA_OWNER before running.
#   docker run -d -p 3000:3000 gitea/gitea:latest   # start gitea
#   GITEA_URL=http://localhost:3000 GITEA_TOKEN=<tok> GITEA_OWNER=<user> just test-github-storage
test-github-storage:
    go test -tags github_e2e -run TestGitHub -v -count=1 ./internal/storage/

# --- housekeeping --------------------------------------------------------

# install dev tooling not in the repo (mprocs, playwright browser, web deps)
setup:
    command -v mprocs >/dev/null || cargo install mprocs
    cd web && npm install && npx playwright install chromium

# reap containers the dev stack leaked (sp-8hf)
reap:
    -docker rm -f $(docker ps -aq --filter ancestor=spawnery/agent:dev --filter ancestor=spawnery/stubagent:dev --filter ancestor=spawnery/sidecar:dev) 2>/dev/null
