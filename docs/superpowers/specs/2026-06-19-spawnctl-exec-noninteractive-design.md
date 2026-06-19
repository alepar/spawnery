# spawnctl exec — non-interactive, scriptable command runner

**Date:** 2026-06-19
**Status:** draft
**Mode:** one-shot (Mode B)

## Problem description

`spawnctl exec -- <cmd>` exists today, but it is an *interactive mosh PTY*: it POSTs to the node's
`/terminal` endpoint, gets back `{host, port, key}`, and execs `mosh-client` (`cmd/spawnctl/terminalcmd.go:60-77,98-135`).
That shape is wrong for the headline use case — running a test command inside a running spawn and
learning whether it passed. The mosh path requires a local `mosh-client` and a TTY, cannot be piped
or redirected, and — fatally — `mosh-client`'s exit status reflects the *mosh session*, not the inner
command, so `spawnctl exec -- pytest; echo $?` can never tell you the tests failed. A non-user-facing
`Manager.ExecRun` already runs `docker/crictl exec` non-interactively (`internal/spawnlet/manager.go:519-534`),
but it buffers output via `CombinedOutput`, only surfaces it on error, and is wired solely for internal
session launch/reap. There is no scriptable "run a command in the spawn, stream its output, exit with
its code" path.

## Main challenges

The change must (a) stream stdout/stderr live so a long test run shows progress, (b) keep stdout and
stderr separable, (c) propagate the inner command's exit code as `spawnctl`'s own exit code, and
(d) work without `mosh-client` or a controlling TTY, over both the Docker/runc lane (`docker exec`)
and the runsc/CRI lane (`crictl exec`). It must do all this while matching the existing terminal
commands' transport posture (node-direct over plain HTTP to `-addr`, CP used only to pick a spawn)
and **without touching `proto/`/`gen/`** — keeping the diff small, hermetically testable, and confined
to a few files. Both `docker exec` and `crictl exec` already propagate the inner process's exit code as
their own and demux stdout/stderr when no TTY is requested, so the node side is a thin wrapper; the work
is the streaming framing across the HTTP boundary and the CLI demux.

## Key decisions made

Repurpose `spawnctl exec` to be **purely non-interactive**, mirroring `docker exec <c> <cmd>` (no TTY):
`exec -- <cmd>` streams output and propagates the exit code. The interactive mosh path is **dropped from
`exec` entirely** — interactive use is already covered by `spawnctl shell` (mosh bash) and
`spawnctl attach` (mosh TUI), so `exec` no longer needs a `-it` mode and the command's role becomes
unambiguous: a scriptable runner. Transport is a **new node-direct streaming HTTP endpoint `/exec`**, a
sibling of `/terminal`, carrying a tiny length-prefixed frame protocol (stdout / stderr / exit / error
frames) so the exit code travels back distinctly from the output. No proto/Connect/CP-relay changes.
`shell` and `attach` are unchanged (they remain mosh-interactive). After implementation, add a one-line
note to the project `CLAUDE.md` that `spawnctl exec` runs test commands inside a spawn.

## Decision points, by section

### CLI surface — `exec` is purely non-interactive; interactive lives in `shell`/`attach`

**Recommended:** `spawnctl exec -- <cmd>` → non-interactive streamed exec, exit code propagated, every
time. The interactive mosh PTY is removed from `exec`; interactive use is already served by
`spawnctl shell` (mosh `bash -il`, falling back to `sh -i`) and `spawnctl attach` (mosh TUI), both
unchanged. This gives each command one clear job — `exec` = scriptable runner, `shell` = interactive
shell, `attach` = TUI — and matches `docker exec <c> <cmd>` (non-TTY) semantics. It is a **deliberate
behavior change**: today `exec -- cmd` gives a mosh PTY; afterward it gives streamed non-interactive
output. The vestigial `-it` flag (presently "accepted; always on") is kept **accepted as a no-op** for
`docker`-muscle-memory compatibility — `exec` is always non-interactive — and documented as such; an
interactive arbitrary command is reached via `spawnctl shell` then running it there.

*Considered and discarded:* keeping a `-it` mosh path on `exec` (the earlier draft) — rejected at the
user's direction: interactive belongs in `shell`, and a dual-mode `exec` muddies the command's role. A
brand-new subcommand (`run`/`test`) leaving `exec` as mosh — rejected; the user wants `spawnctl exec`
itself to be the runner, and a dual interactive/non-interactive `exec` is the wart worth removing.

### Transport — node-direct streaming HTTP `/exec`, not a Connect/CP RPC

**Recommended:** add `Server.HandleExec` serving `POST /exec?spawn=<id>` with body `{"cmd":[...]}` on the
same node server that hosts `/terminal` (`internal/spawnlet/server.go`; route registered in
`cmd/spawnlet/main.go` beside `/terminal`). `spawnctl` POSTs to `-addr` (default `http://127.0.0.1:9092`),
exactly as the terminal commands do, and uses the CP only to *pick* a spawn when `-spawn` is omitted
(`resolveSpawn`). This matches the established terminal posture (node-direct, owner-only, un-audited —
see the `server.go` `HandleTerminal` comment) and keeps the change off the cross-component contract.

*Considered and discarded:* a server-streaming `Exec` RPC on `spawn.v1` + `cp.v1` relayed through the
CP — rejected as a much larger surface (proto + `make gen` + CP relay + auth/audit plumbing) for a
feature whose v1 posture is identical to the existing un-audited terminal path; routing exec through the
CP for audit/ACL is recorded as a future upgrade alongside the same future for `/terminal`. Reusing the
bidi `Session` frame relay — rejected; it is the ACP stdio path, semantically unrelated, and offers no
exit-code channel.

### Wire framing — length-prefixed typed frames over the HTTP response body

