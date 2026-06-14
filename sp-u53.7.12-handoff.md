# Handoff: sp-u53.7.12 — journal-restored agent-created data files come back unwritable

Fix bead **sp-u53.7.12**: journal-restored agent-created data-mount files come back unwritable by the
agent after suspend/resume. Run `bd show sp-u53.7.12` first. This has ALREADY been root-caused (below)
— your job is to apply the one-line fix and add a faithful e2e repro that PROVES it.

## Symptom (reported live, real spawn)

After suspend/resume, inside the agent container:

```
root@<id>:/app/data# ls -l
-rw-rw-rw-. 1 nobody nogroup 32 ... README.md      # fine: world-writable seed file
-rw-------. 1 nobody nogroup  7 ... datafs         # BROKEN: agent can't write it
```

On the host the files are owned by the NODE's uid (e.g. 1001), which is unmapped in the userns-remapped
container (daemon runs userns-remap, base 100000) → shows as `nobody:nogroup`. README is 0666 so the
cap-dropped agent can still write it; datafs is 0600 so it cannot (CAP_DAC_OVERRIDE inside a userns does
NOT apply to files owned by an unmapped host uid).

## Root cause (confirmed)

`internal/storage/journal/repo.go` `restore()` builds a Kopia `restore.FilesystemOutput` WITHOUT
`SkipOwners`. Kopia therefore tries to chown each restored file to the snapshot's STORED uid — for a
file the agent created, that's the agent's userns-remap host uid (100000). A rootless node (uid 1001)
gets EPERM, `restore.Entry` returns an error, and `internal/spawnlet/manager.go` `CreateWithSelection`
(~lines 591-602) logs the restore error and SKIPS `storage.NormalizeOwnership` — leaving the file
node-owned and non-world-writable. (If restore had succeeded, NormalizeOwnership's degraded path would
have made it 0606 / world-writable. The seed file README is 0666 because `storage.Prepare` wrote it that
way, bypassing the restore path — that's why only agent-created non-world-writable files hit this.)

## The fix (apply exactly)

In `internal/storage/journal/repo.go` `restore()`, add `SkipOwners: true` to the
`restore.FilesystemOutput` literal (field exists in kopia v0.23.0; leave `SkipPermissions` false so modes
are still restored). Comment it: those uids are container-relative/host-remapped and a rootless node
can't chown to them; `storage.NormalizeOwnership` is the single ownership authority (chowns to the agent
uid when privileged, else falls back to world-writable). With this, restore no longer errors →
NormalizeOwnership runs → the restored file becomes agent-writable.

## The e2e test (the deliverable — a faithful repro)

Add a build-tagged (`//go:build e2e`, package `cp_test`) test in `internal/cp/` (e.g.
`datafs_perms_e2e_test.go`). Model the journaled stack on `TestSuspendResumeLifecycleE2E` in
`internal/cp/lifecycle_e2e_test.go`: it wires the Kopia/Garage journal via `buildJournalForTest` so
secret-app's node-local `main` mount is journaled, and provides the CP+node+client + `waitActive`
helpers. Set the node manager `USERNS_MODE=remap` (as `just node` does). Flow:

1. CreateSpawn(secret-app) → `waitActive` (generation 1).
2. Find the agent container: `docker ps --filter label=spawnery.spawn-id=<id> --filter
   label=spawnery.role=agent -q` (label keys in `internal/runtime/pod.go`: `spawnery.spawn-id`,
   `spawnery.role=agent`). Agent runs as container-root.
3. Agent creates a non-world-writable file it owns in the mount:
   `docker exec <agent> sh -c 'printf survive > /app/data/agentfile && chmod 600 /app/data/agentfile'`
   (the stub/agent images both have sh+chmod and run as container-root → file owned by the remap base
   host uid, 0600 — this is what triggers the Kopia chown-on-restore EPERM).
4. SuspendSpawn → poll to Suspended → ResumeSpawn → `waitActive` (generation 2 = fresh agent container).
5. Find the NEW gen-2 agent container the same way. Assert the agent can STILL WRITE the restored file:
   `docker exec <newAgent> sh -c 'printf more >> /app/data/agentfile'` must exit 0, AND the original
   content survived ("survive" present). On buggy code this append fails (permission denied).

## MANDATORY verification (report both runs)

- **(a)** BEFORE applying the fix, run the test — it MUST FAIL at the agent-write-after-resume step. If it
  doesn't fail, the test is wrong (the file must be agent-owned + 0600 + actually journaled + actually
  restored) — fix the test until it genuinely reproduces; do NOT just make it pass.
- **(b)** Apply the SkipOwners fix. Run again — it MUST PASS. Include both outputs in your report.

## Environment / how to run

- Build/test ONLY in the dev-spawnery distrobox: `distrobox enter --root dev-spawnery -- bash -lc '...'`.
- Garage must be UP: `just garage` (compose; if a stray host-run garage or a crash-looping
  `spawnery-garage` container is fighting for :3900/:3903, kill the stray and `just garage` again — see
  closed bead sp-ldfc). Creds land in `deploy/garage/dev-creds.env` (gitignored).
- Run e2e: `set -a; . deploy/garage/dev-creds.env; set +a; CGO_ENABLED=1 go test -tags e2e -run
  <TestName> -v -count=1 -timeout 5m ./internal/cp/`. `make images` first if stub/sidecar images missing.
- The node process runs rootless (uid 1001); the docker daemon has userns-remap (`dockremap:100000:65536`).

## Workflow (project norms — read CLAUDE.md)

- Work in a git worktree + branch off master (e.g. `git worktree add -b feat/sp-u53.7.12
  ../wt-datafs-perms master`); never in the main tree. `bd` runs ONLY from the main repo dir.
- Project policy (CLAUDE.md): e2e/integration tests FAIL (never `t.Skip`) when a required dep is down.
- Gates green before merge: build, `go test -race` (hermetic), `golangci-lint` 0 issues, plus the new
  e2e test green against live Garage. Then merge `--no-ff` to master, `bd close sp-u53.7.12`, `bd export`
  + commit `issues.jsonl`, `git push` AND `bd dolt push`, clean up the worktree.
- Commit with `--no-verify` (project norm).

## Verified pointers (from the diagnosing session)

- Fix site: `internal/storage/journal/repo.go:172` (the `restore.FilesystemOutput` literal).
- The skip happens at `internal/spawnlet/manager.go:591-602` (restore error → NormalizeOwnership skipped).
- `NormalizeOwnership` is `internal/storage/storage.go:92` (privileged → chown to agent uid; EPERM/degraded
  → world-writable `|0o006` files / `|0o007` dirs).
- Nothing was changed on master (the `SkipOwners` edit was reverted; no stray worktree/branch). The bead
  `sp-u53.7.12` is currently claimed by the diagnosing session — re-claim it.
