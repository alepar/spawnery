# Cross-Agent Installer — Adversarial Review (roast) of sp-l5sx / sp-1bia

**Date:** 2026-06-14 · **Reviews:** [the design spec](2026-06-14-cross-agent-installer-design.md).
**Method:** `roast` skill — 9 critic lenses (premortem/completeness/yagni/failure-mode/feasibility/
security/maintainer + domain experts: multi-agent-tooling, config-management, secrets-delivery),
opus dedup, **3 opus judges per finding** (web-grounded for external claims). 80 raw → 36 distinct →
**26 confirmed** (≥2/3) + 91 judge-raised adjacents. Run `wf_cbc78793-33d`.

> **Independence caveat:** same-family (Claude) panel — "≥2/3 agree" is **panel agreement, not
> independent verification**. Several findings are externally grounded (cited official docs / pinned
> source); those are strong. Pair the empirical ones with the spikes below before trusting them.

## Verdict: **BLOCK**

One unanimous blocker (Claude MCP won't load headlessly), plus dense major clusters in
secrets-for-MCP, the substrate↔engine contract, by-ref, resume, launcher integration, and the
trust model. Do **not** implement until the blocker is resolved and the major clusters are amended
or knowingly accepted.

---

## Confirmed findings, clustered

### A. BLOCKER — Claude MCP does not load in a headless pod
- **[1] (blocker, 3-0)** The claude MCP emitter targets project-scope `.mcp.json`, which Claude Code
  **requires interactive per-session approval** to load (`code.claude.com/docs/en/mcp`: project
  servers sit at "⏸ Pending approval" until approved in an interactive `claude`). A spawn pod has no
  interactive session → injected servers never connect. The baked `claude-settings.json` has no
  `enableAllProjectMcpServers`/`enabledMcpjsonServers`, and approval-free user scope `~/.claude.json`
  is unused. The "no launcher clobber" rationale is also moot — the claude-tui launcher writes no
  Claude config.
- **[2] (major/blocker)** `.mcp.json` is **CWD/project-root-bound**; the spec pins neither the write
  path nor the launcher CWD. User scope `~/.claude.json` is CWD-independent and approval-free.
- **Fix direction:** emit to **user scope `~/.claude.json`** (approval-free, cross-project), and/or
  bake `enableAllProjectMcpServers: true`. Resolve via **Spike S1**.

### B. Per-agent emitter facts are wrong/incomplete
- **[3] (major)** `opencode.json` is **JSONC** (comments/trailing commas) — Go `encoding/json`
  rejects it. Need a JSONC-aware parse (and TOML/YAML comment-preservation is likewise unhandled —
  J15/J16).
- **[4] (major, 3-0)** opencode's stdio env field is **`environment`, not `env`**; and opencode
  collapses `command`+`args` into a **single `command` array** with `type:"local"` (J20–J22). The
  canonical→opencode emitter must translate field names and shape, not pass through.
- **[5] (major)** Codex stdio secrets: verified against **codex rust-v0.137.0** — `McpServerTransportConfig::Stdio`
  **does** have an `env_vars: Vec<McpServerEnvVar>` field (with `source: local|remote`) distinct from
  `env` (key=value). Use `env_vars source=local` for forward-by-name so values never serialize
  (J24/J25). Pin this as the emitter contract.
- **[19] (major, 3-0)** Codex **has** native skills at `~/.agents/skills/<name>/SKILL.md` since
  ~Dec 2025 (`developers.openai.com/codex/skills`) — the "no-op+report" for codex skills is **wrong**;
  the `~/.agents/skills` shared-dir collapse is viable for codex (resolves Open item #4; J70/J71).
- **[J7–J9] (minor)** Claude skill identity is the **directory name**, not frontmatter `name:` — spec
  wording is inaccurate.

### C. Secrets-for-MCP — the deepest cluster (redesign needed)
- **[11] (major, 3-0)** **Timing conflict:** the E2E `SealedSecret` path is **post-container-start**
  (`DeliverSecrets` returns FailedPrecondition without a live container; node handles it async). But
  `agentinstall` runs at launcher startup, and stdio MCP children inherit a frozen env at exec — so
  the secrets tmpfs/env file is empty when MCP servers start.
- **[12] (major/blocker)** The "env file the launcher sources" **does not exist**: no `source`/`.`
  in `deploy/agent/launch`; `start_tmux` forwards only a fixed `-e` list; no sourceable KEY=VALUE
  file is assembled. Path/format/assembler/sequence all undefined.
- **[13] + J51–J57 (major)** Sourcing secret **values into the env contradicts the project's own
  roast-M10 invariant** (`internal/spawnlet/secrets.go`: secrets go to **files, never env** — env is
  runtime-persisted, inherited by every child, visible in `/proc/<pid>/environ`). The design
  reintroduces exactly the exposure M10 rejected, and the existing never-persist canary won't catch
  it (env is memory, not files — J52).
- **[14] + J47/J50 (major)** `secretRefs` (env-var names) ↔ `SealedSecret.target_path` binding is
  undefined; the proto has **no field naming the env var** a secret binds to.
- **[9] (major, 3-0)** A **sensitive skill dir** is multi-file, but `SealedSecret` is a single-file
  envelope and `SecretInjector.Write` writes one path — the sensitive-skill case is unimplementable
  as written (need a packaging policy, or restrict sensitive to single-file).
- **Fix direction:** redesign MCP-secret delivery as **file-based, scoped, pre-exec**: a per-server
  env file written under the secrets tmpfs and injected into **only that MCP server's** launch env
  via each agent's native env-by-name mechanism (codex `env_vars`, opencode `environment`/`{env:}`,
  claude `env`), not a global launcher `source`. Reconcile timing (provision sensitive artifacts
  before agent exec). Resolve via **Spikes S4/S5**.

### D. Substrate↔engine staging contract is undefined
- **[15] (major, 3-0)** The on-disk layout bridging raw `ArtifactSpec` (sp-l5sx, "dumb pipe") and the
  canonical `Artifact` descriptors (sp-1bia, "reads canonical descriptors") is **nowhere defined** —
  serialization (proto-bin/json/raw), naming, manifest index, discovery order, skill-tree flattening.
  This is the load-bearing interface between two independently-implemented units.
- **[10] (major, 3-0)** `ArtifactSpec.content` is a single blob `{inline|byRef}`, but a `skill:{dir}`
  is a multi-file tree with **no archive format** (tar/zip/bare-tree) defined. **[J45]** single `mode`
  can't express per-file perms (executable helper scripts).
- **Fix:** define the staging-dir contract + a `content_type/encoding` enum (bytes/tar/dir) **before**
  either end is coded (**Spike S3** / spec amendment).

### E. by-ref delivery doesn't exist and breaks invariants
- **[7] (major, 3-0)** No general CP-side blob store exists — `internal/cp/store` is relational
  metadata; the only blob stores are **node-owned Kopia journal** (`internal/storage/journal`). The
  "reuse the CP-side blob/store" MVP claim is unbacked.
- **[8] (major, 3-0)** **by-ref + sensitive breaks CP-blind**: HPKE sealing needs the CP to read
  plaintext to seal it, but by-ref plaintext lives in the blob store. `SealedSecret` is inline-bytes
  only (no URI variant).
- **[J35/J37]** inline fallback has **no size cap** → default gRPC/Connect ~4MB limit.
- **Fix direction:** **DROP by-ref from MVP.** Inline-only with an explicit size cap; defer by-ref
  (and its sensitive reconciliation) to a later epic. Simplifies E, parts of C, and D.

### F. Resume/persistence loses all injected artifacts
- **[6] (major, 3-0)** The staging tmpfs is **empty after suspend/resume**; `StartSpawn` is
  create-time only; the launcher regenerates codex/opencode config every start → **every resume
  re-runs `agentinstall` against an empty dir and permanently loses injected MCP/skills/config**.
  **[J32]** claude skills in the ephemeral container HOME are also lost on restart (only `/app` is
  journal-restored). **[J34]** ArtifactSpecs must be **persisted on the spawn row** and re-threaded
  into resume/recreate/migrate `StartSpawn`.

### G. Launcher integration underspecified
- **[16] (major, 3-0)** No per-runnable insertion point; under `set -e` a non-zero `agentinstall`
  exit **kills the entrypoint** (spawn never comes up); no error surface to CP/client; old image
  lacking `agentinstall` silently no-ops (J64). Must run **before** `start_tmux`/`exec` (J49).
- **[17] (major, 3-0)** `runnable_id` → emitter-name mapping is **undefined** (`goose-*`,
  `opencode-tui`/`-served` collapse to one emitter, `shell`/`stub-acp`/`nori` have none).
- **[18] (major, 3-0)** **Goose** is a first-class runnable (Dockerfile + launcher branches) with
  **no emitter and no deferred note**; conversely a `hermes` emitter exists with no hermes runnable
  (J66–J68). Reconcile the emitter set to the actual image runnable set.

### H. No authorization / content-trust model
- **[25] (major, 3-0)** Any `CreateSpawn` caller can inject MCP servers pointing at attacker
  endpoints — a **prompt-injection vector** Claude's own docs flag. No owner-vs-publisher rights,
  size/count limits, URL/command allowlists, or scanning; the catalog-admin `AppManifest` trust model
  isn't extended to user inline artifacts. **[J86]** `destPath` has no path-confinement (`../` escape
  into launcher-managed/host files). **[J87]** CP can tamper non-sensitive bytes. **[J88]** `secretRef`
  + attacker URL = exfil path the egress floor may not block.

