# sp-mqj — Lifecycle Contracts (E0) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Land the proto/contract additions that gate the CP-side spawn lifecycle, regenerate the Go bindings, and keep the whole tree compiling — **without** implementing any lifecycle behavior (that is `sp-pc4`/`sp-gd9`).

**Architecture:** Three `.proto` files drive codegen via **buf** (`make gen` → `buf generate`, plugins `protoc-gen-go` + `protoc-gen-connect-go`, output under `gen/`). This plan edits `proto/node/v1/node.proto` (CP↔node wire) and `proto/cp/v1/cp.proto` (client↔CP RPCs), regenerates, and makes one Go change: embed the generated `UnimplementedSpawnServiceHandler` in `*cp.Server` so the new RPCs auto-stub to `CodeUnimplemented`. New node-side enum values / oneof variants / message fields do **not** break the existing `switch` sites (Go oneof switches need no exhaustiveness; new scalar fields default to zero). `proto/spawn/v1` (the legacy CP-less direct path) is **out of scope**.

**Tech Stack:** Protocol Buffers (proto3), buf v1.45.0, protoc-gen-go v1.34.2, protoc-gen-connect-go v1.16.2, ConnectRPC (`connectrpc.com/connect`), Go 1.25, `google.golang.org/protobuf/proto` for the round-trip test.

**Beads:** `sp-mqj` (this work). Predecessor of `sp-pc4`.

---

## Pre-flight (do once, before Task 1)

The codegen tools already exist in `$(go env GOPATH)/bin` but are **not on `PATH`**. Every task that runs `make gen` needs them resolvable.

- [ ] **Step P1: Put the Go tool bin on PATH for this session**

```bash
export PATH="$PATH:$(go env GOPATH)/bin"
buf --version    # expect: 1.45.0  (if "command not found", install the pinned tools below)
```

If any tool is missing, install the pinned versions (matches the existing `buf.gen.yaml`):

```bash
go install github.com/bufbuild/buf/cmd/buf@v1.45.0
go install google.golang.org/protobuf/cmd/protoc-gen-go@v1.34.2
go install connectrpc.com/connect/cmd/protoc-gen-connect-go@v1.16.2
```

- [ ] **Step P2: Confirm a clean baseline**

```bash
go build ./... && go vet ./...
```
Expected: no output (clean). This is the green state every task must return to.

---

## File Structure

| Path | Change | Responsibility |
|---|---|---|
| `proto/node/v1/node.proto` | modify | CP↔node wire: add `generation`, `SUSPENDED` phase, `Suspend` + `SuspendComplete`, `MountBinding`/`MountMarker`/`RunningSpawn`, inventory on `Register`/`Heartbeat` |
| `proto/cp/v1/cp.proto` | modify | client↔CP: add `ListSpawns`/`SuspendSpawn`/`ResumeSpawn`/`RecreateSpawn`/`DeleteSpawn` RPCs + messages + `SpawnStatus` enum; per-mount bindings on `CreateSpawnRequest` |
| `gen/node/v1/*`, `gen/cp/v1/*` | regenerate | generated Go (never hand-edited) |
| `internal/cp/server.go` | modify (small) | embed `cpv1connect.UnimplementedSpawnServiceHandler` so `*Server` satisfies the grown handler interface; add a compile-time assertion |
| `internal/contract/wire_test.go` | create | a contract test that constructs + round-trips the new messages/fields (red until the proto+regen lands) |

---

### Task 1: node.v1 wire contract additions

**Files:**
- Create: `internal/contract/wire_test.go`
- Modify: `proto/node/v1/node.proto`
- Regenerate: `gen/node/v1/node.pb.go`

- [ ] **Step 1: Write the failing contract test (node half)**

Create `internal/contract/wire_test.go`:

