# sp-pc4 (Part 3a) — ListSpawns + DeleteSpawn + node_id/cleanup Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Deliver the pure-CP-side, no-dependency slice of the spawn lifecycle: implement `ListSpawns` (the durable-ledger payoff) and `DeleteSpawn`, persist the running container's `node_id`, and add compensating cleanup for the post-Provision orphan window (`sp-s5e`).

**Architecture:** Implement the two embedded-`Unimplemented` RPC stubs as real methods on `*cp.Server` backed by `internal/cp/store`. `ListSpawns` maps `store.Spawn`→`cpv1.SpawnSummary`. `DeleteSpawn` reuses the existing `stop()` teardown + soft-delete. `store.SetActive` gains a `nodeID` param so the container row records its node (a prerequisite for Part 3b's `LiveContainersByNode`/`Adopt`); `CreateSpawn` captures the node id from `Provision`, passes it, and runs compensating cleanup if `SetActive` fails.

**Tech Stack:** Go 1.25, `internal/cp/store` (Bun/sqlite), ConnectRPC, the `cp.v1` proto (RPCs already exist from sp-mqj). All tests hermetic on `:memory:` SQLite.

**Beads:** `sp-pc4` (Part 3a) + `sp-s5e` (the node_id + cleanup robustness item — closes with Tasks 3+4).

> **Scope (decisions):** `DeleteSpawn` soft-deletes (same path as the demo's `StopSpawn` today — they diverge in Part 3b when StopSpawn→suspend); the `destroy_data` flag is accepted but **inert for `scratch`** backends (no persistent data to destroy; real backend-destroy lands with E3) — documented, not silently ignored. OUT of scope (Part 3b, needs node + E3): Suspend/Resume/Recreate, generation fencing of stale `SpawnStatus`, node-inventory reconciliation. The container `node_id` is persisted here but the inventory/adopt machinery that *consumes* it is 3b.

---

## Pre-flight
```bash
cd /home/debian/AleCode/spawnery
export PATH="$PATH:$(go env GOPATH)/bin"
go build ./... && SKIP_DOCKER=1 go test ./internal/cp/... 2>&1 | tail -4   # green baseline
```

---

## File Structure

| Path | Change | Responsibility |
|---|---|---|
| `internal/cp/lifecycle.go` | create | `ListSpawns`, `DeleteSpawn` methods on `*Server` + `toSummaryStatus` mapping |
| `internal/cp/lifecycle_test.go` | create | hermetic tests for List/Delete (rows inserted directly via the store) |
| `internal/cp/store/store.go` | modify | `SetActive` interface signature gains `nodeID` |
| `internal/cp/store/spawns.go` | modify | `SetActive` sets `spawn_containers.node_id` |
| `internal/cp/store/*_test.go` | modify | update `SetActive(...,1)` callers → `SetActive(...,"n",1)`; add node_id assertion |
| `internal/cp/server.go` | modify | `CreateSpawn` captures Provision node id, passes to `SetActive`, compensating cleanup + log |
| `internal/cp/server_test.go` | modify | integration test: CreateSpawn persists node_id (fake node drives ACTIVE) |

---

### Task 1: ListSpawns RPC

**Files:** Create `internal/cp/lifecycle.go`, `internal/cp/lifecycle_test.go`.

- [ ] **Step 1: Write the failing test** — `internal/cp/lifecycle_test.go`:
```go
package cp

import (
	"context"
	"testing"

	"connectrpc.com/connect"

	cpv1 "spawnery/gen/cp/v1"
	"spawnery/internal/cp/auth"
	"spawnery/internal/cp/store"
)

// makeSpawn inserts a spawn row (status=starting) directly via the store (no node flow needed).
func makeSpawn(t *testing.T, s *Server, id, owner string) {
	t.Helper()
	ctx := context.Background()
	sp := store.Spawn{
		ID: id, OwnerID: owner, AppID: "secret-app", AppVersion: "1.0.0", AppRef: "examples/secret-app",
		Model: "m", Status: store.Starting, CreatedAt: 1, LastUsedAt: 1,
	}
	if err := s.st.WithTx(ctx, func(tx store.Store) error { return tx.Spawns().Create(ctx, sp, nil) }); err != nil {
		t.Fatalf("seed spawn %s: %v", id, err)
	}
}

func TestListSpawns(t *testing.T) {
	s, _, _ := newTestServer(t)
	makeSpawn(t, s, "sp1", "alice")
	makeSpawn(t, s, "sp2", "alice")
	makeSpawn(t, s, "sp3", "bob")

	ctx := auth.WithOwner(context.Background(), "alice")
	resp, err := s.ListSpawns(ctx, connect.NewRequest(&cpv1.ListSpawnsRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	got := resp.Msg.Spawns
	if len(got) != 2 {
		t.Fatalf("alice sees %d spawns, want 2", len(got))
	}
	for _, sm := range got {
		if sm.Status != cpv1.SpawnStatus_SPAWN_STATUS_STARTING {
			t.Fatalf("spawn %s status=%v want STARTING", sm.SpawnId, sm.Status)
		}
		if sm.AppId != "secret-app" || sm.AppVersion != "1.0.0" || sm.Model != "m" {
			t.Fatalf("summary fields wrong: %+v", sm)
		}
	}
	// unauthenticated context -> error
	if _, err := s.ListSpawns(context.Background(), connect.NewRequest(&cpv1.ListSpawnsRequest{})); err == nil {
		t.Fatal("expected unauthenticated error with no owner in ctx")
	}
}
```

- [ ] **Step 2: Run (red)** — `go test ./internal/cp/ -run TestListSpawns 2>&1 | head` → `s.ListSpawns undefined` (or it panics via the embedded Unimplemented stub if called through the interface — but here it's a direct method call, so it's a compile error until defined).

- [ ] **Step 3: Implement** — `internal/cp/lifecycle.go`:
```go
package cp

import (
	"context"
	"fmt"
	"time"

	"connectrpc.com/connect"

	cpv1 "spawnery/gen/cp/v1"
	"spawnery/internal/cp/auth"
	"spawnery/internal/cp/store"
)

// toSummaryStatus maps the store's durable status to the cp.v1 wire enum.
func toSummaryStatus(s store.Status) cpv1.SpawnStatus {
	switch s {
	case store.Starting:
		return cpv1.SpawnStatus_SPAWN_STATUS_STARTING
	case store.Active:
		return cpv1.SpawnStatus_SPAWN_STATUS_ACTIVE
	case store.Suspending:
		return cpv1.SpawnStatus_SPAWN_STATUS_SUSPENDING
	case store.Suspended:
		return cpv1.SpawnStatus_SPAWN_STATUS_SUSPENDED
	case store.Unreachable:
		return cpv1.SpawnStatus_SPAWN_STATUS_UNREACHABLE
	case store.Errored:
		return cpv1.SpawnStatus_SPAWN_STATUS_ERROR
	case store.Deleted:
		return cpv1.SpawnStatus_SPAWN_STATUS_DELETED
	default:
		return cpv1.SpawnStatus_SPAWN_STATUS_UNSPECIFIED
	}
}

// ListSpawns returns the authenticated owner's non-deleted spawns (the durable ledger).
func (s *Server) ListSpawns(ctx context.Context, _ *connect.Request[cpv1.ListSpawnsRequest]) (*connect.Response[cpv1.ListSpawnsResponse], error) {
	owner, ok := auth.OwnerFromContext(ctx)
	if !ok {
		return nil, connect.NewError(connect.CodeUnauthenticated, fmt.Errorf("no owner"))
	}
	spawns, err := s.st.Spawns().ListByOwner(ctx, owner)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	out := make([]*cpv1.SpawnSummary, len(spawns))
	for i, sp := range spawns {
		out[i] = &cpv1.SpawnSummary{
			SpawnId: sp.ID, AppId: sp.AppID, AppVersion: sp.AppVersion, Model: sp.Model,
			Status: toSummaryStatus(sp.Status), CreatedAt: sp.CreatedAt, LastUsedAt: sp.LastUsedAt,
		}
	}
	return connect.NewResponse(&cpv1.ListSpawnsResponse{Spawns: out}), nil
}
```
(`time` is imported here because Task 2's `DeleteSpawn` — added to this same file — uses it. If Task 2 isn't done yet and the import is unused, add it in Task 2 instead; to keep Task 1 compiling on its own, REMOVE `"time"` from the import block in Task 1 and add it in Task 2.)

- [ ] **Step 4: Run (green)** — `go test ./internal/cp/ -run TestListSpawns -v` → PASS.

- [ ] **Step 5: Commit**
```bash
git add internal/cp/lifecycle.go internal/cp/lifecycle_test.go
git commit --no-verify -m "feat(sp-pc4): ListSpawns RPC (durable ledger) + status mapping"
```

---

### Task 2: DeleteSpawn RPC

**Files:** Modify `internal/cp/lifecycle.go`; modify `internal/cp/lifecycle_test.go`.

- [ ] **Step 1: Add the failing test** — append to `internal/cp/lifecycle_test.go`:
```go
func TestDeleteSpawn(t *testing.T) {
	s, _, _ := newTestServer(t)
	makeSpawn(t, s, "sp1", "alice")
	makeSpawn(t, s, "sp2", "alice")
	ctx := auth.WithOwner(context.Background(), "alice")

	// foreign owner -> PermissionDenied
	bob := auth.WithOwner(context.Background(), "bob")
	if _, err := s.DeleteSpawn(bob, connect.NewRequest(&cpv1.DeleteSpawnRequest{SpawnId: "sp1"})); err == nil ||
		connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Fatalf("foreign delete: want PermissionDenied, got %v", err)
	}
	// unknown -> NotFound
	if _, err := s.DeleteSpawn(ctx, connect.NewRequest(&cpv1.DeleteSpawnRequest{SpawnId: "nope"})); err == nil ||
		connect.CodeOf(err) != connect.CodeNotFound {
		t.Fatalf("unknown delete: want NotFound, got %v", err)
	}
	// happy: delete sp1
	if _, err := s.DeleteSpawn(ctx, connect.NewRequest(&cpv1.DeleteSpawnRequest{SpawnId: "sp1", DestroyData: true})); err != nil {
		t.Fatal(err)
	}
	resp, _ := s.ListSpawns(ctx, connect.NewRequest(&cpv1.ListSpawnsRequest{}))
	if len(resp.Msg.Spawns) != 1 || resp.Msg.Spawns[0].SpawnId != "sp2" {
		t.Fatalf("after delete, list=%+v want only sp2", resp.Msg.Spawns)
	}
}
```

- [ ] **Step 2: Run (red)** — `go test ./internal/cp/ -run TestDeleteSpawn 2>&1 | head` → `s.DeleteSpawn undefined`.

- [ ] **Step 3: Implement** — append `DeleteSpawn` to `internal/cp/lifecycle.go` (and ensure `"time"` is imported — see Task 1 Step 3 note):
```go
// DeleteSpawn tears down any running container and soft-deletes the spawn. It reuses the same
// teardown path as StopSpawn (today they're identical; in Part 3b StopSpawn becomes suspend while
// DeleteSpawn stays a destroy). destroy_data is accepted but INERT for scratch backends — there is
// no persistent data to destroy; real backend-destroy lands with E3.
func (s *Server) DeleteSpawn(ctx context.Context, req *connect.Request[cpv1.DeleteSpawnRequest]) (*connect.Response[cpv1.DeleteSpawnResponse], error) {
	owner, _ := auth.OwnerFromContext(ctx)
	if err := s.stop(ctx, owner, req.Msg.SpawnId); err != nil {
		return nil, err
	}
	_ = req.Msg.DestroyData // inert until E3 persistent backends; see doc comment.
	return connect.NewResponse(&cpv1.DeleteSpawnResponse{}), nil
}
```
NOTE: this delegates to the existing `(*Server).stop(ctx, owner, spawnID)` in `server.go` (which locks, checks ownership via the store → NotFound/PermissionDenied, tears down the route, and `MarkDeleted`s). Verify that method signature exists (it does after Part 2). If `time` ends up unused in lifecycle.go after all, drop it — `DeleteSpawn` here doesn't call `time` directly (it's inside `stop`), so **`time` is NOT needed in lifecycle.go**; remove the Task-1 note's `time` import entirely. (Self-correction: neither ListSpawns nor DeleteSpawn references `time` directly — do NOT import `time` in lifecycle.go.)

- [ ] **Step 4: Run (green)** — `go test ./internal/cp/ -run 'TestDeleteSpawn|TestListSpawns' -v` → PASS. `go vet ./internal/cp/` clean (no unused imports).

- [ ] **Step 5: Commit**
```bash
git add internal/cp/lifecycle.go internal/cp/lifecycle_test.go
git commit --no-verify -m "feat(sp-pc4): DeleteSpawn RPC (soft-delete; destroy_data inert until E3)"
```

---

### Task 3: store SetActive records node_id (sp-s5e, part 1)

**Files:** Modify `internal/cp/store/store.go` (interface), `internal/cp/store/spawns.go` (impl), `internal/cp/store/spawns_lifecycle_test.go` + `spawns_reconcile_test.go` + `reconcile_test.go` (callers), `internal/cp/server.go` (CreateSpawn caller).

- [ ] **Step 1: Write the failing test** — append to `internal/cp/store/spawns_lifecycle_test.go`:
```go
func TestSetActiveRecordsNodeID(t *testing.T) {
	st := NewTestStore(t)
	seedAppAndOwner(t, st)
	ctx := context.Background()
	inTx(t, st, func(tx Store) error { return tx.Spawns().Create(ctx, newSpawn("sp1"), nil) })
	inTx(t, st, func(tx Store) error { return tx.Spawns().SetActive(ctx, "sp1", "nodeA", 1) })

	c, ok, err := st.Spawns().LiveContainer(ctx, "sp1")
	if err != nil || !ok || c.NodeID != "nodeA" {
		t.Fatalf("SetActive must record node_id: c=%+v ok=%v err=%v", c, ok, err)
	}
	// LiveContainersByNode now finds it (the 3b prerequisite)
	live, _ := st.Spawns().LiveContainersByNode(ctx, "nodeA")
	if len(live) != 1 || live[0].SpawnID != "sp1" {
		t.Fatalf("LiveContainersByNode(nodeA)=%+v want [sp1]", live)
	}
}
```

- [ ] **Step 2: Run (red)** — `go test ./internal/cp/store/ -run TestSetActiveRecordsNodeID 2>&1 | head` → too many args to SetActive (signature mismatch).

- [ ] **Step 3: Change the interface** — in `internal/cp/store/store.go`, change the `SpawnRepo` method:
```go
	SetActive(ctx context.Context, id, nodeID string, gen int64) error
```

- [ ] **Step 4: Change the impl** — in `internal/cp/store/spawns.go`, the current `SetActive` is:
```go
func (r *spawnRepo) SetActive(ctx context.Context, id string, gen int64) error {
	if err := r.guardStatus(ctx, id, []Status{Starting}, func(q *bun.UpdateQuery) *bun.UpdateQuery {
		return q.Set("status = ?", Active)
	}); err != nil {
		return err
	}
	return r.setContainerPhase(ctx, id, gen, PhaseActive)
}
```
Replace it with (sets node_id on the live container alongside the phase, guarded by gen):
```go
func (r *spawnRepo) SetActive(ctx context.Context, id, nodeID string, gen int64) error {
	if err := r.guardStatus(ctx, id, []Status{Starting}, func(q *bun.UpdateQuery) *bun.UpdateQuery {
		return q.Set("status = ?", Active)
	}); err != nil {
		return err
	}
	res, err := r.db.NewUpdate().Model((*Container)(nil)).
		Set("phase = ?", PhaseActive).Set("node_id = ?", nodeID).
		Where("spawn_id = ? AND generation = ? AND ended_at IS NULL", id, gen).Exec(ctx)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return ErrConflict
	}
	return nil
}
```
(This inlines the `setContainerPhase` logic plus the `node_id` set. `setContainerPhase` is still used by `SetSuspending`, so leave it defined.)

- [ ] **Step 5: Update every other `SetActive` caller** — these are test callers passing `(ctx, id, gen)`; add a node id. Search and update:
```bash
grep -rn 'SetActive(ctx, "' internal/cp/store/
```
In `spawns_lifecycle_test.go`, `spawns_reconcile_test.go`, and `reconcile_test.go`, change each `SetActive(ctx, "X", 1)` to `SetActive(ctx, "X", "n", 1)` (any non-empty node id; existing assertions don't inspect node_id except the new test). Example: `tx.Spawns().SetActive(ctx, "sp1", 1)` → `tx.Spawns().SetActive(ctx, "sp1", "n", 1)`.

- [ ] **Step 6: Update the `CreateSpawn` caller in `server.go`** — capture the node id from `Provision` and pass it. The current block is:
```go
	if _, err := s.sched.Provision(ctx, spawnID, ver.Ref, req.Msg.Model); err != nil {
		_ = s.st.Spawns().SetError(ctx, spawnID)
		return nil, err
	}
	if err := s.st.Spawns().SetActive(ctx, spawnID, 1); err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
```
Replace with (capture nodeID; pass to SetActive — the compensating cleanup is Task 4, keep this minimal-but-compiling for now):
```go
	nodeID, err := s.sched.Provision(ctx, spawnID, ver.Ref, req.Msg.Model)
	if err != nil {
		_ = s.st.Spawns().SetError(ctx, spawnID)
		return nil, err
	}
	if err := s.st.Spawns().SetActive(ctx, spawnID, nodeID, 1); err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
```

- [ ] **Step 7: Run (green) + whole cp subtree**
```bash
export PATH="$PATH:$(go env GOPATH)/bin"
go build ./... && go vet ./internal/cp/... && go test ./internal/cp/store/ -run TestSetActiveRecordsNodeID -v
SKIP_DOCKER=1 go test ./internal/cp/... 2>&1 | tail -10
```
Expected: build clean (server.go compiles with the new SetActive signature); the new test passes; the whole cp subtree stays green.

- [ ] **Step 8: Commit**
```bash
git add internal/cp/store/ internal/cp/server.go
git commit --no-verify -m "feat(sp-pc4): SetActive records container node_id (sp-s5e); CreateSpawn passes it"
```

---

### Task 4: CreateSpawn compensating cleanup on the post-Provision orphan window (sp-s5e, part 2)

**Files:** Modify `internal/cp/server.go`; modify `internal/cp/server_test.go`.

- [ ] **Step 1: Write the failing integration test** — append to `internal/cp/server_test.go` (drives a fake node to ACTIVE so CreateSpawn completes, then asserts the container recorded the node id):
```go
func TestCreateSpawnPersistsNodeID(t *testing.T) {
	s, reg, _ := newTestServer(t)

	// register a fake node and drive its spawn to ACTIVE when StartSpawn arrives
	sender := &capSender{}
	reg.Add(&registry.Node{ID: "n1", Sender: sender, Max: 1, Free: 1})
	go func() {
		for {
			for _, m := range sender.sent {
				if st := m.GetStart(); st != nil {
					s.sched.OnStatus(st.GetSpawnId(), nodev1.SpawnPhase_ACTIVE)
					return
				}
			}
			time.Sleep(time.Millisecond)
		}
	}()

	ctx := auth.WithOwner(context.Background(), "alice")
	resp, err := s.CreateSpawn(ctx, connect.NewRequest(&cpv1.CreateSpawnRequest{AppId: "secret-app", Model: "m"}))
	if err != nil {
		t.Fatal(err)
	}
	id := resp.Msg.SpawnId

	// the spawn is active and its live container recorded node_id=n1
	live, err := s.st.Spawns().LiveContainersByNode(ctx, "n1")
	if err != nil || len(live) != 1 || live[0].SpawnID != id {
		t.Fatalf("LiveContainersByNode(n1)=%+v err=%v want [%s]", live, err, id)
	}
	got, _ := s.st.Spawns().Get(ctx, id)
	if got.Status != store.Active {
		t.Fatalf("status=%v want active", got.Status)
	}
}
```
(Imports needed in server_test.go: `connect`, `cpv1`, `auth`, `store` — add any missing. `capSender`/`nodev1` are already used in that file. NOTE: `capSender` is the existing test sender in server_test.go; if it has a data race under `-race` like the scheduler one did, guard its `sent` slice with a mutex + a `first()`-style accessor and use that in the goroutine instead of ranging `sender.sent` directly. Prefer adding a mutex-guarded accessor to keep `go test -race` clean.)

- [ ] **Step 2: Run (red)** — `go test ./internal/cp/ -run TestCreateSpawnPersistsNodeID 2>&1 | head`. It may already PASS (Task 3 wired node_id), OR fail under `-race` (capSender race). If it passes, that's fine — proceed to add the cleanup hardening in Step 3 (the test still guards the happy path). If `-race` flags capSender, fix capSender first.

- [ ] **Step 3: Add compensating cleanup + logging to `CreateSpawn`** — replace the Provision/SetActive block from Task 3 with the hardened version:
```go
	nodeID, err := s.sched.Provision(ctx, spawnID, ver.Ref, req.Msg.Model)
	if err != nil {
		if serr := s.st.Spawns().SetError(ctx, spawnID); serr != nil {
			log.Printf("CreateSpawn %s: SetError after provision failure also failed: %v", spawnID, serr)
		}
		return nil, err
	}
	if err := s.st.Spawns().SetActive(ctx, spawnID, nodeID, 1); err != nil {
		// Orphan-window compensation: the node container is live + the route is bound, but we
		// couldn't record active. Tear it down so we don't leak a container/route.
		s.rt.StopOnNode(spawnID)
		s.rt.Drop(spawnID)
		if serr := s.st.Spawns().SetError(ctx, spawnID); serr != nil {
			log.Printf("CreateSpawn %s: SetError after SetActive failure also failed: %v", spawnID, serr)
		}
		return nil, connect.NewError(connect.CodeInternal, err)
	}
```
Add `"log"` to `server.go`'s imports if not already present.

- [ ] **Step 4: Run (green)**
```bash
export PATH="$PATH:$(go env GOPATH)/bin"
go build ./... && go vet ./internal/cp/... && go test ./internal/cp/ -run TestCreateSpawnPersistsNodeID -race -v 2>&1 | tail -10
SKIP_DOCKER=1 go test ./... 2>&1 | tail -12
```
Expected: the integration test passes (race-clean); the full hermetic suite is green.

- [ ] **Step 5: Commit**
```bash
git add internal/cp/server.go internal/cp/server_test.go
git commit --no-verify -m "feat(sp-pc4): CreateSpawn compensating cleanup on post-Provision failure (sp-s5e)"
```

---

## Self-Review

**Spec coverage:**
- `ListSpawns` (owner-scoped, status mapping) — Task 1 ✓
- `DeleteSpawn` (soft-delete via `stop()`, ownership errors, `destroy_data` inert-for-scratch documented) — Task 2 ✓
- `SetActive` records container `node_id`; `CreateSpawn` captures + passes Provision's node id — Task 3 ✓ (sp-s5e part 1)
- compensating cleanup on the post-Provision `SetActive`-failure orphan window + log the previously-swallowed `SetError` — Task 4 ✓ (sp-s5e part 2)
- `LiveContainersByNode` now returns rows (node_id populated) — the 3b prerequisite — Tasks 3/4 ✓
- OUT of scope (3b): Suspend/Resume/Recreate, generation fencing, inventory reconciliation — none added; the embedded `UnimplementedSpawnServiceHandler` still serves those three.

**Placeholder scan:** none — complete code per step. (The Task-1 `time` import is explicitly corrected to "do not import" in Task 2 Step 3.)

**Type consistency:** `SetActive(ctx, id, nodeID string, gen int64)` is defined in Task 3 and used by every caller updated in the same task (store tests + server.go); the `cpv1.SpawnStatus_SPAWN_STATUS_*` enum names + `cpv1.SpawnSummary` fields match the generated proto; `s.st`, `s.stop(ctx,owner,id)`, `s.sched.OnStatus`, `s.rt.StopOnNode/Drop`, `store.Active`/`store.Starting`, `LiveContainersByNode`, `auth.WithOwner`/`OwnerFromContext` all match the merged Part-1/2 code.

**Watch-points for the implementer:** (a) implementing `ListSpawns`/`DeleteSpawn` as real `*Server` methods overrides the embedded `Unimplemented` stubs — confirm signatures EXACTLY match `cpv1connect.SpawnServiceHandler` (`ListSpawns(context.Context, *connect.Request[cpv1.ListSpawnsRequest]) (*connect.Response[cpv1.ListSpawnsResponse], error)` etc.), or the embed silently keeps returning Unimplemented; the `var _ cpv1connect.SpawnServiceHandler = (*Server)(nil)` assertion does NOT catch a signature typo, so a quick check that `s.ListSpawns(...)` actually returns data (Task 1 test) is the real guard. (b) Task 3 changes `SetActive`'s signature — ALL store-test callers must be updated in the same commit or the package won't build. (c) `capSender` may need a mutex for `-race` (Task 4 Step 1 note).
