# Profiles — The Customization Tool (v2)

**Date:** 2026-06-14 · **Builds on:** the merged artifact-injection substrate (`sp-l5sx`) +
cross-agent install engine (`sp-1bia`/`agentinstall`). **Grounded by:**
[installer design](2026-06-14-cross-agent-installer-design.md),
[roast v2](2026-06-14-cross-agent-installer-adversarial-review-v2.md) (REVISE — the productization
gaps this spec closes), [config-settings research](2026-06-14-profile-config-settings-research.md).
**Hard-depends on:** [User Secrets Store `sp-7h6.1`](2026-06-14-user-secrets-store-design.md).
**Reframes the OPEN facet epics** `sp-freg` (user skills) + `sp-hau3` (MCP/config) under one model.

> **Revised 2026-06-14 post-roast** ([adversarial review](2026-06-14-profiles-adversarial-review.md),
> REVISE — 22 confirmed). Binding decisions from that review: config = **all 4 normalized keys** with
> the `approvalPosture` enum expanded to **`always-ask | ask-risky | auto | yolo`**, implemented as a
> **real per-agent engine translation** + **hardened `ForbiddenConfigKeys`** (the security hole);
> secrets = **hard-require owner-online** (documented availability limit); plugins = **kept, gated on a
> headless-viability spike** (per-agent no-op where blocked). Other amendments folded into §§5–9,13.

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

> **The normalized model is implemented as a REAL per-agent translation in the engine
> (`agentinstall`), NOT a value the profile forwards verbatim** (roast A1: the profile vocabulary is
> disjoint from the engine's; today every normalized value silently no-ops). The engine's
> `translateNormalized()` gains a per-(key,agent) mapping; an *unmappable* value is a **hard error with
> a report entry**, never a silent skip. The CP assembler validates normalized values against an
> `agentinstall capabilities` export (§8) at profile-save so mismatches fail early.

- **Normalized keys (MVP — all four):**
  - **`approvalPosture`**: enum **`always-ask | ask-risky | auto | yolo`** → per-agent translation:
    | posture | claude (`permissions.defaultMode`) | codex (`approval_policy`) | opencode (`permission`) | hermes |
    |---|---|---|---|---|
    | `always-ask` | `default`/`plan` | `untrusted` | `ask`/`deny` | gates on |
    | `ask-risky` | `acceptEdits` | `on-request` | per-tool `ask` | partial gates |
    | `auto` | `auto` + launch flag (see caveat) | `on-request` + workspace-write | `allow` | gates off |
    | `yolo` | `bypassPermissions` + `--dangerously-skip-permissions` flag | `never` | `allow` | gates off |
    Caveats wired in: (1) Claude `auto`/`bypassPermissions` need a **launch flag** the launcher must
    inject (roast A2) — the launcher's per-runnable case reads the assembled posture; (2) Claude `auto`'s
    safety classifier is a server call that must route via the sidecar `ANTHROPIC_BASE_URL` or the egress
    floor breaks it (roast A3) — a spike confirms; (3) **`yolo` bypasses APPROVAL only — never sandbox
    or egress** (the pod + egress floor remain the real containment); (4) **`always-ask` is rejected for
    headless Codex** (would hang on a prompt with no human — roast A11): the assembler errors at save if
    a Codex-targeted profile sets `always-ask`.
  - **`disabledTools`**: string[] built-in tool names → opencode `tools:{<n>:false}`, Hermes
    `agent.disabled_toolsets`, Claude/Codex deny patterns.
  - **`allowedCommands` / `deniedCommands`**: string[] of a **defined canonical pattern grammar**
    (`tool` or `tool(arg-glob)`, e.g. `Bash(rm *)`, `WebFetch(domain:x)`) that the engine translates to
    each agent's native syntax (Claude `permissions.allow/deny`, Codex `approval_policy.granular`,
    opencode per-tool map). The grammar + per-agent translation is **a spike** (roast A10 — syntaxes are
    incompatible); until proven, an unmappable pattern is a save-time error.