```go
package contract

import (
	"testing"

	"google.golang.org/protobuf/proto"

	nodev1 "spawnery/gen/node/v1"
)

func TestNodeContractFields(t *testing.T) {
	// generation threaded onto every CP->node command + onto SpawnStatus
	start := &nodev1.StartSpawn{
		SpawnId: "sp1", AppRef: "ref", DataRef: "", Model: "m",
		Generation: 7,
		Mounts:     []*nodev1.MountBinding{{Name: "main", BackendUri: "managed:repo"}},
	}
	_ = &nodev1.StopSpawn{SpawnId: "sp1", Generation: 7}
	_ = &nodev1.Suspend{SpawnId: "sp1", Generation: 7}
	_ = &nodev1.SessionOpen{SpawnId: "sp1", Generation: 7}
	_ = &nodev1.SessionClose{SpawnId: "sp1", Generation: 7}
	_ = &nodev1.SpawnStatus{SpawnId: "sp1", Phase: nodev1.SpawnPhase_SUSPENDED, Generation: 7}

	// new CP->node Suspend command variant; new node->CP SuspendComplete with per-mount markers
	_ = &nodev1.CPMessage{Msg: &nodev1.CPMessage_Suspend{
		Suspend: &nodev1.Suspend{SpawnId: "sp1", Generation: 7}}}
	_ = &nodev1.NodeMessage{Msg: &nodev1.NodeMessage_SuspendComplete{
		SuspendComplete: &nodev1.SuspendComplete{
			SpawnId: "sp1", Generation: 7,
			Markers: []*nodev1.MountMarker{{Name: "main", Marker: "spawnery-suspend/sp1/7"}},
		}}}

	// node inventory on Register + Heartbeat
	_ = &nodev1.Register{NodeId: "n1", Running: []*nodev1.RunningSpawn{
		{SpawnId: "sp1", Generation: 7, Phase: nodev1.SpawnPhase_ACTIVE}}}
	_ = &nodev1.Heartbeat{Running: []*nodev1.RunningSpawn{
		{SpawnId: "sp1", Generation: 7, Phase: nodev1.SpawnPhase_ACTIVE}}}

	// round-trip proves the wire actually encodes the new fields
	b, err := proto.Marshal(start)
	if err != nil {
		t.Fatal(err)
	}
	var got nodev1.StartSpawn
	if err := proto.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	if got.Generation != 7 || len(got.Mounts) != 1 || got.Mounts[0].Name != "main" {
		t.Fatalf("round-trip lost fields: %+v", &got)
	}
}
```

- [ ] **Step 2: Run the test — verify it fails to COMPILE (red)**

```bash
go test ./internal/contract/ 2>&1 | head -20
```
Expected: build errors like `start.Generation undefined`, `undefined: nodev1.Suspend`, `nodev1.SpawnPhase_SUSPENDED undefined`, `undefined: nodev1.MountBinding`. (For proto, "red" is a compile failure against not-yet-generated types.)

- [ ] **Step 3: Edit `proto/node/v1/node.proto` to the new contract**

Replace the entire file with:

```proto
syntax = "proto3";
package node.v1;
option go_package = "spawnery/gen/node/v1;nodev1";

service NodeService {
  // Node dials the CP and holds this stream open. Node sends NodeMessage; CP sends CPMessage.
  rpc Attach(stream NodeMessage) returns (stream CPMessage);
}

// SUSPENDED (6) added; existing values unchanged (keeps wire + Go switch sites stable).
enum SpawnPhase { SPAWN_PHASE_UNSPECIFIED = 0; STARTING = 1; ACTIVE = 2; STOPPING = 3; STOPPED = 4; ERROR = 5; SUSPENDED = 6; }

message NodeMessage {
  oneof msg {
    Register        register         = 1;
    Heartbeat       heartbeat        = 2;
    SpawnStatus     status           = 3;
    Frame           frame            = 4;   // agent -> client bytes
    SuspendComplete suspend_complete = 5;   // node -> CP: per-mount suspend markers
  }
}
message CPMessage {
  oneof msg {
    StartSpawn   start   = 1;
    StopSpawn    stop    = 2;
    SessionOpen  open    = 3;
    SessionClose close   = 4;
    Frame        frame   = 5;             // client -> agent bytes
    Suspend      suspend = 6;             // CP -> node: persist + tear down
  }
}

// Per-mount backend binding chosen at create/resume (CP -> node).
message MountBinding { string name = 1; string backend_uri = 2; }
// Per-mount suspend marker reported back after a clean suspend (node -> CP).
message MountMarker  { string name = 1; string marker = 2; }
// One entry of a node's running-container inventory (node -> CP on Register/Heartbeat).
message RunningSpawn { string spawn_id = 1; uint64 generation = 2; SpawnPhase phase = 3; }

message Register    { string node_id = 1; uint32 max_spawns = 2; repeated string agent_images = 3; repeated RunningSpawn running = 4; }
message Heartbeat   { uint32 active_spawns = 1; uint32 free_slots = 2; uint32 cpu_pct = 3; uint32 gpu_pct = 4; repeated RunningSpawn running = 5; }
message SpawnStatus { string spawn_id = 1; SpawnPhase phase = 2; string detail = 3; uint64 generation = 4; }
message StartSpawn  { string spawn_id = 1; string app_ref = 2; string data_ref = 3; string model = 4; uint64 generation = 5; repeated MountBinding mounts = 6; }
message StopSpawn   { string spawn_id = 1; uint64 generation = 2; }
message Suspend     { string spawn_id = 1; uint64 generation = 2; }
message SuspendComplete { string spawn_id = 1; uint64 generation = 2; repeated MountMarker markers = 3; }
message SessionOpen  { string spawn_id = 1; uint64 generation = 2; }
message SessionClose { string spawn_id = 1; uint64 generation = 2; }
message Frame       { string spawn_id = 1; bytes data = 2; }
```

