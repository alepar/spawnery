# Profile Config Facet — Cross-Agent Settings Research

**Date:** 2026-06-14 · **Method:** deep-research (104 agents, 22 sources, 105 claims → 25 verified,
**25 confirmed / 0 killed**). Run `wf_c428cecb-08e`. Feeds the **config facet** of the Profiles
customization tool ([v2 design](2026-06-14-cross-agent-installer-design.md) + profiles amendment).

> **Goal:** enumerate the superset of profile-relevant *behavior* config across the supported agents
> (excluding sidecar-owned inference wiring), then define the normalized vs passthrough vs
> launcher-managed settings model.

## Verified per-agent config surface (behavior knobs)

- **Claude Code** (`~/.claude/settings.json`): `permissions{allow,deny,ask}` (Tool(pattern) rules) +
  `permissions.defaultMode` (6-value enum: default/acceptEdits/plan/auto/dontAsk/bypassPermissions);
  `hooks` (rich pre/post-tool lifecycle, `disableAllHooks`); telemetry via `env` vars
  (`CLAUDE_CODE_ENABLE_TELEMETRY`, `…DISABLE_AUTO_MEMORY`, `…SKIP_PROMPT_HISTORY`). *Caveat: `auto`
  needs v2.1.83+, ignored from project/local since v2.1.142+.*
- **Codex CLI** (`~/.codex/config.toml`): `approval_policy` (untrusted/on-request[default]/never + a
  `granular{…}` object); `sandbox_mode` (read-only/workspace-write/danger-full-access) +
  `[sandbox_workspace_write]`; reasoning toggles `model_reasoning_effort`/`_summary`/
  `model_verbosity`.
- **opencode** (`opencode.json`/JSONC): `permission` (string OR per-tool map → exactly 3 states
  **allow/ask/deny**); `tools{<name>: bool}` (bash/edit/write/read/grep/glob/lsp/apply_patch…);
  `instructions` (array of file paths + globs); per-agent `temperature`/`top_p`/`steps`/`mode`/
  `prompt`.
- **Hermes** (`~/.hermes/config.yaml`): `terminal.backend` (local/docker/ssh/modal/…) = sandbox;
  per-action approval gates (`skills.write_approval`, …); `agent.disabled_toolsets` (list);
  file-based instructions/memory (`SOUL.md` system-slot-1; `MEMORY.md`/`USER.md` under
  `~/.hermes/memories/`).
- **goose** (`~/.config/goose/config.yaml`): ⚠️ **ZERO verified claims — config surface unverified.**
  Its approval/tool/instruction(`.goosehints`)/toggle surface must be re-checked before any
  normalization. Treated as **passthrough-only/unverified** for MVP (consistent with goose's
  deferred emitter).

## The normalized settings model (for the profile spec)

### Normalized keys (MVP — clear cross-agent equivalents)
| Normalized key | Type | claude | codex | opencode | hermes |
|---|---|---|---|---|---|
| **`approvalPosture`** | enum `never-ask` \| `ask-risky` \| `always-ask` | `permissions.defaultMode` (never-ask→bypassPermissions/auto; ask-risky→acceptEdits; always-ask→default/plan) | `approval_policy` (never / on-request / untrusted) | `permission` string (allow / ask / deny) | per-action `*.write_approval` gates | *(verify)* |
| **`allowedCommands` / `deniedCommands`** | string[] (tool/command patterns) | `permissions.allow`/`deny` | `approval_policy.granular` + patterns | `permission` per-tool map | approval gates | *(verify)* |
| **`disabledTools`** | string[] (built-in tool names) | `permissions.deny` patterns | deny patterns | `tools{<n>: false}` | `agent.disabled_toolsets` | *(verify)* |
| **`instructions`** | content blob (managed-file) | write `~/.claude/CLAUDE.md` | write `AGENTS.md` | `instructions` glob / `AGENTS.md` | `SOUL.md` | `.goosehints` *(verify)* |

- **`approvalPosture` is the strongest normalized setting** — a coarse 3-value enum maps lossily-but-
  usefully to every agent's native posture. It's the MVP anchor.
- **`instructions` is a managed-FILE convention, not a scalar**: the assembler writes the profile's
  instruction content to each agent's native instruction file (different filename per agent), rather
  than normalizing a config key. Marked `managed-by: spawnery-profile`.

### Native passthrough (agent-specific — not normalized)
A per-agent raw fragment (`native:{<agent>:{…}}`, already in the engine): **hooks** (Claude),
**reasoning/verbosity** (Codex `model_reasoning_*`/`model_verbosity`), **telemetry env vars**
(Claude), per-agent behavior knobs (opencode `steps`/`mode`/`color`). The user supplies the
agent-native shape; the emitter merges it verbatim.

### Excluded (launcher/sidecar-managed — MUST NOT be in a profile)
- **Inference wiring** (model/provider/base_url/api_key/catalogs) — sidecar/launcher owns it.
- **Sandbox/isolation** — Codex `sandbox_mode` + `[sandbox_workspace_write]`, Hermes
  `terminal.backend`: spawnery's launcher/runtime owns sandbox + egress selection; a profile must not
  override it (security). Excluded.
- **Sampling params** (opencode `temperature`/`top_p`, Codex `model_verbosity`): inference-adjacent;
  the sidecar owns inference. Excluded from MVP normalized; available via native passthrough only if a
  user explicitly opts in (flagged, not encouraged).

## Decisions resolved (the research's open questions)
1. **goose config:** passthrough-only/unverified for MVP; verify before normalizing (matches deferred goose emitter).
2. **sandbox_mode / terminal.backend:** **EXCLUDE** (launcher-managed; security boundary).
3. **instructions:** modeled as a **managed-file content blob** written to each agent's native file.
4. **sampling/reasoning:** **EXCLUDE** from normalized (sidecar/inference-adjacent); passthrough-only.

## Fast-moving keys (re-validate at implementation)
Claude `defaultMode=auto` (v2.1.83+, project/local-ignored since v2.1.142+); Codex `approval_policy`
legacy `on-failure` removed; opencode `permission` nesting (top-level vs `agent.<name>.permission`)
varies by version. Pin/version-gate per the emitter-versioning amendment.

## Post-Implementation Notes
*As this design is implemented and iterated on — bug fixes, adjustments, anything that diverged from
the assumptions above — append a dated note here, whether or not a formal debugging skill was used.*
