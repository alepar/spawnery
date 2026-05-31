# Control Plane (mediation slice) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Insert a control plane (`cmd/cp`) between clients and nodes — node registry + heartbeat, spawn routing, a transparent session mux/relay, and content-free lifecycle telemetry — turning the direct client↔spawnlet topology into the real two-tier system.

**Architecture:** The node (spawnlet) gains a **CP-attached mode**: it dials the CP and holds one persistent `node.v1.NodeService/Attach` bidi stream over which register/heartbeat/control/relay-frames are muxed (keyed by CP-assigned `spawn_id`). The CP is itself a transparent two-sided byte relay (never parses ACP) plus routing/auth/telemetry. Existing `internal/spawnlet` (`Manager`, `Relay`) is reused **unchanged**; the node's standalone inbound server stays for local dev.

**Tech Stack:** Go, ConnectRPC + h2c (cleartext HTTP/2), buf codegen, `github.com/coder/websocket`, the existing Docker runtime + stub agent.

**Spec:** `docs/superpowers/specs/2026-05-30-cp-mediation-slice-design.md` (authoritative).

---

## Conventions
- Branch: `feat/cp-mediation-slice`. Beads: parent epic E1 `sp-ei4`; one milestone issue per task (or one umbrella + per-task notes) — mark in_progress at task start, close when its tests pass.
- Commit per task; trailer: `Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>`. No git remote — commit only.
- `export PATH="$PATH:$(go env GOPATH)/bin"` so `buf`/`protoc-gen-*` resolve (needed for Task 1's `make gen`).
- Run Go tests with `go test ./... -count=1`. Unit tests are Docker/key-free; the e2e is `//go:build e2e`.
- If a beads auto-export stages `.beads/issues.jsonl` and blocks a commit: `git checkout HEAD -- .beads/issues.jsonl` then retry.

## File Structure
```
proto/node/v1/node.proto          NEW  Attach rendezvous (NodeMessage/CPMessage)
proto/cp/v1/cp.proto              NEW  client-facing SpawnService (CreateSpawn(app_id)/Session/StopSpawn)
gen/node/v1/…, gen/cp/v1/…        NEW  buf-generated (committed)
internal/cp/telemetry/telemetry.go NEW Event + Sink + JSONLSink + NopSink
internal/cp/apps/apps.go          NEW  app_id → app_ref resolver (static map)
internal/cp/auth/auth.go          NEW  dev-token → owner interceptor + ctx helpers
internal/cp/registry/registry.go  NEW  node set + heartbeat capacity + Pick; NodeSender iface
internal/cp/router/router.go      NEW  session mux: spawn_id↔node / spawn_id↔client; transparent relay
internal/cp/scheduler/scheduler.go NEW placement + StartSpawn + await ACTIVE; pending map
internal/cp/server.go             NEW  CP server: node Attach loop + cp.v1 handlers + interceptor wiring
internal/cp/ws.go                 NEW  /ws/session browser relay (mirrors internal/spawnlet/ws.go)
cmd/cp/main.go                    NEW  wire units; serve cp.v1 (+/ws) and node.v1.Attach over h2c
internal/node/attach.go           NEW  node CP-attached mode (dial, register, heartbeat, handle CPMessage)
cmd/spawnlet/main.go              MOD  standalone vs attach mode on CP_ADDR
internal/cp/e2e_test.go           NEW  //go:build e2e — CP+node+stub round-trip + telemetry assertions
web/src/api/spawnlet.ts           MOD  repoint to CP, dev token, app_id
web/vite.config.ts                MOD  proxy → CP
cmd/spawnctl/main.go              MOD  -cp address (gRPC to CP Session)
Justfile                          MOD  `just cp`; `just dev` = cp + node(attach) + web
README.md                         MOD  note the CP in the run docs
```

---

## Task 1: Protos + codegen

**Files:** Create `proto/node/v1/node.proto`, `proto/cp/v1/cp.proto`; generate into `gen/`.

- [ ] **Step 1: `proto/cp/v1/cp.proto`** (mirrors the existing `spawn.v1` style; client-facing):
```proto
syntax = "proto3";
package cp.v1;
option go_package = "spawnery/gen/cp/v1;cpv1";

service SpawnService {
  rpc CreateSpawn(CreateSpawnRequest) returns (CreateSpawnResponse);
  rpc Session(stream Frame) returns (stream Frame);
  rpc StopSpawn(StopSpawnRequest) returns (StopSpawnResponse);
}

message CreateSpawnRequest { string app_id = 1; string model = 2; }
message CreateSpawnResponse { string spawn_id = 1; }
message Frame { string spawn_id = 1; bytes data = 2; }
message StopSpawnRequest { string spawn_id = 1; }
message StopSpawnResponse {}
```

- [ ] **Step 2: `proto/node/v1/node.proto`** (the rendezvous; node dials CP):
```proto
syntax = "proto3";
package node.v1;
option go_package = "spawnery/gen/node/v1;nodev1";

service NodeService {
  // Node dials the CP and holds this stream open. Node sends NodeMessage; CP sends CPMessage.
  rpc Attach(stream NodeMessage) returns (stream CPMessage);
}

enum SpawnPhase { SPAWN_PHASE_UNSPECIFIED = 0; STARTING = 1; ACTIVE = 2; STOPPING = 3; STOPPED = 4; ERROR = 5; }

message NodeMessage {
  oneof msg {
    Register    register  = 1;
    Heartbeat   heartbeat = 2;
    SpawnStatus status    = 3;
    Frame       frame     = 4;   // agent -> client bytes
  }
}
message CPMessage {
  oneof msg {
    StartSpawn   start = 1;
    StopSpawn    stop  = 2;
    SessionOpen  open  = 3;
    SessionClose close = 4;
    Frame        frame = 5;      // client -> agent bytes
  }
}

message Register    { string node_id = 1; uint32 max_spawns = 2; repeated string agent_images = 3; }
message Heartbeat   { uint32 active_spawns = 1; uint32 free_slots = 2; uint32 cpu_pct = 3; uint32 gpu_pct = 4; }
message SpawnStatus { string spawn_id = 1; SpawnPhase phase = 2; string detail = 3; }
message StartSpawn  { string spawn_id = 1; string app_ref = 2; string data_ref = 3; string model = 4; }
message StopSpawn   { string spawn_id = 1; }
message SessionOpen  { string spawn_id = 1; }
message SessionClose { string spawn_id = 1; }
message Frame       { string spawn_id = 1; bytes data = 2; }
```

- [ ] **Step 3: Generate + compile**
```bash
cd /home/debian/AleCode/spawnery
export PATH="$PATH:$(go env GOPATH)/bin"
make gen                          # buf generate (stamp-based)
go build ./gen/...
```
Expected: `make gen` creates `gen/node/v1/*.pb.go` + `gen/node/v1/nodev1connect/*.go` and `gen/cp/v1/*` likewise; `go build ./gen/...` succeeds. If `make gen` is a no-op (stale stamp), force it: `rm -f .make/gen && make gen`.

- [ ] **Step 4: Commit**
```bash
git add proto/node proto/cp gen/node gen/cp
git commit -m "proto: node.v1 rendezvous + cp.v1 client API (codegen)

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

---

## Task 2: Telemetry unit

**Files:** Create `internal/cp/telemetry/telemetry.go`, `internal/cp/telemetry/telemetry_test.go`.

- [ ] **Step 1: Failing test** — `internal/cp/telemetry/telemetry_test.go`:
```go
package telemetry

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestJSONLSinkWritesOneLinePerEvent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "events.jsonl")
	s, err := NewJSONLSink(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ts := time.Unix(1700000000, 0).UTC()
	for _, k := range []string{"spawn_create", "session_start", "session_end"} {
		if err := s.Emit(Event{Kind: k, Owner: "alice", AppID: "secret-app", Tier: "reviewed", Storage: "managed", NodeID: "n1", SpawnID: "sp1", Timestamp: ts}); err != nil {
			t.Fatal(err)
		}
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimRight(string(raw), "\n"), "\n")
	if len(lines) != 3 {
		t.Fatalf("want 3 lines, got %d: %q", len(lines), raw)
	}
	var ev Event
	if err := json.Unmarshal([]byte(lines[0]), &ev); err != nil {
		t.Fatal(err)
	}
	if ev.Kind != "spawn_create" || ev.Owner != "alice" || ev.AppID != "secret-app" {
		t.Fatalf("bad event: %+v", ev)
	}
}

func TestNopSinkIsNoOp(t *testing.T) {
	if err := NopSink{}.Emit(Event{Kind: "spawn_create"}); err != nil {
		t.Fatal(err)
	}
}
```

- [ ] **Step 2: Run, expect FAIL** — `go test ./internal/cp/telemetry/ -run TestJSONL -v` → fails (package/symbols undefined).

- [ ] **Step 3: Implement** — `internal/cp/telemetry/telemetry.go`:
```go
// Package telemetry emits content-free session-lifecycle events. It never
// carries user /data content or relay-frame bytes — metadata only.
package telemetry

import (
	"encoding/json"
	"os"
	"sync"
	"time"
)

type Event struct {
	Kind      string    `json:"kind"` // spawn_create | session_start | session_end
	Owner     string    `json:"owner"`
	AppID     string    `json:"app_id"`
	Tier      string    `json:"tier"`
	Storage   string    `json:"storage"`
	NodeID    string    `json:"node_id"`
	SpawnID   string    `json:"spawn_id"`
	Timestamp time.Time `json:"ts"`
}

