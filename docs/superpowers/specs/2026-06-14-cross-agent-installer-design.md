# General Artifact-Injection (sp-l5sx) + Universal Cross-Agent Install Adapter (sp-1bia)

**Date:** 2026-06-14 · **Epics:** sp-l5sx (substrate), sp-1bia (adapter), under sp-nrzf.
**Grounded by:** [Cross-Agent Installer Research](2026-06-14-cross-agent-installer-research-results.md)
(ultradeep, 106 agents). **Related:** sp-7h6.1 (E2E secrets), sp-freg/sp-hau3 (facets), sp-mofj
(Hermes), [Codex CLI Support](2026-06-08-codex-cli-support-design.md),
[Tmux Terminal Mode](2026-06-06-tmux-terminal-mode-design.md).

## Problem

A user's spawns should come pre-equipped with their own **skills**, **MCP servers**, and **config**,
delivered into the agent container at spawn creation. Today only a single inference key reaches the
*sidecar*; there is no general path to inject artifacts into the *agent* container, and each
supported agent (claude-code, codex, opencode, hermes) stores skills/MCP/config in a different
format and location. We need (1) a general delivery substrate and (2) a universal adapter that takes
**one** logical instruction — "install this skill / MCP / config" — and materializes it correctly
for **every** agent.

## Architecture: three tiers

```
┌─ sp-1bia: agentinstall — standalone Go CLI, ZERO spawnery deps ────────────┐
│   canonical artifact (skill|mcp|config) → per-agent emitter → native file   │
└─────────────────────────────────────────────────────────────────────────────┘
            ▲ reads staging dir; runs in-pod, launcher-sequenced
┌─ sp-l5sx: delivery substrate — content-agnostic "get bytes into the pod" ──┐
│   ArtifactSpec → (sensitive? E2E relay : plain provision) → staging tmpfs    │
└─────────────────────────────────────────────────────────────────────────────┘
```

- **sp-l5sx** is the dumb pipe: gets raw bytes into the right container safely. Knows nothing about
  skills/MCP. Content-agnostic; supports `agent` and `sidecar` targets (engine only uses `agent`).
- **sp-1bia** is the brain: a **standalone** CLI that reads canonical descriptors from a staging dir
  and emits each agent's native config. Independently useful on any dev box; embedded in spawnery via
  the launcher.

**Key decisions (and why):**
- **Canonical-source → per-agent emitters**, not copy/symlink — the four agents use incompatible
  config languages (Claude JSON, Codex TOML, opencode JSON/JSONC, Hermes YAML); research confirmed
  copy/symlink cannot work for MCP/config (3-0).
- **Translate in-container, never CP-side** — sensitive artifacts must stay invisible to the CP;
  translating where you can't see the data is a confirmed antipattern. The node unseals; the
  in-container adapter consumes plaintext from a tmpfs.
- **Standalone CLI** (user's explicit ask) — also makes conformance tests hermetic (run the binary
  against a temp HOME, no pod).
- **Launcher-sequenced** — our `deploy/agent/launch` regenerates `config.toml`/`opencode.json` at
  startup; running the adapter *after* base-config gen avoids the clobber (the single biggest
  correctness constraint, per research §2).

## Canonical artifact model (engine input)

```
Artifact {
  kind: skill | mcp | config
  name: string
  targets: [claude|codex|opencode|hermes] | "all-detected"
  skill:  { dir }                                  # SKILL.md tree; emitter pins frontmatter name:
  mcp:    { transport: stdio{command,args,env} | http{url,headers},
            secretRefs: [ENV_VAR_NAME...] }        # references, NEVER values
  config: { normalized: { model, approvalPosture, ... },   # small cross-agent key set
            native:    { <agent>: <fragment> } }            # passthrough escape hatch
}
```

- **Config facet = normalized keys + native passthrough.** A small set of cross-agent normalized
  keys maps to each agent; anything not normalized rides a per-agent native fragment merged verbatim.
  (Chosen over full normalization — agent schemas drift fast — and over passthrough-only — we want
  *some* genuine cross-agent config.)

## sp-1bia: the standalone `agentinstall` CLI

- **`cmd/agentinstall`**, no spawnery imports → `go install`-able; baked into the agent image
  (Dockerfile).
- Commands: `install skill|mcp|config …`, `apply --from <staging-dir>` (batch), `list-agents`,
  `--agent <id> | --all-detected`.
- **Emitter registry** — interface `Emitter{ InstallSkill, InstallMCP, ApplyConfig }` with
  claude/codex/opencode/hermes implementations.
  - **parse-modify-serialize** the native config; **upsert by name** (idempotent re-runs never
    duplicate); **atomic** temp-file + rename per agent.
  - Never `sed`/`jq`/`awk` text munging.
- **Missing concept → no-op + structured report**: e.g. `{agent:codex, kind:skill,
  status:skipped, reason:"no native skills"}`. Honest and predictable; emulation deferred.
- **Secrets:** MCP `secretRefs` emit env-var-*name* references in native config (Codex
  `bearer_token_env_var`/`env_http_headers`, etc.); values never enter config files.

### Per-agent emitters

