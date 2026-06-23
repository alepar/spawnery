# pi coding agent support (TUI + rich-web ACP, model via sidecar)

**Date:** 2026-06-22
**Status:** Approved — ready for implementation. **Spike S1 GREEN (2026-06-23, pinned binary v0.80.2)** — the launch-case details below and the Dockerfile section have been updated with S1's confirmed results; see Post-Implementation Notes for the full findings. Only Tier C (end-to-end through the real sidecar in a live pod) remains, and the mechanism is fully de-risked.

## Summary

Add **pi** (`earendil-works/pi`, MIT) as a selectable coding agent in Spawnery
with **two runnables**, reusing the two proven integration templates:

- **`pi-tui`** — terminal (tmux) mode, mirroring `codex-tui` / `claude-tui`
  (`ModeTmux`, `RelayRawPTY`).
- **`pi-acp`** — rich-web mode via a **first-party Go adapter** that translates
  pi's `--mode rpc` protocol to ACP, mirroring the opencode `ocadapter` path
  (`acpadapter` + `internal/ocadapter`).

The spawn's model (`SPAWN_MODEL`) is wired in via the in-pod sidecar — exactly
like Codex/Claude Code — and is switchable from inside pi's own model picker. The
work is **phased**: ship `pi-tui` first (low-risk, proven template), then land
`pi-acp`.

## Background: how agent integration works today

Two relay templates exist; pi uses both.

- **Registry.** `internal/agentcaps/agentcaps.go` maps an agent binary → its
  runnables, each with a `Mode` (`ModeTmux` / `ModeACP` / `ModeServed`) and a
  `Relay` (`RelayRawPTY` / `RelayPump` / `RelayOcadapter`). `claude-code` and
  `codex` ship one `ModeTmux`+`RelayRawPTY` runnable each; `goose`/`hermes` ship
  native-ACP runnables (`ModeACP`+`RelayPump`); `opencode` ships
  `opencode-served` (`ModeServed`+`RelayOcadapter`).
- **Launch dispatcher.** `deploy/agent/launch` (baked as `/usr/local/bin/launcher`)
  is the single source of truth for per-runnable env + launch wiring: a
  `case "$RUNNABLE"` that exports env and either `start_tmux … -- <agent>` (tmux
  runnables) or `exec /usr/local/bin/<adapter>` (ACP/served runnables, which
  listen on `ACP_LISTEN=tcp://0.0.0.0:$ACP_PORT`).
- **Sidecar.** `internal/sidecar` is the single egress chokepoint. `mux.Handle("/",
  …)` is a **transparent reverse proxy** to `SIDECAR_UPSTREAM`
  (default `https://openrouter.ai/api`), injecting the real `OPENROUTER_API_KEY`.
  The node injects `OPENAI_BASE_URL=http://<sidecar>:8080/v1` into the pod and
  advertises shipped binaries via `AGENT_BINARIES` (CSV → `Register.Binaries`).
- **ACP relay.** For `ModeACP`/`ModeServed`, the node attaches a `Pump`
  (`internal/node/pump.go`) to the in-pod ACP endpoint and relays ACP JSON-RPC
  to the web client; the web ChatView, `acpdial`/`nori` terminal attach, proto,
  and CP all auto-discover from the registry. **No web/proto/CP changes** are
  needed for a new agent.

## Key facts about pi (deep-research, 2026-06-22, all 3-0 verified)

Sourced from `pi.dev/docs`, `earendil-works/pi` repo, the `rpc.md` protocol doc,
GitHub Releases, and npm. The integration-relevant findings:

- **What/license:** MIT, TypeScript, invoked as `pi`; npm
  `@earendil-works/pi-coding-agent`. Built by Earendil Works (Armin Ronacher's
  org). Pin `PI_VERSION` (S1 ran against **v0.80.2**) — releases ~daily.
- **Modes:** default (no flags) is the interactive **TUI** (drivable under tmux
  like Codex). Also `-p`/`--print` (headless), `--mode json`, and
  **`--mode rpc`** — a *pi-specific* line-delimited-JSON (LF-only) protocol over
  stdin/stdout. **No native ACP** (only an MVP-grade community adapter exists).
- **Custom endpoint:** `~/.pi/agent/models.json` defines custom providers/models
  with `baseUrl`, `apiKey`, an `api` (wire-type) field, `headers`, and a `models`
  array. Per-invocation `--provider` / `--model` / `--api-key` overrides.
