# General Artifact-Injection (sp-l5sx) + Universal Cross-Agent Install Adapter (sp-1bia)

**Date:** 2026-06-14 · **Epics:** sp-l5sx (substrate), sp-1bia (adapter), under sp-nrzf.
**Grounded by:** [Cross-Agent Installer Research](2026-06-14-cross-agent-installer-research-results.md)
(ultradeep, 106 agents). **Related:** sp-7h6.1 (E2E secrets), sp-freg/sp-hau3 (facets), sp-mofj
(Hermes), [Codex CLI Support](2026-06-08-codex-cli-support-design.md),
[Tmux Terminal Mode](2026-06-06-tmux-terminal-mode-design.md).

> **Revised 2026-06-14 post-roast + spikes.** The first draft drew a **BLOCK** —
> [adversarial review](2026-06-14-cross-agent-installer-adversarial-review.md), 26 confirmed findings.
> This version folds in all 10 amendments. Two binding decisions from that review drive the rest:
> **(1) all artifacts install at USER scope, never project scope** (kills the Claude-MCP headless
> blocker and the opencode clobber); **(2) MCP secret values are file-based at 0600** — the launcher
> bounded-waits for async secret delivery, then `agentinstall` writes the literal value into the
> agent's MCP config (honors roast-M10 "files-never-env"). **Spikes S1–S7 are run** (results below):
> S1/S2/S7 confirmed; S4/S5/S6 forced corrections (no pre-exec secret path; codex skills at
> `~/.codex/skills` not `~/.agents/skills`).

## Problem

A user's spawns should come pre-equipped with their own **skills**, **MCP servers**, and **config**,
delivered into the agent container at spawn creation. Today only a single inference key reaches the
*sidecar*; there is no general path to inject artifacts into the *agent* container, and each
supported agent (claude-code, codex, opencode, hermes — plus goose in the shared image) stores
skills/MCP/config in a different format and location. We need (1) a general delivery substrate and
(2) a universal adapter that takes **one** logical instruction — "install this skill / MCP / config"
— and materializes it correctly for **every** agent.

## Architecture: three tiers

```
┌─ sp-1bia: agentinstall — standalone Go CLI, ZERO spawnery deps ────────────┐
│   canonical artifact (skill|mcp|config) → per-agent emitter → native file   │
└─────────────────────────────────────────────────────────────────────────────┘
            ▲ reads staging dir (defined contract); runs in-pod before agent exec
┌─ sp-l5sx: delivery substrate — content-agnostic "get bytes into the pod" ──┐
│   ArtifactSpec → (sensitive? E2E relay : plain provision) → staging tmpfs    │
└─────────────────────────────────────────────────────────────────────────────┘
```

- **sp-l5sx** is the dumb pipe: gets raw bytes into the right container safely. Knows nothing about
  skills/MCP. Content-agnostic; supports `agent` and `sidecar` targets (engine only uses `agent`).
- **sp-1bia** is the brain: a **standalone** CLI that reads canonical descriptors from a defined
  staging-dir contract and emits each agent's native config. Independently useful on any dev box;
  embedded in spawnery via the launcher.

**Key decisions (and why):**
- **Canonical-source → per-agent emitters**, not copy/symlink — the agents use incompatible config
  languages (Claude JSON, Codex TOML, opencode **JSONC**, Hermes YAML, goose YAML).
- **USER scope for everything** — approval-free and CWD-independent; matches "spawns come
  pre-equipped across all projects." Project scope is never used. (Closes blocker [1], [2].)
- **Translate in-container, never CP-side** — sensitive artifacts must stay invisible to the CP.
- **Standalone CLI** — also makes conformance tests hermetic (temp `HOME`, no pod).
- **Run before agent exec**, after the launcher's base-config gen — avoids clobber where the two
  write the same file (codex); for claude/opencode user-scope lands in *separate* files the launcher
  never rewrites.

## Canonical artifact model (engine input)

