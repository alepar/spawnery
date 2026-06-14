# Profiles (Customization Tool v2) — Adversarial Review (roast)

**Date:** 2026-06-14 · **Reviews:** [profiles design](2026-06-14-profiles-customization-tool-design.md).
**Method:** `roast` (11 lenses incl. Security / Distributed-Systems-Availability / API-Design experts;
opus dedup; 3-opus-judge panel). 122 raw → 40 distinct → **22 confirmed** + 9 escalations. 100% judge
completion. Run `wf_b9c266bf-6da`.

> Same-family (Claude) panel — agreement, not independent verification. Many findings are
> code-grounded against the *built* engine (`internal/agentinstall/config.go` etc.), which makes them
> strong.

## Verdict: **REVISE** (no unanimous blocker; one finding drew a blocker vote, median major)

## Confirmed findings, clustered

### A. The config-normalization facet is fictional + insecure (the big one)
- **[A1, major] `approvalPosture` enum is disjoint from the built engine.** The spec's
  `{never-ask|ask-risky|always-ask}` has **zero intersection** with the engine's actual values
  (`never|untrusted|on-request|on-failure`, `config.go`). No CP translation is specified → every
  profile `approvalPosture` hits `translateNormalized()` default and **silently no-ops** for every
  agent. The "anchor" delivers nothing.
- **[A2, major] `bypassPermissions` may need a launch flag.** Claude (v2.1.126+) requires
  `--dangerously-skip-permissions`/`--permission-mode` to *enter* the mode; the launcher injects none
  → `never-ask` may no-op even once translated.
- **[A3, major, UNVERIFIED→if `auto`] `auto` has unsatisfiable prereqs** (model ≥ Opus/Sonnet 4.6,
  env flags, a server-side classifier call that the egress floor may block) — silently degrades.
- **[A8, major (1 blocker vote)] The "emitter rejects sandbox/isolation" exclusion is UNENFORCED.**
  `ForbiddenConfigKeys=['model']` for every emitter; native passthrough writes
  `sandbox_mode:danger-full-access` / `permissions.defaultMode:bypassPermissions` **verbatim** — a
  single profile can disable Claude's permission system *and* Codex's sandbox. **Security hole.**
- **[A9, major] `approvalPosture` is explicitly unmapped for opencode** in the engine (opencode
  hard-errors on invalid config) — contradicts "normalizes across all 4."
- **[A10, major] `allowedCommands`/`deniedCommands` have no pattern language** — Claude
  `Bash(rm *)`, Codex granular TOML, opencode per-tool maps are incompatible; same value → divergent,
  security-divergent behavior per agent.
- **[A11, major] `always-ask` on a headless Codex pod** overrides the launcher's hardcoded
  `approval_policy='never'` → permanent hang (no human to answer).
- **[E4] `allowedCommands`/`deniedCommands`/`disabledTools` have ZERO engine implementation** — only a
  partial codex posture path exists. 3 of 4 "MVP normalized keys" are unbuilt.

### B. Secrets coupling breaks availability + over-blocks
- **[B13, major] Owner-online at EVERY resume** (incl. automated/CI/node-failure auto-recovery): the
  `pendingIntents.await` TTL fires → spawn **Errored**. No degraded-start, no per-secret `optional`,
  no delegation → secret-bearing profiles unusable for hands-off workflows and **breaks auto fault
  recovery**.
- **[B14, minor] sp-7h6.1 declared a blanket hard-dep**, but the non-secret path (skills/MCP-without-
  secrets/config) is independently shippable — need an incremental-delivery statement so the team
  doesn't block the whole feature.
- **[B18, major] Fail-loud-on-missing-manifest collapses two cases:** a legit **secrets-only / BYOK-
  only profile** (no non-sensitive manifest) would be wrongly hard-errored; and a broken catalog entry
  blocks all spawns with no degraded escape. Distinguish "nothing expected" from "assembly failed."
- **[B15, minor] `mcp_secret_ref` validation is warn-only** → broken/unauthenticated MCP at spawn
  time; make typed mismatches a save-time error.

### C. Lifecycle / provenance / safety
- **[C4, major] `instructions` write/merge strategy undefined** — and `CLAUDE.md` is *also* where
  Claude writes runtime memory; snapshot re-apply with naive overwrite **destroys user/agent memory**,
  append accumulates stale; a marker in freeform markdown is **model-visible** (prompt-injection/leak).
- **[C6, major] The `managed-by` marker has no defined encoding** for any format (JSON/TOML/YAML/MD),
  no `ManagedBy` field, zero code → the deferred reconcile/prune can remove **nothing**. Must lock the
  marker (per-format or a sidecar index) and write it from MVP day 1.
