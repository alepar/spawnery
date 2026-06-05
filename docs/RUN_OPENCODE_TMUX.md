# Running the opencode + `spawnctl tmux` stack manually (this box)

Tested 2026-06-05 on this host (rootful docker via sudo, podman, opencode 1.15.13, mosh, tmux).

## Prereqs (one-time)

```bash
# Go (only needed to (re)build binaries/images)
export PATH=/var/home/alepar/sdk/go1.26.0/bin:$PATH

# Build host binaries
go build -o bin/spawnlet ./cmd/spawnlet
go build -o bin/spawnctl ./cmd/spawnctl

# Build images (podman) and load into docker (the spawnlet uses the docker daemon here)
podman build -t spawnery/agent:dev   -f deploy/agent/Dockerfile   .
podman build -t spawnery/sidecar:dev -f deploy/sidecar/Dockerfile .
podman save spawnery/agent:dev   | sudo docker load
podman save spawnery/sidecar:dev | sudo docker load
sudo docker tag localhost/spawnery/agent:dev   spawnery/agent:dev
sudo docker tag localhost/spawnery/sidecar:dev spawnery/sidecar:dev
```

Gotchas found:
- **docker needs sudo** here, so the spawnlet runs under `sudo` (it uses the docker socket AND
  `docker exec` for the terminal).
- **Port 9090 is taken by cockpit** — use `:9091`.
- The egress iptables floor is disabled below (`EGRESS_ENFORCE=false`) so a local run doesn't touch
  host firewall rules.

## Terminal A — run the node (spawnlet)

```bash
set -a; . ./.env; set +a   # loads OPENROUTER_API_KEY
sudo env OPENROUTER_API_KEY="$OPENROUTER_API_KEY" \
  AGENT_IMAGE=spawnery/agent:dev SIDECAR_IMAGE=spawnery/sidecar:dev \
  NODE_CLASS=self-hosted EGRESS_ENFORCE=false NODE_ADVERTISE_IP=127.0.0.1 \
  SPAWNLET_ADDR=127.0.0.1:9091 \
  bin/spawnlet
# -> "spawnlet listening on 127.0.0.1:9091"
```

## Terminal B — create a spawn and drive it (this is the WEB-equivalent client)

```bash
bin/spawnctl -addr http://127.0.0.1:9091 -app examples/secret-app -model openai/gpt-4o-mini
# -> "spawn: <ID>"   then   "ready. type prompts:"
# Type a prompt, e.g.:  What is the secret word?
# KEEP THIS RUNNING — exiting it tears the spawn down.
```

Note the `<ID>`.

## Terminal C — attach the in-container opencode TUI over mosh

```bash
bin/spawnctl tmux -spawn <ID> -addr http://127.0.0.1:9091
# -> mosh connects; you get the opencode TUI (in tmux) attached to the SAME session.
```

Now: a prompt you type in **Terminal B** appears in the **Terminal C** TUI, and a prompt typed in
the **C** TUI appears in **B** — one shared, server-authoritative opencode session. Detach with the
usual tmux/mosh keys; re-running the `tmux` command reattaches (tmux `new-session -A`).

## How it works (what each piece does)
- The agent image runs `opencode serve` (127.0.0.1:4096) + `acpadapter`. The adapter speaks canonical
  ACP to the node and translates to opencode's HTTP/SSE (so the node/CP/web are agent-neutral).
- `spawnctl tmux` POSTs the node `/terminal?spawn=<id>`; the node runs `mosh-server` whose child
  execs into the agent container running `tmux new-session -A opencode attach http://127.0.0.1:4096 -c`.
  The node returns `{host,port,key}` and spawnctl execs `mosh-client` (UDP straight to the node).
- Both the TUI and Terminal-B's client are thin views of the one opencode session → shared visibility.

## Not yet wired (productionization)
- CP-routed control plane for `spawnctl tmux` (owner-auth, node discovery, auto-resume) — `sp-wsu.2`.
  The standalone path above talks directly to the node (per "mosh around CP for now").
- Suspend/resume opencode SQLite quiesce (`sp-5h3.6`); spawnlet opencode e2e (`sp-5h3.7`).
