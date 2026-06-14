# Profiles — Gating Spike Results (load / posture / plugins / commands)

**Date:** 2026-06-14 · Empirical spikes gating the [Profiles design](2026-06-14-profiles-customization-tool-design.md)
(post-[roast](2026-06-14-profiles-adversarial-review.md)). Run on the host against claude 2.1.177 /
codex 0.139.0 / opencode 1.15.13 in isolated HOMEs with real inference. **The per-agent recipes here
are the implementation reference.**

## Spike 1 — Live-agent MCP load proof ✅ CONFIRMED (the load-bearing one)
Files written in the exact `agentinstall` formats cause a live agent to **spawn the MCP server,
complete `initialize`, AND invoke a tool** — proven end-to-end (side-effect markers) for **claude,
codex, AND opencode**. No per-agent enable/approve step for connection.
- claude + opencode have a **real headless health-check** (`mcp list` does a live handshake); **codex
  `mcp list` only reflects config** (no live check) → validate codex via a tool-call e2e.
- OpenRouter is Anthropic-compatible (`ANTHROPIC_BASE_URL=https://openrouter.ai/api`) — usable for the
  e2e harness. codex needs `wire_api="responses"` (0.139 dropped `chat`).
- **Verdict:** the entire profiles value prop's core assumption holds.

## Spike 2 — `approvalPosture` realization (most nuanced; reshapes §5)
Verified mapping + the three flagged unknowns:

| posture | claude `permissions.defaultMode` | codex | opencode `permission` |
|---|---|---|---|
| `always-ask` | `dontAsk`/`default` (deny-by-default headless) | **unsupported headless** (see below) | per-tool `ask`→auto-reject / `deny` |
| `ask-risky` | `acceptEdits`¹ | sandbox `workspace-write` (launcher-owned) | per-tool `{bash:ask,…}` |
| `auto` | `auto` ✅ (classifier; **egress-safe**) | sandbox `workspace-write` | `allow` |
| `yolo` | `bypassPermissions` ✅ | (sandbox `danger-full-access` — launcher-owned) | `allow` |

- **No launch flag needed (overturns roast A2):** claude honors `defaultMode` from `settings.json`
  headlessly (`default` denies; `bypass`/`acceptEdits`/`auto` run). `--dangerously-skip-permissions`
  NOT required.
- **`auto` is egress-viable (overturns roast A3):** the safety classifier is a model round-trip that
  **routes through `ANTHROPIC_BASE_URL`** (proven via a logging proxy: the classifier's `/v1/messages`
  calls transit the proxy and blocked a C2-like curl) → it goes via the **sidecar**, not
  `api.anthropic.com`. **BUT** `auto` requires a model tier **≥ Sonnet/Opus 4.6** (not Haiku/4.5-era);
  on a lower sidecar model `auto` is silently unavailable → degrade to `yolo` (or report).
- **codex `always-ask` fails OPEN, doesn't hang (worse than roast A11 thought):** `codex exec` pins
  `approval=never` regardless of `approval_policy` and runs unprompted, bounded only by the **sandbox**
  (`-s read-only|workspace-write|danger-full-access`). Since the **sandbox is launcher-managed and
  EXCLUDED from profiles**, a profile **cannot meaningfully set codex posture via approval_policy** in
  the headless lane. **codex `approvalPosture` ⇒ limited/best-effort + report** (see capability signal).
  *Caveat:* spawnery runs codex as `codex-tui` (tmux pty), not `codex exec` — in the pty lane
  `approval_policy` may surface prompts; the tmux/ACP approval path is **unverified** and must be
  confirmed before claiming codex posture works.
- **`always-ask` is non-functional in a headless pod across all agents** (no human to prompt): codex→
  never (fail-open), opencode `ask`→auto-reject, claude `default`→denies. **Redefine `always-ask` as a
  headless "locked-down / deny-by-default" posture** (claude `dontAsk` + an allowlist; opencode
  `deny`; codex unsupported) — NOT a literal prompt.
- **`acceptEdits` (ask-risky) may be too loose** — it did not gate a cwd-writing Bash command for
  claude; if ask-risky must gate shell, use `default` + targeted `permissions.allow` instead.
- Admin kill-switches (`disableBypassPermissionsMode`, `disableAutoMode`) can override — N/A for our
  self-managed pods but note.

## Spike 3 — Plugin headless viability (overturns roast D7 — build all 3)
**No interactive trust-dialog gate on any agent.** Build the plugin emitter for **all three**,
targeting **local / image-baked** plugins (fully offline, deterministic):
- **claude:** write `extraKnownMarketplaces` + `enabledPlugins:{ "p@mp":true }` to `~/.claude/settings.json`
  (or `claude plugin install p@mp </dev/null`). Local marketplace = no network.
