# sp-pc4 (Part 2/N) — CP Rewire onto the Store Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Rewire the control plane to use the `internal/cp/store` package as its durable ledger + ownership authority — replacing the in-memory `apps.Resolver` and the router's `owner` field — while keeping the demo (create → session → stop through the stub agent) working, and adding the per-spawn lock + crude reconciliation scaffolding.

**Architecture:** `Server` gains a `store.Store`. `cmd/cp/main.go` opens the store, seeds owners (from the dev-token map) + the demo app/version/mounts, and boot-reconciles orphans. `CreateSpawn` mints a uuidv7, writes a durable `starting` spawn + live container (gen 1) via the store under a per-spawn lock, provisions the node (scheduler split: `Provision(id,…)`), then `SetActive`/`SetError`. Ownership for `Session`/WebSocket/`StopSpawn` reads from the store (not the router). `StopSpawn` soft-deletes. Node-evict and CP-boot mark orphans `unreachable`.

**Tech Stack:** Go 1.25, the `internal/cp/store` package (Bun/sqlite), ConnectRPC, google/uuid (v1.6.0, `uuid.NewV7`).

**Bead:** `sp-pc4` (Part 2 of N — CP rewire). Depends on Part 1 (merged).

> **Scope decisions baked in (deviations from earlier specs, deliberate for the demo):**
> - **D1 — public app id = `secret-app`** (what clients send today; spawnctl `-app-id` default + web `APP_ID`). The seed registers `apps.id='secret-app'`. The manifest `id` (`spawnery/secret`) canonicalization is deferred to E5. *(Overrides DAO spec §7's "use manifest id".)*
> - **D2 — reconciliation is crude:** CP boot marks `{starting,active}`→`unreachable`; node stream-close marks that node's live spawns `unreachable` (keeping their containers). NO grace window / inventory / adopt (that's Part 3). This is honest (orphaned-after-restart spawns become user-recoverable) but coarse.
> - **D3 — `StartSpawn` wire unchanged:** the node still gets `{SpawnId, AppRef, Model}` and re-parses its manifest; the CP records `spawn_mounts` but does NOT yet populate `StartSpawn.mounts`/`generation` (that + node consumption = Part 3 / sp-gd9). Node behavior is unchanged.
> - **D4 — `StopSpawn` → soft-delete** (`MarkDeleted`): the honest current teardown behavior. Suspend/resume verbs are Part 3.

---

## Pre-flight
```bash
cd /home/debian/AleCode/spawnery
export PATH="$PATH:$(go env GOPATH)/bin"
go build ./... && SKIP_DOCKER=1 go test ./internal/cp/... 2>&1 | tail -5   # green baseline
```

---

## File Structure

| Path | Change | Responsibility |
|---|---|---|
| `internal/cp/lock/lock.go` | create | per-spawn-id keyed mutex |
| `internal/cp/store/reconcile.go` | create | `MarkBootUnreachable` (boot orphan sweep) on `SpawnRepo` |
| `internal/cp/scheduler/scheduler.go` | modify | split `Create`→`Provision(id,…)`; drop owner from `Bind` call |
| `internal/cp/router/router.go` | modify | `Bind` drops `owner`; remove `Owner()` |
| `internal/cp/server.go` | modify | `Server` holds `store.Store`; `NewServer` sig; CreateSpawn/StopSpawn/Session/runNode rewired |
| `internal/cp/ws.go` | modify | ownership via store |
| `cmd/cp/main.go` | modify | open store, seed, boot-reconcile, new `NewServer` args |
| `internal/cp/seed.go` | create | `Seed(ctx, store, tokens, apps)` helper (owners + app/version/mounts) |
| tests | modify/create | `server_test.go`, `scheduler_test.go`, `router_test.go`, `e2e_test.go` updated for the store; new `lock_test.go`, `seed_test.go`, `reconcile_test.go` |

---

### Task 1: Per-spawn keyed lock

**Files:** Create `internal/cp/lock/lock.go`, `internal/cp/lock/lock_test.go`.

