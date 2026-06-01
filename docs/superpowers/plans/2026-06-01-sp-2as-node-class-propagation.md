# Node-Class Propagation to CP (sp-2as) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development. Steps use checkbox (`- [ ]`) syntax.

**Goal:** Propagate a node's class (`cloud`/`self-hosted`) from the node to the CP at registration, record it on the registry, and stamp it on the `spawn_create` telemetry event.

**Architecture:** Node `Register` message gains `node_class` → CP records it on the in-memory `registry.Node` → telemetry consumes it. Pure-CP + node contract; hermetically testable via the existing `runNode` channel harness.

**Tech Stack:** Go, ConnectRPC, buf.

**Source spec:** `docs/superpowers/specs/2026-06-01-node-class-propagation-sp-2as.md`

**Conventions:** commit `--no-verify`; codegen via `export PATH="$PATH:$(go env GOPATH)/bin" && make gen` (install missing tools, never stub); bead `sp-2as`; branch `sp-2as-node-class` off master.

---

## Task 1: Contract — `node_class` on `Register`

**Files:** Modify `proto/node/v1/node.proto`; regenerated `gen/node/v1/*`.

- [ ] **Step 1:** In `proto/node/v1/node.proto`, the `Register` message is:
`message Register    { string node_id = 1; uint32 max_spawns = 2; repeated string agent_images = 3; repeated RunningSpawn running = 4; }`
Add field 5:
```proto
message Register    { string node_id = 1; uint32 max_spawns = 2; repeated string agent_images = 3; repeated RunningSpawn running = 4; string node_class = 5; }
```

- [ ] **Step 2:** `export PATH="$PATH:$(go env GOPATH)/bin" && make gen`

- [ ] **Step 3:** Verify: `go build ./... && grep -c "func (x \*Register) GetNodeClass" gen/node/v1/node.pb.go` — builds clean, accessor present (count 1).

- [ ] **Step 4:** Commit:
```bash
git add proto/node/v1/node.proto gen/node/v1
git commit --no-verify -m "feat(node): node_class on Register (sp-2as)"
```
(Pure codegen; verification is build + accessor grep.)

---

## Task 2: Propagate + record + telemetry

**Files:** Modify `internal/cp/registry/registry.go`, `internal/cp/server.go`, `internal/cp/telemetry/telemetry.go`, `internal/node/attach.go`, `cmd/spawnlet/main.go`, `internal/cp/server_test.go` (refactor helper); create `internal/cp/node_class_test.go`.

> Capable model — multi-file propagation + two hermetic tests via the `runNode` harness.

- [ ] **Step 1: Failing tests** — create `internal/cp/node_class_test.go`:
```go
package cp

import (
	"context"
	"sync"
	"testing"
	"time"

	nodev1 "spawnery/gen/node/v1"
	"spawnery/internal/cp/registry"
	"spawnery/internal/cp/telemetry"
)

// captureSink records emitted telemetry events.
type captureSink struct {
	mu     sync.Mutex
	events []telemetry.Event
}

func (c *captureSink) Emit(e telemetry.Event) error {
	c.mu.Lock()
	c.events = append(c.events, e)
	c.mu.Unlock()
	return nil
}
func (c *captureSink) find(kind string) (telemetry.Event, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, e := range c.events {
		if e.Kind == kind {
			return e, true
		}
	}
	return telemetry.Event{}, false
}

func feedRegister(in chan *nodev1.NodeMessage, nodeID, class string) {
	in <- &nodev1.NodeMessage{Msg: &nodev1.NodeMessage_Register{Register: &nodev1.Register{NodeId: nodeID, MaxSpawns: 1, NodeClass: class}}}
}

func waitNodeClass(t *testing.T, reg *registry.Registry, id, want string) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for {
		if n, ok := reg.Get(id); ok && n.Class == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("node %s did not reach class %q", id, want)
		}
		time.Sleep(time.Millisecond)
	}
}

func TestRegisterRecordsNodeClass(t *testing.T) {
	s, reg, _ := newTestServer(t)
	in := make(chan *nodev1.NodeMessage, 4)
	recv := func() (*nodev1.NodeMessage, error) {
		m, ok := <-in
		if !ok {
			return nil, context.Canceled
		}
		return m, nil
	}
	go s.runNode(context.Background(), &capSender{}, recv)
	feedRegister(in, "n1", "self-hosted")
	waitNodeClass(t, reg, "n1", "self-hosted")

	// a second node with empty class defaults to "cloud" (safe default).
	feedRegister(in, "n2", "")
	waitNodeClass(t, reg, "n2", "cloud")
	close(in)
}
```
> Confirm `reg.Get(id)` returns `(*registry.Node, bool)` (it does — used in `server_test.go:73`). This mirrors the registration poll loop in `TestRunNodeRegistersAndRoutesFrames`.

