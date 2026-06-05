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

## Option 2 — the full dev stack (CP + node + web UI) + `spawnctl tmux`

This is the `just dev` flow (docker/runc lane). **No `runsc` dev recipe exists** — runsc is a
separate CRI/containerd setup (see `E2E_TEST_RUNSC.md`); use the docker lane below.

On this box **docker needs sudo**, so the node runs under sudo (CP + web are root-free):

```bash
# Terminal A — control plane (root-free)
just cp                       # -> 127.0.0.1:8080

# Terminal B — node attached to the CP (needs sudo here for the docker socket + docker exec)
set -a; . ./.env; set +a
sudo env OPENROUTER_API_KEY="$OPENROUTER_API_KEY" \
  AGENT_IMAGE=spawnery/agent:dev SIDECAR_IMAGE=spawnery/sidecar:dev DATA_ROOT=$PWD/.spawns \
  CP_ADDR=http://127.0.0.1:8080 NODE_ID=node-1 NODE_CLASS=self-hosted EGRESS_ENFORCE=false \
  NODE_ADVERTISE_IP=127.0.0.1 NODE_TERMINAL_ADDR=127.0.0.1:9092 \
  bin/spawnlet
#   -> "terminal endpoint on 127.0.0.1:9092" + "node connected to CP"
#   (if your user can use docker without sudo, `just node` works directly, as does `just dev`.)

# Terminal C — web UI
just web                      # -> open the printed URL (vite, ~http://localhost:5173)
```

In the browser: create a spawn, send prompts — it runs on opencode (validated: CP→node→opencode
reaches "ready" and streams). Note the spawn id (shown in the web UI / CP).

```bash
# Terminal D — attach the opencode TUI over mosh (terminal control goes direct to the node :9092)
bin/spawnctl tmux -spawn <SPAWN_ID> -addr http://127.0.0.1:9092
```
Typing in the browser and in the TUI both drive the one shared opencode session.

Validated this session (Option 2): CP + node (opencode image) register; a CP-created spawn reaches
"ready"; `POST :9092/terminal` returns `{host,port,key}` and launches mosh-server. The final
`mosh-client` render needs your TTY (Terminal D).

## Option 3 — run it from a (root) distrobox: `dev-spawnery`

Verified 2026-06-05: Option 2 runs unchanged inside a **debian:stable root distrobox** using
**docker-out-of-docker** (the host docker socket mounted in). The sidecar/agent containers are
created as **siblings on the host docker** (visible to host `docker ps`) — which is what makes
mounts (shared HOME, same paths) and pod-IP dialing (shared host netns) just work.

Already created on this box:
```bash
distrobox create --root --yes --nvidia --name dev-spawnery --image debian:stable \
  --additional-flags "--volume /var/run/docker.sock:/var/run/docker.sock"
distrobox enter --root dev-spawnery
```
Tooling installed inside: `docker.io` (CLI) + `mosh`. For the web UI also: `sudo apt-get install -y nodejs npm`.

Key facts about this distrobox:
- Inside you are user **alepar (uid 1001) with passwordless sudo** — `--root` makes it a
  *rootful-podman* container, NOT a root login. The docker socket is `root:docker`, so **docker
  still needs `sudo` inside** (exactly like the host). So run the node with `sudo env … bin/spawnlet`.
- Shared HOME means the static Go binaries (`bin/*`) and the go1.26 SDK (`~/sdk/go1.26.0`) are
  available — no Debian Go/toolchain needed.
- Shared host netns means `:5173`/`:9092` land on the host edge (the opened firewall ports apply),
  and the node can reach the docker bridge (`172.17.0.0/16`).

Then run the **Option 2** commands inside the distrobox (no change). Smoke-tested end-to-end:
CP + node register, a CP-created spawn reached "ready" on opencode, and `POST :9092/terminal`
returned `{host,port,key}`. A reusable smoke script is at `~/smoke-spawnery.sh`.

Do **not** create the box with `--unshare-netns` (breaks pod-IP reachability + host ports).

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
