# All-Agent Skill Install — Canonical Shared Dir + Verified Emitters

**Date:** 2026-07-12
**Status:** draft
**Epic:** `sp-mwco.2`
**Extends:** [Artifact-Injection + Cross-Agent Installer Design](2026-06-14-cross-agent-installer-design.md) (`sp-l5sx`/`sp-1bia`),
[Cross-Agent Installer Research](2026-06-14-cross-agent-installer-research-results.md)

## 1. Problem

"Skills in a profile get auto-installed for all agents" is today true only for claude-code. The
`agentinstall` registry (`internal/agentinstall/registry.go`) installs skills for claude
(`~/.claude/skills/`, e2e-proven) and codex (`$CODEX_HOME/skills/`, written on faith); opencode is
a permanent no-op ("skills layout unconfirmed (S6)"), goose and hermes are deferred no-ops, and
pi/nori are not in the registry at all. The codex capability entry claims `supported` with no
read-side evidence, and the runnable→emitter mapping is duplicated between the Go registry and
`deploy/agent/apply-artifacts.sh:23-29`.

## 2. Ground truth (deep-research, 2026-07-12, adversarially verified)

The landscape changed since the June research: **`~/.agents/skills` is now a real cross-agent
standard**, read natively by four of the five non-claude agents:

| Agent | Native skills | Reads `~/.agents/skills` | Caveat for our pinned binary |
|---|---|---|---|
| codex | yes (Dec 2025; `~/.agents/skills` since rust-v0.95.0, `~/.codex/skills` kept as compat) | **yes** | pinned rust-v0.137.0 includes v1 skills prompt-injection; feature-flag state at 0.137 unverified |
| opencode | yes (native since v1.0.190; also reads `~/.claude/skills`) | **yes** | image version must be ≥1.0.190 |
| goose | yes (Skills ext v1.16–1.24 → **Summon** ext v1.25+) | **yes** | extension must be enabled; image version + headless enablement unverified |
| pi | yes (Agent Skills standard; `/skill:name`) | **yes** (global dirs not trust-gated) | none expected |
| hermes | yes (`~/.hermes/skills/`, agentskills.io format) | **no by default** — `skills.external_dirs` must list it | one config.yaml line |
| claude | yes (`~/.claude/skills/`) | **unverified** (excluded from the research fan-out; this repo's own beads-skill convention hints newer Claude Code may read it) | spike decides: native pickup → no glue at all; else symlink/copy from canonical |

All six use the same SKILL.md + frontmatter format, so one canonical install serves everyone.

## 3. Key decisions (user-pinned)

1. **Canonical shared dir**: `agentinstall` installs each skill exactly once into
   `~/.agents/skills/<name>/`. That alone natively covers codex, opencode, goose, and pi.
2. **Per-agent glue where needed**: claude symlinks `~/.claude/skills/<name>` → canonical
   (copy fallback if the spike shows symlinked skill dirs don't load); hermes gets
   `skills.external_dirs: [~/.agents/skills]` emitted into `~/.hermes/config.yaml`; goose gets the
   Summon extension enabled in its config; codex gets the feature flag set if the 0.137 spike says
   it is still required.
3. **Instructions shim demoted to contingency**: the AGENTS.md "here are your skills" block is
   built only for an agent whose pod spike fails despite the above; it is not expected to be
   needed. Capability for such an agent would be `best-effort`.
4. **All agents in scope**: claude (regression only), codex, opencode, goose, hermes, pi.
   nori is an ACP client, not a harness — explicitly no emitter.
5. **Nothing ships on faith**: every agent's read side is proven by a pod-level spike against the
   pinned binaries in the agent image before its capability entry says `supported`.

## 4. Design

### 4.1 Spikes (gating, cheap, one per agent)

Same shape for each: build the agent image, launch the runnable in a pod with a canary skill
pre-installed at the candidate location(s), and probe — file-level (skill visible in the agent's
skill listing where it has one) and behavioral (ask the agent to use the canary skill; it contains
a distinctive instruction, e.g. "reply with token X").

- **claude**: first check native `~/.agents/skills` pickup (unresearched — a yes deletes the glue
  entirely); else validate that a symlink `~/.claude/skills/<name>` → `~/.agents/skills/<name>`
  still loads (today's real-copy path is the proven baseline and the fallback).
- **codex** (rust-v0.137.0): does `~/.agents/skills` load without config? If not, which
  `config.toml` flag (`[features] skills = true`) is needed? Does the legacy `$CODEX_HOME/skills`
  compat path work as the fallback?
- **opencode**: confirm image version ≥1.0.190 and `~/.agents/skills` pickup (docs say it also
  reads `~/.claude/skills` — either satisfies).
- **goose**: image version; if ≥1.25 enable Summon in config, if 1.16–1.24 enable the Skills
  extension; confirm pickup. If the image's goose predates 1.16, decide: bump the image or shim.
- **hermes**: emit `skills.external_dirs` and confirm pickup (also sanity-check the inner
  Claude-delegation still sees skills via the claude glue).
- **pi**: confirm global-dir pickup with no trust prompt in headless pod mode.

Spike outputs are recorded in this spec's Post-Implementation Notes and drive the capability
matrix. A failed spike for an agent → that agent gets the instructions-shim contingency (or an
image bump), decided case by case.

### 4.2 Installer changes (`internal/agentinstall`)

- `installSkillTree` targets the canonical `~/.agents/skills/<name>/` once per skill, independent
  of which agents are being emitted for (new `canonical` phase before per-agent emitters).
- Per-agent skill emitters become thin glue: claude = symlink/copy from canonical; codex =
  config-flag emission if required (no more `$CODEX_HOME/skills` writes unless the spike says the
  compat path is the safer target); opencode/pi = no-op beyond canonical; goose = extension
  enablement in its config; hermes = `external_dirs` upsert in `config.yaml` (reuse the existing
  YAML config machinery).
- `Capabilities()` per agent updated to spike-verified truth; `capabilities.gen.json` regenerated
  so the web CapabilityPreview badges stop lying (codex currently claims `supported` on faith).
- pi emitter registered (deferred out of sp-2aaw); nori documented as intentionally absent.

### 4.3 Mapping cleanup

The runnable→emitter `case` in `deploy/agent/apply-artifacts.sh:23-29` is deleted; the shell
passes `$RUNNABLE` through and `agentinstall apply --runnable <x>` resolves the emitter from the
Go registry (single source of truth). Unknown runnables → clean no-op with a report line, as
today.

### 4.4 Interaction with skill delivery

No changes to ingestion, by-ref delivery, or the staging contract: the node materializes payloads
into the staging dir exactly as today, and `agentinstall` re-homes them. Only the install
destination logic changes. The SELinux relabel fix (`afcfb4c`) already covers the bind mount;
symlinks stay within the agent home volume so no new labeling concerns.

### 4.5 Testing

- **Unit**: canonical-install idempotency (re-apply → same tree); symlink vs copy glue; hermes
  YAML upsert (no clobber of user config); goose/codex flag emission; capability matrix golden.
- **Build-tagged e2e**: extend the skill e2e to a per-agent matrix — for each in-scope runnable,
  spawn with a probe skill and assert placement (canonical dir + agent-specific glue present).
  Full behavioral assertion (agent actually invokes the skill) for claude + one shared-dir agent
  (codex or opencode) to prove the canonical path end-to-end; file-level assertions for the rest
  (behavioral for all six would be slow and model-flaky).

## 5. Out of scope

Per-agent skill *translation* (all six consume SKILL.md as-is); nori/stub/shell runnables; skill
enable/disable UX per agent; upstreaming a spawnery skills catalog; MCP/config/plugin parity work
(tracked separately — this epic is skills only).

## Post-Implementation Notes

*As this design is implemented and iterated on — bug fixes, adjustments, anything that diverged
from the assumptions above — append a dated note here, whether or not a formal debugging skill was
used.*
