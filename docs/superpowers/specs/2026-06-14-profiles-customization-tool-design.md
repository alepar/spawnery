# Profiles — The Customization Tool (v2)

**Date:** 2026-06-14 · **Builds on:** the merged artifact-injection substrate (`sp-l5sx`) +
cross-agent install engine (`sp-1bia`/`agentinstall`). **Grounded by:**
[installer design](2026-06-14-cross-agent-installer-design.md),
[roast v2](2026-06-14-cross-agent-installer-adversarial-review-v2.md) (REVISE — the productization
gaps this spec closes), [config-settings research](2026-06-14-profile-config-settings-research.md).
**Hard-depends on:** [User Secrets Store `sp-7h6.1`](2026-06-14-user-secrets-store-design.md).
**Reframes the OPEN facet epics** `sp-freg` (user skills) + `sp-hau3` (MCP/config) under one model.

## 1. Problem & framing

The substrate + `agentinstall` engine are built and merged, but roast v2 confirmed they form a
**one-shot injector, not a customization tool**: nothing feeds the engine in production (no client
sets `CreateSpawnRequest.artifacts`), there's no layer that turns a semantic "install GitHub MCP"
into the engine's wire format, and the per-user library/selection is undesigned.

A **Profile** closes that: a named, versioned, web-UI-managed bundle of a user's customizations —
**skills, MCP servers, plugins, configs, and secrets** — that is *fluid* (users run myriad
combinations). Base agent images stay generic; we **never build an image per combination.** Instead a
spawn selects `(base image/runnable as today) + a profile`, and the profile is **assembled onto the
base image at spawn creation** — the few-second `agentinstall` apply we already built.

## 2. Architecture

```
Web UI (profile CRUD)  ──►  CP profile store + secrets catalog (sp-7h6.1)
                                   │  CreateSpawn(profile_id)
                                   ▼
              CP-SIDE ASSEMBLY: profile → canonical Artifact set
                → manifest.json + payload ArtifactSpecs (non-sensitive)
                → attach sensitive secret_artifacts (sp-7h6.1)
                                   │  StartSpawn.artifacts (+ owner-online sealed secrets)
                                   ▼
   node materialize → agentinstall apply (skills/mcp/config/plugin emitters) onto the BASE image
                + secrets injected (agent/sidecar) via the sealed path
```

- **Profile = additive overlay** on the existing app/image/runnable spawn (decision: not a new spawn
  type, not an app-model refactor). Works for app-spawns and bare-agent spawns alike.
- **CP-side assembly** (closes roast gap B + C): the CP owns the conversion from the semantic profile
  to the engine's wire format; no caller hand-crafts `manifest.json`.
- **Snapshot at create** (decision): the CP assembles from the profile's *current* state and persists
  the result on the spawn row (`spawn_artifacts`); resume/recreate/migrate re-apply the snapshot
  (already wired). Editing a profile does **not** retro-change running spawns — live propagation +
  reconcile/prune is fast-follow.

## 3. Profile data model + store (CP)

Relational, parallel to `spawn_artifacts`, keyed by `(owner_id, profile_id)`:

```
Profile        { owner_id, profile_id, name, version (uint64, CAS), updated_at }
ProfileEntry   { profile_id, kind: skill|mcp|config|plugin, name,
                 source: catalog_ref{catalog_id} | custom{inline | by-ref},
                 targets: [agent…] | "all",        # which agents it applies to; engine no-ops elsewhere
                 mcp_secret_refs: [ENV_VAR_NAME…] } # for kind=mcp: env-var names it needs
ProfileSecret  { profile_id, secret_id }            # ref into the sp-7h6.1 secrets catalog (values NEVER here)
```

- A profile is **agent-agnostic**; each entry's `targets` scope which agents it applies to (default
  `all`); the cross-agent engine no-ops+reports where an agent lacks the concept.
- Editing = cheap add/remove of entries (supports the "myriad combinations" fluidity). `version` CAS
  guards concurrent edits.

## 4. Content intake: curated catalog + user-supplied