**Recommended:** once the node returns `200`, the response body is a sequence of frames, each
`[type:1][len:4 big-endian][payload:len]`, with `type ∈ {1=stdout, 2=stderr, 3=exit, 4=error}`. stdout/stderr
frames carry raw bytes; the `exit` frame's payload is the 4-byte big-endian exit code (sent once, last);
an `error` frame carries a UTF-8 message for a node-side failure that occurs *after* streaming has begun
(e.g. the exec process failed to start). The node flushes (`http.Flusher`) after every frame so output is
live, and serializes the two output framers behind a mutex (the `docker`/`crictl` CLI writes stdout and
stderr concurrently). `spawnctl` demuxes: type 1 → `os.Stdout`, type 2 → `os.Stderr`, type 3 → captured
exit code, type 4 → print to stderr and exit non-zero; after the stream ends it exits with the captured
code. Pre-stream failures (spawn not found, bad request) use a non-`200` status with a plain-text body,
matching `/terminal`; `spawnctl` treats non-`200` as a fatal error.

*Considered and discarded:* an HTTP trailer (`X-Exit-Code`) over a merged output stream — rejected;
trailers are fragile across the Go client/middleware stack and merging loses stdout/stderr separation.
A bespoke JSON-lines protocol — rejected; binary-unsafe for arbitrary command output without base64
bloat. The 4-type binary framing is self-contained, byte-safe, and trivially unit-testable.

### Node execution — new `Manager.ExecStream`, reusing the non-interactive exec prefix

**Recommended:** add `Manager.ExecStream(ctx, spawnID string, inner []string, stdout, stderr io.Writer) (exitCode int, err error)`.
It resolves the spawn's agent container (`sp.AgentID`), builds argv via the existing
`ExecPrefixNonInteractiveFor(m.cfg.ContainerRuntime)` + `execArgv` (`internal/spawnlet/terminal.go`),
wires `cmd.Stdout`/`cmd.Stderr` to the supplied writers, runs it, and returns the exit code (from
`*exec.ExitError`'s `ExitCode()`; `0` on clean exit). `err` is reserved for failures to *launch* the exec
(unknown spawn, no agent container, `docker`/`crictl` not found) — a non-zero command exit is **not** a
Go error, it is a returned `exitCode`. The existing `ExecRun` (buffered, error-on-nonzero) is left
untouched for its internal session-launch callers; `ExecStream` is the streaming, exit-code-returning
sibling for the user-facing path.

*Considered and discarded:* overloading `ExecRun` to also stream — rejected; its fire-and-forget,
error-on-nonzero contract is relied on by `sessionexec.go`, and conflating the two muddies both.

### Scope cuts for v1 (documented, not built)

**Recommended cuts:** (1) **No stdin forwarding** — the test-runner use case runs a fixed argv with no
input; piping into the command is deferred. (2) **No `-w/--workdir` flag** — `docker exec` supports `-w`
but `crictl exec` does not, so a uniform flag is not available; the lane-agnostic idiom
`spawnctl exec -- sh -lc 'cd /app/<mount> && <cmd>'` covers it. (3) **No `-e` env injection** — the
container's own env applies; deferred. (4) **Client-disconnect does not guarantee in-container process
kill** — cancelling the node request context kills the `docker exec`/`crictl exec` *client*, which may
leave the in-container process orphaned until the spawn stops; documented as a known limitation. Each cut
is recorded so a later bead can lift it without re-litigation.

### Testing

**Recommended:** (1) **Hermetic unit test** for the frame codec — encode/decode round-trip of stdout,
stderr, exit, and error frames, including zero-length and large (>1 frame) payloads (pure, no deps,
`-race`). (2) **Hermetic CLI demux test** — refactor the client path into a testable
`runExec(addr, spawn string, cmd []string, stdout, stderr io.Writer) (int, error)`; point it at an
`httptest.Server` that emits a known frame sequence and assert it routes bytes to the right writer and
returns the right exit code (and that an `error` frame yields a non-zero exit). (3) **e2e** (`//go:build e2e`,
Docker lane, reusing the `tmux_e2e` spawn-up harness in `internal/cp`): create a spawn, `ExecStream`
`sh -c 'echo out; echo err 1>&2; exit 7'`, assert `stdout=="out\n"`, `stderr=="err\n"`, `exitCode==7`.
Per project convention the e2e test **fails** (does not skip) when Docker or the `spawnery/agent:dev`
image is absent, naming the missing dep.

### Deliverable: CLAUDE.md note

After the code lands, add a one-line entry to the project `CLAUDE.md` (near the spawnctl mention in the
Architecture Overview, or the Build & Test section) noting that **`spawnctl exec -- <cmd>` runs a
non-interactive command inside a running spawn and propagates its exit code — use it to execute test
commands inside a spawn** (e.g. `spawnctl exec -spawn <id> -- sh -lc 'cd /app && go test ./...'`).

## Files touched (disjoint-set note)

`cmd/spawnctl/terminalcmd.go` (exec action: always non-interactive stream; `-it` becomes a no-op;
`shell`/`attach` untouched) + a new client streaming/demux helper file; `internal/spawnlet/server.go` (`HandleExec`); `cmd/spawnlet/main.go` (register `/exec`);
`internal/spawnlet/manager.go` (`ExecStream`); a new frame-codec file shared by client and node; tests;
`CLAUDE.md`. These overlap heavily across the same few files, so this is **one cohesive bead implemented
by a single implementer — not a parallel multi-task fan-out** (the parallel workflow requires disjoint
file sets, which this change does not have).

## Post-Implementation Notes

*As this design is implemented and iterated on — bug fixes, adjustments, anything that diverged from the
assumptions above — append a dated note here, whether or not a formal debugging skill was used.*
