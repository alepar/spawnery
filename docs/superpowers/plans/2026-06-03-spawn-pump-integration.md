# Per-Spawn Pump Integration Implementation Plan (Plan 2 of 2)

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Wire the Plan-1 pump live: add `client_id`+`cursor` to the node↔CP protocol, make the CP router multi-client, rewire the node attacher onto the per-spawn pump (replacing the per-WS relay + recorder + readiness probe), and convert the web client to the thin frame protocol — fixing the reconnect-churn race that strands spawns on "working…".

**Architecture:** The pump (Plan 1, `internal/node/pump.go`) becomes the one thing that owns each spawn's goose session. The node creates it at `startSpawn` (its `initialize` is the readiness gate, replacing sp-39u's probe) and routes CP Open/Close/Frame — now carrying `client_id` — to `pump.attachClient/detachClient/fromClient`. The CP router fans node→client frames per `client_id`. The web sends `prompt{text}` frames and renders the streamed frames, carrying a per-tab `clientId` + a resumable `cursor`.

**Tech Stack:** Go 1.26, ConnectRPC + protobuf (`buf` regen), `coder/websocket`; React 19 + Vite + Vitest.

**Beads:** sp-bjd (feature, Plan 2 half) + sp-o4m (the pump-wiring interface reqs from Plan-1 review). **Spec:** `docs/superpowers/specs/2026-06-03-spawn-pump-multiclient-design.md`. **Plan 1 (merged):** the pump core in `internal/node/{frame,pump}.go`.

**Conventions:** commit `--no-verify` (beads hook), local-only (NO push). Proto regen needs `buf` + plugins from `$(go env GOPATH)/bin` on PATH. Hermetic Go + Vitest for implementers; Playwright e2e is orchestrator-run.

**Migration note:** this is a protocol cutover — between tasks the system is half-migrated (each task compiles + passes its own unit tests; full behavior is validated by the e2e in the final task). That's expected.

---

## File Structure
- `proto/node/v1/node.proto` — add `client_id` (Open/Close/Frame) + `cursor` (Open); regen `gen/node/v1`.
- `internal/cp/router/router.go` — single-client → per-spawn client **set** keyed by `clientId`.
- `internal/cp/ws.go` + `internal/cp/server.go` — bind `{spawnId, clientId, token, cursor}`; per-client attach/detach/frame routing.
- `internal/node/pump.go` — add `closeFn`/`exitFn` hooks (sp-o4m); no behavior change to the core.
- `internal/node/attach.go` — rewire `handle`/`startSpawn`/`stopSpawn` onto a per-spawn `*Pump`; delete `openSession`/`closeSession`/`feed`/`recorders`/`brokerEndpoint`/`probeReady` and the now-dead `record.go`/`ready.go` helpers they used (keep `awaitInitialize`? no — the pump owns the handshake; remove `ready.go`).
- `web/src/acp/frames.ts` (new) — the thin frame protocol (encode `prompt`/`perm_response`; decode + apply server frames).
- `web/src/App.tsx` — `openSession` binds with `clientId`+`cursor`, applies frames, sends `prompt`; drop the ACP `Client`/`Conn` handshake.

---

## Task 1: Proto — `client_id` + `cursor`

**Files:** Modify `proto/node/v1/node.proto`; regen `gen/node/v1`.

- [ ] **Step 1: Edit the proto**

In `proto/node/v1/node.proto`, change the three messages to add the fields (keep `generation`):
```proto
message SessionOpen  { string spawn_id = 1; uint64 generation = 2; string client_id = 3; int64 cursor = 4; }
message SessionClose { string spawn_id = 1; uint64 generation = 2; string client_id = 3; }
message Frame        { string spawn_id = 1; bytes data = 2; string client_id = 3; }
```

- [ ] **Step 2: Regenerate**