- [ ] **Step 4: Regenerate**

```bash
export PATH="$PATH:$(go env GOPATH)/bin"
make gen
```
Expected: `buf generate && touch .make/gen`, no errors; `git status` shows `gen/node/v1/node.pb.go` modified.

- [ ] **Step 5: Run the test — verify it passes (green)**

```bash
go test ./internal/contract/ -run TestNodeContractFields -v
```
Expected: `PASS`.

- [ ] **Step 6: Verify nothing else broke**

```bash
go build ./... && go test ./internal/cp/... ./internal/node/... 2>&1 | tail -20
```
Expected: builds clean; existing CP/node tests pass (the new node fields default to zero; existing `switch m := msg.Msg.(type)` sites in `internal/cp/server.go` and `internal/node/attach.go` ignore the new variants).

- [ ] **Step 7: Commit**

```bash
git add proto/node/v1/node.proto gen/node/v1/node.pb.go internal/contract/wire_test.go .make/gen
git commit --no-verify -m "feat(sp-mqj): node.v1 lifecycle wire — generation, Suspend, SUSPENDED, mounts, inventory"
```

---

### Task 2: cp.v1 lifecycle RPCs + keep the handler compiling

**Files:**
- Modify: `proto/cp/v1/cp.proto`
- Regenerate: `gen/cp/v1/cp.pb.go`, `gen/cp/v1/cpv1connect/cp.connect.go`
- Modify: `internal/cp/server.go` (embed the unimplemented handler + compile-time assert)
- Modify: `internal/contract/wire_test.go` (add the cp half)

- [ ] **Step 1: Add the failing cp-half of the contract test**

Append to `internal/contract/wire_test.go` (and add `cpv1 "spawnery/gen/cp/v1"` to its imports):

```go
func TestCPContractSurface(t *testing.T) {
	// per-mount backend choices now ride the CreateSpawn request
	_ = &cpv1.CreateSpawnRequest{AppId: "a", Model: "m",
		Mounts: []*cpv1.MountBinding{{Name: "main", BackendUri: "managed:repo"}}}
	// new lifecycle RPC request/response messages exist
	_ = &cpv1.ListSpawnsRequest{}
	_ = &cpv1.ListSpawnsResponse{Spawns: []*cpv1.SpawnSummary{
		{SpawnId: "sp1", Status: cpv1.SpawnStatus_SPAWN_STATUS_SUSPENDED}}}
	_ = &cpv1.SuspendSpawnRequest{SpawnId: "sp1"}
	_ = &cpv1.ResumeSpawnRequest{SpawnId: "sp1"}
	_ = &cpv1.RecreateSpawnRequest{SpawnId: "sp1"}
	_ = &cpv1.DeleteSpawnRequest{SpawnId: "sp1", DestroyData: true}
}
```

- [ ] **Step 2: Run it — verify it fails to compile (red)**

```bash
go test ./internal/contract/ 2>&1 | head -20
```
Expected: `undefined: cpv1.MountBinding`, `undefined: cpv1.ListSpawnsRequest`, `cpv1.SpawnStatus_SPAWN_STATUS_SUSPENDED undefined`, etc.

- [ ] **Step 3: Edit `proto/cp/v1/cp.proto`**

Replace the entire file with:

```proto
syntax = "proto3";
package cp.v1;
option go_package = "spawnery/gen/cp/v1;cpv1";

service SpawnService {
  rpc CreateSpawn(CreateSpawnRequest) returns (CreateSpawnResponse);
  rpc ListSpawns(ListSpawnsRequest) returns (ListSpawnsResponse);
  rpc Session(stream Frame) returns (stream Frame);
  rpc SuspendSpawn(SuspendSpawnRequest) returns (SuspendSpawnResponse);
  rpc ResumeSpawn(ResumeSpawnRequest) returns (ResumeSpawnResponse);
  rpc RecreateSpawn(RecreateSpawnRequest) returns (RecreateSpawnResponse);
  rpc DeleteSpawn(DeleteSpawnRequest) returns (DeleteSpawnResponse);
  rpc StopSpawn(StopSpawnRequest) returns (StopSpawnResponse);   // legacy cleanup path; kept (used by spawnctl + e2e)
}

// The durable spawn lifecycle, surfaced to clients (mirrors the DAO status machine).
enum SpawnStatus {
  SPAWN_STATUS_UNSPECIFIED = 0;
  SPAWN_STATUS_STARTING    = 1;
  SPAWN_STATUS_ACTIVE      = 2;
  SPAWN_STATUS_SUSPENDING  = 3;
  SPAWN_STATUS_SUSPENDED   = 4;
  SPAWN_STATUS_UNREACHABLE = 5;
  SPAWN_STATUS_ERROR       = 6;
  SPAWN_STATUS_DELETED     = 7;
}

// Per-mount backend choice the user picks at spawn time (validated against the app version's
// declared mounts CP-side).
message MountBinding { string name = 1; string backend_uri = 2; }

message CreateSpawnRequest  { string app_id = 1; string model = 2; repeated MountBinding mounts = 3; }
message CreateSpawnResponse { string spawn_id = 1; }

message SpawnSummary {
  string spawn_id     = 1;
  string app_id       = 2;
  string app_version  = 3;
  string model        = 4;
  SpawnStatus status  = 5;
  int64  created_at   = 6;
  int64  last_used_at = 7;
}
message ListSpawnsRequest  {}
message ListSpawnsResponse { repeated SpawnSummary spawns = 1; }

message SuspendSpawnRequest   { string spawn_id = 1; }
message SuspendSpawnResponse  {}
message ResumeSpawnRequest    { string spawn_id = 1; }
message ResumeSpawnResponse   {}
message RecreateSpawnRequest  { string spawn_id = 1; }
message RecreateSpawnResponse {}
message DeleteSpawnRequest    { string spawn_id = 1; bool destroy_data = 2; }
message DeleteSpawnResponse   {}

message Frame { string spawn_id = 1; bytes data = 2; }

message StopSpawnRequest  { string spawn_id = 1; }
message StopSpawnResponse {}
```

- [ ] **Step 4: Regenerate**

```bash
export PATH="$PATH:$(go env GOPATH)/bin"
make gen
```
Expected: no errors; `gen/cp/v1/cp.pb.go` and `gen/cp/v1/cpv1connect/cp.connect.go` modified. The connect interface `SpawnServiceHandler` now declares 8 methods.

- [ ] **Step 5: Verify the build BREAKS at the handler (red, the expected compile gap)**

```bash
go build ./... 2>&1 | head
```
Expected: `*cp.Server` no longer implements `cpv1connect.SpawnServiceHandler` (missing `ListSpawns`, `SuspendSpawn`, `ResumeSpawn`, `RecreateSpawn`, `DeleteSpawn`) — surfaced at `cmd/cp/main.go`'s `NewSpawnServiceHandler(srv ...)`.

- [ ] **Step 6: Embed the generated unimplemented handler in `*cp.Server`**

In `internal/cp/server.go`, add the import (it already imports `cpv1 "spawnery/gen/cp/v1"`; add the connect package) and embed the unimplemented struct. The `Server` struct today is:

```go
type Server struct {
	reg   *registry.Registry
	rt    *router.Router
	sched *scheduler.Scheduler
	apps  *apps.Resolver
	tel   telemetry.Sink
}
```

Change it to embed the generated stub and add a compile-time assertion just below the struct:

```go
type Server struct {
	cpv1connect.UnimplementedSpawnServiceHandler // new RPCs default to CodeUnimplemented until sp-pc4
	reg   *registry.Registry
	rt    *router.Router
	sched *scheduler.Scheduler
	apps  *apps.Resolver
	tel   telemetry.Sink
}

// Server must satisfy the (now larger) connect handler interface; the 5 new lifecycle RPCs are
// served by the embedded Unimplemented handler until sp-pc4 overrides them.
var _ cpv1connect.SpawnServiceHandler = (*Server)(nil)
```

Add the import to the existing import block in `internal/cp/server.go`:

```go
	"spawnery/gen/cp/v1/cpv1connect"
```

(The explicit `CreateSpawn`/`Session`/`StopSpawn` methods on `*Server` still win over the embedded defaults — outer methods shadow promoted ones.)

- [ ] **Step 7: Run the contract test + full build (green)**