type Sink interface{ Emit(Event) error }

// NopSink discards events (tests/dev).
type NopSink struct{}

func (NopSink) Emit(Event) error { return nil }

// JSONLSink appends one JSON object per line. Concurrency-safe.
type JSONLSink struct {
	mu sync.Mutex
	f  *os.File
}

func NewJSONLSink(path string) (*JSONLSink, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, err
	}
	return &JSONLSink{f: f}, nil
}

func (s *JSONLSink) Emit(ev Event) error {
	b, err := json.Marshal(ev)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := s.f.Write(append(b, '\n')); err != nil {
		return err
	}
	return nil
}

func (s *JSONLSink) Close() error { return s.f.Close() }
```

- [ ] **Step 4: Run, expect PASS** — `go test ./internal/cp/telemetry/ -v` → PASS.

- [ ] **Step 5: Commit**
```bash
git add internal/cp/telemetry
git commit -m "cp/telemetry: content-free Event + JSONL/Nop sinks

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

---

## Task 3: Apps resolver + auth interceptor

**Files:** Create `internal/cp/apps/apps.go`(+test), `internal/cp/auth/auth.go`(+test).

- [ ] **Step 1: apps failing test** — `internal/cp/apps/apps_test.go`:
```go
package apps

import "testing"

func TestResolveKnownAndUnknown(t *testing.T) {
	r := New(map[string]string{"secret-app": "examples/secret-app"})
	ref, ok := r.Resolve("secret-app")
	if !ok || ref != "examples/secret-app" {
		t.Fatalf("known: got %q ok=%v", ref, ok)
	}
	if _, ok := r.Resolve("nope"); ok {
		t.Fatal("unknown should not resolve")
	}
}
```

- [ ] **Step 2: implement apps** — `internal/cp/apps/apps.go`:
```go
// Package apps resolves a public app_id to the definition ref a node mounts at
// /app. A static map for the slice; the E5 catalog replaces it later.
package apps

type Resolver struct{ m map[string]string }

func New(m map[string]string) *Resolver { return &Resolver{m: m} }

func (r *Resolver) Resolve(appID string) (ref string, ok bool) {
	ref, ok = r.m[appID]
	return
}
```

- [ ] **Step 3: auth failing test** — `internal/cp/auth/auth_test.go`:
```go
package auth

import (
	"context"
	"testing"
)

func TestOwnerLookup(t *testing.T) {
	a := New(map[string]string{"dev-token": "alice"})
	if o, ok := a.Owner("dev-token"); !ok || o != "alice" {
		t.Fatalf("got %q ok=%v", o, ok)
	}
	if _, ok := a.Owner("bad"); ok {
		t.Fatal("bad token resolved")
	}
}

func TestContextRoundTrip(t *testing.T) {
	ctx := WithOwner(context.Background(), "alice")
	if o, ok := OwnerFromContext(ctx); !ok || o != "alice" {
		t.Fatalf("ctx owner: %q ok=%v", o, ok)
	}
	if _, ok := OwnerFromContext(context.Background()); ok {
		t.Fatal("empty ctx should have no owner")
	}
}
```

- [ ] **Step 4: implement auth** — `internal/cp/auth/auth.go`:
```go
// Package auth is the demo's stubbed identity: a dev bearer token maps to an
// owner_id. The only piece E4 OAuth later replaces; the rest of the CP is
// owner-id-agnostic.
package auth

import (
	"context"
	"strings"

	"connectrpc.com/connect"
)

type Auth struct{ tokens map[string]string } // token -> owner

func New(tokens map[string]string) *Auth { return &Auth{tokens: tokens} }

func (a *Auth) Owner(token string) (string, bool) {
	o, ok := a.tokens[token]
	return o, ok
}

type ownerKey struct{}

func WithOwner(ctx context.Context, owner string) context.Context {
	return context.WithValue(ctx, ownerKey{}, owner)
}
func OwnerFromContext(ctx context.Context) (string, bool) {
	o, ok := ctx.Value(ownerKey{}).(string)
	return o, ok && o != ""
}

// bearer extracts the token from an "Authorization: Bearer <t>" header value.
func bearer(h string) string { return strings.TrimSpace(strings.TrimPrefix(h, "Bearer ")) }

// Interceptor authenticates unary + streaming Connect calls: it reads the
// Authorization header, resolves the owner, and stashes it on the context.
func (a *Auth) Interceptor() connect.Interceptor { return &interceptor{a: a} }

type interceptor struct{ a *Auth }

func (i *interceptor) WrapUnary(next connect.UnaryFunc) connect.UnaryFunc {
	return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
		owner, ok := i.a.Owner(bearer(req.Header().Get("Authorization")))
		if !ok {
			return nil, connect.NewError(connect.CodeUnauthenticated, errUnauth)
		}
		return next(WithOwner(ctx, owner), req)
	}
}
func (i *interceptor) WrapStreamingHandler(next connect.StreamingHandlerFunc) connect.StreamingHandlerFunc {
	return func(ctx context.Context, conn connect.StreamingHandlerConn) error {
		owner, ok := i.a.Owner(bearer(conn.RequestHeader().Get("Authorization")))
		if !ok {
			return connect.NewError(connect.CodeUnauthenticated, errUnauth)
		}
		return next(WithOwner(ctx, owner), conn)
	}
}
func (i *interceptor) WrapStreamingClient(next connect.StreamingClientFunc) connect.StreamingClientFunc {
	return next // client-side: no-op
}

var errUnauth = connectError("missing or invalid auth token")

type connectError string

func (e connectError) Error() string { return string(e) }
```

- [ ] **Step 5: Run both, expect PASS** — `go test ./internal/cp/apps/ ./internal/cp/auth/ -v` → PASS.

- [ ] **Step 6: Commit**
```bash
git add internal/cp/apps internal/cp/auth
git commit -m "cp/apps + cp/auth: app_id resolver + dev-token owner interceptor

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

---

## Task 4: Node registry

**Files:** Create `internal/cp/registry/registry.go`(+test).

- [ ] **Step 1: Failing test** — `internal/cp/registry/registry_test.go`:
```go
package registry

import (
	"testing"

	nodev1 "spawnery/gen/node/v1"
)

type fakeSender struct{ sent []*nodev1.CPMessage }

func (f *fakeSender) Send(m *nodev1.CPMessage) error { f.sent = append(f.sent, m); return nil }

func TestAddHeartbeatPickEvict(t *testing.T) {
	r := New()
	if r.Pick() != nil {
		t.Fatal("empty registry should pick nothing")
	}
	s1, s2 := &fakeSender{}, &fakeSender{}
	r.Add(&Node{ID: "n1", Sender: s1, Max: 2, Free: 0})
	r.Add(&Node{ID: "n2", Sender: s2, Max: 4, Free: 3})
	// n1 has no free slots, n2 has 3 -> Pick returns n2 (most free).
	if n := r.Pick(); n == nil || n.ID != "n2" {
		t.Fatalf("pick: %+v", n)
	}
	r.Heartbeat("n2", 4, 0) // active=4 free=0 -> now nobody has capacity
	if r.Pick() != nil {
		t.Fatal("no capacity -> pick nil")
	}
	r.Remove("n2")
	if _, ok := r.Get("n2"); ok {
		t.Fatal("n2 should be gone")
	}
}
```

- [ ] **Step 2: Implement** — `internal/cp/registry/registry.go`:
```go
// Package registry tracks nodes attached to the CP and their live capacity.
package registry

import (
	"sync"

	nodev1 "spawnery/gen/node/v1"
)

// NodeSender is the CP->node side of a node's Attach stream (concurrency-safe).
type NodeSender interface{ Send(*nodev1.CPMessage) error }

type Node struct {
	ID     string
	Sender NodeSender
	Max    uint32
	Free   uint32
	Images []string
}

type Registry struct {
	mu sync.Mutex
	m  map[string]*Node
}

func New() *Registry { return &Registry{m: map[string]*Node{}} }

func (r *Registry) Add(n *Node)            { r.mu.Lock(); r.m[n.ID] = n; r.mu.Unlock() }
func (r *Registry) Remove(id string)       { r.mu.Lock(); delete(r.m, id); r.mu.Unlock() }
func (r *Registry) Get(id string) (*Node, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	n, ok := r.m[id]
	return n, ok
}

func (r *Registry) Heartbeat(id string, active, free uint32) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if n, ok := r.m[id]; ok {
		n.Free = free
	}
}

// Pick returns the node with the most free slots (>0), or nil if none.
func (r *Registry) Pick() *Node {
	r.mu.Lock()
	defer r.mu.Unlock()
	var best *Node
	for _, n := range r.m {
		if n.Free == 0 {
			continue
		}
		if best == nil || n.Free > best.Free {
			best = n
		}
	}
	return best
}
```

- [ ] **Step 3: Run, expect PASS** — `go test ./internal/cp/registry/ -v` → PASS.

- [ ] **Step 4: Commit**
```bash
git add internal/cp/registry
git commit -m "cp/registry: node set + heartbeat capacity + Pick

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

---

## Task 5: Session router (the mux) + content-free invariant

**Files:** Create `internal/cp/router/router.go`(+test).