- **[C12, major] No emergency kill-switch** for a compromised curated-catalog skill/MCP: snapshot +
  deferred reconcile means running spawns keep executing it until manually killed.
- **[C17, minor] Snapshot doesn't store `profile_id`/`version`** → no audit, prune can't query.
- **[C5, minor] Codex instructions file** `AGENTS.md` has no path; project-scope is CWD-relative →
  unreachable at an unknown spawn cwd (violates the user-scope constraint).

### D. Plugins (4th kind) are headless-hostile
- **[D7, major] Claude plugin activation is gated on an interactive trust dialog** that never fires
  headless → `enabledPlugins` silently ignored; fresh containers are unauthenticated for marketplace.
- **[E2] `InstallPlugin` is a breaking change** to the shipped 3-method `Emitter` interface (all 5
  emitters + base).
- **[E3] opencode plugins are npm packages fetched by Bun at startup** — needs Bun + npm-registry
  egress (egress floor).

### E. Assembly layer
- **[E19, UNVERIFIED] "shared encoder package" conflicts with `TestLeafInvariant`** (rejects
  `spawnery/internal/*` imports) and risks pulling toml/hujson/gen into the CP. Cleaner: a **stdlib-
  only struct package**, or the CP imports `internal/agentinstall` directly (already legal).
- **[E20, major] No schema-version protocol** between CP-generated `manifest.json` and the
  independently-baked `agentinstall` binary → drift. Add `schema_version` + compat policy + startup
  check.
- **[E16, minor] `targets:"all"` must translate to the engine's `"all-detected"` magic string** —
  unspecified → every artifact skipped.
- **[E7, escalation] inject→load→usable still unproven** (the load-proof spike is load-bearing for the
  entire value prop).
- **[E8, escalation] custom SKILL.md content is injected into the system prompt** with only size/path
  validation — a prompt-injection surface for shared/curated content.
- **[E21/E22, minor] config dedup is by `Artifact.Name` not config key** (conflicting `approvalPosture`
  across two entries = undefined merge order); the web "which entries apply to which agent" preview
  duplicates the emitter capability table — needs an `agentinstall` capabilities/dry-run export.

## Revision plan (folded into the spec)
1. **Config facet realignment + hardening** (cluster A): make the normalized model match the *built*
   engine — implement a real per-agent `approvalPosture` translation (incl. opencode `permission`,
   Claude launch-flag, **reject `always-ask` for headless Codex**, security-reviewed `never-ask`
   target); **HARDEN `ForbiddenConfigKeys`** to reject `sandbox_mode`/`terminal.backend`/
   `permissions.defaultMode`/`approval_policy` in native passthrough (CP-side never-allow too). Likely
   **narrow MVP**: `approvalPosture` done properly + `disabledTools`; **defer `allowed/deniedCommands`**
   (no safe pattern language) to passthrough. *(fork — see report)*
2. **Secrets availability** (cluster B): degraded-start (start without secrets when owner offline) +
   per-secret `optional` flag; decouple the non-secret path from sp-7h6.1 (incremental delivery);
   fail-loud predicate distinguishes secrets-only vs assembly-failure; save-time error for typed
   secret-ref mismatch. *(fork — degraded-start vs hard-require)*
3. **Provenance + instructions** (cluster C): lock the `managed-by` encoding (sidecar index
   `~/.spawnery/managed.json` recommended over in-file markers — avoids the model-visible-marker leak)
   + `ManagedBy` on the Artifact + store `profile_id`/`version` on the snapshot; **write instructions
   to a dedicated managed file** in each agent's instructions set, never overwrite `CLAUDE.md` memory;
   add an emergency catalog-revoke/kill-switch (or a spike).
4. **Plugins** (cluster D): the plugin spike must prove **headless** activation per agent; the Emitter
   interface change is acknowledged; **likely defer plugins from MVP** if headless-gated. *(fork)*
5. **Assembly** (cluster E): stdlib-only shared struct package (not a deps-pulling shared pkg);
   `schema_version` + agentinstall check; `targets:"all"→"all-detected"` translation; capabilities
   export for the UI; the load-proof spike stays the gating validation.

## Recommended spikes
- **Live-agent load proof** (E7) — inject an MCP via a profile, prompt the agent to invoke it, assert the tool call works.
- **Claude/opencode plugin headless** (D7/E3) — `claude plugin add </dev/null` + opencode Bun-at-startup, with no TTY/auth.
- **approvalPosture realization** (A1/A2/A8) — drive the enum through the engine for each agent; confirm it changes behavior and that hardened passthrough blocks the dangerous keys.

## Post-Implementation Notes
*As this design is implemented and iterated on — bug fixes, adjustments, anything that diverged from
the assumptions above — append a dated note here.*
