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

## Build & Test

_Add your build and test commands here_

```bash
# Example:
# npm install
# npm test
```

## Architecture Overview

_Add a brief overview of your project architecture_

## Conventions & Patterns

_Add your project-specific conventions here_