- [ ] **Step 1: Failing test** — `internal/cp/router/router_test.go`:
```go
package router

import (
	"testing"

	nodev1 "spawnery/gen/node/v1"
)

type fakeNode struct{ sent []*nodev1.CPMessage }

func (f *fakeNode) Send(m *nodev1.CPMessage) error { f.sent = append(f.sent, m); return nil }

type fakeClient struct{ got [][]byte }

func (f *fakeClient) Send(b []byte) error { f.got = append(f.got, append([]byte(nil), b...)); return nil }

func TestRouteBothWaysAndOwnership(t *testing.T) {
	r := New()
	node := &fakeNode{}
	r.Bind("sp1", "n1", "alice", node)

	if o, _ := r.Owner("sp1"); o != "alice" {
		t.Fatalf("owner: %q", o)
	}

	cl := &fakeClient{}
	done, err := r.AttachClient("sp1", cl)
	if err != nil {
		t.Fatal(err)
	}
	// AttachClient must have told the node to open the relay.
	if len(node.sent) != 1 || node.sent[0].GetOpen().GetSpawnId() != "sp1" {
		t.Fatalf("expected SessionOpen, got %+v", node.sent)
	}

	// client -> node
	if err := r.FromClient("sp1", []byte("hello")); err != nil {
		t.Fatal(err)
	}
	last := node.sent[len(node.sent)-1]
	if string(last.GetFrame().GetData()) != "hello" || last.GetFrame().GetSpawnId() != "sp1" {
		t.Fatalf("client->node frame wrong: %+v", last)
	}

	// node -> client
	r.FromNode("sp1", []byte("world"))
	if len(cl.got) != 1 || string(cl.got[0]) != "world" {
		t.Fatalf("node->client: %v", cl.got)
	}

	// dropping the node closes the client's done channel.
	r.DropNode("n1")
	select {
	case <-done:
	default:
		t.Fatal("done not closed on node drop")
	}
}

func TestUnknownSpawnRoutingIsSafe(t *testing.T) {
	r := New()
	if err := r.FromClient("ghost", []byte("x")); err == nil {
		t.Fatal("FromClient on unknown spawn should error")
	}
	r.FromNode("ghost", []byte("x")) // must not panic with no client/route
}
```

- [ ] **Step 2: Implement** — `internal/cp/router/router.go`:
```go
// Package router is the CP session mux: it relays raw bytes between a client's
// Session stream and the one node stream hosting that spawn, keyed by spawn_id.
// It is a TRANSPARENT relay — it never parses, inspects, or logs frame content.
package router

import (
	"fmt"
	"sync"

	nodev1 "spawnery/gen/node/v1"
	"spawnery/internal/cp/registry"
)

type ClientSender interface{ Send([]byte) error }

type route struct {
	nodeID string
	owner  string
	node   registry.NodeSender
	client ClientSender
	done   chan struct{} // closed when the route is dropped (stop or node evict)
}

type Router struct {
	mu sync.Mutex
	m  map[string]*route // spawn_id -> route
}

func New() *Router { return &Router{m: map[string]*route{}} }

// Bind records which node hosts a spawn and its owner (after StartSpawn ACTIVE).
func (r *Router) Bind(spawnID, nodeID, owner string, node registry.NodeSender) {
	r.mu.Lock()
	r.m[spawnID] = &route{nodeID: nodeID, owner: owner, node: node, done: make(chan struct{})}
	r.mu.Unlock()
}

func (r *Router) Owner(spawnID string) (string, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	rt, ok := r.m[spawnID]
	if !ok {
		return "", false
	}
	return rt.owner, true
}

// AttachClient binds a live client stream and tells the node to open the relay.
// The returned channel closes if the route is dropped while attached.
func (r *Router) AttachClient(spawnID string, c ClientSender) (<-chan struct{}, error) {
	r.mu.Lock()
	rt, ok := r.m[spawnID]
	if !ok {
		r.mu.Unlock()
		return nil, fmt.Errorf("unknown spawn: %s", spawnID)
	}
	rt.client = c
	node := rt.node
	done := rt.done
	r.mu.Unlock()
	return done, node.Send(&nodev1.CPMessage{Msg: &nodev1.CPMessage_Open{Open: &nodev1.SessionOpen{SpawnId: spawnID}}})
}

// DetachClient clears the client and tells the node to detach (pod stays).
func (r *Router) DetachClient(spawnID string) {
	r.mu.Lock()
	rt, ok := r.m[spawnID]
	if ok {
		rt.client = nil
	}
	r.mu.Unlock()
	if ok {
		_ = rt.node.Send(&nodev1.CPMessage{Msg: &nodev1.CPMessage_Close{Close: &nodev1.SessionClose{SpawnId: spawnID}}})
	}
}

// FromClient forwards client->agent bytes to the hosting node.
func (r *Router) FromClient(spawnID string, data []byte) error {
	r.mu.Lock()
	rt, ok := r.m[spawnID]
	r.mu.Unlock()
	if !ok {
		return fmt.Errorf("unknown spawn: %s", spawnID)
	}
	return rt.node.Send(&nodev1.CPMessage{Msg: &nodev1.CPMessage_Frame{Frame: &nodev1.Frame{SpawnId: spawnID, Data: data}}})
}

// FromNode forwards agent->client bytes to the attached client (if any).
func (r *Router) FromNode(spawnID string, data []byte) {
	r.mu.Lock()
	rt, ok := r.m[spawnID]
	var c ClientSender
	if ok {
		c = rt.client
	}
	r.mu.Unlock()
	if c != nil {
		_ = c.Send(data)
	}
}

// Drop removes a single spawn's route (on StopSpawn) and unblocks any client.
func (r *Router) Drop(spawnID string) {
	r.mu.Lock()
	if rt, ok := r.m[spawnID]; ok {
		close(rt.done)
		delete(r.m, spawnID)
	}
	r.mu.Unlock()
}

// DropNode removes every route on a node (on evict) and unblocks their clients.
func (r *Router) DropNode(nodeID string) []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var dropped []string
	for id, rt := range r.m {
		if rt.nodeID == nodeID {
			close(rt.done)
			delete(r.m, id)
			dropped = append(dropped, id)
		}
	}
	return dropped
}
```

