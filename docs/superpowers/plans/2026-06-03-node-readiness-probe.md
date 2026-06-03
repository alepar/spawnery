# Node Readiness Probe Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** The node reports a spawn `ACTIVE` only after the agent actually answers an ACP `initialize`, so the UI's green dot means "ready to chat" instead of "container launched".

**Architecture:** Add a readiness probe to `internal/node`. `attacher.startSpawn` keeps its `STARTING → ACTIVE` shape but inserts, between `mgr.Create` and `ACTIVE`, a `probeReady` call that opens its own throwaway attach (`mgr.Attach`), sends one ACP `initialize`, and waits for the matching response. On timeout it tears the spawn down (`mgr.Stop`) and reports `ERROR`. The pure send-initialize-and-await-response core is factored into `awaitInitialize(io.Writer, io.Reader)` so it's unit-testable without Docker/a Manager.

**Tech Stack:** Go 1.26; `internal/acp` (ndjson JSON-RPC codec: `acp.Message`, `acp.WriteMessage`, `acp.NewReader`/`ReadMessage`); `internal/spawnlet` Manager (`Create`, `Attach`, `Stop`); `internal/runtime.AttachedStream` (`Stdin io.WriteCloser`, `Stdout io.Reader`, `Close`).

**Bead:** sp-39u. **Spec:** `docs/superpowers/specs/2026-06-03-node-readiness-probe-design.md`.

**Conventions:** Commit with `--no-verify` (the beads export hook dirties commits). Local-only repo — NO push, NO remote. Hermetic Go tests only (no Docker/root) for the unit task; the e2e (real Docker) is run by the orchestrator, not the implementer.

---

## File Structure

- **Create `internal/node/ready.go`** — the readiness probe. Holds `awaitInitialize` (pure, testable core: write one `initialize`, read until the matching response / timeout / EOF), `probeReady` (the `attacher` method: retry-attach then `awaitInitialize`), and the `probeInitID` / `readyProbeTimeout` constants plus the `ptrInt` helper. One responsibility: "is the agent ready to serve ACP?".
- **Create `internal/node/ready_test.go`** — hermetic unit tests for `awaitInitialize` using in-memory readers/writers.
- **Modify `internal/node/attach.go`** — `startSpawn` captures the spawn from `Create` and calls `probeReady` before reporting `ACTIVE`; on probe failure, `mgr.Stop` + `ERROR`.

---

## Task 1: `awaitInitialize` core + unit tests

**Files:**
- Create: `internal/node/ready.go`
- Test: `internal/node/ready_test.go`

- [ ] **Step 1: Write the failing tests**

Create `internal/node/ready_test.go`:

```go
package node

import (
	"context"
	"io"
	"strings"
	"testing"
	"time"
)

func TestAwaitInitialize_Ready(t *testing.T) {
	stdout := strings.NewReader(`{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":1}}` + "\n")
	var stdin strings.Builder
	if err := awaitInitialize(context.Background(), &stdin, stdout, time.Second); err != nil {
		t.Fatalf("want nil, got %v", err)
	}
	if !strings.Contains(stdin.String(), `"method":"initialize"`) {
		t.Fatalf("initialize not written to stdin: %q", stdin.String())
	}
}

func TestAwaitInitialize_IgnoresOtherFrames(t *testing.T) {
	// A notification (no id) before the matching response must be skipped, not mistaken for ready.
	stdout := strings.NewReader(
		`{"jsonrpc":"2.0","method":"session/update","params":{}}` + "\n" +
			`{"jsonrpc":"2.0","id":1,"result":{}}` + "\n")
	var stdin strings.Builder
	if err := awaitInitialize(context.Background(), &stdin, stdout, time.Second); err != nil {
		t.Fatalf("want nil, got %v", err)
	}
}

func TestAwaitInitialize_ErrorResponseCountsAsAnswered(t *testing.T) {
	// An error reply still proves the agent is up and answering — treat as ready.
	stdout := strings.NewReader(`{"jsonrpc":"2.0","id":1,"error":{"code":-32601,"message":"x"}}` + "\n")
	var stdin strings.Builder
	if err := awaitInitialize(context.Background(), &stdin, stdout, time.Second); err != nil {
		t.Fatalf("want nil (agent answered), got %v", err)
	}
}

func TestAwaitInitialize_Timeout(t *testing.T) {
	pr, pw := io.Pipe() // never written, never closed -> the read blocks
	defer pw.Close()
	var stdin strings.Builder
	if err := awaitInitialize(context.Background(), &stdin, pr, 50*time.Millisecond); err == nil {
		t.Fatal("want timeout error, got nil")
	}
}