Run: `PATH="$(go env GOPATH)/bin:$PATH" buf generate` (from repo root, or wherever `buf.gen.yaml` lives — check `ls buf*.yaml`). Expected: `gen/node/v1/*.pb.go` updated with `ClientId`/`Cursor` getters. Confirm: `grep -rn "ClientId\|Cursor" gen/node/v1/ | head`.

- [ ] **Step 3: Build**

Run: `go build ./gen/...` → clean. (The rest of the tree won't use the fields yet; `go build ./...` may still pass since the new fields are additive.)

- [ ] **Step 4: Commit**
```bash
git add proto/node/v1/node.proto gen/node/v1/
git commit --no-verify -m "proto(node): client_id on Open/Close/Frame; cursor on Open [sp-bjd]"
```

---

## Task 2: CP — multi-client router + ws.go bind + frame routing

**Files:** Modify `internal/cp/router/router.go`, `internal/cp/ws.go`, `internal/cp/server.go`; Test: `internal/cp/router/router_test.go` (add).

- [ ] **Step 1: Write the failing router test** — add to `internal/cp/router/router_test.go` (create if absent; mirror existing test style — there may already be a router test):

```go
package router

import (
	"sync"
	"testing"

	nodev1 "spawnery/gen/node/v1"
)

type capNode struct{ mu sync.Mutex; sent []*nodev1.CPMessage }
func (n *capNode) Send(m *nodev1.CPMessage) error { n.mu.Lock(); n.sent = append(n.sent, m); n.mu.Unlock(); return nil }
func (n *capNode) opens() (out []*nodev1.SessionOpen) {
	n.mu.Lock(); defer n.mu.Unlock()
	for _, m := range n.sent { if o := m.GetOpen(); o != nil { out = append(out, o) } }
	return
}

type capClient struct{ mu sync.Mutex; got [][]byte }
func (c *capClient) Send(b []byte) error { c.mu.Lock(); c.got = append(c.got, b); c.mu.Unlock(); return nil }
func (c *capClient) count() int { c.mu.Lock(); defer c.mu.Unlock(); return len(c.got) }

func TestMultiClientFanoutAndPerClientRouting(t *testing.T) {
	r := New()
	node := &capNode{}
	r.Bind("sp1", "node-1", node)
	a, b := &capClient{}, &capClient{}
	if _, err := r.AttachClient("sp1", "ca", a, 0); err != nil { t.Fatal(err) }
	if _, err := r.AttachClient("sp1", "cb", b, 7); err != nil { t.Fatal(err) }
	// Open messages carry client_id + cursor.
	opens := node.opens()
	if len(opens) != 2 { t.Fatalf("want 2 opens, got %d", len(opens)) }
	if opens[0].ClientId != "ca" || opens[0].Cursor != 0 { t.Fatalf("open0 %+v", opens[0]) }
	if opens[1].ClientId != "cb" || opens[1].Cursor != 7 { t.Fatalf("open1 %+v", opens[1]) }
	// FromNode routes to the right client by id.
	r.FromNode("sp1", "ca", []byte("for-a"))
	r.FromNode("sp1", "cb", []byte("for-b"))
	if a.count() != 1 || b.count() != 1 { t.Fatalf("routing: a=%d b=%d", a.count(), b.count()) }
	// Detaching ca leaves cb intact; a stale detach of an absent id is a no-op.
	r.DetachClient("sp1", "ca")
	r.DetachClient("sp1", "ca")
	r.FromNode("sp1", "ca", []byte("dropped"))
	r.FromNode("sp1", "cb", []byte("still"))
	if a.count() != 1 { t.Fatalf("ca should get nothing after detach, got %d", a.count()) }
	if b.count() != 2 { t.Fatalf("cb should still receive, got %d", b.count()) }
}

func TestFromClientTagsClientID(t *testing.T) {
	r := New()
	node := &capNode{}
	r.Bind("sp1", "node-1", node)
	a := &capClient{}
	r.AttachClient("sp1", "ca", a, 0)
	if err := r.FromClient("sp1", "ca", []byte("hi")); err != nil { t.Fatal(err) }
	var fr *nodev1.Frame
	for _, m := range node.sent { if f := m.GetFrame(); f != nil { fr = f } }
	if fr == nil || fr.ClientId != "ca" || string(fr.Data) != "hi" { t.Fatalf("frame %+v", fr) }
}
```

- [ ] **Step 2: Run, verify FAIL** — `go test ./internal/cp/router/ -v` → signature/field errors.

- [ ] **Step 3: Rewrite `internal/cp/router/router.go`** — replace single `client` with a client set:

```go
type route struct {
	nodeID  string
	node    registry.NodeSender
	clients map[string]ClientSender // client_id -> sender
	done    chan struct{}
}

func (r *Router) Bind(spawnID, nodeID string, node registry.NodeSender) {
	r.mu.Lock()
	r.m[spawnID] = &route{nodeID: nodeID, node: node, clients: map[string]ClientSender{}, done: make(chan struct{})}
	r.mu.Unlock()
}

// AttachClient registers a client by id and tells the node to open the relay for it (carrying cursor).
func (r *Router) AttachClient(spawnID, clientID string, c ClientSender, cursor int64) (<-chan struct{}, error) {
	r.mu.Lock()
	rt, ok := r.m[spawnID]
	if !ok {
		r.mu.Unlock()
		return nil, fmt.Errorf("unknown spawn: %s", spawnID)
	}
	rt.clients[clientID] = c
	node, done := rt.node, rt.done
	r.mu.Unlock()
	return done, node.Send(&nodev1.CPMessage{Msg: &nodev1.CPMessage_Open{Open: &nodev1.SessionOpen{SpawnId: spawnID, ClientId: clientID, Cursor: cursor}}})
}

func (r *Router) DetachClient(spawnID, clientID string) {
	r.mu.Lock()
	rt, ok := r.m[spawnID]
	if ok {
		delete(rt.clients, clientID)
	}
	r.mu.Unlock()
	if ok {
		_ = rt.node.Send(&nodev1.CPMessage{Msg: &nodev1.CPMessage_Close{Close: &nodev1.SessionClose{SpawnId: spawnID, ClientId: clientID}}})
	}
}

func (r *Router) FromClient(spawnID, clientID string, data []byte) error {
	r.mu.Lock()
	rt, ok := r.m[spawnID]
	r.mu.Unlock()
	if !ok {
		return fmt.Errorf("unknown spawn: %s", spawnID)
	}
	return rt.node.Send(&nodev1.CPMessage{Msg: &nodev1.CPMessage_Frame{Frame: &nodev1.Frame{SpawnId: spawnID, ClientId: clientID, Data: data}}})
}

// FromNode forwards an agent->client frame to the addressed client (if still attached).
func (r *Router) FromNode(spawnID, clientID string, data []byte) {
	r.mu.Lock()
	var c ClientSender
	if rt, ok := r.m[spawnID]; ok {
		c = rt.clients[clientID]
	}
	r.mu.Unlock()
	if c != nil {
		_ = c.Send(data)
	}
}
```
(`Drop`/`DropNode`/`StopOnNode`/`Bind` keep working; `done` stays per-route. Keep the other methods unchanged.)

- [ ] **Step 4: Update `internal/cp/server.go`** — the `NodeMessage_Frame` case now passes the client id:
```go
		case *nodev1.NodeMessage_Frame:
			s.rt.FromNode(m.Frame.SpawnId, m.Frame.ClientId, m.Frame.Data) // opaque bytes; never inspected
```

- [ ] **Step 5: Update `internal/cp/ws.go` `HandleWS`** — the bind frame gains `clientId`+`cursor`; attach/detach/frame per client:
```go
		var bind struct {
			SpawnID  string `json:"spawnId"`
			ClientID string `json:"clientId"`
			Token    string `json:"token"`
			Cursor   int64  `json:"cursor"`
		}
		// ... after auth + spawn-owner checks (unchanged) ...
		cs := wsClient{conn: conn, ctx: ctx}
		done, err := s.rt.AttachClient(bind.SpawnID, bind.ClientID, cs, bind.Cursor)
		if err != nil { conn.Close(websocket.StatusInternalError, "attach failed"); return }
		// ... telemetry unchanged ...
		defer func() {
			s.rt.DetachClient(bind.SpawnID, bind.ClientID)
			// ... session_end telemetry ...
		}()
		// recv loop: tag frames with the client id
		go func() {
			for {
				_, b, err := conn.Read(ctx)
				if err != nil { recvErr <- struct{}{}; return }
				if ferr := s.rt.FromClient(bind.SpawnID, bind.ClientID, b); ferr != nil { recvErr <- struct{}{}; return }
			}
		}()
		// select on done / recvErr / ctx.Done() unchanged.
```

- [ ] **Step 6: Build + vet + test**

Run: `go build ./... && go vet ./internal/cp/...`
Run: `go test ./internal/cp/... -count=1` → all pass (the new router tests + existing CP tests; existing CP tests that called `AttachClient(spawnID, cs)` or `FromNode(spawnID, data)` must be updated to the new signatures — update them, e.g. a test client id like `"c1"` and `cursor 0`). Expected: green.

- [ ] **Step 7: Commit**
```bash
git add internal/cp/
git commit --no-verify -m "feat(cp): multi-client router + per-client ws bind/routing (client_id, cursor) [sp-bjd]"
```

---

## Task 3: Pump integration hooks (sp-o4m)

**Files:** Modify `internal/node/pump.go`; Test: add to `internal/node/pump_test.go`.

Adds two hooks the node wiring needs, WITHOUT changing `newPump`'s signature (so the 30 Plan-1 tests are untouched): a `closeFn` (the agent attach's `Close`, called by `stop()` instead of the `Stdout` type-assert) and an `exitFn` (called when `readLoop` exits on agent death, so the node can mark the spawn errored). Both are unexported fields set by the same-package integration.

