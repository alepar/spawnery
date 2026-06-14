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

> **Default posture = `yolo` (spike decision):** isolated spawn containers run full-access by default —
> the pod isolation + egress floor are the real containment, not in-process approval prompts. The
> locked-down postures + per-command rules are **opt-in guardrails**; the complex agent-divergent
> posture paths are the exception, not the path an MVP spawn travels.

- **Normalized keys (MVP — all four):**
  - **`approvalPosture`**: enum **`always-ask | ask-risky | auto | yolo`** (default `yolo`) → per-agent
    translation **verified by the [posture spike](2026-06-14-profiles-spike-results.md)**:
    | posture | claude (`permissions.defaultMode`) | codex | opencode (`permission`) |
    |---|---|---|---|
    | `always-ask` = **locked-down/deny-by-default** (no human headless) | `dontAsk` + allowlist | *limited — report* | per-tool `deny`/`ask` |
    | `ask-risky` | `default` + targeted `allow` (NOT `acceptEdits` — too loose) | *limited — report* | per-tool `{bash:ask,…}` |
    | `auto` | `auto` (model-tier caveat ↓) | *limited — report* | `allow` |
    | `yolo` (default) | `bypassPermissions` | (full within launcher sandbox) | `allow` |
    Verified facts: (1) **no launch flag needed** — claude honors `defaultMode` from `settings.json`
    headlessly (spike overturns roast A2); (2) **`auto` is egress-safe** — its safety classifier routes
    via the sidecar `ANTHROPIC_BASE_URL` (spike overturns roast A3) **BUT requires a model tier ≥
    Sonnet/Opus 4.6**; on a lower sidecar model `auto` is unavailable → degrade to `yolo` + report;
    (3) **`yolo` bypasses APPROVAL only — never the sandbox/egress** (pod + floor = real containment);
    (4) **codex posture is LIMITED**: headless `codex exec` ignores `approval_policy` (fails *open* to
    `never`), and `sandbox_mode` is launcher-managed/excluded → codex non-`yolo` postures are
    **reported limited/best-effort**, never silently applied; the codex **tmux/ACP** approval lane is a
    **follow-up spike**.
  - **`disabledTools`**: string[] built-in tool names → opencode `tools:{<n>:false}`, Hermes
    `agent.disabled_toolsets`, Claude/Codex deny patterns.
  - **`allowedCommands` / `deniedCommands`**: canonical grammar `<Tool>(<prefix> *)` — leading-token
    prefix + trailing `*`, **no `?`/regex/interior wildcards** ([spike-verified enforcement](2026-06-14-profiles-spike-results.md)).
    **claude + opencode are first-class** (deny actually blocks — emitter handles claude's
    space-boundary form + opencode's catch-all-first/last-match ordering). **codex per-command →
    emit the experimental `execpolicy` `.rules`** (`prefix_rule(...)`; decision 2026-06-14, preview-
    stability accepted; the artifact reports an "experimental" capability signal; runtime enforcement
    under confirmation). `Web(domain:x)` is lossy on opencode (no per-domain) → reported.
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

**The [plugin spike](2026-06-14-profiles-spike-results.md) confirmed headless activation works for
ALL THREE agents** (no interactive trust-dialog gate — overturns roast D7). **Build the emitter for
claude/codex/opencode**, targeting **local / image-baked plugins** (fully offline, deterministic):
- **claude:** write `extraKnownMarketplaces` + `enabledPlugins:{ "p@mp":true }` to `~/.claude/settings.json`.
- **codex:** write `[marketplaces.mp]` + `[plugins."p@mp"] enabled=true` to `~/.codex/config.toml`;
  **`no-op + report`** plugins whose entry implies an OAuth `ON_INSTALL` app/MCP (can't complete headless).
- **opencode:** drop a local plugin file + `opencode.json` `plugin:["./…"]`; **npm-module** plugins
  need `registry.npmjs.org` egress every cold start and **fail soft** (configured ≠ active) → treat as
  best-effort + report; prefer local/baked.
Remote (github) marketplaces need git + github egress → prefer baking into the image.

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
  **+ hardened `ForbiddenConfigKeys`**, **default posture `yolo`**, codex per-command via experimental
  `execpolicy`); secrets facet (sp-7h6.1 refs + attach, **hard-require owner-online**, save-time ref
  validation); `plugin` 4th kind (Emitter interface change, **built for all 3 — local/baked**, codex-
  OAuth + opencode-npm `no-op/best-effort`); CP-side assembly (stdlib struct pkg + `schema_version` +
  **capabilities export** + **per-artifact capability/lossiness signal in the report** + fail-loud-
  distinguished); `profile_id` on CreateSpawn + selection; explicit precedence + **sidecar
  `managed.json` provenance index** + `profile_id`/`version` snapshot; **catalog kill-switch**.
- **Deferred (fast-follow):** live propagation of profile edits to running spawns + reconcile/prune
  (roast #5/#4 — the `managed.json` index is laid down now to enable it); per-secret `optional`/
  degraded-start for hands-off resume (roast B13); multi-profile composition; profile sharing/
  marketplace; observability relay of the apply-report (roast G / `sp-nrzf.2`); goose config
  normalization; per-spawn ad-hoc override layer.
- **Depends on:** `sp-7h6.1` (secrets catalog + attach + owner-online delivery); the built substrate +
  `agentinstall` engine; `sp-nrzf.2`/`sp-mofj.1` open items don't block the spine.

## 13. Spikes — resolved + follow-ups
**RESOLVED** ([spike results](2026-06-14-profiles-spike-results.md)): ✅ live-agent load proof
(confirmed all 3); ✅ approvalPosture realization (no launch flag; `auto` egress-safe but model-tier-
gated; codex limited; default=`yolo`); ✅ plugin headless (works all 3 — build them); ✅ command grammar
(claude+opencode native, codex via experimental execpolicy).
**FOLLOW-UPS (during implementation):**
1. **codex `execpolicy` runtime enforcement** — confirm codex enforces auto-loaded `.rules` at runtime
   (not just `execpolicy check`) in the pty lane; *if not*, fall back to codex `deniedCommands` reported
   unsupported. *(spike in progress)*
2. **codex tmux/ACP approval lane** — does `approval_policy` take effect in `codex-tui` (vs `exec`,
   which ignores it)? Determines whether codex non-`yolo` posture is partially supported.
3. **codex MCP tool-call e2e** — codex `mcp list` has no live health-check, so validate MCP load on
   codex via an actual tool-call (the load proof covered this once; keep in the e2e suite).
4. Curated-catalog ↔ E5-marketplace reuse boundary; goose config-surface verification before any goose
   normalization.

## Post-Implementation Notes
*As this design is implemented and iterated on — bug fixes, adjustments, anything that diverged from
the assumptions above — append a dated note here, whether or not a formal debugging skill was used.*
