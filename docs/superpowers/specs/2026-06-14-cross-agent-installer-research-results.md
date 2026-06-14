# Cross-Agent Installer — Research Results (sp-l5sx / sp-1bia)

**Date:** 2026-06-14 · **Method:** ultradeep deep-research (106 agents, 24 sources fetched, 107
claims extracted, 25 adversarially verified — 13 confirmed / 12 killed). Run `wf_6f48bd62-f52`.

Grounds the design of the **general artifact-injection substrate (sp-l5sx)** and the **universal
cross-agent install adapter (sp-1bia)**. Goal of the run: durable *approaches / gotchas /
antipatterns*, with per-agent reality as supporting evidence.

> **Confidence caveat (from the run):** the per-agent *reality* (Part 4) is the strongest verified
> evidence. The architectural *approaches/antipatterns* (Parts 1–3) are synthesized largely by
> inference from verified format/scope/secret divergence. Prior-art tooling (Neon `add-mcp`, the MCP
> registry, chezmoi taxonomy) went **unconfirmed (0-0)** under rate-limited verification — leads to
> re-verify, not settled facts. Schemas drift fast; all format claims are 2026-06-current.

---

## 1. Confirmed architectural findings (high confidence)

1. **Canonical-source + per-agent emitters is the only viable architecture** (3-0). The four agents
   use mutually incompatible config languages — **Claude JSON**, **Codex TOML**, **opencode
   JSON/JSONC**, **Hermes YAML**. One logical model must be **parse-modify-serialize re-emitted**
   per agent into its native format and path. **Copy/symlink of one file cannot work** for MCP or
   config. *Assuming format convergence is directly contradicted.*

2. **MCP entries share a stdio-vs-HTTP transport split, but every agent names it differently** (3-0).
   - Codex: stdio = `command`(req)/`args`/`env`/`env_vars`/`cwd`; HTTP = `url`/`bearer_token_env_var`/`http_headers`/`env_http_headers`.
   - opencode: requires an explicit discriminator `type: "local"` (stdio, `command` array) vs `type: "remote"` (`url`).
   - Claude: `mcpServers` map (infers transport from `command` vs `url`); `CLAUDE_PLUGIN_ROOT` substitution.
   - Hermes: stdio `command`/`args`/`env`; HTTP `url`/`headers` + `auth: oauth`.
   Emitters must **branch per transport AND per agent**. Wrong field names / missing discriminator
   silently break the entry.

3. **Env-var secret indirection is a first-class, agent-supported pattern and the correct default**
   (3-0). Codex `bearer_token_env_var` / `env_http_headers` reference env-var **names**, not values.
   Inline secrets are an antipattern against the agents' own designs. **Gotcha:** Codex `env_vars`
   (an allowlist of host vars to *forward*) is **not** `env` (explicit key/value pairs) — conflating
   them breaks things.

4. **Scope/precedence is real and divergent — model which-file-wins per agent** (3-0).
   - Codex (low→high): system < user (`~/.codex/config.toml`) < profile < project (`.codex/config.toml`, trusted projects only, closest-to-cwd wins) < CLI flags.
   - Claude: `user` (`~/.claude/settings.json`) / `project` (`.claude/settings.json`, versioned) / `local` (`.claude/settings.local.json`, gitignored) / `managed` (read-only, cannot be overridden). `--scope` accepts only user/project/local.
   Wrong scope silently no-ops.

5. **Skills partially converge on a shared `~/.agents/skills` dir** (3-0) — Hermes
   `skills.external_dirs` lists it; **this repo's own CLAUDE.md already references
   `~/.agents/skills/beads/SKILL.md`**. So for *skills* an overlay/symlink strategy is more viable
   than for MCP. **But** convergence is opt-in, not universal: Claude reads `~/.claude/skills`. Claude
   skills are `SKILL.md` dirs; **the invocation name falls back to the install-dir basename (a
   changing version string for marketplace plugins) unless frontmatter `name:` is pinned** — always
   pin `name`.