- **`instructions`**: a content blob written to a **dedicated managed file** in each agent's
  instruction *set* (NOT `CLAUDE.md`/`SOUL.md`, which hold the agent's own runtime memory — roast C4):
  e.g. `~/.claude/profile-instructions.md` referenced from the instructions glob, opencode
  `instructions:[…profile file…]`, Codex a profile-owned instructions file (user-scope path, roast C5).
  Replace-on-apply (idempotent); never appends or clobbers user/agent memory. Provenance via the
  sidecar index (§9), not an in-file marker (avoids a model-visible metadata leak — roast C4).
- **Native passthrough**: `native:{<agent>:{…}}` merged verbatim (hooks, reasoning/verbosity,
  telemetry, per-agent knobs).
- **Excluded — and ENFORCED, not just documented (roast A8, security):** the emitters' hardened
  `ForbiddenConfigKeys` **reject in both the normalized AND native-passthrough paths**:
  `model`/inference wiring; **sandbox/isolation** (Codex `sandbox_mode`/`[sandbox_workspace_write]`,
  Hermes `terminal.backend`); the **raw approval/permission keys** themselves (Claude
  `permissions.defaultMode`, Codex `approval_policy`, opencode `permission`) so they can only be set via
  the validated `approvalPosture` enum, never smuggled through passthrough; **sampling/reasoning**. The
  CP assembler also rejects these keys at save (defense in depth).
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
  secret-ref maps to a selected secret — a **save-time ERROR** for a typed mismatch (roast B15), not a
  dismissible warning (a missing cred → a silently-broken MCP at spawn time with the observability relay
  deferred).
- **Custody invariant — hard-require owner-online (decision, roast B13):** a secret-bearing profile
  requires the **owner device online at every spawn start AND resume** (sp-7h6.1's locked invariant);
  offline → the `pendingIntents.await` TTL fires → spawn **Errored** (no silent under-credentialed
  start). **Documented limitation:** secret-bearing spawns **cannot hands-off resume** (automated/CI/
  scheduled) and a node-failure auto-recovery that needs re-sealing will Error until the owner is
  online. Non-secret profiles are unaffected. The UI surfaces "this profile needs you online to
  start/resume"; a per-secret `optional`/degraded-start path is a **deferred** follow-up.
- **Incremental delivery (roast B14):** the non-secret path (skills/MCP-without-secrets/config/plugins)
  is **independently shippable** — `sp-7h6.1` is a hard dep only for the *secrets* facet. Assembly
  step 3 (§8) is a no-op when a profile has no `ProfileSecret` entries; the team must not block the
  whole feature on `sp-7h6.1`.

## 7. Plugin as a 4th emitter kind

Add `InstallPlugin` to the `Emitter` interface + a `plugin` artifact kind. **This is a breaking change
to the shipped 3-method `Emitter` interface** (roast E2) — all 5 emitters + `baseEmitter` update in one
task. Per-agent native install (Claude Code `.claude-plugin`/marketplace, opencode npm plugin, codex
`codex plugin`).

**Headless-viability spike is a HARD GATE (decision, roast D7/E3):** before wiring each agent's
plugin emitter, the spike must prove the plugin actually **activates headlessly** — Claude plugin
activation is gated on an interactive trust dialog that never fires in a non-TTY container (so
`enabledPlugins` may be ignored, and fresh containers are unauthenticated for marketplace install);
opencode plugins are npm packages Bun fetches **at agent startup** (needs Bun + npm-registry egress
through the floor). **Implement the plugin emitter only for agents where the spike confirms headless
activation; explicit `no-op + report` where it doesn't** (e.g. Claude if the trust dialog blocks).
Keep `plugin` in MVP but each agent's support is spike-gated.

## 8. CP-side assembly layer (closes roast gap B/C)

At `CreateSpawn` with `profile_id`, the CP:
1. Loads the profile, resolves catalog refs + custom content.
2. **Emits the canonical `Artifact` set → `manifest.json` (BYTES artifact at `destPath=manifest.json`,
   carrying a `schema_version`) + payload `ArtifactSpec`s** (TAR-packs skill trees). The canonical
   `Artifact` schema is extracted into a **stdlib-only struct package** (just the types: `Artifact`,
   `Manifest`, `Kind`, `*Payload`, `schema_version`) that both the CP and `agentinstall` import —
   **NOT** a package that pulls `go-toml`/`hujson`/`gen` (that would violate `agentinstall`'s
   `TestLeafInvariant`, roast E19). Translate `targets:"all"` → the engine's `"all-detected"` magic
   string and validate explicit targets against registered emitters (roast E16).
3. **Attaches** the profile's secrets as sensitive spawn_artifacts (§6) — no-op when the profile has no
   `ProfileSecret` entries.
4. Persists all artifacts on the spawn row (with `profile_id`/`profile_version`, §9) and threads them
   into `StartSpawn` (existing path).

- **Schema version (roast E20):** `manifest.json` carries `schema_version`; the baked `agentinstall`
  binary **rejects a manifest above its known version** (MAJOR mismatch → hard error; MINOR →
  forward-compatible). Pinned in the agent Dockerfile.
- **Fail loud, but distinguish two cases (roast B18):** assembly that *attempts and fails* (bad catalog
  ref, encode error) errors at `CreateSpawn`; a **legitimately non-sensitive-artifact-free profile**
  (e.g. secrets-only / BYOK-only) produces no `manifest.json` and is **valid** — `agentinstall` only
  hard-errors on "manifest expected (the spawn carries a manifest artifact) but absent/corrupt", never
  on "no manifest because the profile is secrets-only".
- **`agentinstall capabilities` export (roast E22):** a `list-agents --capabilities` mode emits the
  `(kind,agent) → supported|no-op` matrix as JSON; the CP/web consume it for validation + the UI
  "which entries apply to which agent" preview instead of duplicating the emitter registry.

## 9. Spawn selection, precedence, provenance

- `CreateSpawnRequest` gains optional `profile_id`. `spawnctl --profile <id>` + web "select profile"
  at spawn create.
- **Precedence (explicit — roast #6):** app-manifest artifacts **<** profile **<** (future) per-spawn
  override. Duplicate logical names per (kind,agent) are rejected at save; **for `kind=config`,
  dedup/precedence is by the config KEY** (two entries setting the same `approvalPosture` is rejected),
  not by `Artifact.Name` (roast C21). The assembler emits in this order and records it.
- **Provenance via a sidecar index, not in-file markers (roast C6/C17):** the node writes a
  **`~/.spawnery/managed.json`** index recording, per installed entry, `{kind, agent, native_path,
  native_key|file, profile_id, profile_version}`. This (a) gives the deferred reconcile/prune an
  unambiguous, format-independent way to remove only profile-managed entries, (b) avoids a
  model-visible marker inside config/instruction files, and (c) records which profile state built the
  spawn (audit). The snapshot's `spawn_artifacts` also store `profile_id`/`profile_version`.
- **Emergency kill-switch (roast C12):** revoking/removing a curated-catalog entry that is later found
  malicious must be able to **stop running spawns** using it — MVP wires a catalog-revoke that flags
  affected live spawns for termination/quarantine (a focused task; not left to the deferred reconcile).

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

- **In:** profile model + store + web/CLI CRUD; curated catalog + custom intake; config facet (all 4
  normalized keys with the engine per-agent translation — `approvalPosture` {always-ask|ask-risky|auto|
  yolo} + disabledTools + allowed/deniedCommands; instructions-as-dedicated-managed-file; passthrough)
  **+ hardened `ForbiddenConfigKeys`**; secrets facet (sp-7h6.1 refs + attach, **hard-require
  owner-online**, save-time ref validation); `plugin` 4th kind (Emitter interface change, **per-agent
  headless-spike-gated**); CP-side assembly (stdlib struct pkg + `schema_version` + capabilities export
  + fail-loud-distinguished); `profile_id` on CreateSpawn + selection; explicit precedence + **sidecar
  `managed.json` provenance index** + `profile_id`/`version` snapshot; **catalog kill-switch**;
  live-agent load + posture + plugin spikes.
- **Deferred (fast-follow):** live propagation of profile edits to running spawns + reconcile/prune
  (roast #5/#4 — the `managed.json` index is laid down now to enable it); per-secret `optional`/
  degraded-start for hands-off resume (roast B13); multi-profile composition; profile sharing/
  marketplace; observability relay of the apply-report (roast G / `sp-nrzf.2`); goose config
  normalization; per-spawn ad-hoc override layer.
- **Depends on:** `sp-7h6.1` (secrets catalog + attach + owner-online delivery); the built substrate +
  `agentinstall` engine; `sp-nrzf.2`/`sp-mofj.1` open items don't block the spine.

## 13. Open items / spikes (gate implementation)
1. **Live-agent load proof** (roast E7) — inject an MCP via a profile, prompt the agent to invoke it,
   assert the tool call works; de-risks the load-bearing "files written ⇒ agent loads" invariant.
2. **`approvalPosture` realization** (roast A1/A2/A3) — drive each posture through the engine per agent;
   confirm it changes behavior; confirm Claude `auto`/`yolo` launch-flag injection works and that
   `auto`'s safety classifier routes via the sidecar `ANTHROPIC_BASE_URL` under the egress floor (else
   downgrade `auto`→`yolo` or document); confirm hardened `ForbiddenConfigKeys` blocks the dangerous
   passthrough keys.
3. **Plugin headless-viability** (roast D7/E3) — `claude plugin add </dev/null` no-TTY/no-auth; opencode
   Bun-at-startup + npm egress; codex `codex plugin`. Gates which agents get a plugin emitter.
4. **`allowedCommands`/`deniedCommands` grammar** (roast A10) — define the canonical pattern grammar +
   per-agent translation; prove a `deny` actually denies per agent, else keep commands as passthrough.
5. Curated-catalog ↔ E5-marketplace reuse boundary; goose config-surface verification before any goose
   normalization.

## Post-Implementation Notes
*As this design is implemented and iterated on — bug fixes, adjustments, anything that diverged from
the assumptions above — append a dated note here, whether or not a formal debugging skill was used.*