- **Curated catalog** (`customization_catalog`, owner-readable, admin-curated; reuses E5
  marketplace moderation/trust patterns, separate table): vetted skills/MCPs/plugins the user picks by
  `catalog_ref`.
- **User-supplied custom**: paste a `SKILL.md` tree / define an MCP (stdio cmd+args+env | http
  url+headers) / supply a plugin ref; owner-scoped. Validated on save reusing the substrate's existing
  rules (size/count caps, name rules, path confinement).

## 5. Config facet (the normalized settings model)

Per [the config research](2026-06-14-profile-config-settings-research.md). A `config` ProfileEntry
carries `{ normalized:{…}, native:{<agent>:<fragment>}, instructions:<content?> }`:

- **Normalized keys (MVP):**
  - `approvalPosture`: enum `never-ask | ask-risky | always-ask` → emitter maps to Claude
    `permissions.defaultMode`, Codex `approval_policy`, opencode `permission`, Hermes write-approval
    gates. *(the anchor — strongest cross-agent normalization)*
  - `allowedCommands` / `deniedCommands`: string[] tool/command patterns.
  - `disabledTools`: string[] built-in tool names.
- **`instructions`**: a content blob written as a **managed file** to each agent's native instruction
  file (`~/.claude/CLAUDE.md` / `AGENTS.md` / opencode `instructions` / Hermes `SOUL.md`), tagged
  `managed-by: spawnery-profile`. Not a normalized scalar.
- **Native passthrough**: `native:{<agent>:{…}}` merged verbatim (hooks, reasoning/verbosity,
  telemetry, per-agent knobs).
- **Excluded (launcher/sidecar-managed; emitter rejects):** inference wiring; **sandbox/isolation**
  (Codex `sandbox_mode`, Hermes `terminal.backend` — security boundary); **sampling/reasoning** (sidecar-adjacent).
- **goose** config: passthrough-only/unverified for MVP (verify before normalizing — matches the
  deferred goose emitter).

## 6. Secrets facet (sp-7h6.1 integration)

A profile's **`secrets`** are references (`secret_id`) into the **`sp-7h6.1` user-secrets catalog**
(typed `github-token | inference-key | generic-kv`, each carrying `target_container` AGENT|SIDECAR +
`env_var_name`/`dest_path` + an opaque CP-blind envelope). The profile stores **only references,
never values** — CP stays blind.

- At spawn create, selecting a profile triggers `sp-7h6.1`'s **attach** for each referenced secret
  (mint a `sensitive` spawn_artifact copying `type`/`target_container`/`env_var_name`/`dest_path` from
  the catalog) → delivered via the existing **owner-online A4-folded sealed path** → injected into the
  agent (GitHub/API tokens) or sidecar (BYOK key). **Reuses `sp-7h6.1` + the built substrate; zero new
  delivery machinery.**
