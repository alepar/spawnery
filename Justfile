set dotenv-load := true              # auto-load .env (OPENROUTER_API_KEY) if present

repo         := justfile_directory()
addr         := "127.0.0.1:9090"
addr_cp      := "127.0.0.1:8080"
addr_cp_node := "127.0.0.1:8081"   # CP mTLS node listener (enforced mode)
addr_as      := "127.0.0.1:8090"
free         := "openai/gpt-oss-120b:free"
data_root    := repo / ".spawns"
devca        := repo / ".dev-ca"

# list recipes
default:
    @just --list

# --- run the dev stack ---------------------------------------------------

# spawnlet, foreground. agent = agent (opencode, default) | stub
spawnlet agent="agent": (_images agent)
    @bin=spawnery/{{ if agent == "stub" { "stubagent" } else { "agent" } }}:dev; \
    AGENT_IMAGE=$bin SIDECAR_IMAGE=spawnery/sidecar:dev \
    AGENT_BINARIES="{{ if agent == "stub" { "" } else { "opencode,goose,claude-code,codex,hermes" } }}" \
    DATA_ROOT={{data_root}} SPAWNLET_ADDR={{addr}} \
    OPENROUTER_API_KEY="${OPENROUTER_API_KEY:-unused}" \
    {{repo}}/bin/spawnlet

# control plane (foreground)
cp:
    @make bin/spawnery_cp
    CP_LISTEN={{addr_cp}} CP_DEV_TOKENS=dev-token=alice CP_TELEMETRY={{repo}}/telemetry/events.jsonl {{repo}}/bin/spawnery_cp

# auth service (foreground; dev = ephemeral in-memory CA, not for production)
authsvc:
    @make bin/authsvc
    AS_DEV=1 AS_LISTEN={{addr_as}} {{repo}}/bin/authsvc

# spawnlet attached to the CP — root-free dev node (self-hosted + egress floor off). `just node stub` = echo agent.
# Sources deploy/garage/dev-creds.env when present (written by `just garage`), enabling the
# transient-tier s3 journal against the dev Garage; without it journaling stays off.
# USERNS_MODE=remap (writable-rootfs, sp-ei4.1) gives the agent the default cap set so apt/useradd/
# chown work in the spawn. REQUIRES the Docker daemon to run with userns-remap — one-time host setup:
#   echo '{"userns-remap":"default"}' | sudo tee /etc/docker/daemon.json && sudo systemctl restart docker
# Without it, spawnlet probes the daemon, logs a warning, and FALLS BACK to cap-drop=ALL (apt fails).
node agent="agent": (_images agent)
    @bin=spawnery/{{ if agent == "stub" { "stubagent" } else { "agent" } }}:dev; \
    set -a; [ -f {{repo}}/deploy/garage/dev-creds.env ] && . {{repo}}/deploy/garage/dev-creds.env; set +a; \
    AGENT_IMAGE=$bin SIDECAR_IMAGE=spawnery/sidecar:dev DATA_ROOT={{data_root}} \
    AGENT_BINARIES="{{ if agent == "stub" { "" } else { "opencode,goose,claude-code,codex,hermes" } }}" \
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

# both, in mprocs panes (one Ctrl-C). Depends on `garage` so the transient-tier journal is
# backed before the node starts (compose runs detached + bootstrap writes dev-creds.env, which
# `just node` sources) — without it, journaled mounts silently fail to persist on suspend.
dev: garage
    mprocs

# full A4 signing path in dev: CP + node with intent flow enabled (CP_DEV_INTENT_ENABLED=1).
# Use `spawnctl create` to exercise the two-phase pollAndSign cycle end-to-end.
# NOT the default for `just dev` because the web SPA does not yet implement GetPendingIntent
# /SubmitIntent (A5 scope) — web-initiated spawns would hang at the await until TTL.
# When ready: `just cp-intent` in one pane, `just node` in another, `spawnctl create ...` from CLI.
cp-intent:
    @make bin/spawnery_cp
    CP_LISTEN={{addr_cp}} CP_DEV_TOKENS=dev-token=alice CP_TELEMETRY={{repo}}/telemetry/events.jsonl \
    CP_DEV_INTENT_ENABLED=1 \
    {{repo}}/bin/spawnery_cp

# --- enforced node-auth dev stack (mTLS node<->CP) -----------------------

# generate a LOCAL dev CA: root, self-hosted intermediate, CP node-listener server cert, and a
# pre-provisioned node identity (node-1/alice) into .dev-ca/. NOT for production.
gen-dev-ca:
    @make bin/spawnery-ca
    {{repo}}/bin/spawnery-ca dev {{devca}}