Then the telemetry test:
```go
func TestSpawnCreateTelemetryCarriesNodeClass(t *testing.T) {
	cap := &captureSink{}
	s, reg, _ := newTestServerSink(t, cap)
	in := make(chan *nodev1.NodeMessage, 4)
	recv := func() (*nodev1.NodeMessage, error) {
		m, ok := <-in
		if !ok {
			return nil, context.Canceled
		}
		return m, nil
	}
	go s.runNode(context.Background(), &capSender{}, recv)
	feedRegister(in, "n1", "self-hosted")
	// wait for registration
	deadline := time.Now().Add(time.Second)
	for {
		if _, ok := reg.Get("n1"); ok {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("node never registered")
		}
		time.Sleep(time.Millisecond)
	}
	// a Status ACTIVE for some spawn id triggers the spawn_create emit in runNode.
	in <- &nodev1.NodeMessage{Msg: &nodev1.NodeMessage_Status{Status: &nodev1.SpawnStatus{SpawnId: "sp1", Phase: nodev1.SpawnPhase_ACTIVE}}}
	deadline = time.Now().Add(time.Second)
	for {
		if e, ok := cap.find("spawn_create"); ok {
			if e.NodeClass != "self-hosted" {
				t.Fatalf("spawn_create NodeClass = %q (want self-hosted)", e.NodeClass)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("no spawn_create telemetry")
		}
		time.Sleep(time.Millisecond)
	}
	close(in)
}
```
> The Status message type is `nodev1.SpawnStatus` (confirm the exact field/enum names: `SpawnId`, `Phase`, `nodev1.SpawnPhase_ACTIVE` — grep `node.proto`/`server.go`). The spawn_create emit does a `s.st.Spawns().Get(ctx, spawnId)` for the owner; a missing spawn just yields empty owner (the emit still fires) — so "sp1" need not exist. Confirm this against the `server.go` Status-ACTIVE branch; if a missing spawn skips the emit, create a spawn first via `CreateSpawn` + use its id.

- [ ] **Step 2: Confirm failure:** `go test ./internal/cp/ -run 'TestRegisterRecordsNodeClass|TestSpawnCreateTelemetry' 2>&1 | head` (no `Register.NodeClass` field consumed, no `registry.Node.Class`, no `newTestServerSink`, no `Event.NodeClass`).

- [ ] **Step 3: `registry.Node.Class`** (`internal/cp/registry/registry.go`): add `Class string` to the `Node` struct.

- [ ] **Step 4: CP handler** (`internal/cp/server.go` `runNode`): add a loop-scoped `var nodeClass string` next to `nodeID`. In the `NodeMessage_Register` case set both:
```go
		case *nodev1.NodeMessage_Register:
			nodeID = m.Register.NodeId
			nodeClass = m.Register.NodeClass
			if nodeClass == "" {
				nodeClass = "cloud" // safe default: an unidentified node is assumed restricted
			}
			s.reg.Add(&registry.Node{ID: nodeID, Sender: sender, Max: m.Register.MaxSpawns, Free: m.Register.MaxSpawns, Images: m.Register.AgentImages, Class: nodeClass})
```
In the `spawn_create` Emit (Status ACTIVE branch), add `NodeClass: nodeClass`:
```go
				_ = s.tel.Emit(telemetry.Event{Kind: "spawn_create", Owner: owner, NodeID: nodeID, NodeClass: nodeClass, SpawnID: m.Status.SpawnId, Tier: "reviewed", Storage: "managed", Timestamp: time.Now().UTC()})
```