```
Artifact {
  kind: skill | mcp | config
  name: string
  targets: [claude|codex|opencode|hermes|goose] | "all-detected"
  skill:  { dir }                                  # SKILL.md tree; the DIRECTORY name is the identity
  mcp:    { transport: stdio{command,args,env} | http{url,headers},
            secretRefs: [ENV_VAR_NAME...] }        # references, NEVER values
  config: { normalized: { <enumerated keys> },     # see "Config facet" — launcher-managed keys forbidden
            native:    { <agent>: <fragment> } }   # passthrough escape hatch
}
```

The emitter performs **field/shape translation** per agent — the canonical model is not assumed to
match any agent's wire shape (e.g. opencode renames `env`→`environment`, requires `enabled`, and
folds `command`+`args` into one `command` array — **S2-validated**). See the emitter table.

## sp-1bia: the standalone `agentinstall` CLI

- **`cmd/agentinstall`**, no spawnery imports → `go install`-able; baked into the agent image
  (Dockerfile).
- Commands: `install skill|mcp|config …`, `apply --from <staging-dir>` (batch), `list-agents`,
  `--agent <id> | --all-detected`.
- **Emitter registry** — interface `Emitter{ InstallSkill, InstallMCP, ApplyConfig }` per agent.
  - **parse-modify-serialize** with **format-preserving** libraries (JSONC for opencode, comment-aware
    TOML for codex, `yaml.v3` for hermes/goose) — never `encoding/json` on JSONC, never text munging.
  - **upsert by name** (idempotent); **atomic** temp-file + rename.
  - **`--agent` takes a normalized emitter name** (`claude|codex|opencode|hermes|goose`). In spawnery
    the launcher maps `runnable_id → emitter name` (table below); unknown/unsupported runnables
    (`shell`, `stub-acp`, `nori`) → **no-op + report**, never an error.