- [ ] **Step 1: Write the failing test** — append to `pump_test.go`:

```go
func TestStopCallsCloseFnAndExitFnNotOnStop(t *testing.T) {
	gooseInR, gooseInW := io.Pipe()
	gooseOutR, gooseOutW := io.Pipe()
	go scriptGoose(gooseInR, gooseOutW)
	var closed, exited int
	p := newPump(gooseInW, gooseOutR)
	p.closeFn = func() error { closed++; return gooseOutR.Close() } // close stdout so readLoop unblocks
	p.exitFn = func() { exited++ }
	if err := p.start(context.Background(), 2*time.Second); err != nil { t.Fatal(err) }
	p.stop()
	select {
	case <-p.readerDone:
	case <-time.After(2 * time.Second):
		t.Fatal("readLoop did not exit")
	}
	if closed != 1 { t.Fatalf("closeFn called %d times, want 1", closed) }
	if exited != 0 { t.Fatalf("exitFn must NOT fire on intentional stop, got %d", exited) }
}

func TestExitFnFiresOnAgentDeath(t *testing.T) {
	gooseInR, gooseInW := io.Pipe()
	gooseOutR, gooseOutW := io.Pipe()
	go scriptGoose(gooseInR, gooseOutW)
	exited := make(chan struct{}, 1)
	p := newPump(gooseInW, gooseOutR)
	p.exitFn = func() { exited <- struct{}{} }
	if err := p.start(context.Background(), 2*time.Second); err != nil { t.Fatal(err) }
	defer p.stop()
	gooseOutW.Close() // agent "dies": stdout EOFs -> readLoop exits -> exitFn fires (not stopped)
	select {
	case <-exited:
	case <-time.After(2 * time.Second):
		t.Fatal("exitFn did not fire on agent death")
	}
}
```

