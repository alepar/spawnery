set dotenv-load := true              # auto-load .env (OPENROUTER_API_KEY) if present

repo      := justfile_directory()
addr      := "127.0.0.1:9090"
addr_cp   := "127.0.0.1:8080"
free      := "openai/gpt-oss-120b:free"
data_root := repo / ".spawns"

# list recipes
default:
    @just --list

# --- run the dev stack ---------------------------------------------------

# spawnlet, foreground. agent = goose (default) | stub
spawnlet agent="goose": (_images agent)
    @bin=spawnery/{{ if agent == "stub" { "stubagent" } else { "goose" } }}:dev; \
    AGENT_IMAGE=$bin SIDECAR_IMAGE=spawnery/sidecar:dev \
    DATA_ROOT={{data_root}} SPAWNLET_ADDR={{addr}} \
    OPENROUTER_API_KEY="${OPENROUTER_API_KEY:-unused}" \
    {{repo}}/bin/spawnlet

# control plane (foreground)
cp:
    @make bin/cp
    CP_LISTEN={{addr_cp}} CP_DEV_TOKENS=dev-token=alice CP_TELEMETRY={{repo}}/telemetry/events.jsonl {{repo}}/bin/cp

# spawnlet attached to the CP — root-free dev node (self-hosted + egress floor off). `just node stub` = echo agent.
node agent="goose": (_images agent)
    @bin=spawnery/{{ if agent == "stub" { "stubagent" } else { "goose" } }}:dev; \
    AGENT_IMAGE=$bin SIDECAR_IMAGE=spawnery/sidecar:dev DATA_ROOT={{data_root}} \
    CP_ADDR=http://{{addr_cp}} NODE_ID=node-1 \
    NODE_CLASS=self-hosted EGRESS_ENFORCE=false \
    OPENROUTER_API_KEY="${OPENROUTER_API_KEY:-unused}" \
    {{repo}}/bin/spawnlet

# build only the artifacts the chosen agent needs
_images agent="goose":
    make bin/spawnlet .make/img-sidecar .make/img-{{ if agent == "stub" { "stubagent" } else { "goose" } }}

# web UI (vite, LAN-accessible)
web:
    cd web && npm run dev -- --host

# both, in mprocs panes (one Ctrl-C)
dev:
    mprocs

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

test-web-e2e:
    cd web && npm run test:e2e

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

# --- housekeeping --------------------------------------------------------

# install dev tooling not in the repo (mprocs, playwright browser, web deps)
setup:
    command -v mprocs >/dev/null || cargo install mprocs
    cd web && npm install && npx playwright install chromium

# reap containers the dev stack leaked (sp-8hf)
reap:
    -docker rm -f $(docker ps -aq --filter ancestor=spawnery/goose:dev --filter ancestor=spawnery/stubagent:dev --filter ancestor=spawnery/sidecar:dev) 2>/dev/null
