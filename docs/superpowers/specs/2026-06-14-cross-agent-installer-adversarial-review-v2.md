# Cross-Agent Installer — Adversarial Review v2 (productization gaps)

**Date:** 2026-06-14 · **Reviews:** [the installer design](2026-06-14-cross-agent-installer-design.md),
post-implementation. **Method:** `roast` (10 lenses incl. domain experts: package-management,
integration-testing, platform-engineering; opus dedup; 3-opus-judge panel). 93 raw → 24 distinct →
**8 confirmed** + 8 escalations. Run `wf_55cd946b-9b4`. Prior roast
([v1](2026-06-14-cross-agent-installer-adversarial-review.md)) findings were treated as closed; this
pass hunted only what the amendments + implementation did **not** close.

> **Independence caveat:** same-family (Claude) panel — agreement, not independent verification.

## Verdict: **REVISE**

No blocker. The engine + substrate are sound and merged; the confirmed gaps are about turning them
into a **usable customization tool** — they are precisely the scope of the next design.

## Confirmed findings (the customization-tool requirements)

1. **[major] The user-facing layer is entirely undesigned.** `sp-freg` (per-user library) + `sp-hau3`
   (per-spawn selection) are OPEN epics with one-sentence descriptions — no store schema, no
   CRUD/selection RPC, no UI, no versioning, no conflict policy, no semantic→wire translation. Without
   them users must hand-supply raw `ArtifactSpec`+`manifest.json` on every `CreateSpawn` with no
   save/name/browse/reuse/update. **This is a one-shot injector, not a customization tool.**
2. **[major] No semantic→wire assembly layer.** Nothing (CP, client, or SDK) converts the canonical
   `Artifact{kind,name,targets,skill,mcp,config}` into the raw `ArtifactSpec` bytes **+ a
   `manifest.json` artifact** the engine requires. `ArtifactSpec` carries only transport fields; the
   canonical descriptor is an internal JSON type a caller must hand-encode + TAR-pack skill trees +
   attach as `destPath=manifest.json`. **If `manifest.json` is absent, `agentinstall apply` silently
   exits 0** — zero install, no error.
3. **[minor] Per-artifact secret-wait serializes** up to N×30 s with no shared deadline (overlaps
   `sp-nrzf.2`).
4. **[major] Create-time-only, no mutation API.** No `Update/Delete/ReplaceArtifacts` RPC; the spawn-row
   snapshot is re-threaded verbatim on every resume/recreate. Updating/removing an injected
   MCP/skill — or propagating a library change to existing spawns — requires destroy-and-recreate,
   losing in-progress work.
5. **[major] Upsert-only emitters, no uninstall/prune, no provenance tag.** Injected entries persist
   indefinitely in shared user-scope config (`~/.claude.json`, `~/.codex/config.toml`,
   `~/.config/opencode/opencode.json`) + skill trees in HOME (survive same-node resume via the rootfs
   delta); re-apply is additive. Removing an item from a future library leaves **stale entries
   indistinguishable from user-native config** → deferred uninstall is architecturally blocked without
   a `managed-by` marker + reconcile/prune mode.
6. **[major] Conflict/precedence is an emergent accident, not an invariant.** Two artifacts emitting the
   same logical name → last-write-wins by staging order, no warning. Owner-overrides-manifest works
   only because the manifest happens to order manifest-entries before owner-entries — any reordering
   silently inverts the trust model.
7. **[major] No live-agent load validation.** Conformance tests only prove emitters write correct files
   to a temp HOME; **nothing proves an injected MCP actually connects / a skill is invocable in a live
   claude/codex/opencode agent.** S1 ran only `claude mcp list`; the one pod e2e (hermes) failed
   (`sp-mofj.1`) and injects no artifacts. The inject→load→tool-usable path is unvalidated.
8. **[minor] No manifest schema-version check.** `apply-artifacts.sh` guards only on binary presence,
   not on staging-manifest schema version — a schema bump silently misparses against an older bundled
   binary.

## Escalations (material dissent / unverified — mostly already tracked)

- **E1** No production client sets `CreateSpawnRequest.artifacts` (spawnctl + web omit it) — the engine
  has no live feed. *(core to finding 1/2)*
- **E2** `apply-report.json` failures never relayed to CP/user (only tmux stderr) — `sp-nrzf.2` item 3.
- **E3** codex skills may need `--with-skills`; launcher runs bare `codex` (UNVERIFIED) — `sp-mofj`/spike.
- **E5** codex emitter has no `ForbiddenConfigKeys` (opencode blocks only `model`) — `sp-nrzf.2` item 2.
- **E6** sensitive MCP secrets **not re-delivered on resume** (Materialize skips empty-inline sensitive;
  no CP auto-trigger) — overlaps `sp-mofj.1` secret path / `sp-nrzf.1`.
- **E7** HTTP-transport MCP `secretRefs` silently unauthenticated — `sp-nrzf.2` item 1.
- **E4/E8** claude CWD-independence + literal-secret-in-config exposure (validated on one version only).

## Implied scope for "the customization tool" design

The confirmed gaps decompose the tool into:
- **A. User library** (`sp-freg`/`sp-hau3` reframed): store schema for named, versioned customizations
  (skill/mcp/config), CRUD, trust/quota — and **per-spawn selection** (pick which apply).
- **B. Assembly layer** (finding 2): canonical `Artifact` → `ArtifactSpec`+`manifest.json`. Decide
  CP-side vs client/SDK. Add a proto-level canonical message or a shared encoder so callers don't
  hand-craft manifests; fail loudly when `manifest.json` is missing.
- **C. Client entry point** (E1): `spawnctl` flag + web UI that set `CreateSpawnRequest.artifacts`.
- **D. Lifecycle** (findings 4,5): update/remove on a live spawn; **`managed-by` provenance + reconcile/
  prune** so library removals propagate and don't orphan entries; mutation RPC vs apply-on-resume.
- **E. Conflict/precedence** (finding 6): explicit, stated owner-vs-library-vs-manifest precedence with
  conflict reporting.
- **F. Validation** (finding 7): a real inject→load→tool-call e2e per agent (the load-proof spike).
- **G. Observability** (E2) + **schema versioning** (finding 8).

## Recommended spikes
- **Live-agent load proof:** inject a trivial stdio MCP in a claude-tui (then codex/opencode) pod,
  prompt the agent to invoke it, assert the tool call succeeds. *Kill:* tool call works + server
  connects. (finding 7)
- **Reconcile/prune model:** define what "user removes X from library" means for a running/suspended
  spawn; prototype a `managed-by`-tagged prune re-applied on resume. (finding 5)
- **Assembly placement:** prototype the canonical→wire encoder CP-side vs SDK; confirm the missing-
  manifest silent-exit-0 becomes a loud error. (finding 2)

## Post-Implementation Notes
*As this design is implemented and iterated on — bug fixes, adjustments, anything that diverged from
the assumptions above — append a dated note here, whether or not a formal debugging skill was used.*