- **Wire API:** native per provider, selected by the `api` field — one option is
  **`openai-completions`** (plain Chat Completions). Our sidecar *is* OpenAI
  Chat-Completions-compatible at `/v1`, so pi hits `/v1/chat/completions`
  directly. **Simpler than Codex** — no Responses API, no `disable_response_storage`.
- **Auth:** plain API key (env / config / `--api-key`) or OAuth. A dummy key +
  sidecar injection works (any string accepted in `models.json`; `--api-key`
  overrides env).
- **Distribution (corrected by S1):** prebuilt **glibc-dynamic** Linux
  `x64`/`arm64` archives (`pi-linux-{x64,arm64}.tar.gz`) with `SHA256SUMS`. **Not**
  musl-static, and **not a single self-contained binary** — the tarball extracts
  to a *directory* (`pi` ELF ~116MB + `photon_rs_bg.wasm` + `theme/` +
  `export-html/` + `node_modules/` + bundled `docs/`). The `pi` ELF resolves its
  siblings via its own real path, so a symlink onto `PATH` works. Install pattern:
  unpack the whole dir (e.g. `/usr/local/share/pi`) + symlink `/usr/local/bin/pi`.
  Our agent image is already glibc (goose installs `-linux-gnu`), so **no Node
  runtime is needed**. pi also wants `ripgrep` + `fd` present (it skips the
  download under `PI_OFFLINE`); install both.

## Phasing

| Phase | Runnable | Template | Risk |
|-------|----------|----------|------|
| 1 | `pi-tui` | `codex-tui` (tmux) | Low — proven path |
| 2 | `pi-acp` | `opencode-served` (first-party adapter) | Medium — new translator |

Both phases share **one model-wiring path** (the inline `models.json` generated in
the launch script), so Phase 2 reuses Phase 1's provider config verbatim.

## Phase 1 — `pi-tui` (terminal)

Mirrors the Codex integration. No web/sidecar/proto changes.

### 1. Registry — `internal/agentcaps/agentcaps.go`

```go
"pi": {
    {ID: "pi-tui", Mode: ModeTmux, Launch: []string{"pi"}, Resume: []string{"pi", "--continue"}, Relay: RelayRawPTY, Label: "pi · terminal"},
},
```

`Known("pi")` becomes true and the runnable auto-expands into the web
`runnable-select` dropdown via `ListAgentImages`. **`Resume` = `pi --continue`**
(S1-confirmed: a fresh `pi --continue` process reloads the prior session, exactly
like `claude-tui`). Sessions persist under `~/.pi/agent/sessions/` by default.

### 2. Dispatcher — `deploy/agent/launch`

Add a `pi-tui)` case (sibling of `codex-tui)`). The model is per-spawn, so
generate `~/.pi/agent/models.json` at runtime defining a `spawnery` custom
provider pointed at the sidecar, then `start_tmux … -- pi`:

```jsonc
// $HOME/.pi/agent/models.json  (HOME-derived; HOME=/root → /root/.pi/agent/. No PI_HOME env var.)
{
  "providers": {
    "spawnery": {
      "baseUrl": "<OPENAI_BASE_URL>",        // already = http://<sidecar>:8080/v1
      "apiKey":  "sk-unused-sidecar-injects-real-key",  // S1: sent as Bearer; sidecar replaces it
      "api":     "openai-completions",        // S1: hits POST <baseUrl>/chat/completions, store:false, stateless
      "models":  [ { "id": "<SPAWN_MODEL>", "name": "Spawnery", "contextWindow": 128000, "maxTokens": 16384 } ]
    }
  }
}
```

S1-confirmed details (folded in):

- **Config path is `$HOME/.pi/agent/` (HOME-derived) — there is NO `PI_HOME` env
  var.** With `HOME=/root` the file is `/root/.pi/agent/models.json`.
- Launch: **`pi --provider spawnery --model spawnery/<SPAWN_MODEL> -a`**. The
  `-a`/`--approve` is **required** — `pi-tui` runs the *interactive* TUI, which
  otherwise asks for project trust on a project with `.pi`/`.agents` resources;
  `-a` settles it non-interactively. (Non-interactive `-p`/`--mode json/rpc` never
  prompt, but the tmux TUI is interactive.)
