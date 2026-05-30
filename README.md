# spawnery

A Go "spawnlet" node service that drives an agent in a Docker container over
**ACP** (Zed's [Agent Client Protocol](https://agentclientprotocol.com/),
JSON-RPC 2.0 over stdio), transparently relaying bytes between a client and the
agent's container stdio. An inference-proxy **sidecar** (its own container,
sharing the agent's network namespace) forwards OpenAI-compatible calls to
OpenRouter, injecting the API key so the agent never holds it.

The spawnlet itself **never parses ACP** — it is a dumb byte relay. ACP smarts
live only in the client (`internal/acp`) and the agent (Goose, or the
deterministic `internal/stubagent` test double).

```
spawnctl ──ConnectRPC(bidi)──▶ spawnlet ──stdio relay──▶ agent container (Goose)
                                                              │ ACP
                                                              ▼
                                          sidecar (shared netns, :8080) ──▶ OpenRouter
```

## Build & Test

```bash
go build ./...
go test ./...    # set SKIP_DOCKER=1 to skip the containerized end-to-end test
```

The containerized `TestEndToEndStub` needs a running Docker daemon and the
sidecar + stub-agent images (built below). It skips cleanly when Docker is
unavailable.

## Running the slice

The slice runs end-to-end two ways: with the deterministic **stub agent** (no
network, no LLM) and with real **Goose + OpenRouter**.

### 1. Build the images

```bash
docker build -t spawnery/sidecar:dev   -f deploy/sidecar/Dockerfile   .
docker build -t spawnery/stubagent:dev -f deploy/stubagent/Dockerfile .
docker build -t spawnery/goose:dev     -f deploy/agent/Dockerfile     .   # Goose v1.36.0
```

### 2. Stub end-to-end (deterministic, no network)

The stub agent echoes the prompt back as a real ACP `agent_message_chunk`. This
path is exercised by the containerized test:

```bash
go test ./internal/spawnlet/ -run TestEndToEndStub -v
```

Or drive it manually by pointing the spawnlet's `AGENT_IMAGE` at the stub:

```bash
go build -o bin/spawnlet ./cmd/spawnlet
go build -o bin/spawnctl ./cmd/spawnctl

AGENT_IMAGE=spawnery/stubagent:dev SIDECAR_IMAGE=spawnery/sidecar:dev \
  DATA_ROOT=$(pwd)/.spawns OPENROUTER_API_KEY=unused bin/spawnlet &

printf 'hello\n' | bin/spawnctl -app "$(pwd)/examples/hello-app" -model x
# => ECHO: hello
```

### 3. Live Goose + OpenRouter round-trip

Put your OpenRouter key in a git-ignored `.env` file at the repo root:

```
OPENROUTER_API_KEY=sk-or-...
```

Then:

```bash
go build -o bin/spawnlet ./cmd/spawnlet
go build -o bin/spawnctl ./cmd/spawnctl

# Source the key from .env (never commit it).
set -a; . ./.env; set +a

AGENT_IMAGE=spawnery/goose:dev SIDECAR_IMAGE=spawnery/sidecar:dev \
  DATA_ROOT=$(pwd)/.spawns bin/spawnlet &

# Pick a free, tool-capable OpenRouter chat model. The -app path must be ABSOLUTE
# (it is bind-mounted read-only into the agent at /app).
printf 'What is the capital of France? Answer in one short sentence.\n' \
  | bin/spawnctl -app "$(pwd)/examples/hello-app" -model "openai/gpt-oss-120b:free"
# => Paris is the capital of France.
```

A real model-generated reply streams back through the spawnlet. Each spawn's
`/data` directory is created under `DATA_ROOT/<spawn-id>/data` (seeded from the
app's `seed/` dir); both the agent and sidecar containers are torn down when the
client sends `StopSpawn`.

> The `.env`, `bin/`, and `.spawns/` paths are git-ignored. **Never commit the
> OpenRouter key.**

### How Goose is configured (deploy/agent)

The Goose base image installs a pinned, static (musl) Goose release and launches
it headless with `goose acp` (ACP agent server over stdio). The entrypoint points
Goose at the sidecar as an OpenAI-compatible endpoint:

| env var            | value                          | purpose                                   |
| ------------------ | ------------------------------ | ----------------------------------------- |
| `GOOSE_PROVIDER`   | `openai`                       | use the OpenAI-compatible provider        |
| `GOOSE_MODEL`      | `$SPAWN_MODEL`                 | OpenRouter model id (from `-model`)       |
| `OPENAI_HOST`      | `http://127.0.0.1:8080`        | the sidecar (shared netns), host only     |
| `OPENAI_BASE_PATH` | `v1/chat/completions`          | forwarded by the sidecar to OpenRouter    |
| `OPENAI_API_KEY`   | dummy                          | sidecar injects the real key; never used  |

The client sets the working directory to `/data` via ACP `session/new`.