# control plane with enforced node auth: mTLS node listener on a second port; clients still use :8080.
cp-enforced:
    @make bin/cp
    CP_LISTEN={{addr_cp}} CP_DEV_TOKENS=dev-token=alice CP_TELEMETRY={{repo}}/telemetry/events.jsonl \
    NODE_AUTH_MODE=enforced CP_NODE_LISTEN={{addr_cp_node}} \
    CP_NODE_ROOT_CA={{devca}}/root.pem \
    CP_NODE_TLS_CERT={{devca}}/cp-server.pem CP_NODE_TLS_KEY={{devca}}/cp-server-key.pem \
    {{repo}}/bin/cp

# auth service loaded with the persistent dev CA (issues real certs; mints enrollment + session tokens).
authsvc-enforced:
    @make bin/authsvc
    AS_LISTEN={{addr_as}} \
    AS_DB_DSN="file:{{data_root}}/authsvc.db?_pragma=foreign_keys(1)" \
    AS_ROOT_CA_PEM={{devca}}/root.pem \
    AS_INTERMEDIATE_CERT_PEM={{devca}}/self-hosted-intermediate.pem \
    AS_INTERMEDIATE_KEY_PEM={{devca}}/self-hosted-intermediate-key.pem \
    {{repo}}/bin/authsvc

# node with enforced auth: pre-provisioned identity from .dev-ca/node, mTLS to the CP node listener.
node-enforced agent="agent": (_images agent)
    @bin=spawnery/{{ if agent == "stub" { "stubagent" } else { "agent" } }}:dev; \
    AGENT_IMAGE=$bin SIDECAR_IMAGE=spawnery/sidecar:dev DATA_ROOT={{data_root}} \
    CP_ADDR=http://{{addr_cp}} NODE_AUTH_MODE=enforced \
    CP_NODE_ADDR=https://{{addr_cp_node}} NODE_ID=node-1 NODE_ID_DIR={{devca}}/node \
    NODE_CLASS=self-hosted EGRESS_ENFORCE=false \
    NODE_ADVERTISE_IP=127.0.0.1 NODE_TERMINAL_ADDR=127.0.0.1:9092 \
    OPENROUTER_API_KEY="${OPENROUTER_API_KEY:-unused}" \
    {{repo}}/bin/spawnlet

# enforced dev stack in mprocs (generates .dev-ca first if missing). Real mTLS between node and CP.
dev-enforced:
    @test -f {{devca}}/root.pem || just gen-dev-ca
    mprocs --config {{repo}}/mprocs-enforced.yaml

# one-shot spawnctl against the running spawnlet
spawnctl prompt="What is the secret word?" model=free:
    @make bin/spawnctl
    printf '%s\n' "{{prompt}}" | {{repo}}/bin/spawnctl -addr http://{{addr}} -app {{repo}}/examples/secret-app -model {{model}}

# --- tests (actions) -----------------------------------------------------

test:
    go test ./... -count=1

test-web:
    cd web && npm test

test-e2e:
    make images
    go test -tags e2e ./... -count=1 -v

# output the Garage S3 env vars for the journaler from deploy/garage/dev-creds.env.
# Pipe into `export $(just test-garage-env | tr -d " ")` before running e2e tests.
test-garage-env:
    @[ -f {{repo}}/deploy/garage/dev-creds.env ] && cat {{repo}}/deploy/garage/dev-creds.env || true

# suspend/resume lifecycle e2e (sp-u53.7.9): real CP + real node + real Docker pods.
# Sources deploy/garage/dev-creds.env for the S3 journal; FAILS (not skips) if Garage is down.
test-e2e-lifecycle:
    make images
    @set -a; [ -f {{repo}}/deploy/garage/dev-creds.env ] && . {{repo}}/deploy/garage/dev-creds.env; set +a; \
    go test -tags e2e -run TestSuspendResumeLifecycleE2E -v -count=1 -timeout 5m ./internal/cp/

# Real node-mTLS -> AS GitHub mint/refresh leg (sp-v40s.19). The node-identity + refresher wiring
# sub-tests are deterministic and always run; TestGitHubE2E_Rotation needs the throwaway App
# (app_id=4065493) creds and FAILS (does not skip) without them: GITHUB_E2E_REFRESH_TOKEN (single-use
# — the test logs its rotated successor to re-seed), GITHUB_CLIENT_ID, GITHUB_CLIENT_SECRET.
test-github:
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

# --- housekeeping --------------------------------------------------------

# install dev tooling not in the repo (mprocs, playwright browser, web deps)
setup:
    command -v mprocs >/dev/null || cargo install mprocs
    cd web && npm install && npx playwright install chromium

# reap containers the dev stack leaked (sp-8hf)
reap:
    -docker rm -f $(docker ps -aq --filter ancestor=spawnery/agent:dev --filter ancestor=spawnery/stubagent:dev --filter ancestor=spawnery/sidecar:dev) 2>/dev/null