- Export **`PI_OFFLINE=1`** (suppresses startup network ops behind the egress
  floor — the analog of Codex's autoupdater/telemetry disables).
- Only model `id` is required per model for `openai-completions`; a dummy `apiKey`
  literal is accepted (the Ollama precedent). `--model` pattern is `provider/id`.
- Forward `HOME`/`PI_OFFLINE` through `start_tmux`'s explicit env list so the
  tmux-launched process inherits them. The model is switchable in pi's own picker.

### 3. Dockerfile — `deploy/agent/Dockerfile`

Install the pinned pi **glibc directory dist** from GitHub releases,
SHA256-verified, symlinked onto `PATH`, with a `pi --version` check. No Node.js.
Also install `ripgrep` + `fd` (pi uses them; mirrors pi's own Docker recipe which
installs `ripgrep`). S1-corrected: the tarball is a **directory**, not a single
binary, so unpack the whole dir and symlink.

```dockerfile
ARG PI_VERSION=v0.80.2
RUN set -eux; \
    arch="$(dpkg --print-architecture)"; case "$arch" in amd64) a=x64;; arm64) a=arm64;; esac; \
    curl -fsSL -o /tmp/pi.tgz   "https://github.com/earendil-works/pi/releases/download/${PI_VERSION}/pi-linux-${a}.tar.gz"; \
    curl -fsSL -o /tmp/pi.sums  "https://github.com/earendil-works/pi/releases/download/${PI_VERSION}/SHA256SUMS"; \
    (cd /tmp && grep "pi-linux-${a}.tar.gz" pi.sums | sha256sum -c -); \
    mkdir -p /usr/local/share/pi; \
    tar -xzf /tmp/pi.tgz -C /usr/local/share/pi --strip-components=1; \
    ln -sf /usr/local/share/pi/pi /usr/local/bin/pi; \
    rm /tmp/pi.tgz /tmp/pi.sums; \
    pi --version
# (ripgrep + fd-find installed via the image's apt layer alongside the other agent deps)
```

### 4. Node binary advertisement — `Justfile`

Add `pi` to the node's `AGENT_BINARIES` CSV (three sites: the `spawnlet`/`node`
and `node-enforced` recipes) so the node Registers it and the CP catalog
advertises it. Config change, not code.

### 5. Tests — `internal/agentcaps/agentcaps_test.go`

`TestKnown` gains `pi`; registry invariants (globally-unique runnable IDs;
tmux runnables declare a `Launch`) extend automatically. Add a focused assertion
that `Lookup("pi", "pi-tui")` resolves with `ModeTmux` / `RelayRawPTY`.

## Phase 2 — `pi-acp` (rich web)

A **first-party Go adapter** that spawns `pi --mode rpc` and translates its
JSONL event stream ↔ ACP, mirroring `ocadapter`/`acpadapter`. Chosen over the
community `pi-acp` npm package to avoid a Node runtime in the image, an MVP-grade
external dependency with documented breaking-change churn, and to keep the
translation under build-tagged test control.

### 1. Translation package — `internal/piadapter/{server.go,translate.go}`

A stateful ACP server (`Serve(r io.Reader, w io.Writer) error`) mirroring
`internal/ocadapter`. Spawns `pi --mode rpc --provider spawnery --model …`
(reusing the Phase-1 `models.json`), owns its stdio, and maps:

| pi `--mode rpc` (stdout event) | → | ACP |
|---|---|---|
| `message_update` (text/thinking deltas) | → | `session/update` `agent_message_chunk` |
| `tool_execution_start` | → | `session/update` `tool_call` |
| `tool_execution_update` | → | `tool_call_update` (partial) |
| `tool_execution_end` (`isError`) | → | `tool_call_update` (final + error) |
| `turn_end` / `agent_end` | → | `respond(id, {stopReason, usage})` — turn complete |
| `extension_error` / delta `error` | → | structured ACP error + stopReason |
| ACP `session/prompt` (request) | → | stdin `{"type":"prompt","message":…}` |
| ACP cancel | → | stdin `{"type":"abort"}` |

`agent_end` is the **turn-complete / ready-for-next-prompt** signal. The
JSONL transport is **LF-only**; the reader MUST NOT use a generic line reader
that splits on U+2028/U+2029 (pi's `rpc.md` calls this out — Node `readline` is
non-compliant; a Go `bufio.Scanner` splitting on `'\n'` is correct). Carry
per-turn token usage where pi exposes it.

### 2. Adapter binary — `deploy/agent/pi-adapter/main.go`

Mirrors `deploy/agent/acpadapter/main.go`: listen on `ACP_LISTEN`, accept one
node connection at a time, run `piadapter.New(...).Serve(conn, conn)`. Built as a
Go binary in the Dockerfile alongside `acpadapter`/`acpmux`.

