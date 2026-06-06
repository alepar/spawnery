# opencode agent image for the spawnery slice.
#
# Runs `opencode serve` (headless HTTP+SSE) plus the in-pod acpadapter, which
# presents canonical ACP to the node and normalizes opencode's API. The TUI
# (Phase 2) attaches in-pod via `opencode attach` inside tmux. opencode reaches
# the model provider through the sidecar's OpenAI-compatible endpoint
# (127.0.0.1:8080), which injects the real OpenRouter key.
#
# Pinned opencode version — see docs/superpowers/notes/opencode-api-pinned.md.
FROM debian:stable AS fetch
ARG OPENCODE_VERSION=v1.15.13
ARG OPENCODE_ASSET=opencode-linux-x64.tar.gz
RUN apt-get update \
 && apt-get install -y --no-install-recommends ca-certificates curl \
 && rm -rf /var/lib/apt/lists/*
RUN curl -fsSL \
      "https://github.com/anomalyco/opencode/releases/download/${OPENCODE_VERSION}/${OPENCODE_ASSET}" \
      -o /tmp/opencode.tar.gz \
 && mkdir -p /tmp/oc \
 && tar -xzf /tmp/opencode.tar.gz -C /tmp/oc \
 && install -m 0755 "$(find /tmp/oc -type f -name opencode | head -1)" /usr/local/bin/opencode \
 && /usr/local/bin/opencode --version

FROM golang:1.26 AS gobuild
WORKDIR /src
COPY . .
RUN CGO_ENABLED=0 go build -o /acpadapter ./deploy/agent/acpadapter

FROM debian:stable
RUN apt-get update \
 && apt-get install -y --no-install-recommends ca-certificates tmux tini \
      procps less iproute2 curl \
      neovim htop btop dnsutils jq \
 && rm -rf /var/lib/apt/lists/*
COPY --from=fetch /usr/local/bin/opencode /usr/local/bin/opencode
COPY --from=gobuild /acpadapter /usr/local/bin/acpadapter
COPY deploy/agent/entrypoint.sh /entrypoint.sh
COPY deploy/agent/spawn-tui.sh /usr/local/bin/spawn-tui
RUN chmod +x /entrypoint.sh /usr/local/bin/spawn-tui
# Pre-run the one-time SQLite migration + models.json fetch at build time (network available here),
# so a fresh container is ready in seconds instead of doing a multi-minute first-run migration.
RUN sh -c 'opencode serve --port 4096 --hostname 127.0.0.1 >/tmp/m.log 2>&1 & P=$!; \
      for i in $(seq 1 120); do grep -q "server listening" /tmp/m.log && break; sleep 1; done; \
      kill "$P" 2>/dev/null || true; sleep 1; tail -3 /tmp/m.log'
WORKDIR /app
# tini reaps the opencode serve child; entrypoint launches serve + adapter.
ENTRYPOINT ["/usr/bin/tini", "--", "/entrypoint.sh"]
