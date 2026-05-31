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

## Quick start

```bash
just setup        # one-time: mprocs, Playwright browser, web deps
just dev          # spawnlet (goose) + web UI in mprocs panes, one Ctrl-C
```

Then open the web UI (`vite` prints the URL) and chat with the agent. Needs Docker
and an OpenRouter key in a git-ignored `.env` (see [Running the slice](#running-the-slice)).
Run `just` with no args to list all recipes.

## Build & Test

```bash
go build ./...
go test ./...    # set SKIP_DOCKER=1 to skip the containerized end-to-end test
```

The containerized `TestEndToEndStub` needs a running Docker daemon and the
sidecar + stub-agent images (built below). It skips cleanly when Docker is
unavailable.

## Running the slice

One-time setup (installs tooling the `just` recipes use — `mprocs`, the Playwright
browser, web deps):

```bash
just setup        # or: cargo install mprocs && (cd web && npm install)
```

Put your OpenRouter key in a git-ignored `.env` at the repo root for the live path:

```
OPENROUTER_API_KEY=sk-or-...
```

### Build the images

The `just` recipes build images automatically when needed. To build them manually:

```bash
make images   # sidecar + stubagent + goose — or build selectively:
docker build -t spawnery/sidecar:dev   -f deploy/sidecar/Dockerfile   .
docker build -t spawnery/stubagent:dev -f deploy/stubagent/Dockerfile .
docker build -t spawnery/goose:dev     -f deploy/agent/Dockerfile     .   # Goose v1.36.0
```

### Dev stack

```bash
just dev          # spawnlet (goose) + web UI in mprocs panes, one Ctrl-C
# or run them separately:
just spawnlet     # goose (real LLM);  `just spawnlet stub` for the deterministic echo
just web          # vite --host on :5173
```

### Drive it from the CLI

```bash
just spawnctl 'What is the secret word?'      # against the running spawnlet, free model
```

### Tests

```bash
just test          # Go unit (hermetic)
just test-web      # web unit (vitest)
just test-e2e      # Go e2e (Docker pods + live OpenRouter; needs the key)
just test-web-e2e  # browser e2e (Playwright vs stub)
```

Leaked per-spawn containers (`sp-8hf`): `just reap`.

Each spawn's `/data` directory is created under `DATA_ROOT/<spawn-id>/data`
(seeded from the app's `seed/` dir); both the agent and sidecar containers are
torn down when the client sends `StopSpawn`.

> The `.env`, `bin/`, and `.spawns/` paths are git-ignored. **Never commit the
> OpenRouter key.**

## Web client (demo)

A React+TypeScript single-page app (`web/`) drives a spawn straight from the
browser: it calls `CreateSpawn` over Connect-JSON (`fetch`), opens a WebSocket to
the spawnlet's `GET /ws/session` endpoint (which reuses the same transparent byte
relay as the ConnectRPC `Session` stream), and speaks ACP itself — `initialize`,
`session/new`, `session/prompt` — streaming `session/update`s into a chat UI
(agent bubbles, tool-call chips, collapsible thoughts, a permission modal). Vite
dev-proxies both paths to the spawnlet, so it's one origin (no CORS).

Run the whole stack with one command from the repo root:

```bash
just dev          # spawnlet (goose) + vite in mprocs panes — open http://localhost:5173
```

In the browser the status banner goes **starting... → ready**. Type
**"What is the secret word?"** and you'll see a tool-call chip (the agent
reading `data/README.md`), optionally a collapsible **thinking** block, then the
agent bubble **"QUOKKA-4417"**. Closing the tab calls `StopSpawn`, tearing down
the agent + sidecar containers.

The browser's exact transport and flow are covered automatically by
`TestWSEndToEndGooseSecret` (`//go:build e2e`, in `internal/spawnlet/`), which
drives the real Goose agent through `/ws/session` and asserts the recited secret.
The manual browser click-through above is the final human verification step.

### Browser e2e

A Playwright test (`web/e2e/chat.spec.ts`) drives the same flow in a real headless
Chromium against the **stub agent** (deterministic, no API key): it loads the app,
waits for the status banner to reach **ready**, sends `say <token>`, and asserts the
agent bubble echoes `ECHO: say <token>`.

Requirements: Docker (the test starts a real spawnlet that runs the
`spawnery/stubagent:dev` + `spawnery/sidecar:dev` images — build them with
`make images` if missing) and a one-time Chromium install (included in `just setup`).

```bash
just test-web-e2e
```

Playwright's `globalSetup` builds `bin/spawnlet`, launches it with the stub image on
:9090, and proxies through Vite; `globalTeardown` stops the spawnlet.

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
