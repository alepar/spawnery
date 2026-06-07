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
RUN CGO_ENABLED=0 go build -o /acpexec ./deploy/agent/acpexec

FROM debian:stable
RUN apt-get update \
 && apt-get install -y --no-install-recommends ca-certificates tmux tini \
      procps less iproute2 curl \
      neovim htop btop dnsutils jq \
      git openssh-client ripgrep fd-find unzip xz-utils build-essential cmake just \
 && rm -rf /var/lib/apt/lists/*
COPY --from=fetch /usr/local/bin/opencode /usr/local/bin/opencode
COPY --from=gobuild /acpadapter /usr/local/bin/acpadapter
COPY --from=gobuild /acpexec /usr/local/bin/acpexec
COPY deploy/agent/entrypoint.sh /entrypoint.sh
COPY deploy/agent/spawn-tui.sh /usr/local/bin/spawn-tui
COPY deploy/agent/spawn-tmux /usr/local/bin/spawn-tmux
RUN chmod +x /entrypoint.sh /usr/local/bin/spawn-tui /usr/local/bin/spawn-tmux

# goose CLI (Block) — TUI/ACP agent
RUN set -eux; \
    curl -fsSL -o /tmp/goose.tar.bz2 \
      "https://github.com/block/goose/releases/download/stable/goose-x86_64-unknown-linux-gnu.tar.bz2"; \
    tar -xjf /tmp/goose.tar.bz2 -C /usr/local/bin; \
    rm /tmp/goose.tar.bz2; \
    /usr/local/bin/goose --version

# claude-code (Anthropic) — native binary via the Debian apt repo
RUN set -eux; \
    install -d -m 0755 /etc/apt/keyrings; \
    curl -fsSL https://downloads.claude.ai/keys/claude-code.asc -o /etc/apt/keyrings/claude-code.asc; \
    echo "deb [signed-by=/etc/apt/keyrings/claude-code.asc] https://downloads.claude.ai/claude-code/apt/stable stable main" > /etc/apt/sources.list.d/claude-code.list; \
    apt-get update; \
    apt-get install -y --no-install-recommends claude-code; \
    rm -rf /var/lib/apt/lists/*; \
    claude --version

# nori CLI (Tilework) — Rust ACP TUI client (spawnctl attach to acp-mode spawns; wiring in sp-9xr.12)
# tarball layout: ./nori/nori — extract to /tmp then install to /usr/local/bin
RUN set -eux; \
    curl -fsSL -o /tmp/nori.tar.gz \
      "https://github.com/tilework-tech/nori-cli/releases/download/nori-v0.22.0/nori-0.22.0-linux-x86_64.tar.gz"; \
    tar -xzf /tmp/nori.tar.gz -C /tmp; \
    install -m 0755 /tmp/nori/nori /usr/local/bin/nori; \
    rm -rf /tmp/nori.tar.gz /tmp/nori; \
    nori --version
# Pre-run the one-time SQLite migration + models.json fetch at build time (network available here),
# so a fresh container is ready in seconds instead of doing a multi-minute first-run migration.
RUN sh -c 'opencode serve --port 4096 --hostname 127.0.0.1 >/tmp/m.log 2>&1 & P=$!; \
      for i in $(seq 1 120); do grep -q "server listening" /tmp/m.log && break; sleep 1; done; \
      kill "$P" 2>/dev/null || true; sleep 1; tail -3 /tmp/m.log'
# The opencode prewarm above leaves /tmp owned non-root with restrictive perms. The agent runs
# with --cap-drop ALL (no CAP_DAC_OVERRIDE), so root is subject to actual dir permissions: tmux
# (claude-tui/opencode-tui/goose-tui) must be able to create /tmp/tmux-<uid>. Restore the standard
# world-writable sticky /tmp so every tmux-wrapped runnable can start.
RUN chmod 1777 /tmp
WORKDIR /app
# tini reaps the opencode serve child; entrypoint launches serve + adapter.
ENTRYPOINT ["/usr/bin/tini", "--", "/entrypoint.sh"]