- [ ] **Step 1: Write the failing test** — `internal/cp/lock/lock_test.go`:
```go
package lock

import (
	"sync"
	"testing"
)

func TestKeyedSerializesSameKey(t *testing.T) {
	k := New()
	var n int
	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			unlock := k.Lock("sp1")
			defer unlock()
			n++ // protected by the per-key lock; race detector would flag if unprotected
		}()
	}
	wg.Wait()
	if n != 100 {
		t.Fatalf("n=%d want 100", n)
	}
}

func TestKeyedDifferentKeysDontBlock(t *testing.T) {
	k := New()
	u1 := k.Lock("a")
	u2 := k.Lock("b") // must not deadlock on a different key
	u2()
	u1()
}
```

- [ ] **Step 2: Run (red)** — `go test ./internal/cp/lock/ 2>&1 | head` → undefined `New`.

- [ ] **Step 3: Implement** — `internal/cp/lock/lock.go`:
```go
// Package lock provides a per-key mutex so the CP can serialize all operations on one spawn id
// (the {claim -> node command -> await} critical section) without a global lock.
package lock

import "sync"

type Keyed struct {
	mu sync.Mutex
	m  map[string]*sync.Mutex
}

func New() *Keyed { return &Keyed{m: map[string]*sync.Mutex{}} }

// Lock acquires the mutex for key and returns its unlock func. Note: per-key mutexes are not
// reclaimed (bounded by the number of distinct spawn ids — acceptable for the demo).
func (k *Keyed) Lock(key string) func() {
	k.mu.Lock()
	m, ok := k.m[key]
	if !ok {
		m = &sync.Mutex{}
		k.m[key] = m
	}
	k.mu.Unlock()
	m.Lock()
	return m.Unlock
}
```

- [ ] **Step 4: Run (green)** — `go test ./internal/cp/lock/ -race -v` → PASS (race-clean).

- [ ] **Step 5: Commit**
```bash
git add internal/cp/lock/
git commit --no-verify -m "feat(sp-pc4): per-spawn keyed lock"
```

---

### Task 2: Store boot-reconcile method

**Files:** Create `internal/cp/store/reconcile.go`; add `MarkBootUnreachable` to `SpawnRepo` in `internal/cp/store/store.go`; create `internal/cp/store/reconcile_test.go`.

- [ ] **Step 1: Write the failing test** — `internal/cp/store/reconcile_test.go`:
```go
package store

import (
	"context"
	"testing"
)

func TestMarkBootUnreachable(t *testing.T) {
	st := NewTestStore(t)
	seedAppAndOwner(t, st)
	ctx := context.Background()
	// one starting, one active, one suspended
	inTx(t, st, func(tx Store) error { return tx.Spawns().Create(ctx, newSpawn("a"), nil) }) // starting
	inTx(t, st, func(tx Store) error { return tx.Spawns().Create(ctx, newSpawn("b"), nil) })
	inTx(t, st, func(tx Store) error { return tx.Spawns().SetActive(ctx, "b", 1) }) // active
	inTx(t, st, func(tx Store) error { return tx.Spawns().Create(ctx, newSpawn("c"), nil) })
	inTx(t, st, func(tx Store) error { return tx.Spawns().SetActive(ctx, "c", 1) })
	inTx(t, st, func(tx Store) error { return tx.Spawns().SetSuspending(ctx, "c", 1) })
	inTx(t, st, func(tx Store) error { return tx.Spawns().SetSuspended(ctx, "c", 1) }) // suspended

	n, err := st.Spawns().MarkBootUnreachable(ctx)
	if err != nil || n != 2 {
		t.Fatalf("MarkBootUnreachable n=%d err=%v want 2 (a,b)", n, err)
	}
	if s, _ := st.Spawns().Get(ctx, "a"); s.Status != Unreachable {
		t.Fatalf("a status=%v want unreachable", s.Status)
	}
	if s, _ := st.Spawns().Get(ctx, "b"); s.Status != Unreachable {
		t.Fatalf("b status=%v want unreachable", s.Status)
	}
	if s, _ := st.Spawns().Get(ctx, "c"); s.Status != Suspended {
		t.Fatalf("c status=%v want suspended (untouched)", s.Status)
	}
}
```

- [ ] **Step 2: Run (red)** — `go test ./internal/cp/store/ -run TestMarkBootUnreachable 2>&1 | head` → undefined method.

- [ ] **Step 3: Add the interface method** — in `internal/cp/store/store.go`, inside `SpawnRepo`, after `MarkUnreachable(...)` add:
```go
	MarkBootUnreachable(ctx context.Context) (int, error)
```

