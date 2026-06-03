# Node Readiness Probe (green = ready) Design

**Date:** 2026-06-03
**Status:** Approved (brainstorming)
**Bead:** sp-wi3 follow-on (file a new bead at plan time)

## Problem

The node reports `ACTIVE` the instant `mgr.Create` returns — i.e. as soon as the
sidecar + agent **containers are launched** (`internal/node/attach.go` `startSpawn`:
`status(STARTING)` → `mgr.Create` → `status(ACTIVE)`). But `mgr.Create` does **not** wait
for the agent (goose) to be ready to serve ACP; goose then takes ~5s to boot to the point
where it answers `initialize`. So the spawn shows **green (active) ~instantly**, yet the WS
can't actually establish an ACP session for another ~5s — the header sits at "connecting…"
while the sidebar dot is already green. This is misleading and defeats the `starting`
lifecycle (sp-wi3): we want the yellow `starting` period to cover the boot, and green to
mean "ready to chat".

Root cause: `ACTIVE` means "container launched", not "agent ready".

## Decisions

- **Readiness probe before `ACTIVE`.** After `mgr.Create`, the node opens an attach and
  sends an ACP `initialize`, waiting for the matching response. Only then does it report
  `ACTIVE`. So `starting`/yellow covers the full agent boot and green means the agent
  answered.
- **`initialize` is the probe.** It's a stateless capability handshake (no session created;
  `session/new` is what creates a session). The probe sends only `initialize` — never
  `session/new` — so it leaves no session behind.
- **Repeated `initialize` is safe — verified.** Our model is non-standard for ACP (one
  long-lived agent, many client sessions over its lifetime), so the client already re-sends
  `initialize` on every reconnect. Verified against real goose (2026-06-03): back-to-back
  `initialize` calls both return capabilities, and back-to-back `session/new` calls both
  succeed with distinct session ids. The probe's `initialize` is one more on top of that.
  (Enforced by the e2e "a reconnected spawn still serves new prompts"; spec-compliant
  filtering is the separate bead sp-r7t.)