- [ ] **Step 5: `telemetry.Event.NodeClass`** (`internal/cp/telemetry/telemetry.go`): add to the `Event` struct:
```go
	NodeClass string    `json:"node_class"`
```
(place after `NodeID`).

- [ ] **Step 6: Node send** (`internal/node/attach.go`): add `NodeClass string` to `node.Config` (the struct at ~line 19), and include it in the `Register{...}` send (~line 52-53):
```go
		NodeId: cfg.NodeID, MaxSpawns: cfg.MaxSpawns, AgentImages: []string{cfg.AgentImage}, NodeClass: cfg.NodeClass,
```

- [ ] **Step 7: cmd wiring** (`cmd/spawnlet/main.go`): in the CP-attached branch where `node.Config{...}` is built (the block with `NodeID: env("NODE_ID", "node-1")`), add:
```go
			NodeClass:  env("NODE_CLASS", "cloud"),
```
(reuses the same `NODE_CLASS` env the `ManagerConfig` already reads.)

- [ ] **Step 8: Refactor the test helper** (`internal/cp/server_test.go`): split `newTestServer` so a sink can be injected, keeping existing callers working:
```go
func newTestServer(t *testing.T) (*Server, *registry.Registry, *router.Router) {
	return newTestServerSink(t, telemetry.NopSink{})
}

func newTestServerSink(t *testing.T, sink telemetry.Sink) (*Server, *registry.Registry, *router.Router) {
	reg := registry.New()
	rt := router.New()
	sc := scheduler.New(reg, rt, time.Second)
	st := store.NewTestStore(t)
	if err := Seed(context.Background(), st, map[string]string{"dev-token": "alice"},
		[]AppSeed{{ID: "secret-app", Ref: "examples/secret-app", Version: "1.0.0", Mounts: []string{"main"}}}); err != nil {
		t.Fatal(err)
	}
	s := NewServer(reg, rt, sc, st, sink)
	return s, reg, rt
}
```

- [ ] **Step 9: Verify the test file compiles** — imports (`context`, `sync`, `testing`, `time`, `nodev1`, `registry`, `telemetry`) resolve; `captureSink`/`feedRegister`/`waitNodeClass` don't collide with names already in the package's test files (grep).

- [ ] **Step 10: Run:** `go test ./internal/cp/ -run 'TestRegisterRecordsNodeClass|TestSpawnCreateTelemetry'` — PASS.

- [ ] **Step 11: Full package + race + build:** `go test ./internal/cp/ -race && go build ./...` — PASS/clean (existing `runNode` tests send `Register` without `node_class` → defaults to `cloud`; nothing breaks).

- [ ] **Step 12: Commit:**
```bash
git add internal/cp internal/node/attach.go cmd/spawnlet/main.go
git commit --no-verify -m "feat(cp): record + telemetry-stamp node class from Register (sp-2as)"
```

---

## Final Verification
- [ ] `export PATH="$PATH:$(go env GOPATH)/bin" && make gen` — no diff.
- [ ] `go build ./... && go build -tags e2e ./... && go vet ./...` — clean.
- [ ] `go test ./...` — pass; `go test ./internal/cp/ -race` — race-clean.

Then **superpowers:finishing-a-development-branch** (Option 1: merge to master locally).

---

## Self-Review Notes
- **Spec coverage:** §2.1 proto → T1; §2.2 node send + cmd → T2 (S6/S7); §2.3 registry + handler → T2 (S3/S4); §2.4 telemetry → T2 (S5/S4). Tests → T2 (S1/S9). Out-of-scope (scheduler routing, durable node store, client API) absent. ✓
- **Types:** `Register.NodeClass`/`GetNodeClass`, `registry.Node.Class`, `telemetry.Event.NodeClass`, `node.Config.NodeClass`, `newTestServerSink` consistent. ✓
- **Safe default:** empty `node_class` → `"cloud"` at the CP handler (N.2). ✓
- **Risk:** the telemetry test depends on the Status-ACTIVE branch emitting even when the spawn row is absent — Step 1's note tells the implementer to verify and, if needed, create a real spawn first.
