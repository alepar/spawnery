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
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY . .
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 go build -o /acpadapter ./deploy/agent/acpadapter \
 && CGO_ENABLED=0 go build -o /acpexec ./deploy/agent/acpexec \
 && CGO_ENABLED=0 go build -o /acpmux ./deploy/agent/acpmux \
 && CGO_ENABLED=0 go build -o /acpdial ./deploy/agent/acpdial \
 && CGO_ENABLED=0 go build -o /agentinstall ./cmd/agentinstall \
 && CGO_ENABLED=0 go build -o /pi-adapter ./deploy/agent/pi-adapter \
 && CGO_ENABLED=0 go build -o /spawnery-test-mcp ./deploy/agent/testmcp

FROM debian:stable
# UTF-8 locale so tmux + claude render box/line-drawing glyphs (─│●) instead of ACS q/x or the
# ASCII fallback _. C.UTF-8 (a.k.a. C.utf8) is built into glibc — no `locales` package needed.
ENV LANG=C.UTF-8 LC_ALL=C.UTF-8
RUN apt-get update \
 && apt-get install -y --no-install-recommends ca-certificates tmux tini ncurses-term \
      procps less iproute2 curl \
      neovim htop btop dnsutils jq \
      git openssh-client ripgrep fd-find unzip xz-utils build-essential cmake just \
 && rm -rf /var/lib/apt/lists/*
COPY --from=fetch /usr/local/bin/opencode /usr/local/bin/opencode

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
# codex CLI (OpenAI) — Rust TUI/headless agent (codex-tui launcher case)
ARG CODEX_VERSION=rust-v0.137.0
RUN set -eux; \
    curl -fsSL -o /tmp/codex.tar.gz \
      "https://github.com/openai/codex/releases/download/${CODEX_VERSION}/codex-x86_64-unknown-linux-musl.tar.gz"; \
    tar -xzf /tmp/codex.tar.gz -C /tmp; \
    install -m 0755 /tmp/codex-x86_64-unknown-linux-musl /usr/local/bin/codex; \
    rm /tmp/codex.tar.gz /tmp/codex-x86_64-unknown-linux-musl; \
    codex --version; \
    # codex's stock base instructions, baked for the launcher's model_catalog_json entry: a
    # custom catalog REPLACES the bundled one, and each entry must carry base_instructions —
    # anything other than the version-matched stock prompt would change codex's system prompt.
    mkdir -p /usr/local/share/codex; \
    curl -fsSL -o /usr/local/share/codex/base-instructions.md \
      "https://raw.githubusercontent.com/openai/codex/${CODEX_VERSION}/codex-rs/models-manager/prompt.md"; \
    test -s /usr/local/share/codex/base-instructions.md

# nori CLI (Tilework) — Rust ACP TUI client (spawnctl attach to acp-mode spawns; wiring in sp-9xr.12)
# tarball layout: ./nori/nori — extract to /tmp then install to /usr/local/bin
RUN set -eux; \
    curl -fsSL -o /tmp/nori.tar.gz \
      "https://github.com/tilework-tech/nori-cli/releases/download/nori-v0.22.0/nori-0.22.0-linux-x86_64.tar.gz"; \
    tar -xzf /tmp/nori.tar.gz -C /tmp; \
    install -m 0755 /tmp/nori/nori /usr/local/bin/nori; \
    rm -rf /tmp/nori.tar.gz /tmp/nori; \
    nori --version
# gh CLI (GitHub) — PINNED >= 2.63.0 for CVE-2024-53858 / clone2leak fix (§16.8).
# gh < 2.63.0 can be exploited via repo-injected credential.helper in .gitmodules/.git/config.
ARG GH_VERSION=2.63.0
RUN set -eux; \
    curl -fsSL -o /tmp/gh.deb \
      "https://github.com/cli/cli/releases/download/v${GH_VERSION}/gh_${GH_VERSION}_linux_amd64.deb"; \
    dpkg -i /tmp/gh.deb; \
    rm /tmp/gh.deb; \
    gh --version
# hermes (Nous Research hermes-agent) — Python ACP agent (hermes-acp launcher case).
# Pinned; the [acp] extra pulls agent-client-protocol. Isolated venv (PEP-668).
ARG HERMES_VERSION=0.15.2
RUN set -eux; \
    chmod 1777 /tmp; \
    apt-get update; \
    apt-get install -y --no-install-recommends python3 python3-venv; \
    rm -rf /var/lib/apt/lists/*; \
    python3 -m venv /opt/hermes/venv; \
    /opt/hermes/venv/bin/pip install --no-cache-dir "hermes-agent[acp]==${HERMES_VERSION}"; \
    ln -sf /opt/hermes/venv/bin/hermes /usr/local/bin/hermes; \
    hermes acp --check

# pi (Earendil Works) — TypeScript TUI/rpc agent (pi-tui launcher case). Prebuilt glibc-dynamic
# Linux dist. NB: the tarball extracts to a DIRECTORY (the `pi` ELF + photon_rs_bg.wasm + theme/ +
# export-html/ + node_modules/ + bundled docs/), NOT a single binary — the ELF resolves its
# siblings via its own real path, so install the whole tree and symlink onto PATH. No Node runtime
# needed (the image base is glibc). SHA256-verified. ripgrep + fd already installed above.
ARG PI_VERSION=v0.80.2
RUN set -eux; \
    arch="$(dpkg --print-architecture)"; case "$arch" in amd64) a=x64;; arm64) a=arm64;; *) echo "unsupported arch $arch" >&2; exit 1;; esac; \
    curl -fsSL -o "/tmp/pi-linux-${a}.tar.gz" "https://github.com/earendil-works/pi/releases/download/${PI_VERSION}/pi-linux-${a}.tar.gz"; \
    curl -fsSL -o /tmp/pi.sums  "https://github.com/earendil-works/pi/releases/download/${PI_VERSION}/SHA256SUMS"; \
    (cd /tmp && grep "pi-linux-${a}.tar.gz" pi.sums | sha256sum -c -); \
    mkdir -p /usr/local/share/pi; \
    tar -xzf "/tmp/pi-linux-${a}.tar.gz" -C /usr/local/share/pi --strip-components=1; \
    ln -sf /usr/local/share/pi/pi /usr/local/bin/pi; \
    rm "/tmp/pi-linux-${a}.tar.gz" /tmp/pi.sums; \
    pi --version

# Node.js (provides `npx`) for the hermes-acp runnable. hermes 0.15.2 probes browser-tool
# availability at session/new; with NO node on PATH it lazy-downloads Node + agent-browser +
# Chromium and streams the installer progress to STDOUT — which is acpmux's ACP JSON-RPC
# channel, corrupting it so the handshake never completes and acpmux never binds :7000
# (sp-055b). With `npx` present hermes resolves its browser CLI WITHOUT that download; lacking
# a Chromium build it then simply does not advertise browser tools — the right posture for an
# egress-floored spawn (the runtime download would hang anyway). Node version matches hermes'
# own bootstrap target.
ARG NODE_VERSION=22.23.0
RUN set -eux; \
    curl -fsSL -o /tmp/node.tar.gz \
      "https://nodejs.org/dist/v${NODE_VERSION}/node-v${NODE_VERSION}-linux-x64.tar.gz"; \
    tar -xzf /tmp/node.tar.gz -C /opt; \
    rm /tmp/node.tar.gz; \
    for b in node npx npm; do \
      ln -sf "/opt/node-v${NODE_VERSION}-linux-x64/bin/$b" "/usr/local/bin/$b"; \
    done; \
    node --version; npx --version

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

# --- volatile layers LAST: our Go binaries + config files change on every code edit, so they sit
# below all the network-heavy installs above — a Go change re-runs only these cheap COPYs.
COPY --from=gobuild /acpadapter /usr/local/bin/acpadapter
COPY --from=gobuild /acpexec /usr/local/bin/acpexec
COPY --from=gobuild /acpmux /usr/local/bin/acpmux
COPY --from=gobuild /acpdial /usr/local/bin/acpdial
COPY --from=gobuild /agentinstall /usr/local/bin/agentinstall
COPY --from=gobuild /pi-adapter /usr/local/bin/pi-adapter
COPY --from=gobuild /spawnery-test-mcp /usr/local/bin/spawnery-test-mcp
COPY deploy/agent/entrypoint.sh /entrypoint.sh
COPY deploy/agent/launch /usr/local/bin/launcher
COPY deploy/agent/spawn-tui.sh /usr/local/bin/spawn-tui
COPY deploy/agent/apply-artifacts.sh /usr/local/bin/apply-artifacts
COPY deploy/agent/tmux.conf /etc/tmux.conf
# Dummy Anthropic key via apiKeyHelper instead of ANTHROPIC_API_KEY, so Claude Code does NOT print
# "Detected a custom API key in your environment" at startup; the sidecar injects the real
# OpenRouter key on the wire regardless (sp-9xr.18).
RUN mkdir -p /root/.claude
COPY deploy/agent/claude-settings.json /root/.claude/settings.json
# Bake nori's config so `nori -a spawnery` (the acp-mode terminal attach) launches the acpdial
# shim -> acpmux on 127.0.0.1:7000 = the shared session (sp-9xr.12.2).
RUN mkdir -p /root/.nori/cli
COPY deploy/agent/nori-config.toml /root/.nori/cli/config.toml
RUN chmod +x /entrypoint.sh /usr/local/bin/launcher /usr/local/bin/spawn-tui /usr/local/bin/apply-artifacts /usr/local/bin/spawnery-test-mcp

WORKDIR /app
# tini reaps the opencode serve child; entrypoint launches serve + adapter.
ENTRYPOINT ["/usr/bin/tini", "--", "/entrypoint.sh"]