6. **"Hermes" = Hermes Agent by Nous Research** (config facts 3-0; identity 1-1). YAML config at
   `~/.hermes/config.yaml` with `mcp_servers` + a `skills` system; v0.14.0 / ~Feb 2026; CLI/TUI
   (`hermes --tui`) + desktop. Notably it is **partly a meta-agent that delegates coding to the
   Claude Code CLI via a bundled skill** — so Hermes support may partially reduce to "configure the
   Claude Code it shells out to." Treat Hermes **CLI-command UX as least certain** (several `hermes
   mcp add` / `hermes skills install` command claims were refuted); rely on the *file* facts.

---

## 2. Gotchas that directly hit our architecture

- **Launcher-regenerates-config clobber.** Our `deploy/agent/launch` **regenerates**
  `$CODEX_HOME/config.toml` and `/etc/opencode/opencode.json` at startup. If the adapter merges user
  MCP/config into those files and the launcher then rewrites them, the entries vanish. → The adapter
  **must run after base-config generation**, or merge into a **separate scope/layer** the launcher
  never rewrites (e.g. Codex project-scope `.codex/config.toml`, Claude `.mcp.json` as a standalone
  file). This is the single biggest correctness constraint. *(Flagged as an open question in the run
  — "does any agent rewrite its config at startup and clobber injected entries?" — for us the answer
  is yes, our own launcher does.)*
- **Non-idempotent re-runs duplicate entries** — re-apply must **upsert** (insert-or-update by key),
  never blind-append.
- **TOML/JSON/JSONC/YAML deep-merge edge cases** — arrays, comments, key ordering, JSONC comments,
  YAML anchors. Use **parse-modify-serialize**, never text munging.
- **Runtime-not-present** — a skill/MCP that needs `node`/`python`/`uv` fails if the agent image
  lacks it. Validate or declare runtime deps.
- **Secrets in world-readable config / perms** — keep `0600`, prefer env-var indirection.
- **Partial-apply / no atomicity** — leave an agent half-configured. Apply per-agent atomically
  (temp-file + rename).

## 3. Antipatterns (confirmed reasoning)

- Blindly overwriting user config (deep-merge, don't clobber).
- Storing secrets inline instead of env-var indirection.
- Assuming all agents converge on one format (they don't — JSON/TOML/JSONC/YAML).
- One-way installs with no uninstall/dedupe.
- **Translating at a layer that can't see the data** — e.g. a blind control plane rendering a config
  that embeds a secret. (Validates our "translate in-container / node-side, never CP-side" call.)
- Fragile `sed`/`jq`/`awk` text merges instead of parse-modify-serialize.
- Tying the installer to a single agent version (schemas drift fast — version-gate the emitters).

## 4. Per-agent reality table (2026-06, cite + flag drift)

| Agent | Skills | MCP config | General config |
|---|---|---|---|
| **Claude Code** | `SKILL.md` dirs under `~/.claude/skills` (+ project `.claude/skills`); pin frontmatter `name`. Partial `~/.agents/skills` convergence. | `.mcp.json` (JSON) or `mcpServers` in `settings.json`; scopes user/project/local/managed. | `~/.claude/settings.json` (JSON). |
| **Codex CLI** | No native skills concept (closest: `AGENTS.md` instructions). | `~/.codex/config.toml` `[mcp_servers.*]` (TOML); stdio vs HTTP fields above; project `.codex/config.toml` overrides (trusted only). | `~/.codex/config.toml` (TOML); system<user<profile<project<flags. |
| **opencode** | Plugin/skill dir (verify; treat as TBD). | `opencode.json` top-level `mcp` key (JSON/JSONC, **explicitly not TOML**); `type: local\|remote` discriminator required. | `opencode.json` (JSON/JSONC). |
| **Hermes** | `skills` system + `skills.external_dirs` (incl. `~/.agents/skills`); CLI UX uncertain. | `~/.hermes/config.yaml` `mcp_servers` (YAML); stdio `command/args/env`, HTTP `url/headers/auth`. **Partly delegates to Claude Code CLI.** | `~/.hermes/config.yaml` (YAML). |

## 5. Prior art & standards — what to reuse vs build

- **Neon `add-mcp`** (github.com/neondatabase/add-mcp) — claims a per-agent-emitter CLI for 14+
  agents with `upsertServer` idempotency and a `sync/unify` reconcile. **All claims went 0-0
  (unverified under rate-limiting)** — strong *lead* worth a direct read as prior art for our emitter
  set, but not confirmed.
- **AGENTS.md** — candidate lowest-common-denominator carrier for skill-like instructions on agents
  lacking native skills (Codex, maybe opencode). Adoption breadth **unverified** — open question.
- **MCP registry** (`registry.modelcontextprotocol.io`), **Smithery**, **mcp-get** — existence/role
  unverified in this run.
- **Dotfile managers** (chezmoi/dotbot/home-manager) — symlink-farm-vs-render taxonomy **unconfirmed**.

## 6. Open questions carried into design

1. Does a verified ready-made cross-agent MCP installer exist (re-verify Neon `add-mcp`), or build
   from scratch? (Plan: read `add-mcp` directly as prior art; assume we build the Go emitters.)
2. Which agents honor **AGENTS.md**, and is it a viable carrier for "skills" on Codex/opencode?
3. Per-agent deep-merge edge cases; confirm exactly which launcher-generated files we must avoid
   clobbering and which scope/layer to write user artifacts into instead.
4. Is `~/.agents/skills` a real multi-adopter standard beyond Hermes-opt-in + this repo, and do
   Claude/Codex/opencode actually read it?

---

## Design implications (carried into the spec)

- **Architecture = canonical artifact model → per-agent Go emitters** (one `install skill|mcp|config`
  → N native renders). Standalone CLI, embeddable in spawnery.
- **Emitters branch per (artifact-type × agent × transport)**; parse-modify-serialize with **upsert**
  semantics; **atomic** per-agent writes.
- **Secrets via env-var indirection by default**; plaintext only reaches the **in-container/node**
  layer (CP stays blind) — confirmed against the "don't translate where you can't see the data"
  antipattern.
- **Avoid the launcher-clobber** by ordering the adapter after base-config-gen and/or writing to a
  non-regenerated scope/layer.
- **Skills** may use a lighter path (shared `~/.agents/skills` + per-agent symlink/placement) than
  MCP/config; still pin Claude frontmatter `name`.
- **Hermes** support partially reduces to configuring the Claude Code CLI it delegates to; rely on
  file facts, not its CLI UX.