- [ ] **Step 4: Implement** — `internal/cp/store/reconcile.go`:
```go
package store

import "context"

// MarkBootUnreachable marks every {starting, active} spawn unreachable — the crude CP-restart sweep
// (the CP lost all live routes on restart). Live container rows are KEPT (user recreates). The
// grace-window + node-inventory + adopt protocol is a later (CP-wiring part 3) refinement.
func (r *spawnRepo) MarkBootUnreachable(ctx context.Context) (int, error) {
	res, err := r.db.NewUpdate().Model((*Spawn)(nil)).
		Set("status = ?", Unreachable).
		Where("status IN (?)", []Status{Starting, Active}).
		Exec(ctx)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}
```
NOTE: `Where("status IN (?)", []Status{...})` needs `bun.In(...)`. Use `bun.In([]Status{Starting, Active})` (import `github.com/uptrace/bun`), matching the pattern in `spawns.go`'s `MarkUnreachable`.

- [ ] **Step 5: Run (green)** — `go test ./internal/cp/store/ -run 'TestMarkBootUnreachable|TestModelsBindToTables' -v` → PASS. Also `go build ./internal/cp/store/` (the `var _ SpawnRepo` assertion proves the new method is implemented).

- [ ] **Step 6: Commit**
```bash
git add internal/cp/store/
git commit --no-verify -m "feat(sp-pc4): store MarkBootUnreachable (crude boot orphan sweep)"
```

---

### Task 3: Scheduler split (mint → Provision)

**Files:** Modify `internal/cp/scheduler/scheduler.go`; modify `internal/cp/router/router.go` (Bind drops owner, remove Owner); update `internal/cp/scheduler/scheduler_test.go` + `internal/cp/router/router_test.go`.

- [ ] **Step 1: Change `router.Bind` to drop `owner` + delete `Owner()`** — in `internal/cp/router/router.go`:
  - `route` struct: remove the `owner string` field.
  - `Bind(spawnID, nodeID, owner string, node registry.NodeSender)` → `Bind(spawnID, nodeID string, node registry.NodeSender)` (remove owner; remove the `owner: owner` from the struct literal).
  - DELETE the `Owner(spawnID string) (string, bool)` method entirely.

- [ ] **Step 2: Split `scheduler.Create` into `Provision`** — in `internal/cp/scheduler/scheduler.go`, replace the `Create` method with:
```go
// Provision picks a node, sends StartSpawn for the (already-minted) spawn id, waits for ACTIVE,
// and binds the route. Returns the chosen node id. The caller owns id-minting + persistence.
func (s *Scheduler) Provision(ctx context.Context, id, appRef, model string) (string, error) {
	n := s.reg.Pick()
	if n == nil {
		return "", connect.NewError(connect.CodeResourceExhausted, errors.New("no node with capacity"))
	}
	ch := make(chan nodev1.SpawnPhase, 1)
	s.mu.Lock()
	s.pending[id] = ch
	s.mu.Unlock()
	defer func() { s.mu.Lock(); delete(s.pending, id); s.mu.Unlock() }()

	if err := n.Sender.Send(&nodev1.CPMessage{Msg: &nodev1.CPMessage_Start{Start: &nodev1.StartSpawn{
		SpawnId: id, AppRef: appRef, Model: model,
	}}}); err != nil {
		return "", connect.NewError(connect.CodeUnavailable, err)
	}
	select {
	case ph := <-ch:
		if ph != nodev1.SpawnPhase_ACTIVE {
			return "", connect.NewError(connect.CodeInternal, errors.New("spawn failed to start"))
		}
		s.rt.Bind(id, n.ID, n.Sender)
		return n.ID, nil
	case <-time.After(s.timeout):
		return "", connect.NewError(connect.CodeDeadlineExceeded, errors.New("spawn start timed out"))
	case <-ctx.Done():
		return "", ctx.Err()
	}
}
```
(Remove the now-unused `uuid` import from scheduler.go if `Create` was its only user.)

- [ ] **Step 3: Update `scheduler_test.go` + `router_test.go`** to the new signatures. In `router_test.go`, replace any `rt.Bind("sp1","n1","alice",sender)` with `rt.Bind("sp1","n1",sender)` and delete any assertions calling `rt.Owner(...)`. In `scheduler_test.go`, if a test calls `sched.Create(...)`, change it to mint an id and call `sched.Provision(ctx, id, appRef, model)` and assert the returned node id (read the existing test first; adapt its assertions — the behavior is the same minus owner).