- **The profile IS the per-spawn least-privilege secret selection** `sp-7h6.1` calls for (only the
  profile's referenced secrets attach).
- **Cross-link:** an MCP entry's `mcp_secret_refs` (env-var names) are satisfied by the profile's
  selected secrets (catalog entries carry `env_var_name`). Validate at profile-save: every MCP
  secret-ref maps to a selected secret (else warn).
- **Inherited custody invariant:** a secret-bearing profile requires the **owner device online at
  spawn start/resume** (sp-7h6.1's locked invariant); the spawn waits on the secrets-ready gate. The
  UI surfaces "this profile needs you online to start."

## 7. Plugin as a 4th emitter kind

Add `InstallPlugin` to the `Emitter` interface + a `plugin` artifact kind; per-agent native install
(Claude Code `.claude-plugin`/marketplace, opencode npm plugin, codex `codex plugin`;
no-op+report where unsupported). **Needs a spike** (like S2/S6) to confirm each agent's plugin
install format + headless behavior before implementing the emitters.

## 8. CP-side assembly layer (closes roast gap B/C)

At `CreateSpawn` with `profile_id`, the CP:
1. Loads the profile, resolves catalog refs + custom content.
2. **Emits the canonical `Artifact` set → `manifest.json` (BYTES artifact at `destPath=manifest.json`)
   + payload `ArtifactSpec`s** (TAR-packs skill trees). The canonical `Artifact` schema (today
   internal to `agentinstall`) is promoted to a **shared encoder package** the CP and `agentinstall`
   both import (engine stays spawnery-dep-free; only the *schema* is shared).
3. **Attaches** the profile's secrets as sensitive spawn_artifacts (§6).
4. Persists all artifacts on the spawn row and threads them into `StartSpawn` (existing path).

- **Fail loud, not silent:** a profile that yields no `manifest.json`, or whose assembly fails, errors
  at `CreateSpawn` (roast #2: today missing-manifest → `agentinstall` silently exits 0). `agentinstall
  apply` also gains a "manifest expected but absent" hard error.

## 9. Spawn selection, precedence, provenance

- `CreateSpawnRequest` gains optional `profile_id`. `spawnctl --profile <id>` + web "select profile"
  at spawn create.
- **Precedence (explicit — roast #6):** app-manifest artifacts **<** profile **<** (future) per-spawn
  override. Within a profile, duplicate logical names per (kind,agent) are rejected at save. The
  assembler emits in this order and records it; not an emergent property of staging order.
- **Provenance:** emitted entries carry a **`managed-by: spawnery-profile`** marker so the fast-follow
  reconcile/prune (roast #5) can remove deselected entries without clobbering user-native config.

## 10. Web UI + CLI

- **Web:** profile list/create/edit/clone; add entries from the catalog or as custom; pick secrets
  from the secrets catalog; per-entry `targets`; preview which entries apply to which agents; select a
  profile at spawn create.
- **CLI:** `spawnctl --profile <id>` at create; profile CRUD via `spawnctl profile …` (parallels the
  `sp-7h6.1.10` secrets CLI).

## 11. Validation (closes roast gap F)

- **Live-agent load proof e2e** (the key spike): inject a trivial stdio MCP via a profile into a real
  claude-tui (then codex/opencode) pod, prompt the agent to invoke it, assert the tool call succeeds —
  proving inject→load→usable, not just file-written.
- Unit: CP profile→artifacts assembly (per kind incl. the normalized config mappings + instructions
  managed-file); profile CRUD/CAS; precedence ordering; secret-ref ↔ secret validation.

## 12. Scope / non-goals

- **In:** profile model + store + web/CLI CRUD; curated catalog + custom intake; config facet
  (normalized `approvalPosture`/allow-deny/disabledTools + instructions-managed-file + passthrough);
  secrets facet (sp-7h6.1 refs + attach); `plugin` 4th kind (+spike); CP-side assembly + fail-loud;
  `profile_id` on CreateSpawn + selection; explicit precedence + `managed-by` marker; live-agent load e2e.
- **Deferred (fast-follow):** live propagation of profile edits to running spawns + reconcile/prune
  (roast #5/#4); multi-profile composition; profile sharing/marketplace; observability relay of the
  apply-report (roast G / `sp-nrzf.2`); goose config normalization; per-spawn ad-hoc override layer.
- **Depends on:** `sp-7h6.1` (secrets catalog + attach + owner-online delivery); the built substrate +
  `agentinstall` engine; `sp-nrzf.2`/`sp-mofj.1` open items don't block the spine.

## 13. Open items / spikes
1. **Plugin install spike** — per-agent plugin format + headless install (claude/opencode/codex).
2. **Live-agent load proof** — the inject→load→tool-call e2e (also de-risks the still-unverified
   "files written ⇒ agent loads" assumption from roast #7).
3. Confirm the curated-catalog ↔ E5-marketplace reuse boundary during implementation.
4. goose config-surface verification before any goose normalization.

## Post-Implementation Notes
*As this design is implemented and iterated on — bug fixes, adjustments, anything that diverged from
the assumptions above — append a dated note here, whether or not a formal debugging skill was used.*