- [ ] **Step 3: Run, expect PASS** — `go test ./internal/cp/router/ -v` → PASS. (The two tests pin both relay directions, ownership, node-drop fan-out, and unknown-spawn safety. The "never inspects content" invariant is structural — the router only ever moves `data` opaquely; the test's `world`/`hello` payloads are arbitrary bytes it never branches on.)

- [ ] **Step 4: Commit**
```bash
git add internal/cp/router
git commit -m "cp/router: transparent session mux (spawn_id <-> node/client)

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

---

## Task 6: Scheduler (placement + StartSpawn + await ACTIVE)

**Files:** Create `internal/cp/scheduler/scheduler.go`(+test).

- [ ] **Step 1: Failing test** — `internal/cp/scheduler/scheduler_test.go`:
```go
package scheduler

import (
	"context"
	"testing"
	"time"

	nodev1 "spawnery/gen/node/v1"
	"spawnery/internal/cp/registry"
	"spawnery/internal/cp/router"
)

type fakeSender struct{ sent []*nodev1.CPMessage }

func (f *fakeSender) Send(m *nodev1.CPMessage) error { f.sent = append(f.sent, m); return nil }

func TestCreateRoutesAndAwaitsActive(t *testing.T) {
	reg := New_reg()
	rt := router.New()
	s := New(reg, rt, 2*time.Second)

	send := &fakeSender{}
	reg.Add(&registry.Node{ID: "n1", Sender: send, Max: 1, Free: 1})

	// Drive ACTIVE asynchronously once the StartSpawn lands.
	go func() {
		for {
			if len(send.sent) > 0 {
				id := send.sent[0].GetStart().GetSpawnId()
				s.OnStatus(id, nodev1.SpawnPhase_ACTIVE)
				return
			}
			time.Sleep(time.Millisecond)
		}
	}()

	id, nodeID, err := s.Create(context.Background(), "alice", "secret-app", "examples/secret-app", "m")
	if err != nil {
		t.Fatal(err)
	}
	if id == "" || nodeID != "n1" {
		t.Fatalf("create: id=%q node=%q", id, nodeID)
	}
	if got := send.sent[0].GetStart(); got.GetAppRef() != "examples/secret-app" || got.GetModel() != "m" {
		t.Fatalf("StartSpawn payload wrong: %+v", got)
	}
	if o, _ := rt.Owner(id); o != "alice" { // Create must Bind the route
		t.Fatalf("route not bound, owner=%q", o)
	}
}

func TestCreateNoCapacity(t *testing.T) {
	s := New(New_reg(), router.New(), time.Second)
	if _, _, err := s.Create(context.Background(), "alice", "a", "ref", "m"); err == nil {
		t.Fatal("expected ResourceExhausted when no node")
	}
}

// New_reg is a tiny helper to keep the test readable.
func New_reg() *registry.Registry { return registry.New() }
```

- [ ] **Step 2: Implement** — `internal/cp/scheduler/scheduler.go`:
```go
// Package scheduler assigns a spawn to a node, issues StartSpawn over that
// node's stream, and blocks until the node reports ACTIVE (or ERROR/timeout).
package scheduler

import (
	"context"
	"errors"
	"sync"
	"time"

	"connectrpc.com/connect"
	"github.com/google/uuid"

	nodev1 "spawnery/gen/node/v1"
	"spawnery/internal/cp/registry"
	"spawnery/internal/cp/router"
)

type Scheduler struct {
	reg     *registry.Registry
	rt      *router.Router
	timeout time.Duration

	mu      sync.Mutex
	pending map[string]chan nodev1.SpawnPhase // spawn_id -> ACTIVE/ERROR signal
}

func New(reg *registry.Registry, rt *router.Router, timeout time.Duration) *Scheduler {
	return &Scheduler{reg: reg, rt: rt, timeout: timeout, pending: map[string]chan nodev1.SpawnPhase{}}
}

// OnStatus is called by the node receive loop when a SpawnStatus arrives.
func (s *Scheduler) OnStatus(spawnID string, phase nodev1.SpawnPhase) {
	s.mu.Lock()
	ch, ok := s.pending[spawnID]
	s.mu.Unlock()
	if ok && (phase == nodev1.SpawnPhase_ACTIVE || phase == nodev1.SpawnPhase_ERROR) {
		select {
		case ch <- phase:
		default:
		}
	}
}

// Create picks a node, starts the spawn, and waits for ACTIVE. Returns the
// CP-assigned spawn_id and the chosen node id.
func (s *Scheduler) Create(ctx context.Context, owner, appID, appRef, model string) (string, string, error) {
	n := s.reg.Pick()
	if n == nil {
		return "", "", connect.NewError(connect.CodeResourceExhausted, errors.New("no node with capacity"))
	}
	id := uuid.NewString()
	ch := make(chan nodev1.SpawnPhase, 1)
	s.mu.Lock()
	s.pending[id] = ch
	s.mu.Unlock()
	defer func() { s.mu.Lock(); delete(s.pending, id); s.mu.Unlock() }()

	if err := n.Sender.Send(&nodev1.CPMessage{Msg: &nodev1.CPMessage_Start{Start: &nodev1.StartSpawn{
		SpawnId: id, AppRef: appRef, Model: model,
	}}}); err != nil {
		return "", "", connect.NewError(connect.CodeUnavailable, err)
	}

	select {
	case ph := <-ch:
		if ph != nodev1.SpawnPhase_ACTIVE {
			return "", "", connect.NewError(connect.CodeInternal, errors.New("spawn failed to start"))
		}
		s.rt.Bind(id, n.ID, owner, n.Sender)
		return id, n.ID, nil
	case <-time.After(s.timeout):
		return "", "", connect.NewError(connect.CodeDeadlineExceeded, errors.New("spawn start timed out"))
	case <-ctx.Done():
		return "", "", ctx.Err()
	}
}
```

- [ ] **Step 3: Add the uuid dep + run** — `go get github.com/google/uuid && go mod tidy`, then `go test ./internal/cp/scheduler/ -v` → PASS.

- [ ] **Step 4: Commit**
```bash
git add internal/cp/scheduler go.mod go.sum
git commit -m "cp/scheduler: pick node + StartSpawn + await ACTIVE

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

---

## Task 7: CP server (node Attach loop + cp.v1 handlers + ws)

**Files:** Create `internal/cp/server.go`, `internal/cp/ws.go`, `internal/cp/server_test.go`.

- [ ] **Step 1: `internal/cp/server.go`** — the integration glue tying the tested units together:
```go
// Package cp is the control plane: it accepts node Attach streams and client
// cp.v1 calls, routing between them. Both the node and the CP are transparent
// byte relays — ACP smarts live in the client and agent only.
package cp

import (
	"context"
	"fmt"
	"sync"
	"time"

	"connectrpc.com/connect"

	cpv1 "spawnery/gen/cp/v1"
	nodev1 "spawnery/gen/node/v1"
	"spawnery/internal/cp/apps"
	"spawnery/internal/cp/auth"
	"spawnery/internal/cp/registry"
	"spawnery/internal/cp/router"
	"spawnery/internal/cp/scheduler"
	"spawnery/internal/cp/telemetry"
)

type Server struct {
	reg   *registry.Registry
	rt    *router.Router
	sched *scheduler.Scheduler
	apps  *apps.Resolver
	tel   telemetry.Sink
}

func NewServer(reg *registry.Registry, rt *router.Router, sched *scheduler.Scheduler, ar *apps.Resolver, tel telemetry.Sink) *Server {
	return &Server{reg: reg, rt: rt, sched: sched, apps: ar, tel: tel}
}

// --- node side: NodeService/Attach ----------------------------------------

// nodeStream is the concurrency-safe CP->node sender (one writer per stream).
type nodeStream struct {
	mu     sync.Mutex
	stream *connect.BidiStream[nodev1.NodeMessage, nodev1.CPMessage]
}

func (n *nodeStream) Send(m *nodev1.CPMessage) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.stream.Send(m)
}

func (s *Server) Attach(ctx context.Context, stream *connect.BidiStream[nodev1.NodeMessage, nodev1.CPMessage]) error {
	sender := &nodeStream{stream: stream}
	return s.runNode(ctx, sender, stream.Receive)
}

// runNode is the receive loop, split out so it is unit-testable without gRPC.
func (s *Server) runNode(ctx context.Context, sender registry.NodeSender, recv func() (*nodev1.NodeMessage, error)) error {
	var nodeID string
	defer func() {
		if nodeID != "" {
			s.reg.Remove(nodeID)
			for _, id := range s.rt.DropNode(nodeID) {
				_ = s.tel.Emit(telemetry.Event{Kind: "session_end", NodeID: nodeID, SpawnID: id, Timestamp: time.Now().UTC()})
			}
		}
	}()
	for {
		msg, err := recv()
		if err != nil {
			return nil // stream closed
		}
		switch m := msg.Msg.(type) {
		case *nodev1.NodeMessage_Register:
			nodeID = m.Register.NodeId
			s.reg.Add(&registry.Node{ID: nodeID, Sender: sender, Max: m.Register.MaxSpawns, Free: m.Register.MaxSpawns, Images: m.Register.AgentImages})
		case *nodev1.NodeMessage_Heartbeat:
			s.reg.Heartbeat(nodeID, m.Heartbeat.ActiveSpawns, m.Heartbeat.FreeSlots)
		case *nodev1.NodeMessage_Status:
			s.sched.OnStatus(m.Status.SpawnId, m.Status.Phase)
			if m.Status.Phase == nodev1.SpawnPhase_ACTIVE {
				owner, _ := s.rt.Owner(m.Status.SpawnId)
				_ = s.tel.Emit(telemetry.Event{Kind: "spawn_create", Owner: owner, NodeID: nodeID, SpawnID: m.Status.SpawnId, Tier: "reviewed", Storage: "managed", Timestamp: time.Now().UTC()})
			}
		case *nodev1.NodeMessage_Frame:
			s.rt.FromNode(m.Frame.SpawnId, m.Frame.Data) // opaque bytes; never inspected/logged
		}
	}
}

// --- client side: cp.v1 SpawnService --------------------------------------

func (s *Server) CreateSpawn(ctx context.Context, req *connect.Request[cpv1.CreateSpawnRequest]) (*connect.Response[cpv1.CreateSpawnResponse], error) {
	owner, ok := auth.OwnerFromContext(ctx)
	if !ok {
		return nil, connect.NewError(connect.CodeUnauthenticated, fmt.Errorf("no owner"))
	}
	ref, ok := s.apps.Resolve(req.Msg.AppId)
	if !ok {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("unknown app: %s", req.Msg.AppId))
	}
	id, _, err := s.sched.Create(ctx, owner, req.Msg.AppId, ref, req.Msg.Model)
	if err != nil {
		return nil, err // scheduler already returns connect codes
	}
	return connect.NewResponse(&cpv1.CreateSpawnResponse{SpawnId: id}), nil
}

func (s *Server) StopSpawn(ctx context.Context, req *connect.Request[cpv1.StopSpawnRequest]) (*connect.Response[cpv1.StopSpawnResponse], error) {
	owner, _ := auth.OwnerFromContext(ctx)
	if err := s.stop(owner, req.Msg.SpawnId); err != nil {
		return nil, err
	}
	return connect.NewResponse(&cpv1.StopSpawnResponse{}), nil
}

// stop validates ownership, tells the node to destroy the pod, drops the route,
// and emits session_end. Shared by StopSpawn and (future) idle teardown.
func (s *Server) stop(owner, spawnID string) error {
	rtOwner, ok := s.rt.Owner(spawnID)
	if !ok {
		return connect.NewError(connect.CodeNotFound, fmt.Errorf("unknown spawn"))
	}
	if owner != rtOwner {
		return connect.NewError(connect.CodePermissionDenied, fmt.Errorf("not your spawn"))
	}
	// Route still knows the node; send StopSpawn via FromClient's node handle by
	// reusing DetachClient's node path is not enough — issue an explicit stop.
	s.rt.StopOnNode(spawnID) // see router addition below
	s.rt.Drop(spawnID)
	_ = s.tel.Emit(telemetry.Event{Kind: "session_end", Owner: rtOwner, SpawnID: spawnID, Timestamp: time.Now().UTC()})
	return nil
}

func (s *Server) Session(ctx context.Context, stream *connect.BidiStream[cpv1.Frame, cpv1.Frame]) error {
	first, err := stream.Receive()
	if err != nil {
		return err
	}
	spawnID := first.SpawnId
	owner, _ := auth.OwnerFromContext(ctx)
	rtOwner, ok := s.rt.Owner(spawnID)
	if !ok {
		return connect.NewError(connect.CodeNotFound, fmt.Errorf("unknown spawn"))
	}
	if owner != rtOwner {
		return connect.NewError(connect.CodePermissionDenied, fmt.Errorf("not your spawn"))
	}

	cs := &clientStream{stream: stream, spawnID: spawnID}
	done, err := s.rt.AttachClient(spawnID, cs)
	if err != nil {
		return connect.NewError(connect.CodeInternal, err)
	}
	_ = s.tel.Emit(telemetry.Event{Kind: "session_start", Owner: rtOwner, SpawnID: spawnID, Timestamp: time.Now().UTC()})
	defer func() {
		s.rt.DetachClient(spawnID)
		_ = s.tel.Emit(telemetry.Event{Kind: "session_end", Owner: rtOwner, SpawnID: spawnID, Timestamp: time.Now().UTC()})
	}()

	if len(first.Data) > 0 {
		_ = s.rt.FromClient(spawnID, first.Data)
	}
	// client -> node, until the client ends or the route is dropped.
	recvErr := make(chan error, 1)
	go func() {
		for {
			f, err := stream.Receive()
			if err != nil {
				recvErr <- err
				return
			}
			if ferr := s.rt.FromClient(spawnID, f.Data); ferr != nil {
				recvErr <- ferr
				return
			}
		}
	}()
	select {
	case <-done: // node evicted or spawn stopped
		return nil
	case <-recvErr:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// clientStream is the CP->client sender for the router.
type clientStream struct {
	stream  *connect.BidiStream[cpv1.Frame, cpv1.Frame]
	spawnID string
}

func (c *clientStream) Send(b []byte) error {
	return c.stream.Send(&cpv1.Frame{SpawnId: c.spawnID, Data: b})
}
```

- [ ] **Step 2: Add `StopOnNode` to the router** — append to `internal/cp/router/router.go`:
```go
// StopOnNode tells the hosting node to destroy the pod (used by StopSpawn).
func (r *Router) StopOnNode(spawnID string) {
	r.mu.Lock()
	rt, ok := r.m[spawnID]
	r.mu.Unlock()
	if ok {
		_ = rt.node.Send(&nodev1.CPMessage{Msg: &nodev1.CPMessage_Stop{Stop: &nodev1.StopSpawn{SpawnId: spawnID}}})
	}
}
```

- [ ] **Step 3: `internal/cp/ws.go`** — browser relay mirroring `internal/spawnlet/ws.go`, but the bytes go through the router (first frame carries `spawnId` + dev `token`):
```go
package cp

import (
	"encoding/json"
	"net/http"

	"github.com/coder/websocket"

	"spawnery/internal/cp/auth"
	"spawnery/internal/cp/telemetry"
	"time"
)

// HandleWS bridges a browser WebSocket to a spawn via the router. First message:
// {"spawnId":"...","token":"..."} (text); then raw ACP bytes both ways.
func (s *Server) HandleWS(authn *auth.Auth) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{OriginPatterns: []string{"*"}}) // dev only
		if err != nil {
			return
		}
		conn.SetReadLimit(16 * 1024 * 1024)
		ctx := r.Context()
		defer conn.CloseNow()

		_, first, err := conn.Read(ctx)
		if err != nil {
			return
		}
		var bind struct {
			SpawnID string `json:"spawnId"`
			Token   string `json:"token"`
		}
		if err := json.Unmarshal(first, &bind); err != nil {
			conn.Close(websocket.StatusUnsupportedData, "bad bind frame")
			return
		}
		owner, ok := authn.Owner(bind.Token)
		if !ok {
			conn.Close(websocket.StatusPolicyViolation, "unauthenticated")
			return
		}
		rtOwner, ok := s.rt.Owner(bind.SpawnID)
		if !ok || rtOwner != owner {
			conn.Close(websocket.StatusPolicyViolation, "unknown or foreign spawn")
			return
		}

		cs := wsClient{conn: conn, ctx: ctx}
		done, err := s.rt.AttachClient(bind.SpawnID, cs)
		if err != nil {
			conn.Close(websocket.StatusInternalError, "attach failed")
			return
		}
		_ = s.tel.Emit(telemetry.Event{Kind: "session_start", Owner: owner, SpawnID: bind.SpawnID, Timestamp: time.Now().UTC()})
		defer func() {
			s.rt.DetachClient(bind.SpawnID)
			_ = s.tel.Emit(telemetry.Event{Kind: "session_end", Owner: owner, SpawnID: bind.SpawnID, Timestamp: time.Now().UTC()})
		}()

		recvErr := make(chan struct{}, 1)
		go func() {
			for {
				_, b, err := conn.Read(ctx)
				if err != nil {
					recvErr <- struct{}{}
					return
				}
				if ferr := s.rt.FromClient(bind.SpawnID, b); ferr != nil {
					recvErr <- struct{}{}
					return
				}
			}
		}()
		select {
		case <-done:
		case <-recvErr:
		case <-ctx.Done():
		}
		conn.Close(websocket.StatusNormalClosure, "")
	}
}

type wsClient struct {
	conn *websocket.Conn
	ctx  contextContext
}

func (c wsClient) Send(b []byte) error { return c.conn.Write(c.ctx, websocket.MessageBinary, b) }
```
> Replace `contextContext` with `context.Context` and add `"context"` to the import block — written this way only to flag that `wsClient.ctx` is the request context. (Implementer: just use `context.Context` and import `context`.)

- [ ] **Step 4: Unit test the node receive-loop dispatch** — `internal/cp/server_test.go` (exercises `runNode` via a channel-backed `recv`, no gRPC):
```go
package cp

import (
	"context"
	"testing"
	"time"

	nodev1 "spawnery/gen/node/v1"
	"spawnery/internal/cp/apps"
	"spawnery/internal/cp/registry"
	"spawnery/internal/cp/router"
	"spawnery/internal/cp/scheduler"
	"spawnery/internal/cp/telemetry"
)

type capSender struct{ sent []*nodev1.CPMessage }

func (c *capSender) Send(m *nodev1.CPMessage) error { c.sent = append(c.sent, m); return nil }

func newTestServer() (*Server, *registry.Registry, *router.Router) {
	reg := registry.New()
	rt := router.New()
	sc := scheduler.New(reg, rt, time.Second)
	s := NewServer(reg, rt, sc, apps.New(map[string]string{"secret-app": "examples/secret-app"}), telemetry.NopSink{})
	return s, reg, rt
}

func TestRunNodeRegistersAndRoutesFrames(t *testing.T) {
	s, reg, rt := newTestServer()
	in := make(chan *nodev1.NodeMessage, 8)
	recv := func() (*nodev1.NodeMessage, error) {
		m, ok := <-in
		if !ok {
			return nil, context.Canceled
		}
		return m, nil
	}
	sender := &capSender{}
	go s.runNode(context.Background(), sender, recv)

	in <- &nodev1.NodeMessage{Msg: &nodev1.NodeMessage_Register{Register: &nodev1.Register{NodeId: "n1", MaxSpawns: 1}}}
	// bind a route + attach a client so node frames have somewhere to go.
	cl := &capClient{}
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
	rt.Bind("sp1", "n1", "alice", sender)
	if _, err := rt.AttachClient("sp1", cl); err != nil {
		t.Fatal(err)
	}
	in <- &nodev1.NodeMessage{Msg: &nodev1.NodeMessage_Frame{Frame: &nodev1.Frame{SpawnId: "sp1", Data: []byte("hi")}}}

	deadline = time.Now().Add(time.Second)
	for len(cl.got) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("node frame never reached client")
		}
		time.Sleep(time.Millisecond)
	}
	if string(cl.got[0]) != "hi" {
		t.Fatalf("got %q", cl.got[0])
	}
	close(in)
}

type capClient struct{ got [][]byte }

func (c *capClient) Send(b []byte) error { c.got = append(c.got, append([]byte(nil), b...)); return nil }
```

- [ ] **Step 5: Run** — `go test ./internal/cp/... -v` → all PASS. Fix any signature drift surfaced by the compiler.

- [ ] **Step 6: Commit**
```bash
git add internal/cp/server.go internal/cp/ws.go internal/cp/server_test.go internal/cp/router/router.go
git commit -m "cp/server: node Attach loop + cp.v1 handlers + ws relay

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

---

## Task 8: `cmd/cp` binary

**Files:** Create `cmd/cp/main.go`.

- [ ] **Step 1: Implement** — `cmd/cp/main.go`:
```go
package main

import (
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"connectrpc.com/connect"
	"golang.org/x/net/http2"
	"golang.org/x/net/http2/h2c"

	"spawnery/gen/cp/v1/cpv1connect"
	"spawnery/gen/node/v1/nodev1connect"
	"spawnery/internal/cp"
	"spawnery/internal/cp/apps"
	"spawnery/internal/cp/auth"
	"spawnery/internal/cp/registry"
	"spawnery/internal/cp/router"
	"spawnery/internal/cp/scheduler"
	"spawnery/internal/cp/telemetry"
)

func main() {
	reg := registry.New()
	rt := router.New()
	sched := scheduler.New(reg, rt, 60*time.Second)
	appMap := apps.New(map[string]string{
		"secret-app": "examples/secret-app",
	})
	authn := auth.New(parseTokens(env("CP_DEV_TOKENS", "dev-token=dev")))

	var tel telemetry.Sink = telemetry.NopSink{}
	if p := env("CP_TELEMETRY", "telemetry/events.jsonl"); p != "" {
		if err := os.MkdirAll(dir(p), 0o755); err == nil {
			if js, err := telemetry.NewJSONLSink(p); err == nil {
				tel = js
				defer js.Close()
			}
		}
	}

	srv := cp.NewServer(reg, rt, sched, appMap, tel)

	mux := http.NewServeMux()
	mux.Handle(nodev1connect.NewNodeServiceHandler(srv)) // node side: no auth (internal nodes)
	mux.Handle(cpv1connect.NewSpawnServiceHandler(srv, connect.WithInterceptors(authn.Interceptor())))
	mux.HandleFunc("/ws/session", srv.HandleWS(authn))

	addr := env("CP_ADDR", "127.0.0.1:8080")
	log.Printf("cp listening on %s", addr)
	log.Fatal(http.ListenAndServe(addr, h2c.NewHandler(mux, &http2.Server{})))
}

func parseTokens(s string) map[string]string {
	m := map[string]string{}
	for _, pair := range strings.Split(s, ",") {
		if kv := strings.SplitN(pair, "=", 2); len(kv) == 2 {
			m[strings.TrimSpace(kv[0])] = strings.TrimSpace(kv[1])
		}
	}
	return m
}
func dir(p string) string {
	if i := strings.LastIndex(p, "/"); i >= 0 {
		return p[:i]
	}
	return "."
}
func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
```
> Note: `CP_ADDR` here is the CP's own **listen** address (`:8080`). The node's `CP_ADDR` (Task 9) is the address it **dials** — different env var meaning per binary; that's fine. To avoid confusion, the node uses `CP_ADDR` to mean "the CP to dial," and `cmd/cp` uses `CP_ADDR` to mean "listen addr." If the implementer prefers, name the CP's listen var `CP_LISTEN` — update the Justfile accordingly. (Default chosen: keep `cmd/cp` on `CP_LISTEN`; rename in this file: replace `env("CP_ADDR", "127.0.0.1:8080")` with `env("CP_LISTEN", "127.0.0.1:8080")`.)

- [ ] **Step 2: Build** — `go build ./cmd/cp` → succeeds.

- [ ] **Step 3: Commit**
```bash
git add cmd/cp
git commit -m "cmd/cp: serve cp.v1 (+/ws, auth) and node.v1.Attach over h2c

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

---

## Task 9: Node CP-attached mode

**Files:** Create `internal/node/attach.go`; modify `cmd/spawnlet/main.go`.

- [ ] **Step 1: Implement the attach loop** — `internal/node/attach.go`:
```go
// Package node implements the spawnlet's CP-attached mode: it dials the CP,
// registers, heartbeats, and services CPMessages by reusing the existing
// spawnlet Manager + transparent Relay. It never accepts inbound connections.
package node

import (
	"context"
	"log"
	"sync"
	"time"

	"connectrpc.com/connect"

	nodev1 "spawnery/gen/node/v1"
	"spawnery/gen/node/v1/nodev1connect"
	"spawnery/internal/spawnlet"
)

type Config struct {
	NodeID     string
	CPURL      string // e.g. http://127.0.0.1:8080
	MaxSpawns  uint32
	AgentImage string
}

// session tracks a live relay so SessionClose can cancel it.
type session struct{ cancel context.CancelFunc }

type attacher struct {
	cfg    Config
	mgr    *spawnlet.Manager
	httpc  connect.HTTPClient

	mu       sync.Mutex
	sessions map[string]*session // spawn_id -> relay cancel
	active   uint32

	sendMu sync.Mutex
	stream *connect.BidiStreamForClient[nodev1.NodeMessage, nodev1.CPMessage]
}

func Run(ctx context.Context, mgr *spawnlet.Manager, httpc connect.HTTPClient, cfg Config) error {
	a := &attacher{cfg: cfg, mgr: mgr, httpc: httpc, sessions: map[string]*session{}}
	client := nodev1connect.NewNodeServiceClient(httpc, cfg.CPURL, connect.WithGRPC())
	a.stream = client.Attach(ctx)

	if err := a.send(&nodev1.NodeMessage{Msg: &nodev1.NodeMessage_Register{Register: &nodev1.Register{
		NodeId: cfg.NodeID, MaxSpawns: cfg.MaxSpawns, AgentImages: []string{cfg.AgentImage},
	}}}); err != nil {
		return err
	}
	go a.heartbeatLoop(ctx)

	for {
		msg, err := a.stream.Receive()
		if err != nil {
			return err
		}
		a.handle(ctx, msg)
	}
}

func (a *attacher) send(m *nodev1.NodeMessage) error {
	a.sendMu.Lock()
	defer a.sendMu.Unlock()
	return a.stream.Send(m)
}

func (a *attacher) heartbeatLoop(ctx context.Context) {
	t := time.NewTicker(5 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			a.mu.Lock()
			active := a.active
			a.mu.Unlock()
			free := a.cfg.MaxSpawns - active
			_ = a.send(&nodev1.NodeMessage{Msg: &nodev1.NodeMessage_Heartbeat{Heartbeat: &nodev1.Heartbeat{
				ActiveSpawns: active, FreeSlots: free,
			}}})
		}
	}
}

func (a *attacher) status(spawnID string, ph nodev1.SpawnPhase, detail string) {
	_ = a.send(&nodev1.NodeMessage{Msg: &nodev1.NodeMessage_Status{Status: &nodev1.SpawnStatus{SpawnId: spawnID, Phase: ph, Detail: detail}}})
}

func (a *attacher) handle(ctx context.Context, msg *nodev1.CPMessage) {
	switch m := msg.Msg.(type) {
	case *nodev1.CPMessage_Start:
		go a.startSpawn(ctx, m.Start)
	case *nodev1.CPMessage_Stop:
		a.stopSpawn(ctx, m.Stop.SpawnId)
	case *nodev1.CPMessage_Open:
		a.openSession(ctx, m.Open.SpawnId)
	case *nodev1.CPMessage_Close:
		a.closeSession(m.Close.SpawnId)
	case *nodev1.CPMessage_Frame:
		a.feed(m.Frame.SpawnId, m.Frame.Data)
	}
}

func (a *attacher) startSpawn(ctx context.Context, st *nodev1.StartSpawn) {
	a.status(st.SpawnId, nodev1.SpawnPhase_STARTING, "")
	if _, err := a.mgr.Create(ctx, st.SpawnId, st.AppRef, st.Model); err != nil {
		log.Printf("startSpawn %s: %v", st.SpawnId, err)
		a.status(st.SpawnId, nodev1.SpawnPhase_ERROR, err.Error())
		return
	}
	a.mu.Lock()
	a.active++
	a.mu.Unlock()
	a.status(st.SpawnId, nodev1.SpawnPhase_ACTIVE, "")
}

func (a *attacher) stopSpawn(ctx context.Context, spawnID string) {
	a.closeSession(spawnID)
	_ = a.mgr.Stop(ctx, spawnID)
	a.mu.Lock()
	if a.active > 0 {
		a.active--
	}
	a.mu.Unlock()
	a.status(spawnID, nodev1.SpawnPhase_STOPPED, "")
}

// openSession attaches the existing transparent Relay to the pod stdio, with a
// per-spawn inbound channel fed by CPMessage.Frame and outbound bytes wrapped
// as NodeMessage.Frame back to the CP.
func (a *attacher) openSession(ctx context.Context, spawnID string) {
	sp, ok := a.mgr.Store().Get(spawnID)
	if !ok {
		return
	}
	att, err := a.mgr.Runtime().Attach(ctx, sp.AgentID)
	if err != nil {
		return
	}
	rctx, cancel := context.WithCancel(ctx)
	inbox := make(chan []byte, 64)
	a.mu.Lock()
	a.sessions[spawnID] = &session{cancel: cancel}
	a.inboxes[spawnID] = inbox
	a.mu.Unlock()

	ep := spawnlet.StreamEndpoint{
		Recv: func() ([]byte, error) {
			select {
			case b := <-inbox:
				return b, nil
			case <-rctx.Done():
				return nil, rctx.Err()
			}
		},
		Send: func(b []byte) error {
			return a.send(&nodev1.NodeMessage{Msg: &nodev1.NodeMessage_Frame{Frame: &nodev1.Frame{SpawnId: spawnID, Data: append([]byte(nil), b...)}}})
		},
	}
	go func() {
		defer att.Close()
		spawnlet.Relay(rctx, ep, spawnlet.AgentIO{Stdin: att.Stdin, Stdout: att.Stdout})
	}()
}

func (a *attacher) feed(spawnID string, data []byte) {
	a.mu.Lock()
	inbox, ok := a.inboxes[spawnID]
	a.mu.Unlock()
	if ok {
		select {
		case inbox <- append([]byte(nil), data...):
		default:
		}
	}
}

func (a *attacher) closeSession(spawnID string) {
	a.mu.Lock()
	if s, ok := a.sessions[spawnID]; ok {
		s.cancel()
		delete(a.sessions, spawnID)
	}
	delete(a.inboxes, spawnID)
	a.mu.Unlock()
}
```
> Add `inboxes map[string]chan []byte` to the `attacher` struct and initialize it in `Run` (`inboxes: map[string]chan []byte{}`). It is declared separately here for readability; the implementer must add the field + init.

- [ ] **Step 2: Expose the runtime on Manager** — `internal/spawnlet/manager.go` add one accessor (the attach loop needs `rt.Attach`). After the existing `Store()` method:
```go
func (m *Manager) Runtime() runtime.ContainerRuntime { return m.rt }
```
(`m.rt` already exists on the struct; this is a pure additive getter — confirm `m.rt` is the field name via `grep -n 'rt ' internal/spawnlet/manager.go`.)

- [ ] **Step 3: Mode switch** — modify `cmd/spawnlet/main.go` so `CP_ADDR` selects attach mode; otherwise the existing standalone server runs unchanged:
```go
func main() {
	rt, err := runtime.NewDocker()
	if err != nil {
		log.Fatalf("docker: %v", err)
	}
	mgr := spawnlet.NewManager(rt, spawnlet.ManagerConfig{
		AgentImage:    env("AGENT_IMAGE", "spawnery/stubagent:dev"),
		SidecarImage:  env("SIDECAR_IMAGE", "spawnery/sidecar:dev"),
		OpenRouterKey: os.Getenv("OPENROUTER_API_KEY"),
		DataRoot:      env("DATA_ROOT", "/var/lib/spawnlet/spawns"),
	})

	if cpURL := os.Getenv("CP_ADDR"); cpURL != "" {
		// CP-attached mode: dial the CP, no inbound listener.
		var max uint32 = 4
		cfg := node.Config{
			NodeID:     env("NODE_ID", "node-1"),
			CPURL:      cpURL,
			MaxSpawns:  max,
			AgentImage: env("AGENT_IMAGE", "spawnery/stubagent:dev"),
		}
		log.Printf("spawnlet attaching to CP at %s as %s", cpURL, cfg.NodeID)
		log.Fatal(node.Run(context.Background(), mgr, h2cClient(), cfg))
	}

	// Standalone mode (unchanged): inbound spawn.v1 server + /ws.
	srv := spawnlet.NewServer(mgr)
	mux := http.NewServeMux()
	mux.Handle(spawnv1connect.NewSpawnServiceHandler(srv))
	mux.HandleFunc("/ws/session", srv.HandleWS)
	addr := env("SPAWNLET_ADDR", "127.0.0.1:9090")
	log.Printf("spawnlet listening on %s", addr)
	log.Fatal(http.ListenAndServe(addr, h2c.NewHandler(mux, &http2.Server{})))
}

// h2cClient mirrors cmd/spawnctl's: cleartext HTTP/2 for the CP dial.
func h2cClient() *http.Client {
	return &http.Client{Transport: &http2.Transport{
		AllowHTTP: true,
		DialTLSContext: func(ctx context.Context, network, addr string, _ *tls.Config) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, network, addr)
		},
	}}
}
```
Add imports: `context`, `crypto/tls`, `net`, `spawnery/internal/node`. (`http2`/`h2c` already imported.)

- [ ] **Step 4: Build + existing tests still green** — `go build ./... && go test ./internal/spawnlet/ -count=1` → builds; standalone spawnlet tests pass (proves standalone mode untouched).

- [ ] **Step 5: Commit**
```bash
git add internal/node cmd/spawnlet/main.go internal/spawnlet/manager.go
git commit -m "node: CP-attached mode (dial, register, heartbeat, relay) behind CP_ADDR

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

---

## Task 10: End-to-end (CP + node + stub agent)

**Files:** Create `internal/cp/e2e_test.go`.

- [ ] **Step 1: The e2e** — `internal/cp/e2e_test.go` (`//go:build e2e`; fails loudly, no skips):
```go
//go:build e2e

package cp_test

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	"golang.org/x/net/http2"
	"golang.org/x/net/http2/h2c"

	cpv1 "spawnery/gen/cp/v1"
	"spawnery/gen/cp/v1/cpv1connect"
	"spawnery/internal/acp"
	"spawnery/internal/cp"
	"spawnery/internal/cp/apps"
	"spawnery/internal/cp/auth"
	"spawnery/internal/cp/registry"
	"spawnery/internal/cp/router"
	"spawnery/internal/cp/scheduler"
	"spawnery/internal/cp/telemetry"
	"spawnery/internal/node"
	"spawnery/internal/runtime"
	"spawnery/internal/spawnlet"
)

// TestCPEndToEndStub drives the whole mediation path: a node attaches to the CP,
// a client CreateSpawns + Sessions through the CP, the stub agent echoes, and
// telemetry records spawn_create -> session_start -> session_end. Requires Docker
// + the stub/sidecar images; FAILS (no skip) if the env is broken.
func TestCPEndToEndStub(t *testing.T) {
	rt, err := runtime.NewDocker()
	if err != nil {
		t.Fatalf("docker unavailable: %v", err)
	}
	if err := rt.Ping(context.Background()); err != nil {
		t.Fatalf("docker not pingable: %v", err)
	}

	// --- CP ---
	reg := registry.New()
	rtr := router.New()
	sched := scheduler.New(reg, rtr, 60*time.Second)
	telPath := filepath.Join(t.TempDir(), "events.jsonl")
	tel, err := telemetry.NewJSONLSink(telPath)
	if err != nil {
		t.Fatal(err)
	}
	defer tel.Close()
	authn := auth.New(map[string]string{"dev-token": "alice"})
	srv := cp.NewServer(reg, rtr, sched, apps.New(map[string]string{"secret-app": "examples/secret-app"}), tel)

	mux := http.NewServeMux()
	mux.Handle(nodev1Handler(srv))
	mux.Handle(cpv1connect.NewSpawnServiceHandler(srv, connect.WithInterceptors(authn.Interceptor())))
	cpSrv := httptest.NewUnstartedServer(h2c.NewHandler(mux, &http2.Server{}))
	cpSrv.Start()
	defer cpSrv.Close()

	// --- node (attached) ---
	mgr := spawnlet.NewManager(rt, spawnlet.ManagerConfig{
		AgentImage: "spawnery/stubagent:dev", SidecarImage: "spawnery/sidecar:dev",
		OpenRouterKey: "unused", DataRoot: t.TempDir(),
	})
	nodeCtx, stopNode := context.WithCancel(context.Background())
	defer stopNode()
	go node.Run(nodeCtx, mgr, h2c(), node.Config{
		NodeID: "n1", CPURL: cpSrv.URL, MaxSpawns: 2, AgentImage: "spawnery/stubagent:dev",
	})
	// wait for the node to register
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, ok := reg.Get("n1"); ok {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("node never registered with CP")
		}
		time.Sleep(50 * time.Millisecond)
	}

	// --- client ---
	cl := cpv1connect.NewSpawnServiceClient(h2c(), cpSrv.URL, connect.WithGRPC(),
		connect.WithInterceptors(bearer("dev-token")))
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	cs, err := cl.CreateSpawn(ctx, connect.NewRequest(&cpv1.CreateSpawnRequest{AppId: "secret-app", Model: "x"}))
	if err != nil {
		t.Fatalf("createSpawn: %v", err)
	}
	id := cs.Msg.SpawnId
	defer cl.StopSpawn(context.Background(), connect.NewRequest(&cpv1.StopSpawnRequest{SpawnId: id}))

	stream := cl.Session(ctx)
	if err := stream.Send(&cpv1.Frame{SpawnId: id}); err != nil { // bind frame
		t.Fatal(err)
	}
	pr, pw := ioPipe()
	go func() {
		for {
			f, err := stream.Receive()
			if err != nil {
				pw.CloseWithError(err)
				return
			}
			pw.Write(f.Data)
		}
	}()
	c := acp.NewClient(pr, frameWriter(stream, id))
	if err := c.Initialize(); err != nil {
		t.Fatalf("init: %v", err)
	}
	if err := c.NewSession("/app"); err != nil {
		t.Fatalf("session: %v", err)
	}
	var got strings.Builder
	if err := c.Prompt("say hi", func(s string) { got.WriteString(s) }); err != nil {
		t.Fatalf("prompt: %v", err)
	}
	if !strings.Contains(got.String(), "ECHO: say hi") {
		t.Fatalf("want ECHO, got %q", got.String())
	}

	stream.CloseRequest()
	// let session_end + the StopSpawn flush, then assert telemetry.
	time.Sleep(500 * time.Millisecond)
	assertTelemetry(t, telPath, id)
}

func assertTelemetry(t *testing.T, path, spawnID string) {
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	kinds := map[string]bool{}
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		if line == "" {
			continue
		}
		var ev telemetry.Event
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			t.Fatal(err)
		}
		if ev.SpawnID == spawnID || ev.Kind == "spawn_create" {
			kinds[ev.Kind] = true
		}
		// content-free invariant: no event carries any frame bytes
		if strings.Contains(line, "ECHO") || strings.Contains(line, "say hi") {
			t.Fatalf("telemetry leaked content: %s", line)
		}
	}
	for _, k := range []string{"spawn_create", "session_start", "session_end"} {
		if !kinds[k] {
			t.Fatalf("missing telemetry event %q; file:\n%s", k, raw)
		}
	}
}
```
Plus small test helpers at the bottom of the file (so the e2e is self-contained):
```go
func h2c() *http.Client {
	return &http.Client{Transport: &http2.Transport{AllowHTTP: true,
		DialTLSContext: func(ctx context.Context, network, addr string, _ *tls.Config) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, network, addr)
		}}}
}

func bearer(token string) connect.Interceptor {
	return connect.UnaryInterceptorFunc(func(next connect.UnaryFunc) connect.UnaryFunc {
		return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
			req.Header().Set("Authorization", "Bearer "+token)
			return next(ctx, req)
		}
	})
}
```
> **Note for the implementer:** `bearer` as written only sets the header on **unary** calls; the bidi `Session` stream also needs the header. ConnectRPC streaming clients set request headers via a `connect.Interceptor` whose `WrapStreamingClient` sets `conn.RequestHeader()`. Implement a small interceptor that sets the `Authorization` header in **both** `WrapUnary` and `WrapStreamingClient` (mirror `internal/cp/auth`'s server interceptor shape). `nodev1Handler`, `ioPipe` (use `io.Pipe`), and `frameWriter` (an `io.Writer` that sends `cpv1.Frame{SpawnId:id, Data:b}` — mirror `cmd/spawnctl`'s `writerFunc`) are trivial helpers to add. `nodev1Handler` = `nodev1connect.NewNodeServiceHandler(srv)`.

- [ ] **Step 2: Ensure images, run the e2e**
```bash
cd /home/debian/AleCode/spawnery
make images                      # stub + sidecar (+ goose) if not present
go test -tags e2e ./internal/cp/ -run TestCPEndToEndStub -v -count=1
```
Expected: PASS — the ACP `ECHO: say hi` round-trips through CP→node→stub, and `events.jsonl` contains `spawn_create`, `session_start`, `session_end` with **no** content. Debug for real if it fails (node registration timeout → check the node goroutine's CP URL / h2c; missing telemetry → check the emit call sites). Do NOT weaken assertions.

- [ ] **Step 3: Commit**
```bash
git add internal/cp/e2e_test.go
git commit -m "cp: e2e — CP+node+stub ECHO round-trip + telemetry assertions (e2e tag)

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

---

## Task 11: Client + tooling repoint

**Files:** Modify `cmd/spawnctl/main.go`, `web/src/api/spawnlet.ts`, `web/src/ui/App.tsx`, `web/vite.config.ts`, `Justfile`, `README.md`.

- [ ] **Step 1: `spawnctl` — add `-cp`** (talk to the CP's gRPC `Session`; the CP needs an auth token). In `cmd/spawnctl/main.go`, add flags + token header. Replace the client construction:
```go
	addr := flag.String("addr", "http://127.0.0.1:9090", "spawnlet address (standalone)")
	cp := flag.String("cp", "", "control-plane address (e.g. http://127.0.0.1:8080); overrides -addr")
	appID := flag.String("app-id", "secret-app", "app id (CP mode)")
	token := flag.String("token", "dev-token", "dev auth token (CP mode)")
	appPath := flag.String("app", "examples/secret-app", "app definition dir (standalone)")
	model := flag.String("model", "openai/gpt-oss-120b:free", "OpenRouter model")
	flag.Parse()
```
Then branch: if `*cp != ""`, use the `cpv1connect` client with a bearer interceptor (set `Authorization: Bearer <token>` on unary + streaming) and `CreateSpawn{AppId:*appID, Model:*model}` / `Frame{SpawnId,Data}` / `StopSpawn`; else keep the existing `spawnv1connect` standalone path verbatim. (Reuse the `bearer` interceptor shape from Task 10.) Keep the same `acp.Client` driving code; only the stream/types differ. Verify: `go build ./cmd/spawnctl`.

- [ ] **Step 2: Web API repoint** — `web/src/api/spawnlet.ts`: `createSpawn(appId, model)` POSTs `/cp.v1.SpawnService/CreateSpawn` with body `{ appId, model }` and header `Authorization: Bearer ${token}`; `stopSpawn` → `/cp.v1.SpawnService/StopSpawn`. Add a `DEV_TOKEN = "dev-token"` const. Update `web/src/ui/App.tsx`: `APP_ID = "secret-app"` (replacing `APP_PATH`), pass it to `createSpawn`, and in the WS bind frame send `{ spawnId, token: DEV_TOKEN }` (the CP's `/ws/session` expects the token). The ACP client + rendering are unchanged.

- [ ] **Step 3: Vite proxy → CP** — `web/vite.config.ts`: change the proxy targets from `:9090` to the CP `http://127.0.0.1:8080`, and add a `/cp.v1.SpawnService` proxy entry (keep `/ws` with `ws:true`). Remove/replace the `/spawn.v1.SpawnService` entry:
```ts
  server: {
    host: true,
    proxy: {
      "/cp.v1.SpawnService": { target: "http://127.0.0.1:8080", changeOrigin: true },
      "/ws": { target: "http://127.0.0.1:8080", ws: true, changeOrigin: true },
    },
  },
```

- [ ] **Step 4: Justfile** — add `cp` + make `dev` three panes. Add recipe and a private image dep:
```just
# control plane (foreground)
cp:
    @make bin/cp
    CP_LISTEN={{addr_cp}} CP_DEV_TOKENS=dev-token=alice CP_TELEMETRY={{repo}}/telemetry/events.jsonl {{repo}}/bin/cp

# spawnlet attached to the CP (goose by default)
node agent="goose": (_images agent)
    @bin=spawnery/{{ if agent == "stub" { "stubagent" } else { "goose" } }}:dev; \
    AGENT_IMAGE=$bin SIDECAR_IMAGE=spawnery/sidecar:dev DATA_ROOT={{data_root}} \
    CP_ADDR=http://{{addr_cp}} NODE_ID=node-1 \
    OPENROUTER_API_KEY="${OPENROUTER_API_KEY:-unused}" \
    {{repo}}/bin/spawnlet
```
Add `addr_cp := "127.0.0.1:8080"` to the variables block and a Make target for `bin/cp` is already covered by the `bin/%` pattern rule. Update `mprocs.yaml` to three procs:
```yaml
procs:
  cp:   { shell: "just cp" }
  node: { shell: "just node" }
  web:  { shell: "just web" }
```
> The standalone `just spawnlet` recipe stays (local dev without a CP). The new `just node` is the CP-attached spawnlet; `just dev` (mprocs) now runs cp + node + web.

- [ ] **Step 5: README** — in "Running the slice", add a short note that `just dev` now runs the **CP + an attached node + web**, the browser talks to the CP (dev token), and `just spawnlet`/`spawnctl` (no `-cp`) remain the CP-less direct path.

- [ ] **Step 6: Verify build + web unit** — `go build ./... && (cd web && npx tsc -b && npm test)` → Go builds; web typechecks; vitest green.

- [ ] **Step 7: Commit**
```bash
git add cmd/spawnctl web Justfile mprocs.yaml README.md
git commit -m "client+tooling: repoint web/spawnctl to the CP; just cp + 3-pane dev

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

---

## Self-Review

**Spec coverage:** §1 topology → Tasks 1,7,8,9,11; §2 rendezvous protocol (envelopes, spawn_id mux, open/close vs start/stop bracketing) → Tasks 1,7,9; §3 internals (registry/router/scheduler/auth/apps) → Tasks 3,4,5,6; §4 telemetry (3 events + JSONL sink + no-content invariant) → Tasks 2,7,10; §5 node dual-mode + client transport (Connect unary + /ws) → Tasks 8,9,11; §6 error handling (no-capacity ResourceExhausted, unknown app InvalidArgument, bad token Unauthenticated, non-owner PermissionDenied, node-disconnect fails clients) → Tasks 3,5,6,7; §7 testing (unit per pure unit + content invariant + build-tagged e2e) → Tasks 2–7,10; §8 file structure → all tasks. **No gaps.**

**Placeholder scan:** the few "implementer must…" notes (the `wsClient.ctx` `context.Context` fix in Task 7; the `inboxes` field add in Task 9; the streaming bearer interceptor in Tasks 10/11) are **explicit, complete instructions with the exact change named**, not vague TODOs — each names the precise field/method/signature to add. No "add error handling"/"etc." placeholders remain.

**Type consistency:** `registry.NodeSender` (`Send(*nodev1.CPMessage) error`) is the single CP→node sender type used by registry, router (`Bind`/`route.node`), scheduler (`n.Sender`), and the CP server's `nodeStream`. `router.ClientSender` (`Send([]byte) error`) is implemented by `cp.clientStream` (gRPC) and `cp.wsClient` (WS). `scheduler.Create(ctx, owner, appID, appRef, model) (id, nodeID, error)` matches its one caller (`server.CreateSpawn`). `telemetry.Event` field names match across the sink, the emit sites, and the e2e assertion. Proto oneof accessors (`GetStart`/`GetOpen`/`GetFrame`/`Msg.(*nodev1.…)`) match the `node.proto` field names in Task 1. `Manager.Runtime()` (added Task 9) is used by `attacher.openSession`.

---

## Beads
Parent epic E1 (`sp-ei4`). One milestone per task (or an umbrella "CP mediation slice" issue with per-task progress notes); in_progress at task start, close when its tests pass. The final e2e (Task 10) closing is the slice's done-gate.