- [ ] **Step 4: Run** — `go build ./internal/cp/... 2>&1 | head` will FAIL in `server.go`/`server_test.go`/`ws.go` (they call `rt.Owner`/`sched.Create`/`rt.Bind` with owner) — that's expected; those are fixed in Tasks 4-6. To check THIS task in isolation:
```bash
go test ./internal/cp/router/ ./internal/cp/scheduler/ -v 2>&1 | tail -15
```
Expected: router + scheduler packages build and their tests pass.

- [ ] **Step 5: Commit**
```bash
git add internal/cp/router/ internal/cp/scheduler/
git commit --no-verify -m "feat(sp-pc4): scheduler Provision(id) split; router Bind drops owner"
```

---

### Task 4: Seeding helper

**Files:** Create `internal/cp/seed.go`, `internal/cp/seed_test.go`.

- [ ] **Step 1: Write the failing test** — `internal/cp/seed_test.go`:
```go
package cp

import (
	"context"
	"testing"

	"spawnery/internal/cp/store"
)

func TestSeed(t *testing.T) {
	st := store.NewTestStore(t)
	ctx := context.Background()
	tokens := map[string]string{"dev-token": "dev", "alice-token": "alice"}
	apps := []AppSeed{{ID: "secret-app", Ref: "examples/secret-app", Version: "1.0.0", Mounts: []string{"main"}}}
	if err := Seed(ctx, st, tokens, apps); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Owners().Get(ctx, "dev"); err != nil {
		t.Fatalf("owner dev not seeded: %v", err)
	}
	if _, err := st.Owners().Get(ctx, "alice"); err != nil {
		t.Fatalf("owner alice not seeded: %v", err)
	}
	v, err := st.Apps().LatestReviewed(ctx, "secret-app")
	if err != nil || v.Ref != "examples/secret-app" {
		t.Fatalf("app not seeded: v=%+v err=%v", v, err)
	}
	m, err := st.Apps().DeclaredMounts(ctx, "secret-app", "1.0.0")
	if err != nil || len(m) != 1 || m[0].Name != "main" {
		t.Fatalf("mounts=%+v err=%v", m, err)
	}
	// idempotent: seeding twice is fine
	if err := Seed(ctx, st, tokens, apps); err != nil {
		t.Fatalf("re-seed: %v", err)
	}
}
```

- [ ] **Step 2: Run (red)** — `go test ./internal/cp/ -run TestSeed 2>&1 | head` → undefined `Seed`/`AppSeed`.

- [ ] **Step 3: Implement** — `internal/cp/seed.go`:
```go
package cp

import (
	"context"

	"spawnery/internal/cp/store"
)

// AppSeed describes a demo app to register at boot (until E5's real publishing/registration).
type AppSeed struct {
	ID      string   // public app id (what clients send, e.g. "secret-app")
	Ref     string   // definition ref the node mounts (e.g. "examples/secret-app")
	Version string   // seeded version (e.g. "1.0.0")
	Mounts  []string // declared mount names (from the app manifest, e.g. ["main"])
}

// Seed idempotently registers the dev-token owners + the demo apps/versions/mounts so CreateSpawn
// can resolve them. Owners come FROM the token map (every token's owner -> a row), so auth always
// resolves to a real owner. Replaced by E4 (OAuth) + E5 (catalog) later; no schema change.
func Seed(ctx context.Context, st store.Store, tokens map[string]string, apps []AppSeed) error {
	seen := map[string]bool{}
	for _, owner := range tokens {
		if seen[owner] {
			continue
		}
		seen[owner] = true
		if err := st.Owners().Upsert(ctx, store.Owner{ID: owner, CreatedAt: 0}); err != nil {
			return err
		}
	}
	for _, a := range apps {
		if err := st.Apps().Upsert(ctx, store.App{ID: a.ID, DisplayName: a.ID, CreatedAt: 0}); err != nil {
			return err
		}
		decls := make([]store.MountDecl, len(a.Mounts))
		for i, name := range a.Mounts {
			decls[i] = store.MountDecl{AppID: a.ID, Version: a.Version, Name: name, Required: true}
		}
		if err := st.Apps().UpsertVersion(ctx,
			store.AppVersion{AppID: a.ID, Version: a.Version, Ref: a.Ref, Reviewed: true, CreatedAt: 0},
			decls); err != nil {
			return err
		}
	}
	return nil
}
```