### I. Remaining majors/minors
- **[22] (major)** Normalized config key set never enumerated (only `model, approvalPosture, ...`);
  **[23]/J82/J83** normalized `model` collides with the launcher's sidecar-coupled model and would
  clobber it (emitter must be **prohibited from writing launcher-managed keys**); **J79/J80**
  `approvalPosture` is semantically lossy across agents (codex `approval_policy` vs claude permissions
  vs opencode).
- **[20] (minor)** No runtime-dep validation (node/python/uv) despite the cited gotcha.
- **[21] (minor)** `--all-detected` has no detection algorithm; **J77/J78** `list-agents` undefined.
- **[24] (minor)** No emitter-versioning despite the cited "version-gate the emitters" antipattern.
- **[26] (minor)** Hermes "reduces to claude emitter" unverified; **J89–J91** Hermes drives Claude
  Code with `--strict-mcp-config`/`--bare`/per-task `workdir`, which **bypasses** an in-pod
  `.mcp.json` → use the **native YAML `~/.hermes/config.yaml` emitter**, don't rely on discovery.

---

## Recommended spikes (cheapest empirical tests; resolve before/at impl)

- **S1 — Claude headless MCP:** write one stdio MCP to `~/.claude.json` (user scope) AND to
  `/app/.mcp.json` in a fresh claude-tui container; launch claude non-interactively; `claude mcp
  list`. *Kill:* if user scope also needs approval, bake `enableAllProjectMcpServers`. (Finding 1/2)