- **Missing concept → no-op + structured report** (`{agent, kind, status:skipped, reason}`).
- **Runtime-dep check** — before emitting an MCP `command`, verify the runtime (`node`/`npx`/`python`/
  `uv`) is on `PATH`; if absent, still emit but **flag it in the report** (don't fail).
- **Emitter versioning** — each emitter records the agent schema version it targets and, where cheap,
  re-reads its output to assert the upsert took (guards schema drift — the cited antipattern).
- **`--all-detected` detection algorithm** — an agent is "present" if its config root exists
  (`~/.claude`, `~/.codex`/`$CODEX_HOME`, `~/.config/opencode`, `~/.hermes`, `~/.config/goose`).
  `list-agents` prints detected + registered. (Spec it explicitly; don't leave to impl.)

### Per-agent emitters (USER scope; corrected facts)

| Agent | skill | mcp | config |
|---|---|---|---|
| **claude** | `~/.claude/skills/<dir>/SKILL.md` (**dir name = identity**) — **S1 ✓** | **`~/.claude.json`** top-level `mcpServers` map (USER scope — **S1 ✓ approval-free + headless**; project `.mcp.json` is gated, confirmed); env = `env` map | `~/.claude/settings.json` merge |
| **codex** | **`~/.codex/skills/<name>/SKILL.md`** (**native**, S6 ✓ — **NOT `~/.agents/skills`**; may require the `--with-skills` launch flag) | `$CODEX_HOME/config.toml` `[mcp_servers.*]` merge **after base-gen**; HTTP secret-by-name via `bearer_token_env_var`/`env_http_headers` (but see "MCP secrets" — stdio secret values land in `env` at 0600) | config.toml merge (no launcher-managed keys) |
| **opencode** | no-op+report (S6 — opencode skills layout unconfirmed) | **`~/.config/opencode/opencode.json`** (USER global — **S2 ✓ read**; separate from launcher's `OPENCODE_CONFIG` → no clobber, opencode deep-merges). **JSONC** (opencode **hard-errors** on invalid config); `type:local\|remote` (**`enabled` required**); env field is **`environment`**; `command` is a **single array** (S2 validator ✓) | opencode.json merge |
| **hermes** | `~/.agents/skills` (`skills.external_dirs`) | **native YAML** `~/.hermes/config.yaml` `mcp_servers` — **do not rely on Claude-Code discovery** (Hermes drives Claude with `--strict-mcp-config`/`--bare`, bypassing in-pod `.mcp.json`). Deferred to **sp-mofj** spike | YAML merge |
| **goose** | no-op+report (deferred) | `~/.config/goose/config.yaml` merge **after base-gen** (launcher-regenerated) | config.yaml merge |

## sp-l5sx: delivery substrate

### Declaration (proto)

- Add `repeated ArtifactSpec artifacts` to `CreateSpawnRequest` (`proto/cp/v1/cp.proto`) and
  `StartSpawn` (`proto/node/v1/node.proto`); add `artifacts` to `AppManifest`. Run `make gen`;
  serialize `proto/`-touching work.
  ```
  ArtifactSpec {
    id; content: { inline bytes };            # by-ref DROPPED from MVP (see below)
    contentType: bytes | tar;                 # tar => a packaged dir (skill tree); spawnlet unpacks
    targetContainer: agent | sidecar; destPath; mode; sensitive: bool;
    envVarName;                               # for sensitive MCP secrets: binds this secret to an env var
  }
  ```
- **Inline-only for MVP** (finding [7]/[8]/[10]): there is **no general CP-side blob store** today,
  and by-ref + sensitive breaks CP-blindness. Carry content inline with an explicit **size cap**
  (well under the gRPC/Connect ~4 MB message limit; reject oversize at `CreateSpawn` with a clear
  error). by-ref (and a real content store) is a **deferred** follow-up.
- **Skill dirs** are packaged as a **`tar`** (`contentType:tar`) → one blob; spawnlet unpacks into the
  staging dir preserving per-file modes (fixes the single-`mode` limit [J45]).
- **Persisted on the spawn row** and **re-threaded into resume/recreate/migrate `StartSpawn`**
  (finding [6]/[J34]) — artifacts are create-time-declared but durable across the spawn's life.

### Delivery split

- **Non-sensitive** → CP relays inline bytes in `StartSpawn` → spawnlet materializes into the staging
  tmpfs **`/run/spawnery/artifacts`** per the staging contract below.
- **Sensitive** → rides the **E2E `SealedSecret` path** (sp-7h6.1.3): CP ciphertext-blind, node
  unseals via HPKE sub-key, `SecretInjector.Write` lands plaintext at `0600` under
  `/run/spawnery/secrets`. **Sensitive artifacts are restricted to single-file** (config/MCP-secret
  values) for MVP — multi-file sensitive skill dirs are out (finding [9]); revisit with a sealed-tar
  packaging if needed.

### Staging-dir contract (the sp-l5sx ↔ sp-1bia interface — finding [15])

`/run/spawnery/artifacts/` contains:
- `manifest.json` — an index: ordered list of canonical `Artifact` descriptors (kind, name, targets,
  per-kind fields, `payload` path, `sensitive`, `secretRefs`).
- `payloads/<id>/…` — materialized bytes: a single file for mcp/config, an **unpacked SKILL.md tree**
  for skills.
- Sensitive secret values live under `/run/spawnery/secrets/` keyed by `envVarName`; the manifest's
  `secretRefs` point at those names. `agentinstall` reads the manifest, never the network.

## In-pod integration (before agent exec)

- spawnlet adds the `/run/spawnery/artifacts` staging mount alongside `/run/spawnery/secrets`.
- `deploy/agent/launch`, per-runnable, **after base-config gen and BEFORE `start_tmux`/`exec`**, maps
  `runnable_id → emitter name` and invokes:
  ```
  agentinstall apply --agent <emitter-name> \
      --artifacts /run/spawnery/artifacts --secrets /run/spawnery/secrets
  ```
- **Failure policy (finding [16]):** the call runs in a subshell that **does not** let a non-zero exit
  kill the entrypoint under `set -e`; failures are captured and surfaced as a spawn-level diagnostic
  (artifact/emitter that failed) back through spawnlet to the CP/client, while the agent still starts.
- **Old-image guard:** if `agentinstall` is not on `PATH` (image predates it) but artifacts were
  delivered, spawnlet reports an incompatibility rather than silently dropping them.

### MCP secrets — launcher-waits-then-writes-config-0600 (S4/S5-corrected; finding [11]–[14])

Two spike findings reshape this from the first amendment:
- **S4:** there is **no pre-exec secret path**. Sealing targets the hosting node's HPKE sub-key,
  which exists only after the node registers (post-placement); the agent container execs immediately
  at `StartAgent` with an **empty** secrets tmpfs; secret values arrive **async, after start** via the
  separate `SecretDelivery` channel. `StartSpawn` carries no secret material. (Architectural — not a
  bounded change.)
- **S5:** all three agents resolve env-var indirection **from the agent's own process env** (claude
  `${VAR}`, opencode `{env:VAR}`, codex `env_vars source=local` forwards a host var). So "give the MCP
  server a var without the value being in the agent's env" is infeasible, and putting it in the agent
  env violates **roast-M10** (inherited by every child + `/proc/<pid>/environ`).

**Corrected flow:**
1. The auto-inject secret flow (sp-7h6.1) delivers sealed MCP secret values; the node unseals them to
   `/run/spawnery/secrets/<envVarName>` **asynchronously, after container start**.
2. When a delivered MCP artifact carries `secretRefs`, the launcher **blocks (bounded wait) until
   those secret files exist** *before* exec'ing the agent (the agent spawns its MCP servers on start,
   so the wait must precede exec). No `secretRefs` → no wait. Timeout → diagnostic + start without
   that server.
3. `agentinstall` then writes the **literal secret value into the agent's native MCP config at mode
   0600** (claude `mcpServers.<n>.env`, codex `[mcp_servers.<n>].env`, opencode `environment`). This
   is **file-based (not env), and the agent passes the var to only that MCP server's child** — so it
   honors M10's actual concerns (not runtime-env-persisted, not inherited by the whole process tree)
   *better* than env indirection would. (A live env-isolation test confirms scoping at impl.)
- `ArtifactSpec.envVarName` binds a SealedSecret to its target var name; the existing path is
  otherwise reused.

## Trust & safety model (finding [25])

- **Injection rights:** owner-supplied per-spawn artifacts vs publisher-supplied `AppManifest`
  artifacts are distinct trust tiers; the existing catalog-admin model governs manifest artifacts,
  and owner artifacts are scoped to the owner's own spawn.
- **Limits:** per-spawn artifact count + total size caps; reject oversize at `CreateSpawn`.
- **Path confinement:** `destPath` is validated to stay within the staging/agent-config roots — no
  `..`, no absolute escape, no writing launcher-managed/host-adjacent files.
- **MCP endpoints:** injected MCP `url`/`command` is a prompt-injection / exfil surface (Claude's own
  docs warn); MVP surfaces them in the report and relies on the per-pod egress floor; an
  allowlist/scan hook is a defined extension point.
- **Emitters never write launcher-managed keys** (notably `model`, which is sidecar-coupled) —
  enforced in the config emitter.

## Config facet (finding [22]/[23])

- **Enumerate** the small normalized key set up front (MVP candidates: a curated, non-launcher-managed
  subset — e.g. permission/approval posture where mappable). `model` and other launcher-managed keys
  are **excluded by construction**.
- `approvalPosture` is semantically lossy across agents (codex `approval_policy` enum vs claude
  permissions vs opencode); ship it only with an explicit per-agent value mapping + a fallback for
  unmappable values, else defer it to native-passthrough.

## Testing

- **Conformance (sp-arz9):** run standalone `agentinstall` against a temp `HOME` per agent; parse the
  emitted native file back; assert path + format + idempotency + **the launcher-clobber scenario**
  (run base-gen, then agentinstall, then assert survival) [J31]. Hermetic, no pod.
- **Substrate:** ArtifactSpec routing (sensitive vs plain), inline size-cap rejection, tar unpack +
  per-file modes, staging-manifest round-trip, **resume re-threading** (artifacts survive
  suspend/resume).
- **Secret scoping:** assert MCP secret values reach only the target server's env, not the agent tree
  (extends the M10 canary to cover env exposure [J52]).
- All unit tests hermetic, `-race`, in the `dev-spawnery` distrobox.

## Spike results (run 2026-06-14)

- **S1 ✅ confirmed** — claude 2.1.177: user-scope MCP added via `claude mcp add -s user` (→
  `~/.claude.json` top-level `mcpServers`) is **active headlessly, no approval**; project-scope
  `.mcp.json` shows `⏸ Pending approval` (the blocker, reproduced). **Decision validated:** emit to
  user scope. No `enableAllProjectMcpServers` needed.
- **S2 ✅ confirmed** — opencode 1.15.13: reads `~/.config/opencode/opencode.json` (XDG user); its
  validator **rejects** `command` as a string ("Expected array"), requires `enabled`, and uses
  `environment` (not `env`). opencode **hard-errors** on invalid config → JSONC-aware + exact emission
  mandatory.
- **S4 ⚠️ corrected** — **no pre-exec secret path** (architectural). Drove the "MCP secrets" redesign:
  launcher bounded-wait for async delivery, then write-to-config-0600.
- **S5 ⚠️ corrected** — agents resolve env indirection from their own process env → literal value into
  config-0600 (scoped per-server) is the M10-honoring path, not env-by-name. (See "MCP secrets".)
- **S6 ⚠️ corrected** — codex 0.139.0 **has** native skills (`--with-skills`, `codex_core_skills`) but
  at **`~/.codex/skills`**, *not* `~/.agents/skills`. The shared-dir collapse is **dead** for codex;
  keep **per-agent skill placement** (claude `~/.claude/skills`, codex `~/.codex/skills`). opencode
  skills layout still unconfirmed → no-op+report.
- **S7 ✅ confirmed + located** — all four lifecycle paths (Create/Resume/Recreate/Migrate) converge on
  `Scheduler.Provision()` (`internal/cp/scheduler/scheduler.go:80-99`), which already threads
  `BaseImageDigest`/rootfs. **Fix:** persist artifacts in a `spawn_artifacts` table (parallel to
  `spawn_mounts`), fetch alongside `GetMounts`, add an `artifacts` param to `Provision` →
  one point covers all paths. Agent HOME survives *same-node* resume via the rootfs delta, but the
  staging tmpfs is ephemeral → re-thread + idempotent re-apply is mandatory.
- **S3 — contract defined** (not yet round-tripped in code): the staging-dir contract is specified
  above (`manifest.json` + `payloads/<id>/`); the literal materialize↔apply round-trip lands in the
  first substrate PR (`sp-l5sx.1`/`.3`).

## Scope / non-goals (MVP)

- **In:** substrate (proto + inline delivery + tar skill packaging + staging contract + resume
  re-threading); standalone `agentinstall`; **claude/codex/opencode** emitters for skill/mcp/config
  at user scope; enumerated normalized config + passthrough; no-op+report for missing concepts +
  unknown runnables; **file-based per-server MCP secrets**; trust limits + path confinement.
- **Deferred:** **by-ref** + a real content store (and sensitive-by-ref reconciliation); **hermes**
  emitter (behind sp-mofj); **goose** emitter (add if S-goose shows a stable YAML path, else explicit
  no-op); multi-file **sensitive** skill dirs; per-user library + selection UX (sp-freg/sp-hau3);
  AGENTS.md emulation; MCP endpoint allowlist/scanning; explicit uninstall (upsert keys anticipate it).

## Open items to resolve during implementation

1. Confirm opencode/goose skills directory + format (S2/S6) — until confirmed, no-op+report.
2. Read Neon `add-mcp` directly as prior art for the emitter set.
3. Finalize the enumerated normalized config key set + per-agent mappings (design-review before code).
4. Decide goose's inclusion based on its config.yaml MCP path stability.