func TestAwaitInitialize_StreamClosed(t *testing.T) {
	stdout := strings.NewReader("") // immediate EOF
	var stdin strings.Builder
	if err := awaitInitialize(context.Background(), &stdin, stdout, time.Second); err == nil {
		t.Fatal("want read error on EOF, got nil")
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/node/ -run TestAwaitInitialize -v`
Expected: FAIL — compile error `undefined: awaitInitialize`.

- [ ] **Step 3: Write `internal/node/ready.go` (core only)**

```go
package node

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"time"

	"spawnery/internal/acp"
)

// probeInitID is the JSON-RPC id used by the readiness probe's initialize. It lives only on the
// probe's own throwaway attach (closed before any client connects), so it can't collide with the
// client's request ids.
const probeInitID = 1

func ptrInt(i int) *int { return &i }

// awaitInitialize writes one ACP initialize request to stdin and reads stdout until the matching
// response arrives (the agent is ready), the timeout elapses, or the stream closes. Frames that are
// not our initialize response (e.g. notifications, other ids) are ignored. An error reply to our id
// still counts as "answered" (the agent is up). This is the pure, unit-tested core of probeReady.
func awaitInitialize(ctx context.Context, stdin io.Writer, stdout io.Reader, timeout time.Duration) error {
	req := acp.Message{
		ID:     ptrInt(probeInitID),
		Method: "initialize",
		Params: json.RawMessage(`{"protocolVersion":1,"clientCapabilities":{}}`),
	}
	if err := acp.WriteMessage(stdin, req); err != nil { // WriteMessage sets jsonrpc:"2.0" for us
		return fmt.Errorf("write initialize: %w", err)
	}
	done := make(chan error, 1) // buffered so the reader goroutine never leaks after a timeout
	go func() {
		rd := acp.NewReader(stdout)
		for {
			msg, err := rd.ReadMessage()
			if err != nil {
				done <- fmt.Errorf("read initialize response: %w", err)
				return
			}
			if msg.ID != nil && *msg.ID == probeInitID && (msg.Result != nil || msg.Error != nil) {
				done <- nil // the agent answered our initialize -> ready
				return
			}
		}
	}()
	select {
	case err := <-done:
		return err
	case <-time.After(timeout):
		return fmt.Errorf("agent did not answer initialize within %s", timeout)
	case <-ctx.Done():
		return ctx.Err()
	}
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/node/ -run TestAwaitInitialize -v`
Expected: PASS (all 5).

Also run with the race detector (there's a goroutine + channel): `go test ./internal/node/ -run TestAwaitInitialize -race`
Expected: PASS, no race.

- [ ] **Step 5: Commit**

```bash
git add internal/node/ready.go internal/node/ready_test.go
git commit --no-verify -m "feat(node): awaitInitialize readiness core (write ACP initialize, await response) [sp-39u]"
```

---

## Task 2: `probeReady` + wire into `startSpawn`

**Files:**
- Modify: `internal/node/ready.go` (add `readyProbeTimeout` + `probeReady`)
- Modify: `internal/node/attach.go` (`startSpawn`)

`probeReady` needs a real `Manager`/Docker to exercise, so it has no isolated Go unit test; it's covered by the e2e (stub, real node lane) and the host (goose) check. This task is integration wiring — verify by build + vet + the full hermetic suite + the e2e (run by the orchestrator).

- [ ] **Step 1: Add `readyProbeTimeout` + `probeReady` to `internal/node/ready.go`**

Add these imports to the existing import block in `ready.go`: `"spawnery/internal/runtime"` and `"spawnery/internal/spawnlet"`. Then append:

```go
// readyProbeTimeout bounds how long startSpawn waits for the agent to answer an ACP initialize
// before declaring the spawn failed. Kept well under the CP scheduler's 60s Provision wait
// (cmd/cp/main.go) so the node reports ERROR (with a useful detail) rather than the scheduler
// timing out. goose boots to ACP-ready in ~5s; 30s is generous headroom for a slow node.
const readyProbeTimeout = 30 * time.Second

// probeReady blocks until the spawn's agent answers an ACP initialize, or the timeout elapses. It
// opens its OWN attach (retrying while the attach itself fails — e.g. the CRI in-pod adapter is still
// starting), sends one initialize, and waits for the matching response. The attach is closed before
// returning; detaching does not disturb the long-lived agent (the relay attaches/detaches per client
// session the same way).
func (a *attacher) probeReady(ctx context.Context, sp *spawnlet.Spawn, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	var att *runtime.AttachedStream
	for {
		var err error
		att, err = a.mgr.Attach(ctx, sp)
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("attach agent: %w", err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(250 * time.Millisecond): // CRI: adapter UDS not listening yet
		}
	}
	defer att.Close()
	return awaitInitialize(ctx, att.Stdin, att.Stdout, time.Until(deadline))
}
```

- [ ] **Step 2: Verify it compiles**

Run: `go build ./internal/node/`
Expected: builds clean (no unused-import / type errors).

- [ ] **Step 3: Wire `probeReady` into `startSpawn` (`internal/node/attach.go`)**

Replace the current `startSpawn` body. The change: capture `sp` from `Create` (it currently discards with `_`), and insert the probe before the `active++` / `ACTIVE`:

```go
func (a *attacher) startSpawn(ctx context.Context, st *nodev1.StartSpawn) {
	a.status(st.SpawnId, nodev1.SpawnPhase_STARTING, "")
	sp, err := a.mgr.Create(ctx, st.SpawnId, st.AppRef, st.Model)
	if err != nil {
		logErr("startSpawn "+st.SpawnId, err)
		a.status(st.SpawnId, nodev1.SpawnPhase_ERROR, err.Error())
		return
	}
	// Readiness gate: don't report ACTIVE until the agent answers an ACP initialize, so the CP
	// ledger's 'active' (green in the UI) means "ready to chat", not just "container launched".
	if err := a.probeReady(ctx, sp, readyProbeTimeout); err != nil {
		logErr("startSpawn "+st.SpawnId+": agent not ready", err)
		if serr := a.mgr.Stop(ctx, st.SpawnId); serr != nil { // tear down the half-started spawn
			logErr("startSpawn "+st.SpawnId+": stop after not-ready", serr)
		}
		a.status(st.SpawnId, nodev1.SpawnPhase_ERROR, err.Error())
		return
	}
	a.mu.Lock()
	a.active++
	a.mu.Unlock()
	a.status(st.SpawnId, nodev1.SpawnPhase_ACTIVE, "")
}
```

(The `active++` stays AFTER a successful probe, so a failed probe never inflates the count; the `mgr.Stop` on failure tears down the container `Create` made — the CP only marks the ledger `error`, it does not tell the node to stop.)

- [ ] **Step 4: Build + vet + full hermetic suite**

Run: `go build ./... && go vet ./internal/node/`
Expected: clean.

Run: `go test ./internal/node/ -race -count=1`
Expected: PASS (the existing node tests + the Task 1 `awaitInitialize` tests), no race.

Run: `go test ./... 2>&1 | grep -E "FAIL|ok  " | grep -v "no test files" | tail -30`
Expected: no `FAIL` lines (every package `ok`). In particular `internal/cp` still passes — CreateSpawn's async path is unchanged; the probe only delays the node's `ACTIVE`, and the CP tests fake the node via `OnStatus`, so they're unaffected.

- [ ] **Step 5: Commit**

```bash
git add internal/node/ready.go internal/node/attach.go
git commit --no-verify -m "feat(node): gate ACTIVE on an ACP initialize readiness probe [sp-39u]"
```

- [ ] **Step 6: e2e confirmation (orchestrator-run)**

The implementer's sandbox may kill the long browser+Docker run, so the **orchestrator** runs this (or dispatches a dedicated subagent for it), not the implementer:

Run: `cd web && npx playwright test`
Expected: all 11 tests pass (a test may be `flaky` and pass on retry — the known `net::ERR_NETWORK_CHANGED` / marketplace-load env flake — that counts as passing). The point: the probe now runs inside `startSpawn` against the **stub** (the e2e uses the real node lane, `cmd/spawnlet`+`cmd/cp`, stub image). The stub answers `initialize`, so spawns still reach `ACTIVE` → header "connected"; a probe that hung or errored would redden/stall every spawn and fail the suite. No assertion changes are expected.

If any test fails on a real product assertion (not the env flake), STOP — the probe broke the stub path; investigate before proceeding.

---

## Out of scope (already-filed follow-ons)

- **sp-r7t** — node owns one ACP session and filters redundant `initialize`/`session-new` on reconnect (spec-compliant; gives agent-memory). The probe deliberately uses a throwaway attach instead.
- **sp-pfm** — make `ResumeSpawn` async like `CreateSpawn` (and, implicitly, give resume the same readiness gate later).
