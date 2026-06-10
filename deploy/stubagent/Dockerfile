# Fixture image for the multi-session e2e (sp-npxq.5): a deterministic ACP echo agent that ALSO
# carries the in-image launcher + tmux + bash, so the node can exec additional sessions (a shell, a
# 2nd stub-acp Pump) into it — credential-free. NOT distroless (the launcher needs a shell + tmux).
#
# Session #0 = stub-acp: the entrypoint dispatches to the shared launcher, which runs acpmux over a
# stdio stubagent bound to ACP_LISTEN=tcp://0.0.0.0:7000 (the port the node dials). The stub echoes
# "ECHO: <prompt>" with no model/sidecar/keys, so the live e2e runs with no credentials.
FROM golang:1.26 AS build
WORKDIR /src
COPY . .
RUN CGO_ENABLED=0 go build -o /stubagent ./cmd/stubagent \
 && CGO_ENABLED=0 go build -o /acpmux ./deploy/agent/acpmux

FROM debian:bookworm-slim
# UTF-8 locale so tmux + ncurses render line-drawing glyphs instead of the ASCII fallback.
ENV LANG=C.UTF-8 LC_ALL=C.UTF-8
RUN apt-get update \
 && apt-get install -y --no-install-recommends bash tmux ncurses-term ca-certificates \
 && rm -rf /var/lib/apt/lists/*
COPY --from=build /stubagent /usr/local/bin/stubagent
COPY --from=build /acpmux    /usr/local/bin/acpmux
COPY deploy/agent/launch       /usr/local/bin/launcher
COPY deploy/agent/tmux.conf    /etc/tmux.conf
COPY deploy/stubagent/entrypoint.sh /entrypoint.sh
RUN chmod +x /usr/local/bin/launcher /entrypoint.sh
ENTRYPOINT ["/entrypoint.sh"]
