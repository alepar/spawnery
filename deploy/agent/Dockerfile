# Goose ACP agent base image for the spawnlet slice.
#
# Goose specifics (resolved against block/goose v1.36.0 + agentclientprotocol.com):
#   1. ACP launch invocation: `goose acp` — runs Goose as an ACP agent over
#      stdio (JSON-RPC 2.0, newline-delimited), the mode editors like Zed use.
#   2. OpenAI-compatible provider with a custom base URL is configured via env:
#        GOOSE_PROVIDER=openai
#        GOOSE_MODEL=<model>                  (from $SPAWN_MODEL)
#        OPENAI_HOST=http://127.0.0.1:8080    (the sidecar; host only, no path)
#        OPENAI_BASE_PATH=v1/chat/completions (default; sidecar forwards /v1/* )
#        OPENAI_API_KEY=<dummy>               (sidecar injects the real key)
#   3. cwd=/app is set by the client via ACP session/new {"cwd":"/app"};
#      WORKDIR /app is the container default working dir. Goose reads /app/AGENTS.md
#      in place; data mounts live rw at /app/<path>.
#
# We install a pinned, statically-linked (musl) Goose release so the image can
# stay slim and has no runtime glibc dependency.
FROM debian:bookworm-slim AS fetch
ARG GOOSE_VERSION=v1.36.0
ARG GOOSE_ASSET=goose-x86_64-unknown-linux-musl.tar.gz
RUN apt-get update \
 && apt-get install -y --no-install-recommends ca-certificates curl \
 && rm -rf /var/lib/apt/lists/*
RUN curl -fsSL \
      "https://github.com/block/goose/releases/download/${GOOSE_VERSION}/${GOOSE_ASSET}" \
      -o /tmp/goose.tar.gz \
 && tar -xzf /tmp/goose.tar.gz -C /usr/local/bin \
 && chmod +x /usr/local/bin/goose \
 && rm /tmp/goose.tar.gz

FROM golang:1.26 AS gobuild
WORKDIR /src
COPY . .
RUN CGO_ENABLED=0 go build -o /acpadapter ./deploy/agent/acpadapter

FROM debian:bookworm-slim
RUN apt-get update \
 && apt-get install -y --no-install-recommends ca-certificates \
 && rm -rf /var/lib/apt/lists/*
COPY --from=fetch /usr/local/bin/goose /usr/local/bin/goose
COPY --from=gobuild /acpadapter /usr/local/bin/acpadapter
COPY deploy/agent/entrypoint.sh /entrypoint.sh
RUN chmod +x /entrypoint.sh
WORKDIR /app
ENTRYPOINT ["/entrypoint.sh"]