- **codex:** write `[marketplaces.mp]` + `[plugins."p@mp"] enabled=true` to `~/.codex/config.toml`.
  **`no-op+report`** plugins whose marketplace entry implies an OAuth `ON_INSTALL` app/MCP (can't
  complete headless).
- **opencode:** drop a local plugin file + `opencode.json` `plugin:["./…"]` / `~/.config/opencode/plugin/`.
  **npm-module plugins** need `registry.npmjs.org` egress at **every cold start** and **fail soft**
  (configured ≠ active) → prefer local/baked or treat npm plugins as best-effort + report.
- Remote (github) marketplaces need git + github egress → prefer baking into the image.

## Spike 4 — `allowedCommands`/`deniedCommands` grammar (reshapes §5; codex hard wall)
Feasible + **enforced on claude + opencode** (deny actually blocks — verified). Canonical grammar:
`<Tool>(<prefix> *)` — leading-token prefix + trailing `*`, **no `?`/regex/interior wildcards**.
Emitter must handle claude's space-boundary form (`Bash(ls *)` ≠ `lsof`) and opencode's
**catch-all-first / last-match-wins** ordering (emit `"*"` before specifics).
- **codex:** stable `config.toml` has **no per-command path** (only coarse `approval_policy` +
  launcher-owned `sandbox_mode`). **DECISION (2026-06-14):** when a profile defines per-command
  perms for codex, **emit the experimental `execpolicy` `.rules`** (Starlark `prefix_rule(pattern=[…],
  decision="allow|forbidden")` in the codex rules dir), accepting the preview-stability risk. The
  emitted artifact still carries a **capability signal** noting codex per-command uses an experimental
  path. **Runtime enforcement CONFIRMED (follow-up spike):** codex auto-loads
  **`$CODEX_HOME/rules/default.rules`** at startup (on by default, no flag; `--ignore-rules` is the
  off-switch) and rejects a `forbidden` command in `codex_core::tools::router` **before** spawning —
  proven under `danger-full-access` (so the sandbox wasn't the blocker). Enforcement is the **shared
  exec path for both `codex exec` and the interactive TUI**, so it holds in spawnery's `codex-tui`
  lane. ⇒ codex `deniedCommands` is **genuinely supported**, not best-effort. *Stability:* vendor-
  flagged "experimental, may change" → **pin the codex version + isolate the `.rules` (Starlark)
  emission behind an adapter**.
- opencode `webfetch`/`websearch` take only a bare action (no per-domain) — `Web(domain:x)` degrades
  to all-or-nothing there.

## Cross-cutting decision: a per-agent CAPABILITY / LOSSINESS signal
Multiple spikes converge on the same need (roast's core failure mode): the engine must emit, per
applied artifact, a **capability outcome** — `applied | unsupported(reason) | best-effort(reason)` —
surfaced in the report and via the `agentinstall capabilities` export, so the CP/UI can tell the user
"`deniedCommands` is **not enforced on codex**", "`auto` **needs a ≥4.6 model**", "this opencode npm
plugin is **best-effort**" — rather than silently no-op'ing something the operator believes is active.

## Decision: default posture = `yolo` (the container IS the sandbox)
In isolated spawn containers, **most usage runs full-access** — the pod (userns/gVisor isolation) +
the per-pod egress floor are the real containment, not the agent's in-process approval prompts. So the
**default `approvalPosture` for a profile is `yolo`** (claude `bypassPermissions`, opencode `allow`,
codex effectively never/full within the launcher-set sandbox). The locked-down postures
(`always-ask`/`ask-risky`/`auto`) and per-command rules are **opt-in guardrails** for users who want
them — which de-risks the whole config facet: the complex, agent-divergent posture paths are the
exception, not the default an MVP spawn travels.

## Net effect on the design
- **§5 config:** approvalPosture realized per the table; `always-ask`=locked-down(headless);
  **codex posture limited+reported** (sandbox excluded; tmux/ACP approval path a follow-up spike);
  `auto` carries the **model-tier caveat**; commands **claude+opencode-first-class, codex-unsupported-
  reported**. ForbiddenConfigKeys hardening unchanged (still rejects raw posture/sandbox keys).
- **§7 plugins:** build all three, local/baked target; codex-OAuth + opencode-npm = report best-effort/no-op.
- **§8/§11:** add the **capability/lossiness signal** to the report + `capabilities` export; codex MCP
  needs a tool-call e2e (no live health-check).
- **De-risked:** load proof (✅), plugin headless (✅), claude posture-from-config + auto-egress (✅).
- **New follow-up spike:** codex tmux/ACP approval path (does `approval_policy` work in the pty lane?).

## Post-Implementation Notes
*Append dated notes here as this is implemented/iterated.*
