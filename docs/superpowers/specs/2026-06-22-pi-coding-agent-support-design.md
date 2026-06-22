# pi coding agent support (TUI + rich-web ACP, model via sidecar)

**Date:** 2026-06-22
**Status:** Approved — ready for implementation (Spike S1 gates the launch-case details)

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
  org). Latest v0.79.10 — releases ~daily, so **pin a version**.
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
- **Distribution:** prebuilt **glibc-dynamic** Linux `x64`/`arm64` archives
  (`pi-linux-{x64,arm64}.tar.gz`, ~116MB self-contained ELF bundling its JS
  runtime) with `SHA256SUMS`. **Not** musl-static. Our agent image is already
  glibc (goose installs the `-linux-gnu` build), so the binary drops in — **no
  Node runtime needed**.

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
    {ID: "pi-tui", Mode: ModeTmux, Launch: []string{"pi"}, Resume: <S1>, Relay: RelayRawPTY, Label: "pi · terminal"},
},
```

`Known("pi")` becomes true and the runnable auto-expands into the web
`runnable-select` dropdown via `ListAgentImages`. The exact `Resume` args
(Codex uses `resume --last`, Claude `--continue`) are an **S1 output** — pi has
client-side sessions (`--session-dir` / `switch_session`); confirm the TUI's
resume flag empirically.

### 2. Dispatcher — `deploy/agent/launch`

Add a `pi-tui)` case (sibling of `codex-tui)`). The model is per-spawn, so
generate `~/.pi/agent/models.json` at runtime defining a `spawnery` custom
provider pointed at the sidecar, then `start_tmux … -- pi`:

```jsonc
// ~/.pi/agent/models.json  (PI_HOME default ~/.pi → /root/.pi)
{
  "providers": {
    "spawnery": {
      "baseUrl": "<OPENAI_BASE_URL>",        // already = http://<sidecar>:8080/v1
      "apiKey":  "sk-unused-sidecar-injects-real-key",
      "api":     "openai-completions",        // sidecar speaks Chat Completions
      "models":  [ { "id": "<SPAWN_MODEL>", "contextWindow": <CTX>, "maxTokens": <MAX> } ]
    }
  }
}
```

Then launch pi pinned to that provider/model (e.g.
`pi --provider spawnery --model spawnery/<SPAWN_MODEL>` — **exact flag/value form
is an S1 output**), forwarding `PI_HOME` (and any dummy-key env) through
`start_tmux`'s explicit env list so the tmux-launched process inherits them.
Mirror the env-with-default idiom (`export PI_HOME="${PI_HOME:-/root/.pi}"`).
Model switching inside pi's own picker satisfies "switchable models, like Codex."

The exact `models.json` schema (top-level shape, field names, whether a `models`
catalog is required vs. discovered) is **load-bearing** — verify against pi's
`custom-provider.md` / `models.md` for the **pinned** version during S1.

### 3. Dockerfile — `deploy/agent/Dockerfile`

Install the pinned pi **glibc** binary from GitHub releases, SHA256-verified,
with a `pi --version` check — mirroring the goose tarball approach. No Node.js.

```dockerfile
ARG PI_VERSION=v0.79.10
RUN set -eux; \
    arch="$(dpkg --print-architecture)"; case "$arch" in amd64) a=x64;; arm64) a=arm64;; esac; \
    curl -fsSL -o /tmp/pi.tgz "https://github.com/earendil-works/pi/releases/download/${PI_VERSION}/pi-linux-${a}.tar.gz"; \
    # verify against the release SHA256SUMS, extract the self-contained `pi` ELF to /usr/local/bin, chmod, then:
    pi --version
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

## Spike S1 (pre-implementation verification, manual, in a pod)

Mirrors the Codex spike. Run against the **pinned** pi binary; outputs feed the
launch-case details above.

1. **TUI under tmux + resume.** Confirm `pi` runs cleanly in a detached tmux
   session non-interactively (no first-run OAuth/trust prompt blocking it with an
   API-key provider), and capture the **resume flag/command** for the registry
   `Resume` field.
2. **`openai-completions` round-trips the sidecar.** Confirm pi with
   `api: openai-completions` + `baseUrl=<sidecar>/v1` reaches the model and that
   pi sends **full conversation history each turn** (no server-side response-store
   dependency). Capture the exact `models.json` schema and the launch flag form
   (`--provider`/`--model`) that pins our provider.
3. **`--mode rpc` semantics.** Confirm the documented event/command shapes and the
   `agent_end` turn-complete signal against the pinned binary, so the `piadapter`
   translation table is built on observed reality, not just docs (pi ships ~daily).

If S1 surfaces that the TUI cannot be driven non-interactively under tmux, fall
back Phase 1 to driving `--mode rpc` behind the Phase-2 adapter (the rich-web
path), and reconsider whether a separate tmux runnable is worthwhile.

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
