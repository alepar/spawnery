# Project Instructions for AI Agents

This file provides instructions and context for AI coding agents working on this project.

<!-- BEGIN BEADS INTEGRATION v:1 profile:minimal hash:7510c1e2 -->
## Beads Issue Tracker

This project uses **bd (beads)** for issue tracking. Run `bd prime` to see full workflow context and commands.

### Quick Reference

```bash
bd ready              # Find available work
bd show <id>          # View issue details
bd update <id> --claim  # Claim work
bd close <id>         # Complete work
```

### Rules

- Use `bd` for ALL task tracking — do NOT use TodoWrite, TaskCreate, or markdown TODO lists
- Run `bd prime` for detailed command reference and session close protocol
- Use `bd remember` for persistent knowledge — do NOT use MEMORY.md files

**Architecture in one line:** issues live in a local Dolt DB; sync uses `refs/dolt/data` on your git remote; `.beads/issues.jsonl` is a passive export. See https://github.com/gastownhall/beads/blob/main/docs/SYNC_CONCEPTS.md for details and anti-patterns.

## Session Completion

**When ending a work session**, you MUST complete ALL steps below. Work is NOT complete until `git push` succeeds.

**MANDATORY WORKFLOW:**

1. **File issues for remaining work** - Create issues for anything that needs follow-up
2. **Run quality gates** (if code changed) - Tests, linters, builds
3. **Update issue status** - Close finished work, update in-progress items
4. **PUSH TO REMOTE** - This is MANDATORY:
   ```bash
   git pull --rebase
   git push
   git status  # MUST show "up to date with origin"
   ```
5. **Clean up** - Clear stashes, prune remote branches
6. **Verify** - All changes committed AND pushed
7. **Hand off** - Provide context for next session

**CRITICAL RULES:**
- Work is NOT complete until `git push` succeeds
- NEVER stop before pushing - that leaves work stranded locally
- NEVER say "ready to push when you are" - YOU must push
- If push fails, resolve and retry until it succeeds
<!-- END BEADS INTEGRATION -->

## Beads Sync — Dolt remote (this repo)

Configured 2026-06-08 to the **canonical Dolt-remote sync**: `sync.remote` points at the
GitHub `origin` and issue history lives under `refs/dolt/data` (a git ref, separate from
`refs/heads/*` where the code lives). The Dolt DB (`.beads/embeddeddolt`) is the source of
truth; `.beads/issues.jsonl` is a passive, regenerable export — **not** the wire protocol.

- **After any `bd` change:** `bd dolt push` — publishes issue history to the remote.
  - **NEVER pass `--force` to `bd dolt push`.** A default push is fast-forward-or-reject; a diverged
    remote is reconciled by `bd dolt pull` (Dolt does a row/cell-level three-way merge, so concurrent
    sessions editing different issues merge cleanly and nothing is silently lost). `--force` overwrites
    the remote and **clobbers a concurrent session's pushed issue changes**. If a push is rejected, run
    `bd dolt pull` then push again — do **not** reach for `--force`. The only legitimate `--force` use is
    a deliberate remote-working-set repair, and even then you MUST confirm with the user before running it.
- **Get others' issue changes:** `bd dolt pull`.
- **Fresh clone / new git worktree / missing DB:** `bd bootstrap` — a plain `git clone` does
  NOT include the Dolt DB; bootstrap clones it from `refs/dolt/data`.
- **Session close:** the MANDATORY WORKFLOW's `git push` must be followed by `bd dolt push`.
  Both must succeed — issue changes are not durable until `bd dolt push` lands.

Now that the Dolt remote is configured, the `post-merge`/`post-checkout` hooks no longer import
`issues.jsonl` into Dolt — so a branch op can no longer silently revert a `bd close`/`update`
via a stale JSONL. `issues.jsonl` is safe to discard/regenerate (`bd export`). Do NOT use routine
`bd import` as a sync mechanism; use `bd dolt pull`. See
https://github.com/gastownhall/beads/blob/main/docs/SYNC_CONCEPTS.md for anti-patterns.

**Perms warning:** if bd warns `.beads` is `0777`, that's bd creating dirs under `umask 000`
(hook-launched, not your interactive shell). `chmod 700 .beads` clears it; the durable fix is
running bd under `umask 077`.

## Non-Interactive Shell Commands

**ALWAYS use non-interactive flags** with file operations to avoid hanging on confirmation
prompts. `cp`/`mv`/`rm` may be aliased to `-i` on some systems, which hangs the agent waiting
for y/n input.

```bash
cp -f source dest           # NOT: cp source dest
mv -f source dest           # NOT: mv source dest
rm -f file                  # NOT: rm file
rm -rf directory            # NOT: rm -r directory
cp -rf source dest          # NOT: cp -r source dest
```

Others that may prompt: `scp`/`ssh` (`-o BatchMode=yes`), `apt-get` (`-y`), `brew`
(`HOMEBREW_NO_AUTO_UPDATE=1`).

## Commands

When writing files or running commands that involve shell chaining/redirection, prefer writing in
smaller discrete steps (e.g. use the Write tool or `tee` per-chunk) since complex command chaining
triggers approval blocks.