### 3. Registry — `internal/agentcaps/agentcaps.go`

Add a `pi-acp` runnable. **Open wrinkle (settled during implementation, not a
design fork):** the recon flagged that `RelayOcadapter` may be hardcoded to
opencode. Read the node-side dispatch (`internal/node/attach.go`,
`internal/spawnlet`) and pick the field values that route pi's adapter through
the `Pump` the same way `opencode-served` does — either reuse `ModeServed`+a
generalized served relay, or wire as `ModeACP`+`RelayPump` (the adapter presents
a plain ACP endpoint, which the `Pump` attaches to regardless). Generalize
`RelayOcadapter` only if the dispatch genuinely hardcodes opencode.

### 4. Dispatcher — `deploy/agent/launch`

Add a `pi-acp)` case mirroring `opencode-served)`: export `ACP_LISTEN`, generate
the same `models.json`, then `exec /usr/local/bin/pi-adapter`.

### 5. Tests

- Hermetic unit tests for `internal/piadapter` translation (golden pi-rpc event
  fixtures → expected ACP frames; turn-lifecycle/ready-signal handling).
- A build-tagged ACP e2e mirroring the opencode/goose ACP e2e (fails, never
  skips, when its image/dep is absent — per the project's lane-test rule).

## Wire protocol / model data flow

```
pi (TUI or --mode rpc)
  → reads ~/.pi/agent/models.json  (provider "spawnery", api=openai-completions)
  → HTTP POST <OPENAI_BASE_URL>/chat/completions   (= http://<sidecar>:8080/v1/chat/completions)
  → sidecar catch-all reverse-proxy, injects real OPENROUTER_API_KEY
  → https://openrouter.ai/api/v1/chat/completions
```

`base_url + /chat/completions` resolves correctly because `SIDECAR_UPSTREAM` is
`…/api` (no `/v1`) — identical to existing `/v1/chat/completions` traffic. **Zero
sidecar/proxy/proto changes**, both phases.

## Spike S1 — DONE, GREEN (2026-06-23, pinned binary v0.80.2)

Run locally against the real pinned binary with a logging OpenAI-compatible stub
server + a live tmux session (no pod needed except the real-sidecar end-to-end,
which is Tier C below). Full findings + the captured artifacts are in
Post-Implementation Notes; the launch-case/Dockerfile sections above already
incorporate them. Results:

1. **TUI under tmux + resume — GREEN.** The interactive TUI launches cleanly in a
   detached tmux session with **no blocking trust/login/welcome prompt** when
   passed `-a`/`--approve`; an interactive prompt round-trips to the endpoint.
   **Resume flag = `--continue`** (a fresh `pi --continue` process reloaded prior
   session history — mirrors `claude-tui`).
2. **`openai-completions` round-trips — GREEN, fully stateless.** pi posts to
   `<baseUrl>/chat/completions` with `Authorization: Bearer <dummy>` (sidecar
   replaces it), `store: false`, and **resends the full conversation history each
   turn** (verified across a 2-turn rpc session and a `--continue` resume). No
   server-side state. Declarative `~/.pi/agent/models.json` schema confirmed.
3. **`--mode rpc` semantics — GREEN.** Per-turn lifecycle observed:
   `response`(ack) → `agent_start` → `turn_start` → `message_start`/`message_end`
   (user echo) → `message_start` → `message_update`* (deltas) → `message_end` →
   `turn_end` → **`agent_end`** (the ready-for-next-prompt signal). Matches the
   documented spec — the `piadapter` translation table is grounded in observed
   reality.

**Tier C (the only remaining check):** end-to-end through the *real* sidecar in a
live pod. The mechanism is fully de-risked (wire shape, statelessness, dummy-key
Bearer, trust, resume all confirmed); Tier C is a confirmation, not a gate. The
fallback contemplated here (drive `--mode rpc` instead of the TUI) is **not
needed** — the TUI drives cleanly under tmux.

## Out of scope

- **Profiles / `agentinstall` emitter for pi** (user settings/MCP/skills
  injection). `pi-tui`/`pi-acp` fall through `apply-artifacts.sh` as no-ops (like
  `goose-acp`/`hermes-acp`). Tracked as a follow-up; not required to ship the
  agent.
- The community `pi-acp` npm adapter (rejected — see Phase 2 rationale).
- pi's `--mode json` / `-p` headless surfaces beyond what the adapter uses.
- No curated multi-model menu (mirror Codex: single wired model, switchable
  in-agent).
- No sidecar translation, proxy, web, or proto changes.

## Risks

1. **pi releases ~daily.** Pin `PI_VERSION`; re-verify `models.json` schema and
   CLI flags against the pinned release at integration time (S1).
2. **Stateless assumption.** `openai-completions` should be stateless; S1 #2
   confirms pi doesn't lean on any server-side state.
3. **glibc binary.** Fine today (image is glibc); if the agent base ever moves to
   musl/Alpine, switch to the npm install path (adds Node) or a glibc base.
4. **Adapter fidelity.** The `piadapter` turn-lifecycle/error mapping is the main
   Phase-2 risk; golden-fixture unit tests + the build-tagged e2e are the guard.
5. **`RelayOcadapter` generality.** May need a small generalization for a second
   served adapter; bounded, decided by reading the dispatch (Phase 2 §3).

## Acceptance

- **Phase 1:** `pi` appears as a selectable runnable ("pi · terminal") in the web
  marketplace detail view; spawning it launches pi in a tmux terminal routed
  through the sidecar to OpenRouter (Chat Completions) with the spawn's model
  active; in-agent model switching works for OpenRouter ids. `go test
  ./internal/agentcaps/...` passes; `golangci-lint` clean.
- **Phase 2:** "pi · rich web" is selectable; spawning it drives pi over ACP in
  the web ChatView (streaming text + tool calls render); `internal/piadapter`
  unit tests pass; the build-tagged ACP e2e passes in its lane.

## Post-Implementation Notes

*As this design is implemented and iterated on — bug fixes, adjustments, anything
that diverged from the assumptions above — append a dated note here, whether or
not a formal debugging skill was used.*

### 2026-06-23 — Spike S1 executed, GREEN (sp-2aaw.1 closed)

Ran S1 locally against the real pinned binary (latest **v0.80.2**; spec had pinned
v0.79.10 — bumped) using a logging OpenAI-compatible stub server + a live tmux
session. No pod required; only Tier C (real sidecar end-to-end) is unrun and is a
confirmation, not a gate. Everything verified GREEN. Divergences from the original
assumptions, all folded into the sections above:

- **Distribution is a directory, not a single binary.** `pi-linux-x64.tar.gz`
  extracts to a dir: the `pi` ELF (~116MB, glibc-dynamic) + `photon_rs_bg.wasm` +
  `theme/` + `export-html/` + `node_modules/` + bundled `docs/`. The ELF resolves
  siblings via its own real path, so the Dockerfile installs the whole dir to
  `/usr/local/share/pi` and symlinks `/usr/local/bin/pi`. SHA256SUMS verified.
- **No `PI_HOME` env var.** Config is HOME-derived at `~/.pi/agent/` (so
  `/root/.pi/agent/` with `HOME=/root`). The spec's earlier `PI_HOME` references
  were wrong and have been corrected.
- **`pi-tui` must pass `-a`/`--approve`** to avoid the interactive project-trust
  prompt (the TUI is interactive; `-p`/`--mode json`/`--mode rpc` never prompt).
  Also export **`PI_OFFLINE=1`** to suppress startup network ops behind the egress
  floor. Resume = **`pi --continue`** (verified).
- **Wire confirmed:** `POST <baseUrl>/chat/completions`, `stream:true`,
  `Authorization: Bearer <dummy>` (sidecar injects the real key), `store:false`,
  and **full conversation history resent each turn** (stateless — verified on a
  2-turn rpc session and a `--continue` resume). The design's "simplest wire path,
  no Responses API" holds.
- **`--mode rpc` lifecycle confirmed** (per turn): `response` → `agent_start` →
  `turn_start` → `message_start`/`message_end` (user echo) → `message_start` →
  `message_update`* → `message_end` → `turn_end` → `agent_end` (ready signal).
  This is the ground truth for the Phase-2 `piadapter` translation table.
- **Install `ripgrep` + `fd`** in the agent image (pi uses them; it skips the
  auto-download under `PI_OFFLINE`). pi's own Docker recipe installs `ripgrep`.
- **Minor:** tmux `extended-keys` off triggers a cosmetic warning about modified
  Enter keys; harmless (set `extended-keys on` if it bites).

S1 scratch artifacts (stub, rpc-event capture, request log) lived under
`/tmp/pi-spike/` during the spike; not committed.