- [ ] **Step 2: Run, verify FAIL** — `go test ./internal/node/ -run 'TestStopCallsCloseFn|TestExitFn' -v` → undefined `closeFn`/`exitFn`.

- [ ] **Step 3: Implement in `pump.go`**
- Add fields to `Pump`: `closeFn func() error` and `exitFn func()`.
- In `stop()`, replace the `io.Closer` type-assert with: prefer `closeFn`, else fall back to the assert:
```go
	if p.closeFn != nil {
		_ = p.closeFn()
	} else if c, ok := p.stdout.(io.Closer); ok {
		_ = c.Close()
	}
```
- In `readLoop`, change the exit so it fires `exitFn` only on a NON-intentional exit (agent death), not when `stop()` closed us:
```go
func (p *Pump) readLoop() {
	defer func() {
		close(p.readerDone)
		p.mu.Lock()
		stopped := p.stopped
		fn := p.exitFn
		p.mu.Unlock()
		if !stopped && fn != nil {
			fn()
		}
	}()
	rd := acp.NewReader(p.stdout)
	for { ... unchanged ... }
}
```

- [ ] **Step 4: Run, verify PASS + race** — `go test ./internal/node/ -race -count=1` → all pass (incl. the 2 new + the 30 existing). `go vet ./internal/node/`.