- **Timeout → `ERROR` + teardown.** If the agent doesn't answer within the probe timeout
  (30s; comfortably under the scheduler's 60s `Provision` wait — `cmd/cp/main.go`), the node
  tears down the half-started spawn (`mgr.Stop`) and reports `ERROR`. The CP's async
  `provisionSpawn` (sp-wi3) maps that to `SetError` → red, removable via Stop.
- **Lane-agnostic via `mgr.Attach`.** The probe uses the same `mgr.Attach` the relay uses, so
  it works in both lanes: Docker (attach to goose's stdio) and CRI (`AttachACP` UDS to the
  in-pod adapter). Because the CRI in-pod adapter may still be starting when `Create`
  returns, the probe **retries the attach** until it succeeds or the deadline elapses — a
  bonus: once a spawn is `ACTIVE`, the adapter+agent are provably up, so the first client
  `openSession` no longer races the adapter startup.
- **Detach is safe.** The probe's attach + `att.Close()` mirrors exactly what the relay does
  on every client session end (`openSession`'s `defer att.Close()`); that path is known not
  to disturb the long-lived agent (reconnect works, including the new-prompt-after-reconnect
  e2e). So the probe's attach/detach is safe by the same property.

## Architecture

### `internal/node/ready.go` (new)

```go
package node

// readyProbeTimeout bounds how long startSpawn waits for the agent to answer an ACP initialize
// before declaring the spawn failed. Kept well under the CP scheduler's 60s Provision wait
// (cmd/cp/main.go) so the node reports ERROR (with a useful detail) rather than the scheduler
// timing out. goose boots to ACP-ready in ~5s; 30s is generous headroom for a slow node.
const readyProbeTimeout = 30 * time.Second

// probeInitID is the JSON-RPC id used for the probe's initialize. Local to the probe's own attach
// (closed before any client connects), so it can't collide with the client's request ids.
const probeInitID = 1

// probeReady blocks until the spawn's agent answers an ACP initialize, or the timeout elapses.
// It opens its OWN attach (retrying while the attach itself fails — e.g. the CRI in-pod adapter is
// still starting), sends one initialize, and waits for the matching response. The attach is closed
// before returning; detaching does not disturb the long-lived agent (same as the relay per session).
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

// awaitInitialize writes one ACP initialize request to stdin and reads stdout until the matching
// response arrives (ready), the deadline elapses, or the stream closes. Other frames the agent emits
// (e.g. notifications) are ignored. This is the unit-tested core (pure io.Reader/io.Writer).
func awaitInitialize(ctx context.Context, stdin io.Writer, stdout io.Reader, timeout time.Duration) error {
	req := acp.Message{
		JSONRPC: "2.0",
		ID:      ptrInt(probeInitID),
		Method:  "initialize",
		Params:  json.RawMessage(`{"protocolVersion":1,"clientCapabilities":{}}`),
	}
	if err := acp.WriteMessage(stdin, req); err != nil {
		return fmt.Errorf("write initialize: %w", err)
	}
	done := make(chan error, 1) // buffered so the reader goroutine never leaks on timeout
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

func ptrInt(i int) *int { return &i }
```

On timeout/ctx-cancel the `select` returns and `probeReady`'s `defer att.Close()` closes the
stream, which unblocks the reader goroutine's `ReadMessage` (it sends to the buffered `done`
and exits — no leak).

### `internal/node/attach.go` — `startSpawn`

Capture the spawn from `Create` (currently discarded) and insert the probe between `Create`
and `ACTIVE`:

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

Notes:
- The active-count increment stays **after** a successful probe, so a failed probe never
  inflates the count, and the `mgr.Stop` on failure tears down the container `Create` made
  (no node-side leak; the CP only marks the ledger `error`, it doesn't tell the node to stop).
- No recorder exists yet at this point (recorders are created in `openSession`), so the
  failure path needs no recorder cleanup.

## Testing

- **Hermetic unit test** (`internal/node/ready_test.go`) for `awaitInitialize`, using
  in-memory `io.Pipe`s (no Manager/Docker):
  - **ready:** a stdout that emits a valid `{"id":1,"result":{...}}` → returns `nil`; assert
    the bytes written to stdin contain an `initialize` request.
  - **ignores other frames:** stdout emits a `session/update` notification (no id) *then* the
    `id:1` result → returns `nil`.
  - **error response counts as answered:** stdout emits `{"id":1,"error":{...}}` → returns
    `nil` (the agent is up and answered).
  - **timeout:** stdout that never emits (a blocking pipe) with a 50ms timeout → returns a
    timeout error.
  - **stream closed:** stdout closed immediately (EOF) → returns a read error.
- **e2e** (`web/e2e/spawn-lifecycle.spec.ts`): the suite already runs the **real node lane**
  (`cmd/spawnlet` attached to `cmd/cp`, stub agent image). The stub answers `initialize`, so
  `startSpawn`'s probe succeeds and the spawn reaches `ACTIVE` → header "connected". Thus the
  probe is exercised integration-level by every existing test (a probe that never returned, or
  returned error, would hang/redden the spawn and fail them). No new e2e needed; the transient
  yellow→green is too fast with the stub to assert reliably (same rationale as sp-wi3).
- **Host (goose), manual:** `just dev`, spawn the Secret App — the sidebar dot now stays
  **yellow for ~5s** (the real boot) and turns **green** only when goose answers, with the WS
  connecting near-instantly after green. A goose that never came up (or a deliberately broken
  image) turns the dot **red** at ~30s.

## Non-goals

- Caching/holding the probe's connection as a warm session (that's the node-owns-one-session
  redesign — bead **sp-r7t**). This probe opens its own throwaway attach and closes it.
- Making `ResumeSpawn` apply the same readiness gate (separate follow-on **sp-pfm**, which
  makes resume async like create).
- Tuning the 30s timeout per agent type or making it configurable (YAGNI; revisit if a real
  agent boots slower).