- [ ] **Step 4: Run (green)** — `go test ./internal/cp/ -run TestSeed -v` → PASS.

  NOTE: `go build ./internal/cp/` will still fail (server.go not yet rewired). Run ONLY the seed test here: `go test ./internal/cp/ -run TestSeed`. If the package doesn't compile because of server.go, temporarily that's expected — but the test can't run if the package won't build. To keep Task 4 self-contained, this task must come AFTER server.go compiles. **Reorder note:** if the package won't build, do Task 5 (server rewire) first, then this test passes. The controller should dispatch Task 5's server changes together with Task 4 if needed, OR put seed.go in its own buildable unit. SIMPLEST: `seed.go` only imports `store` (no server deps), so `internal/cp/seed.go` compiles independently — but the test is in package `cp`, which won't build until server.go is fixed. **Resolution: merge Tasks 4 + 5 into one dispatch** (seed + server rewire land together so package `cp` compiles). The controller should dispatch Task 4 and Task 5 as a single implementer task.

- [ ] **Step 5: Commit** (folded into Task 5's commit).

---

### Task 5: Server holds the store + CreateSpawn/StopSpawn/Session rewire + ownership

**Files:** Modify `internal/cp/server.go`, `internal/cp/ws.go`, `internal/cp/server_test.go`. (Dispatched together with Task 4's `seed.go`.)

**Context for the implementer:** the current `Server` struct + `NewServer` + the four handlers are in `internal/cp/server.go` (read it). `runNode`'s ACTIVE branch reads `s.rt.Owner(...)` for telemetry — change it to read the store. `Session` and `ws.go` read `s.rt.Owner(...)` for the ownership check — change to `s.st.Spawns().Get(...)`. `StopSpawn`'s `stop()` reads `rt.Owner` — change to the store + `MarkDeleted`.

- [ ] **Step 1: Update the `Server` struct + `NewServer`** in `internal/cp/server.go`:
  - Add imports: `"github.com/google/uuid"`, `"spawnery/internal/cp/lock"`, `"spawnery/internal/cp/store"`. Remove `"spawnery/internal/cp/apps"`.
  - Struct: replace `apps *apps.Resolver` with `st store.Store` and add `locks *lock.Keyed`. Keep the embedded `cpv1connect.UnimplementedSpawnServiceHandler`.
  - `NewServer`: signature `func NewServer(reg *registry.Registry, rt *router.Router, sched *scheduler.Scheduler, st store.Store, tel telemetry.Sink) *Server` returning `&Server{reg: reg, rt: rt, sched: sched, st: st, tel: tel, locks: lock.New()}`.

- [ ] **Step 2: Rewire `CreateSpawn`** in `internal/cp/server.go`:
```go
func (s *Server) CreateSpawn(ctx context.Context, req *connect.Request[cpv1.CreateSpawnRequest]) (*connect.Response[cpv1.CreateSpawnResponse], error) {
	owner, ok := auth.OwnerFromContext(ctx)
	if !ok {
		return nil, connect.NewError(connect.CodeUnauthenticated, fmt.Errorf("no owner"))
	}
	appID := req.Msg.AppId
	ver, err := s.st.Apps().LatestReviewed(ctx, appID)
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("unknown app: %s", appID))
	}
	decls, err := s.st.Apps().DeclaredMounts(ctx, appID, ver.Version)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	mounts := make([]store.Mount, len(decls))
	for i, d := range decls {
		mounts[i] = store.Mount{Name: d.Name, BackendURI: "scratch"} // D3: scratch default until E3 storage
	}
	id, err := uuid.NewV7()
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	spawnID := id.String()

	unlock := s.locks.Lock(spawnID)
	defer unlock()

	now := time.Now().Unix()
	sp := store.Spawn{
		ID: spawnID, OwnerID: owner, AppID: appID, AppVersion: ver.Version, AppRef: ver.Ref,
		Model: req.Msg.Model, Status: store.Starting, CreatedAt: now, LastUsedAt: now,
	}
	if err := s.st.WithTx(ctx, func(tx store.Store) error { return tx.Spawns().Create(ctx, sp, mounts) }); err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	if _, err := s.sched.Provision(ctx, spawnID, ver.Ref, req.Msg.Model); err != nil {
		_ = s.st.Spawns().SetError(ctx, spawnID)
		return nil, err
	}
	if err := s.st.Spawns().SetActive(ctx, spawnID, 1); err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	return connect.NewResponse(&cpv1.CreateSpawnResponse{SpawnId: spawnID}), nil
}
```
(Ensure `time` is imported.)

- [ ] **Step 3: Rewire `StopSpawn`/`stop`** in `internal/cp/server.go`:
```go
func (s *Server) StopSpawn(ctx context.Context, req *connect.Request[cpv1.StopSpawnRequest]) (*connect.Response[cpv1.StopSpawnResponse], error) {
	owner, _ := auth.OwnerFromContext(ctx)
	if err := s.stop(ctx, owner, req.Msg.SpawnId); err != nil {
		return nil, err
	}
	return connect.NewResponse(&cpv1.StopSpawnResponse{}), nil
}

// stop validates ownership via the store, tells the node to destroy the pod, drops the route, and
// soft-deletes the spawn (D4: today's StopSpawn is a destroy; suspend is Part 3).
func (s *Server) stop(ctx context.Context, owner, spawnID string) error {
	unlock := s.locks.Lock(spawnID)
	defer unlock()
	sp, err := s.st.Spawns().Get(ctx, spawnID)
	if err != nil {
		return connect.NewError(connect.CodeNotFound, fmt.Errorf("unknown spawn"))
	}
	if sp.OwnerID != owner {
		return connect.NewError(connect.CodePermissionDenied, fmt.Errorf("not your spawn"))
	}
	s.rt.StopOnNode(spawnID)
	s.rt.Drop(spawnID)
	if err := s.st.Spawns().MarkDeleted(ctx, spawnID, time.Now().Unix()); err != nil {
		return connect.NewError(connect.CodeInternal, err)
	}
	_ = s.tel.Emit(telemetry.Event{Kind: "session_end", Owner: owner, SpawnID: spawnID, Timestamp: time.Now().UTC()})
	return nil
}
```

- [ ] **Step 4: Rewire `Session` ownership + `runNode` telemetry** in `internal/cp/server.go`:
  - In `Session`, replace `rtOwner, ok := s.rt.Owner(spawnID)` + the check with:
```go
	sp, err := s.st.Spawns().Get(ctx, spawnID)
	if err != nil {
		return connect.NewError(connect.CodeNotFound, fmt.Errorf("unknown spawn"))
	}
	if owner != sp.OwnerID {
		return connect.NewError(connect.CodePermissionDenied, fmt.Errorf("not your spawn"))
	}
```
  (use `sp.OwnerID` where `rtOwner` was used in the telemetry emits below it.)
  - In `runNode`'s ACTIVE branch, replace `owner, _ := s.rt.Owner(m.Status.SpawnId)` with:
```go
			var owner string
			if sp, err := s.st.Spawns().Get(ctx, m.Status.SpawnId); err == nil {
				owner = sp.OwnerID
			}
```
  - In `runNode`'s deferred node-evict (`for _, id := range s.rt.DropNode(nodeID)`), after dropping routes, mark those spawns unreachable (D2):
```go
		if nodeID != "" {
			s.reg.Remove(nodeID)
			dropped := s.rt.DropNode(nodeID)
			if len(dropped) > 0 {
				_, _ = s.st.Spawns().MarkUnreachable(context.Background(), dropped)
			}
			for _, id := range dropped {
				_ = s.tel.Emit(telemetry.Event{Kind: "session_end", NodeID: nodeID, SpawnID: id, Timestamp: time.Now().UTC()})
			}
		}
```

- [ ] **Step 5: Rewire `ws.go` ownership** — in `internal/cp/ws.go`, replace:
```go
		rtOwner, ok := s.rt.Owner(bind.SpawnID)
		if !ok || rtOwner != owner {
			conn.Close(websocket.StatusPolicyViolation, "unknown or foreign spawn")
			return
		}
```
with:
```go
		sp, err := s.st.Spawns().Get(ctx, bind.SpawnID)
		if err != nil || sp.OwnerID != owner {
			conn.Close(websocket.StatusPolicyViolation, "unknown or foreign spawn")
			return
		}
```

- [ ] **Step 6: Update `internal/cp/server_test.go`'s `newTestServer`** to inject a seeded `:memory:` store:
```go
func newTestServer(t *testing.T) (*Server, *registry.Registry, *router.Router) {
	reg := registry.New()
	rt := router.New()
	sc := scheduler.New(reg, rt, time.Second)
	st := store.NewTestStore(t)
	if err := Seed(context.Background(), st, map[string]string{"dev-token": "alice"},
		[]AppSeed{{ID: "secret-app", Ref: "examples/secret-app", Version: "1.0.0", Mounts: []string{"main"}}}); err != nil {
		t.Fatal(err)
	}
	s := NewServer(reg, rt, sc, st, telemetry.NopSink{})
	return s, reg, rt
}
```
(Add the `store` import; change the existing `newTestServer()` callsite in `TestRunNodeRegistersAndRoutesFrames` to `newTestServer(t)`; that test uses `rt.Bind("sp1","n1","alice",sender)` — change to `rt.Bind("sp1","n1",sender)`.)

- [ ] **Step 7: Build + run the hermetic CP tests**
```bash
export PATH="$PATH:$(go env GOPATH)/bin"
go build ./... && go vet ./internal/cp/... && SKIP_DOCKER=1 go test ./internal/cp/... -run 'TestSeed|TestRunNode|TestMarkBoot' -v 2>&1 | tail -20
```
Expected: package `cp` builds; the seed + runNode tests pass. (The Docker e2e is build-tagged and updated in Task 6.)

- [ ] **Step 8: Commit**
```bash
git add internal/cp/seed.go internal/cp/seed_test.go internal/cp/server.go internal/cp/ws.go internal/cp/server_test.go
git commit --no-verify -m "feat(sp-pc4): CP uses store for ledger + ownership; CreateSpawn/StopSpawn/Session/ws rewired"
```

---

### Task 6: Wire main.go (open store, seed, boot-reconcile) + update the Docker e2e

**Files:** Modify `cmd/cp/main.go`, `internal/cp/e2e_test.go`.

- [ ] **Step 1: Rewire `cmd/cp/main.go`**:
  - Remove the `apps` import + the `appMap := apps.New(...)`.
  - Add imports: `"context"`, `"spawnery/internal/cp/store"`.
  - After parsing tokens, before `NewServer`:
```go
	tokens := parseTokens(env("CP_DEV_TOKENS", "dev-token=dev"))
	authn := auth.New(tokens)

	ctx := context.Background()
	st, err := store.Open(ctx, store.Config{
		Driver: env("CP_DB_DRIVER", "sqlite"),
		DSN:    env("CP_DB_DSN", "file:cp.db?_pragma=foreign_keys(1)&_pragma=journal_mode(WAL)"),
	})
	if err != nil {
		log.Fatalf("open store: %v", err)
	}
	defer st.Close()
	if err := cp.Seed(ctx, st, tokens, []cp.AppSeed{
		{ID: "secret-app", Ref: "examples/secret-app", Version: "1.0.0", Mounts: []string{"main"}},
	}); err != nil {
		log.Fatalf("seed: %v", err)
	}
	if n, err := st.Spawns().MarkBootUnreachable(ctx); err != nil {
		log.Fatalf("boot reconcile: %v", err)
	} else if n > 0 {
		log.Printf("boot reconcile: marked %d orphaned spawn(s) unreachable", n)
	}
```
  - `srv := cp.NewServer(reg, rt, sched, st, tel)`.

- [ ] **Step 2: Update `internal/cp/e2e_test.go`** (the `//go:build e2e` Docker test): it constructs a `Server` via `cp.NewServer(reg, rtr, sched, apps.New(...), tel)`. Change to build a store + seed (read the file; near the `// --- CP ---` block):
```go
	st, err := store.Open(context.Background(), store.Config{Driver: "sqlite", DSN: "file:cpe2e?mode=memory&cache=shared&_pragma=foreign_keys(1)"})
	if err != nil { t.Fatal(err) }
	defer st.Close()
	if err := cp.Seed(context.Background(), st, map[string]string{"dev-token": "alice"},
		[]cp.AppSeed{{ID: "secret-app", Ref: "examples/secret-app", Version: "1.0.0", Mounts: []string{"main"}}}); err != nil {
		t.Fatal(err)
	}
	srv := cp.NewServer(reg, rtr, sched, st, tel)
```
  Replace the `apps` import with `spawnery/internal/cp/store`. (The test's client still sends `AppId: "secret-app"`.)

- [ ] **Step 3: Build everything (incl. e2e tag) + run hermetic suite**
```bash
export PATH="$PATH:$(go env GOPATH)/bin"
go build ./... && go build -tags e2e ./internal/cp/ && go vet ./internal/cp/...
SKIP_DOCKER=1 go test ./... 2>&1 | tail -20
```
Expected: everything builds (incl. the e2e-tagged test compiles); the full hermetic suite passes. The Docker e2e itself is not run here (needs Docker), but it MUST compile against the new `NewServer`/`Seed`.

- [ ] **Step 4: Commit**
```bash
git add cmd/cp/main.go internal/cp/e2e_test.go
git commit --no-verify -m "feat(sp-pc4): wire CP main onto the store (open+seed+boot-reconcile); fix e2e construction"
```

---

### Task 7: Demo smoke verification (manual, documented)

**Files:** none — this task documents the human verification path (the automated coverage is the hermetic CP tests + the build-tagged Docker e2e).

- [ ] **Step 1: Confirm the hermetic + compile gates**
```bash
export PATH="$PATH:$(go env GOPATH)/bin"
go build ./... && go build -tags e2e ./internal/cp/ && go vet ./...
SKIP_DOCKER=1 go test ./... 2>&1 | tail -15
```
Expected: all green; the Docker e2e compiles.

- [ ] **Step 2: (If Docker is available) run the real CP e2e**
```bash
make images 2>/dev/null || true
go test -tags e2e ./internal/cp/ -run TestCPEndToEndStub -v 2>&1 | tail -20
```
Expected: PASS (create → session → stub echo → stop), now backed by the store. If Docker is unavailable, this is skipped — the hermetic gates above stand.

- [ ] **Step 3: Record the manual web click-through note** — no code; the human runs `just dev`, creates a spawn, chats, closes the tab (StopSpawn → soft-delete). The durable spawn row + container episode are now recorded in `cp.db`.

---

## Self-Review

**Spec coverage (DAO design §7 part-2 scope + handoff notes):**
- Per-spawn lock — Task 1 ✓
- Store-as-ledger: CreateSpawn writes spawn(starting)+container(gen1)+mounts via WithTx — Task 5 ✓
- Ownership via store on gRPC Session AND ws.go — Task 5 ✓
- apps.Resolver replaced by store.Apps().LatestReviewed — Task 5 ✓
- Router owner-authority removed (Bind drops owner, Owner() deleted) — Task 3 ✓
- Scheduler split (Provision takes a pre-minted id) — Task 3 ✓
- Seeding owners-from-token-map + app/version/mounts — Tasks 4/5/6 ✓
- StopSpawn → MarkDeleted (D4) — Task 5 ✓
- Boot reconciliation (MarkBootUnreachable) — Tasks 2/6 ✓
- Node-evict → MarkUnreachable (D2) — Task 5 ✓
- uuidv7 ids — Task 5 ✓
- Demo stays green (hermetic CP tests + Docker e2e compile/pass) — Tasks 5/6/7 ✓
- OUT of scope (Part 3): lifecycle RPCs (List/Suspend/Resume/Recreate/Delete), generation fencing of stale SpawnStatus, node inventory + grace-window + adopt, StartSpawn.mounts/generation wire population, the uint64↔int64 cast (no generation on the wire yet in part 2).

**Placeholder scan:** none — every step has complete code. The one ordering caveat (Task 4 package-compiles-only-after-Task-5) is resolved by dispatching Tasks 4+5 together.

**Type consistency:** `store.Spawn`/`store.Mount`/`store.MountDecl`/`store.AppVersion` fields match Part 1's types; `AppSeed`, `Seed`, `Provision(ctx,id,appRef,model)`, `Bind(spawnID,nodeID,node)`, `MarkBootUnreachable`, `MarkUnreachable(ids)`, `SetActive(id,1)`, `MarkDeleted(id,ts)` are used consistently across tasks. `uuid.NewV7()` returns `(UUID, error)` — handled.

**Watch-points for the implementer:** (a) `bun.In(...)` for the `status IN (?)` in MarkBootUnreachable (Task 2); (b) Tasks 4+5 MUST be one dispatch (package `cp` won't build until server.go is rewired); (c) the Docker e2e is build-tagged — it must COMPILE against the new signatures even though it's not run hermetically; (d) `Session`/`runNode` now take `ctx` into store calls — ensure the right context is threaded (the receive-loop `ctx` for runNode).