- [ ] **Step 5: Commit**
```bash
git add internal/node/pump.go internal/node/pump_test.go
git commit --no-verify -m "feat(node): pump closeFn/exitFn hooks for integration teardown + agent-death (sp-o4m) [sp-bjd]"
```

---

## Task 4: Node — rewire `attach.go` onto the pump

**Files:** Modify `internal/node/attach.go`; Delete the now-dead `internal/node/record.go` + `internal/node/ready.go` (and their tests `record_test.go`/`ready_test.go` if present) — the pump owns history + handshake. Keep `frame.go`/`pump.go`/`log.go`.

This is integration wiring; verified by `go build`/`vet`/the node `-race` suite (the pump's own tests cover the hard logic) + the e2e (Task 6). No new unit test required, but DELETE the obsolete helpers so the package is clean.

- [ ] **Step 1: Read the current `attach.go`** (handle/startSpawn/stopSpawn/openSession/closeSession/feed + the `attacher` struct fields `recorders`/`sessions`/`inboxes`/`active`). Confirm what `record.go`/`ready.go` export so you delete only the now-unused bits.

- [ ] **Step 2: Replace the `attacher`'s per-session machinery with a pump registry.**
- In the `attacher` struct: remove `recorders *recorderRegistry`, `sessions map[string]*session`, `inboxes map[string]chan []byte`. Add `pumps map[string]*Pump`. Keep `active`, `mu`, `cfg`, `mgr`, `httpc`, `stream`, `sendMu`.
- In `runOnce`/`Run`: drop the `recorders` plumbing; init `pumps: map[string]*Pump{}`.
- `handle`:
```go
	case *nodev1.CPMessage_Open:
		a.attachClient(m.Open.SpawnId, m.Open.ClientId, m.Open.Cursor)
	case *nodev1.CPMessage_Close:
		a.detachClient(m.Close.SpawnId, m.Close.ClientId)
	case *nodev1.CPMessage_Frame:
		a.fromClient(m.Frame.SpawnId, m.Frame.ClientId, m.Frame.Data)
```
- New thin methods:
```go
func (a *attacher) attachClient(spawnID, clientID string, cursor int64) {
	a.mu.Lock(); p := a.pumps[spawnID]; a.mu.Unlock()
	if p == nil { log.Printf("warn: attachClient: no pump for %s", spawnID); return }
	send := func(line []byte) error {
		return a.send(&nodev1.NodeMessage{Msg: &nodev1.NodeMessage_Frame{Frame: &nodev1.Frame{SpawnId: spawnID, ClientId: clientID, Data: append([]byte(nil), line...)}}})
	}
	p.attachClient(clientID, cursor, send)
}
func (a *attacher) detachClient(spawnID, clientID string) {
	a.mu.Lock(); p := a.pumps[spawnID]; a.mu.Unlock()
	if p != nil { p.detachClient(clientID) }
}
func (a *attacher) fromClient(spawnID, clientID string, data []byte) {
	a.mu.Lock(); p := a.pumps[spawnID]; a.mu.Unlock()
	if p != nil { p.fromClient(clientID, data) }
}
```

- [ ] **Step 3: Rewrite `startSpawn` to create + start the pump (the pump's `initialize` is the readiness gate, replacing `probeReady`).**
```go
func (a *attacher) startSpawn(ctx context.Context, st *nodev1.StartSpawn) {
	a.status(st.SpawnId, nodev1.SpawnPhase_STARTING, "")
	sp, err := a.mgr.Create(ctx, st.SpawnId, st.AppRef, st.Model)
	if err != nil {
		logErr("startSpawn "+st.SpawnId, err)
		a.status(st.SpawnId, nodev1.SpawnPhase_ERROR, err.Error())
		return
	}
	att, err := a.mgr.Attach(ctx, sp)
	if err != nil {
		logErr("startSpawn attach "+st.SpawnId, err)
		_ = a.mgr.Stop(ctx, st.SpawnId)
		a.status(st.SpawnId, nodev1.SpawnPhase_ERROR, err.Error())
		return
	}
	p := newPump(att.Stdin, att.Stdout)
	p.closeFn = att.Close
	p.exitFn = func() { // goose died after going active -> surface ERROR (don't strand "working…")
		a.status(st.SpawnId, nodev1.SpawnPhase_ERROR, "agent exited")
	}
	a.mu.Lock(); a.pumps[st.SpawnId] = p; a.mu.Unlock()
	if err := p.start(ctx, readyProbeTimeout); err != nil {
		logErr("startSpawn "+st.SpawnId+": agent not ready", err)
		p.stop()
		a.mu.Lock(); delete(a.pumps, st.SpawnId); a.mu.Unlock()
		_ = a.mgr.Stop(ctx, st.SpawnId)
		a.status(st.SpawnId, nodev1.SpawnPhase_ERROR, err.Error())
		return
	}
	a.mu.Lock(); a.active++; a.mu.Unlock()
	a.status(st.SpawnId, nodev1.SpawnPhase_ACTIVE, "")
}
```
(Keep `readyProbeTimeout` from `ready.go` OR inline `const readyProbeTimeout = 30 * time.Second` into `attach.go` and delete `ready.go`.)

- [ ] **Step 4: Rewrite `stopSpawn` to stop the pump.**
```go
func (a *attacher) stopSpawn(ctx context.Context, spawnID string) {
	a.mu.Lock(); p := a.pumps[spawnID]; delete(a.pumps, spawnID); a.mu.Unlock()
	if p != nil { p.stop() }
	if err := a.mgr.Stop(ctx, spawnID); err != nil { logErr("stopSpawn "+spawnID, err) }
	a.mu.Lock(); if a.active > 0 { a.active-- }; a.mu.Unlock()
	a.status(spawnID, nodev1.SpawnPhase_STOPPED, "")
}
```

- [ ] **Step 5: Delete the dead code.** Remove `openSession`, `closeSession`, `feed`, the `session` type, and delete `internal/node/record.go` + `internal/node/ready.go` (+ `record_test.go`/`ready_test.go`). Move `const readyProbeTimeout` into `attach.go` if you deleted `ready.go`. If `awaitInitialize`/`probeReady` (ready.go) are referenced anywhere else, they're not — the pump replaces them. Remove the `record.go` imports (`transcript`, `bytes`) from the package where unused.

- [ ] **Step 6: Build + vet + node suite**

Run: `go build ./... && go vet ./internal/node/`
Run: `go test ./internal/node/ -race -count=1` → pump tests pass (the deleted record/ready tests are gone). `go test ./... 2>&1 | grep FAIL` → expect none (the CP tests fake the node via `OnStatus`, unaffected; `internal/spawnlet` unaffected).

If `internal/cp` tests reference node history/replay behavior that changed, note it — but they shouldn't (they fake the node).

- [ ] **Step 7: Commit**
```bash
git add internal/node/
git commit --no-verify -m "feat(node): rewire attach onto the per-spawn pump; drop per-WS relay/recorder/probe [sp-bjd]"
```

---

## Task 5: Web — thin frame client

**Files:** Create `web/src/acp/frames.ts`; Modify `web/src/App.tsx`. Test: `web/src/acp/frames.test.ts`.

The web stops speaking ACP. It sends `prompt{text}` / `perm_response` frames and renders the server's `user/agent/thought/tool/turn/perm_request/reset` frames, carrying a per-tab `clientId` and a resumable `cursor` (`lastSeq`).

- [ ] **Step 1: Write `web/src/acp/frames.test.ts`** (the codec contract):

```ts
import { describe, it, expect } from "vitest";
import { encodePrompt, encodePermResponse, decodeFrame, type Frame } from "./frames";

describe("frames codec", () => {
  it("encodes a prompt as an ndjson line", () => {
    expect(new TextDecoder().decode(encodePrompt("hi"))).toBe(`{"kind":"prompt","text":"hi"}\n`);
  });
  it("encodes a perm response", () => {
    expect(new TextDecoder().decode(encodePermResponse("p1", true))).toBe(`{"kind":"perm_response","reqId":"p1","allow":true}\n`);
  });
  it("decodes server frames", () => {
    const f = decodeFrame(`{"seq":3,"kind":"agent","text":"hi"}`) as Frame;
    expect(f.seq).toBe(3); expect(f.kind).toBe("agent"); expect(f.text).toBe("hi");
  });
});
```

- [ ] **Step 2: Run, verify FAIL** — `cd web && npx vitest run src/acp/frames.test.ts`.

- [ ] **Step 3: Write `web/src/acp/frames.ts`**:
```ts
export interface Frame {
  seq?: number;
  kind: "user" | "agent" | "thought" | "tool" | "turn" | "perm_request" | "reset" | "prompt" | "perm_response";
  text?: string;
  toolId?: string; title?: string; status?: string;
  state?: "busy" | "idle"; queued?: number;
  reqId?: string; allow?: boolean;
  fromSeq?: number;
}
const enc = new TextEncoder();
export function encodePrompt(text: string): Uint8Array { return enc.encode(JSON.stringify({ kind: "prompt", text }) + "\n"); }
export function encodePermResponse(reqId: string, allow: boolean): Uint8Array {
  return enc.encode(JSON.stringify({ kind: "perm_response", reqId, allow }) + "\n");
}
export function decodeFrame(line: string): Frame { return JSON.parse(line) as Frame; }
```

- [ ] **Step 4: Rewire `web/src/App.tsx` `openSession`** to the frame protocol. Add (near the top): a per-tab client id `const CLIENT_ID = crypto.randomUUID();` (module scope) and a `lastSeqRef = useRef(0)`. Replace the `onOpen` body (drop `new Client`/`initialize`/`newSession`) with:
```ts
      onOpen: () => {
        if (genRef.current !== gen) return;
        sock.send(JSON.stringify({ spawnId, clientId: CLIENT_ID, token: DEV_TOKEN, cursor: lastSeqRef.current }));
        connected();
      },
```
Wrap the socket's incoming frames (the `ReconnectingSocket` is a `WebSocketLike` — set `sock.onmessage`) to a line splitter that applies frames. Use the existing `acp/conn.ts` line-framing OR a small inline buffer; for each decoded `Frame f`, if `genRef.current !== gen` return, else:
- `f.seq && (lastSeqRef.current = f.seq)` for logged frames (any frame with a seq).
- `reset` → `setItems([])`, `lastSeqRef.current = f.fromSeq ?? 0`.
- `user` → `add({ kind: "user", text: f.text })` (server-authoritative; the local optimistic add in `onSend` is removed).
- `agent` → `appendChunk("agent")(f.text!)`; `thought` → `appendChunk("thought")(f.text!)`.
- `tool` → `add({ kind: "tool", title: f.title ?? "tool", status: f.status })`.
- `turn` → `setTurn({ state: f.state!, queued: f.queued ?? 0 })` + `setItems((c) => reconcilePending(c, f.queued ?? 0))`.
- `perm_request` → `setPerm({ title: f.title ?? "an action", resolve: (allow) => { setPerm(null); sock.send(encodePermResponse(f.reqId!, allow)); } })`.

Change `onSend` to send a `prompt` frame instead of `clientRef.current.prompt(...)`:
```ts
  const onSend = (text: string) => {
    const sock = wsRef.current;
    if (!sock) return;
    sock.send(encodePrompt(text)); // the server echoes a `user` frame + drives turn-state
  };
```
Remove `clientRef`, the `Client` import, the optimistic `add({kind:"user"})`, and the `onHistory`/`onTurn` client callbacks (their data now arrives as frames). Keep `add`/`appendChunk`/`withId`/`buffersRef`/`reconcilePending`/the poll/`nextConnAction`/teardown. `teardown` still `sock.close()`. On `teardown`/`selectSpawn`, reset `lastSeqRef.current = 0` for a fresh spawn (so switching spawns replays from 0 — buffers are per-spawn; simplest is to reset the cursor on switch and rely on the node replaying that spawn's log).

(Implementer: this is the largest edit. Preserve the multi-spawn buffer behavior — `buffersRef` per spawn, `setItems` on switch — but the live data is now frames. On `selectSpawn` to an active spawn, `openSession` re-binds with `cursor: lastSeqRef.current`; since `lastSeq` is per the CURRENT spawn, reset it to 0 when switching to a different spawn so the node replays that spawn's history. Keep it simple and correct over clever.)

- [ ] **Step 5: Typecheck + web unit suite**

Run: `cd web && npx tsc --noEmit && npx vitest run` → tsc clean; all unit tests pass (the new frames test + existing; some existing `acp/client.test.ts` tests may now be obsolete — if `Client` is removed, delete `client.ts`+`client.test.ts`+`conn.test.ts` if unused, OR keep `conn.ts` for line-framing and delete only `client.*`). Update/remove tests that referenced the removed `Client`.

- [ ] **Step 6: Commit**
```bash
git add web/src/
git commit --no-verify -m "feat(web): thin frame client (prompt/subscribe, clientId+cursor); drop ACP handshake [sp-bjd]"
```

---

## Task 6: e2e + verification (orchestrator-run)

**Files:** possibly `web/e2e/*` (stub must work end-to-end with the pump; the stub already speaks ACP so the pump's handshake works).

- [ ] **Step 1: Full Go suite + builds** — `go build ./... && go vet ./... && go test ./... -count=1 2>&1 | grep -E "FAIL" || echo clean`.

- [ ] **Step 2: e2e (orchestrator/subagent runs it — sandbox kills long browser+Docker runs).** `cd web && npx playwright test`. The existing suite must pass: spawning reaches "connected", chat echoes, transcript-restore on reload/switch, the reconnect test. The stub agent answers `initialize`/`session/new` (pump handshake) and echoes prompts; the web renders the streamed frames. A failure here means a wiring gap (the pump's frames not reaching the web, or the bind/clientId/cursor not flowing) — investigate with the trace + node logs.

- [ ] **Step 3: Host (goose) manual** — `just dev`; open 2 spawns; chat; force reconnects (flap the WS / restart the node); confirm turns complete, NO stuck "working…", a second tab mirrors live, and a permission prompt can be answered from either tab. This is the real fix validation (the bug this whole epic targets).

- [ ] **Step 4: Commit any e2e tweaks**, then close `sp-bjd` + `sp-o4m`.

---

## Out of scope (filed)
- **sp-j5b** — CRI in-pod adapter multi-client (this is the Docker lane only).
- History persistence across a node restart (in-memory; same as before).