## Build & Test

Go 1.26 monorepo (host binaries in `cmd/`) + a Vite/React SPA in `web/`. Recipes live in the
`Justfile` (`just --list`); `make` builds binaries, images, and generated code.

```bash
make build          # bin/spawnlet + bin/spawnctl   (also: make bin/spawnery_cp)
make gen            # regenerate protobuf/Connect code (buf) — after proto/ changes
make images         # build sidecar/stubagent/agent container images

just dev            # full stack (CP + spawnlet + web) in mprocs (one Ctrl-C)
just cp             # control plane only (127.0.0.1:8080)
just node           # spawnlet attached to the CP (root-free, egress floor off)
just web            # web UI (vite, LAN-accessible)

just test           # go test ./... -count=1   (hermetic unit tests)
just test-web       # vitest
just test-e2e       # builds images, then go test -tags e2e ./...
just lint           # golangci-lint (go) + eslint/tsc (web)
just setup          # one-time: mprocs, web deps, playwright chromium
```

`.env` with `OPENROUTER_API_KEY` is auto-loaded by Just; container images need Docker/Podman.

### Build/test environment — the `dev-spawnery` distrobox

**The host shell does NOT carry the full toolchain. Run all builds, tests, codegen, and lint inside the
`dev-spawnery` distrobox**, which has `make`, Go 1.26 + `gcc` (so `-race`/CGO works), `buf`, and `docker`
+ the compose plugin. The distrobox shares the host home filesystem, so it sees the repo (and any
worktree) at the same path:

```bash
distrobox enter --root dev-spawnery -- bash -lc 'cd <repo-or-worktree> && <cmd>'
```

- **Race/unit tests:** `CGO_ENABLED=1 go test -race ./...` — the `-race` detector needs CGO/gcc, which
  the bare host lacks; running `go test -race` outside the distrobox fails to link.
- **Lint:** golangci-lint must be built with Go ≥1.26 — `GOTOOLCHAIN=go1.26.0 "$(go env GOPATH)/bin/golangci-lint" run ./...`
  (or `just lint`, which also runs eslint/tsc for `web/`). Aim for **0 issues**.
- **Codegen:** `make gen` (buf) — run it here too; never hand-edit `gen/`.
- **e2e (`-tags e2e`):** build the container images first (`make images`) and have Docker reachable.
- **Garage-backed e2e** (journaled suspend/resume — `garage_e2e` and the CP lifecycle test): bring Garage
  up with `just garage`, then source its creds before `go test`:
  `set -a; . deploy/garage/dev-creds.env; set +a; CGO_ENABLED=1 go test -tags e2e -run <Name> ./internal/cp/`.
  `deploy/garage/dev-creds.env` is gitignored. If a stray host-run garage or a crash-looping
  `spawnery-garage` container is fighting for `:3900/:3903`, kill the stray and `just garage` again.
- The dev node (`just node`) runs the **Docker/runc lane** (it sets `USERNS_MODE=remap` but not
  `CONTAINER_RUNTIME`, so `buildManager` uses `runtime.NewDocker()`), **rootless** (uid 1001), with the
  Docker daemon in **userns-remap** (base 100000). Code touching container/file ownership must account
  for this (see `internal/storage`). The **runsc lane is different** — selected by
  `CONTAINER_RUNTIME=runsc` (containerd/CRI backend, runtime handler `runsc`); it uses gVisor
  *sentry-native* privilege (`overlay2=none`), **not** kernel userns-remap. No dev `just node` variant
  wires runsc — the only runsc path is the `test-cri-delta` (`cri_delta_e2e`) recipe.
- Prefer installing a proper tool in the distrobox over a host-side workaround (`distrobox enter --root
  dev-spawnery -- <install>`); don't shell out to a one-off binary in `/tmp`.

## Architecture Overview

Spawnery runs sandboxed coding-agent **spawns** on local/cloud nodes, driven over ACP.

- **Control plane** (`cmd/spawnery_cp`, `internal/cp`) — spawn lifecycle, scheduler/placement, app
  catalog/marketplace, auth; relays ACP between clients and nodes. Store in `internal/cp/store`.
- **Node / spawnlet** (`cmd/spawnlet`, `internal/spawnlet`, `internal/runtime`) — runs spawns as
  pods via pluggable `PodBackend`s (Docker/runc + CRI lanes), applies the per-pod egress floor,
  mediates storage.
- **Spawn = a 2-container pod**: a **sidecar** (`cmd/sidecar`, `internal/sidecar` — OpenAI-compatible
  inference proxy holding the model key; Anthropic↔OpenAI translation) + an **agent** container
  sharing the sidecar's netns.
- **spawnctl** (`cmd/spawnctl`) — driver/attach CLI (create/attach/exec/shell/list).
- **Storage** (`internal/storage`) — per-mount `Backend` (`Prepare`/`Finalize`); only `Scratch`
  (ephemeral) is implemented today.