- **S2 — opencode field shape:** two `opencode.json` variants (`env` vs `environment`; `command`
  array vs `command`+`args`) with an MCP server that prints its env; observe which delivers. (Finding 4)
- **S3 — staging contract:** draft a concrete staging-dir layout (manifest + payloads) and run both
  spawnlet-materialize and `agentinstall apply` against it before either is coded. (Finding 15/10)
- **S4 — secret timing:** trace `SealedSecret`/`InjectSecret` end-to-end; confirm whether any
  pre-exec write path exists (unseal+write during Create before agent exec). (Finding 11)
- **S5 — file-based MCP secret:** prototype per-server env injection (codex `env_vars source=local`,
  opencode `environment`/`{env:}`, claude `env`) reading from the secrets tmpfs **without** a global
  launcher `source`; confirm no `/proc/environ` leak to siblings. (Finding 12/13)
- **S6 — codex skills + ~/.agents/skills:** confirm codex/opencode read `~/.agents/skills`; if so,
  collapse skills to a shared dir. (Finding 19)
- **S7 — resume:** suspend/resume a spawn; confirm the staging dir is empty on the resume launch;
  decide persist-on-row + re-thread vs write to a journal-restored path. (Finding 6)

## Amendment plan (fold into the spec on REVISE)

1. **Claude MCP → user scope `~/.claude.json`** (+ pre-approval fallback). Drop the false
   "no-clobber" rationale.
2. **Per-agent emitter contracts** corrected: opencode JSONC + `environment` + single `command`
   array + `type`; codex `env_vars source=local` + native skills at `~/.agents/skills`; claude skill
   identity = dir name.
3. **MCP-secret delivery** redesigned file-based/scoped/pre-exec, reconciling roast-M10 and the
   post-start timing; define the `secretRef`↔secret binding (proto field for the env-var name).
4. **Define the staging-dir contract** + `content_type` enum + skill-tree packaging + per-file perms.
5. **Drop by-ref from MVP** (inline-only + size cap); defer by-ref and sensitive-by-ref.
6. **Persist ArtifactSpecs on the spawn row; re-thread into resume/recreate/migrate StartSpawn.**
7. **Launcher integration:** per-runnable insertion before exec, `set -e`-safe failure policy +
   error surface, old-image guard; **runnable_id→emitter map**; add a **goose** emitter (or explicit
   no-op), reconcile emitter↔runnable sets.
8. **Trust model:** owner-vs-publisher injection rights, size/count limits, MCP URL/command
   allowlist hook, `destPath` confinement; prohibit emitters writing launcher-managed keys (`model`).
9. **Enumerate** the normalized config key set (+ per-agent value mapping); add **runtime-dep
   validation**, **emitter-versioning**, **all-detected detection algorithm**.
10. **Hermes** via the native YAML emitter, not Claude-Code discovery.