```bash
export PATH="$PATH:$(go env GOPATH)/bin"
go build ./... && go test ./internal/contract/ -v
```
Expected: builds clean; both `TestNodeContractFields` and `TestCPContractSurface` `PASS`.

- [ ] **Step 8: Commit**

```bash
git add proto/cp/v1/cp.proto gen/cp/v1/ internal/cp/server.go internal/contract/wire_test.go .make/gen
git commit --no-verify -m "feat(sp-mqj): cp.v1 lifecycle RPCs + CreateSpawn mounts; stub new handlers"
```

---

### Task 3: regression gate + close out

**Files:** none changed — this task is verification + bookkeeping.

- [ ] **Step 1: Full hermetic test + vet pass**

```bash
export PATH="$PATH:$(go env GOPATH)/bin"
go vet ./... && SKIP_DOCKER=1 go test ./... 2>&1 | tail -30
```
Expected: `go vet` clean; all hermetic Go tests pass (Docker e2e skips via `SKIP_DOCKER=1`). The existing `internal/cp/server_test.go`, `scheduler_test.go`, `router_test.go` still pass — none of them assert the new fields, and the new variants are additive.

- [ ] **Step 2: buf lint (informational) + confirm additions are non-breaking**

```bash
export PATH="$PATH:$(go env GOPATH)/bin"
buf lint 2>&1 | tail -20 || true   # pre-existing SpawnPhase values are unprefixed; not introduced here
buf build  2>&1 | tail -5          # must succeed: proto compiles
```
Expected: `buf build` succeeds. `buf lint` may report the pre-existing unprefixed `SpawnPhase` enum values (`STARTING`/`ACTIVE`/…/`SUSPENDED`) — these match the established file style and are not a gate (`make gen` runs `buf generate`, not `buf lint`).

- [ ] **Step 3: Confirm the generated handler surface**

```bash
grep -nE 'ListSpawns|SuspendSpawn|ResumeSpawn|RecreateSpawn|DeleteSpawn' gen/cp/v1/cpv1connect/cp.connect.go | head
```
Expected: each new RPC appears in the `SpawnServiceHandler` interface, the client, and `UnimplementedSpawnServiceHandler`.

- [ ] **Step 4: Close the bead**

```bash
bd close sp-mqj --reason="Lifecycle contracts landed: node.v1 (generation, Suspend, SUSPENDED, mounts, RunningSpawn inventory, SuspendComplete) + cp.v1 (ListSpawns/SuspendSpawn/ResumeSpawn/RecreateSpawn/DeleteSpawn + CreateSpawn mounts). Regenerated; tree compiles; new RPCs stub to Unimplemented pending sp-pc4."
bd ready   # confirm sp-pc4 is now unblocked
```

---

## Self-Review

**Spec coverage** (against state-dao §10 + lifecycle §9):
- `generation` on StartSpawn/StopSpawn/Suspend/SessionOpen/SessionClose + SpawnStatus — Task 1 ✓
- `Suspend` CPMessage — Task 1 ✓
- `SUSPENDED` SpawnPhase — Task 1 ✓
- node→CP suspend-complete with per-mount markers (`SuspendComplete` + `MountMarker`) — Task 1 ✓
- StartSpawn repeated mount field (`MountBinding`) — Task 1 ✓
- `RunningSpawn` inventory on Register + Heartbeat — Task 1 ✓
- cp.v1 RPCs ListSpawns/SuspendSpawn/ResumeSpawn/RecreateSpawn/DeleteSpawn — Task 2 ✓
- CreateSpawn per-mount bindings — Task 2 ✓
- "make it compile, stub behavior" (no lifecycle logic) — embedded `UnimplementedSpawnServiceHandler`, Task 2 ✓
- Out of scope confirmed: `proto/spawn/v1` (legacy direct path) untouched; scheduler `(spawn_id, gen)` rendezvous + ownership-via-DB + reconciliation are `sp-pc4`.

**Placeholder scan:** none — every proto block and Go edit is complete and literal.

**Type consistency:** Go names match protoc-gen-go camelCasing used in the test: `backend_uri`→`BackendUri`, `destroy_data`→`DestroyData`, `suspend_complete`→oneof wrapper `NodeMessage_SuspendComplete`/field `SuspendComplete`, `running`→`Running`, enum `SPAWN_STATUS_SUSPENDED`→`cpv1.SpawnStatus_SPAWN_STATUS_SUSPENDED`, `SUSPENDED`→`nodev1.SpawnPhase_SUSPENDED`. The contract test in Task 1/2 uses exactly these and is the executable check.