| Agent | skill | mcp | config |
|---|---|---|---|
| **claude** | `~/.claude/skills/<name>/SKILL.md`, pin frontmatter `name:` | **`.mcp.json`** (standalone → no launcher clobber) | `~/.claude/settings.json` merge |
| **codex** | no-op + report | `$CODEX_HOME/config.toml` `[mcp_servers.*]` merge **after** base gen | config.toml merge (normalized keys + native passthrough) |
| **opencode** | no-op + report (skills layout TBD) | `opencode.json` `mcp` key, explicit `type: local\|remote` | opencode.json merge |
| **hermes** | stub (deferred to sp-mofj spike) | YAML `~/.hermes/config.yaml` `mcp_servers` | YAML merge |

- **Hermes** delegates coding to the Claude Code CLI it bundles, so much of its support reduces to
  the claude emitter; the native YAML emitter is deferred behind the sp-mofj spike. Register it as a
  known target that currently reports "deferred".
- **Claude MCP → `.mcp.json`** (not `settings.json`) deliberately: a standalone file the launcher
  never regenerates, dodging the clobber.

## sp-l5sx: delivery substrate

### Declaration (proto)

- Add `repeated ArtifactSpec artifacts` to `CreateSpawnRequest` (`proto/cp/v1/cp.proto`) and
  `StartSpawn` (`proto/node/v1/node.proto`); add `artifacts` to `AppManifest` for per-app defaults.
  Regenerate with `make gen` (never hand-edit `gen/`). Serialize `proto/`-touching work.
  ```
  ArtifactSpec {
    id; content: { inline bytes | byRef uri };
    targetContainer: agent | sidecar; destPath; mode; sensitive: bool
  }
  ```
- **MVP source:** declared **per-spawn at create** (and optionally per-app manifest). Small content
  inline; large content (skill dirs) **by-ref** to a content store (MVP: reuse the CP-side
  blob/store; Garage object path deferred). Per-user library + selection UX belong to the facet
  epics (sp-freg/sp-hau3).

### Delivery split

- **Non-sensitive** → CP relays plaintext in `StartSpawn` → spawnlet materializes into a staging
  tmpfs **`/run/spawnery/artifacts`** (mirrors the existing secrets tmpfs at
  `/run/spawnery/secrets`, `internal/spawnlet/manager.go:713`).
- **Sensitive** → rides the **existing E2E `SealedSecret` path** (sp-7h6.1.3): CP stays
  ciphertext-blind; node unseals via its HPKE sub-key (`internal/node/secrets.go`,
  `internal/secrets/seal`); `SecretInjector.Write` (`internal/spawnlet/secrets.go`) lands plaintext
  in the secrets tmpfs at mode `0600`.

## In-pod integration (launcher-sequenced)

- spawnlet adds the `/run/spawnery/artifacts` staging mount alongside the secrets tmpfs (new mount
  in `internal/spawnlet/manager.go`; surfaced via `AgentSpec.Mounts`, `internal/runtime/pod.go`).
- `deploy/agent/launch`, per-runnable, **after** base-config generation, invokes:
  ```
  agentinstall apply --agent <runnable_id> \
      --artifacts /run/spawnery/artifacts --secrets /run/spawnery/secrets
  ```
- Secret **values** are delivered into the secrets tmpfs as an env file the launcher **sources**, so
  MCP env-var references resolve at runtime. Values never enter config files; the CP never sees
  plaintext.

## Testing

- **Conformance (sp-arz9):** run the standalone `agentinstall` against a temp `HOME` per agent;
  **parse the emitted file back** and assert path + format + idempotency (re-run = no duplicate).
  Hermetic, no pod — table-driven per (artifact × agent). Direct payoff of the standalone design.
- **Substrate:** hermetic unit tests for ArtifactSpec routing (sensitive vs plain), inline vs by-ref
  materialization, staging-mount creation. Sensitive-path e2e reuses the existing secret-delivery
  test lane (build-tagged).
- All unit tests hermetic, run with `-race` in the `dev-spawnery` distrobox.

## Scope / non-goals (MVP)

- **In:** substrate (proto + delivery split + staging mount); standalone `agentinstall` CLI;
  claude/codex/opencode emitters for skill/mcp/config; normalized-keys+passthrough config;
  no-op+report for missing concepts; inline+by-ref per-spawn delivery; sensitive via E2E.
- **Deferred:** Hermes native emitter (behind sp-mofj spike); per-user library + selection UX
  (sp-freg/sp-hau3); AGENTS.md skill emulation for codex/opencode; full config normalization; Garage
  by-ref backing; explicit uninstall (upsert keys anticipate it).

## Open items to resolve during implementation

1. Confirm opencode's skills/plugins directory + format (research left it TBD) — until confirmed,
   opencode skills = no-op+report.
2. Read Neon `add-mcp` directly as prior art for the emitter set (its claims went unverified under
   rate-limiting).
3. Confirm exactly which launcher-generated files each emitter must sequence after, and validate the
   `.mcp.json`-vs-`settings.json` choice against the current Claude Code loader.
4. Whether `~/.agents/skills` is read by claude/codex/opencode (vs Hermes-opt-in only) — if broadly
   read, skills could collapse to a single shared dir.