- **web/** — React/Vite SPA (Connect-JSON over the CP).
- **proto/ + gen/** — buf-generated protobuf + Connect RPC: the cross-component contract.

Design docs + per-slice plans live in `docs/superpowers/specs/` and `docs/superpowers/plans/`.

## Conventions & Patterns

- **Design-first per slice:** brainstorm/spec → plan → implement → review. Specs/plans in
  `docs/superpowers/`. Track ALL work in beads (prefix `sp`; see the Beads sections above).
- **Consult prior designs before designing.** Before writing a spec for a similar or adjacent
  feature, scan [`docs/superpowers/specs/INDEX.md`](docs/superpowers/specs/INDEX.md) and read the
  related docs — most cross-cutting decisions (and their rationale) are already made there. Build
  on them; don't silently re-litigate.
- **Maintain the spec index.** When you add a new design spec to `docs/superpowers/specs/`, add a
  one-line entry to `docs/superpowers/specs/INDEX.md` in the same commit (right section, one line).
- **`git commit --no-verify`** is the project norm — the beads pre-commit hook exports
  `issues.jsonl`; verify your `bd close`/`update`s survived after any branch op.
- **Unit tests are hermetic** — in-memory store, no network/keys; run with `-race`. End-to-end
  tests are **build-tagged** (`e2e`, `egress_e2e`, `cni_egress_e2e`) and need images/root.
- **Every integration/e2e/lane test is BUILD-TAGGED and FAILS (never `t.Skip`s) when its dep is
  down.** The build tag IS the opt-in for that lane/environment (`e2e`, `garage_e2e`, `cri_delta_e2e`,
  `pgtest`, …), so once you run under a tag its preconditions are required: a missing dep — Docker,
  Garage, containerd/runsc, a built image, a Postgres DSN — is an error, not a no-op. Use `t.Fatalf`
  naming the missing dep and how to provide it. This applies to **lane-specific** tests too (runsc/CRI,
  mosh, postgres, macOS): give them their lane's build tag and let them fail; do NOT leave an
  integration test untagged in the hermetic default suite relying on a runtime skip (split it into a
  `//go:build <tag>` file if it shares a file with hermetic tests). The ONLY legitimate `t.Skip`s are
  non-dep test *modes* — a `-update` golden-vector regen, a manual spike needing hand-provided file
  paths — not "a service/lane is down". `SKIP_DOCKER` remains an explicit opt-out.
- **Regenerate after proto changes** (`make gen`); never hand-edit `gen/`.
- **Toolchain pinned to go 1.26**; golangci-lint must be built with go ≥1.26 (`just lint-go` sets
  `GOTOOLCHAIN`).

## Implementing Specced bd Tasks (multi-agent)

When implementing tasks that are already specced + tracked in bd (design spec written, roast
amendments folded in, bead notes carry the binding deltas), run them as **parallel subagents
scheduled via a dynamic workflow** (the Workflow tool) — NOT as a long-lived coordinator agent
spawning opaque headless processes. The workflow gives reviewable live progress: per-task phase
groups, labeled agents, structured outputs at every stage, and a resumable journal.

- **One coordinator, deterministic:** the workflow script is the coordinator — it encodes the
  task dependency graph, dispatches planners/implementers/reviewers per task, waits on their
  structured results, and serializes merges back to master (promise-mutex; one merge integrator
  at a time).
- **Per-task pipeline:** planner (writes a focused plan) → implementer → spec-compliance
  reviewer → code-quality reviewer → bounded fix loops (≤2 per stage) → merge integrator
  (merge `--no-ff`, full quality gates in the `dev-spawnery` distrobox, `bd close`, export +
  commit, `git push` + `bd dolt push`, worktree cleanup). Never push a red master.
- **Isolation posture:** every implementer works in its **own git worktree + branch**
  (`spawnery-wt-<task>` / `feat/<task>`, cut from current master), never in the main repo's
  working tree. Parallel tasks must have **disjoint file sets** (serialize `proto/`-touching
  tasks; `gen/` merge conflicts → take either side, re-run `make gen`). `bd` commands run ONLY
  from the main repo dir — worktrees lack the Dolt DB.
- **Model preferences:** planners + spec/quality reviewers = **opus**; implementers + fixers +
  merge integrators + final gate = **sonnet**. The reviewers are the quality bar — don't
  economize there below opus.

<!-- BEGIN BEADS CODEX SETUP: generated by bd setup codex -->
## Beads Issue Tracker

Use Beads (`bd`) for durable task tracking in repositories that include it. Use the `beads` skill at `.agents/skills/beads/SKILL.md` (project install) or `~/.agents/skills/beads/SKILL.md` (global install) for Beads workflow guidance, then use the `bd` CLI for issue operations.

### Quick Reference

```bash
bd ready                # Find available work
bd show <id>            # View issue details
bd update <id> --claim  # Claim work
bd close <id>           # Complete work
bd prime                # Refresh Beads context
```

### Rules

- Use `bd` for all task tracking; do not create markdown TODO lists.
- Run `bd prime` when Beads context is missing or stale.
- Keep persistent project memory in Beads via `bd remember`; do not create ad hoc memory files.
<!-- END BEADS CODEX SETUP -->
