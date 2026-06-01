# Scheduler Routing by Node Class + Author-Self-Host Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: superpowers:subagent-driven-development. Checkbox steps.

**Goal:** Route unverified app versions only to a self-hosted node owned by the app's author; reviewed/scanned route anywhere.

**Source spec:** `docs/superpowers/specs/2026-06-01-scheduler-routing-sp-t5p.md`. Bead `sp-t5p`. Branch `sp-t5p-routing` off master. Commits `--no-verify`. Codegen on PATH (`export PATH="$PATH:$(go env GOPATH)/bin"`).

---

## Task 1: Contract — `node_owner` on `Register`

**Files:** Modify `proto/node/v1/node.proto`; regenerated `gen/node/v1/*`.

- [ ] **Step 1:** `Register` is `message Register { string node_id = 1; uint32 max_spawns = 2; repeated string agent_images = 3; repeated RunningSpawn running = 4; string node_class = 5; }`. Add field 6:
```proto
  string node_owner = 6;
```
(so the full message ends `... string node_class = 5; string node_owner = 6; }`)
- [ ] **Step 2:** `export PATH="$PATH:$(go env GOPATH)/bin" && make gen`
- [ ] **Step 3:** Verify: `go build ./... && grep -c "func (x \*Register) GetNodeOwner" gen/node/v1/node.pb.go` (1).
- [ ] **Step 4:** Commit: `git add proto/node/v1/node.proto gen/node/v1 && git commit --no-verify -m "feat(node): node_owner on Register (sp-t5p)"`

---

## Task 2: Node→owner propagation

**Files:** Modify `internal/cp/registry/registry.go`, `internal/cp/server.go`, `internal/node/attach.go`, `cmd/spawnlet/main.go`; extend `internal/cp/node_class_test.go`.

> Context: this mirrors `sp-2as`'s class plumbing. `registry.Node` already has `Class string`. `server.go runNode` Register case sets `nodeClass` (defaulting "cloud") + `s.reg.Add(&registry.Node{ID, Sender, Max, Free, Images, Class: nodeClass})`. `node.Config` has `NodeID/CPURL/MaxSpawns/AgentImage/NodeClass`; `attach.go` Register send sets `NodeId/MaxSpawns/AgentImages/NodeClass`. `cmd/spawnlet` CP-attached `node.Config{...}` sets `NodeClass: env("NODE_CLASS","cloud")`.

- [ ] **Step 1: Failing test** — extend `internal/cp/node_class_test.go` (the `feedRegister` helper sends `NodeClass`; add owner). Add a test:
```go
func TestRegisterRecordsNodeOwner(t *testing.T) {
	s, reg, _ := newTestServer(t)
	in := make(chan *nodev1.NodeMessage, 4)
	go s.runNode(context.Background(), &capSender{}, recvFromChan(in))
	in <- &nodev1.NodeMessage{Msg: &nodev1.NodeMessage_Register{Register: &nodev1.Register{NodeId: "n1", MaxSpawns: 1, NodeClass: "self-hosted", NodeOwner: "alice"}}}
	deadline := time.Now().Add(time.Second)
	for {
		if n, ok := reg.Get("n1"); ok && n.Owner == "alice" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("node owner not recorded")
		}
		time.Sleep(time.Millisecond)
	}
	close(in)
}
```
(`recvFromChan` + `capSender` already exist in the package's test files.)

- [ ] **Step 2: Confirm failure:** `go test ./internal/cp/ -run TestRegisterRecordsNodeOwner 2>&1 | head` (no `Node.Owner`).

- [ ] **Step 3:** `registry.Node` (registry.go): add `Owner string` field.

- [ ] **Step 4:** `server.go runNode` Register case: add `Owner: m.Register.NodeOwner` to the `registry.Node{...}` literal (no default — empty is valid for cloud).

- [ ] **Step 5:** `node.Config` (attach.go): add `NodeOwner string`; the `Register{...}` send: add `NodeOwner: cfg.NodeOwner`.

- [ ] **Step 6:** `cmd/spawnlet/main.go` CP-attached `node.Config{...}`: add `NodeOwner: env("NODE_OWNER", "")` (or `os.Getenv("NODE_OWNER")`).

- [ ] **Step 7:** `go test ./internal/cp/ -run 'TestRegister' && go build ./...` — PASS/clean.

- [ ] **Step 8:** Commit: `git add internal/cp/registry internal/cp/server.go internal/node/attach.go cmd/spawnlet/main.go internal/cp/node_class_test.go && git commit --no-verify -m "feat(cp): propagate node owner from Register (sp-t5p)"`

---

## Task 3: Placement — `registry.PickFor` + `scheduler.Provision(placement)`

**Files:** Modify `internal/cp/registry/registry.go`, `internal/cp/scheduler/scheduler.go`, `internal/cp/server.go` (the Provision call), `internal/cp/scheduler/scheduler_test.go`, `internal/cp/registry/registry_test.go`.

- [ ] **Step 1: Failing registry test** — add to `internal/cp/registry/registry_test.go`:
```go
func TestPickFor(t *testing.T) {
	r := New()
	r.Add(&Node{ID: "cloud1", Free: 5, Class: "cloud"})
	r.Add(&Node{ID: "selfA", Free: 1, Class: "self-hosted", Owner: "alice"})
	r.Add(&Node{ID: "selfB", Free: 9, Class: "self-hosted", Owner: "bob"})

	if n := r.PickFor(Placement{}); n == nil || n.ID != "selfB" {
		t.Fatalf("unconstrained should pick max-free selfB, got %v", n)
	}
	if n := r.PickFor(Placement{Class: "self-hosted", Owner: "alice"}); n == nil || n.ID != "selfA" {
		t.Fatalf("class+owner filter should pick selfA, got %v", n)
	}
	if n := r.PickFor(Placement{Class: "self-hosted", Owner: "carol"}); n != nil {
		t.Fatalf("no node owned by carol -> nil, got %v", n)
	}
	if n := r.PickFor(Placement{Class: "cloud"}); n == nil || n.ID != "cloud1" {
		t.Fatalf("class filter should pick cloud1, got %v", n)
	}
}
```

- [ ] **Step 2: Confirm failure:** `go test ./internal/cp/registry/ -run TestPickFor 2>&1 | head`.

- [ ] **Step 3:** `registry.go` — add the type + method; refactor `Pick`:
```go
// Placement constrains node selection. An empty field is unconstrained.
type Placement struct {
	Class string
	Owner string
}

// PickFor returns the node with the most free capacity that satisfies the placement, or nil.
func (r *Registry) PickFor(p Placement) *Node {
	r.mu.Lock()
	defer r.mu.Unlock()
	var best *Node
	for _, n := range r.m {
		if n.Free == 0 {
			continue
		}
		if p.Class != "" && n.Class != p.Class {
			continue
		}
		if p.Owner != "" && n.Owner != p.Owner {
			continue
		}
		if best == nil || n.Free > best.Free {
			best = n
		}
	}
	return best
}

func (r *Registry) Pick() *Node { return r.PickFor(Placement{}) }
```
(Delete the old `Pick` body — it's now the `PickFor(Placement{})` delegation. Existing `Pick()` callers/tests are unchanged.)

- [ ] **Step 4:** `scheduler.go` `Provision` — add a `placement registry.Placement` param and use `s.reg.PickFor(placement)`; make the no-node error placement-aware:
```go
func (s *Scheduler) Provision(ctx context.Context, id, appRef, model string, placement registry.Placement) (string, error) {
	n := s.reg.PickFor(placement)
	if n == nil {
		return "", connect.NewError(connect.CodeResourceExhausted, errors.New("no eligible node with capacity"))
	}
	...
```

- [ ] **Step 5:** Update the two `scheduler_test.go` `Provision(...)` calls to pass `registry.Placement{}` as the final arg (add the `registry` import if missing). Update `server.go:183` `s.sched.Provision(ctx, spawnID, ver.Ref, req.Msg.Model)` → add `, registry.Placement{}` for now (Task 4 computes the real placement). Add the `"spawnery/internal/cp/registry"` import to server.go if not present.

- [ ] **Step 6:** `go test ./internal/cp/registry/ ./internal/cp/scheduler/ ./internal/cp/ && go build ./...` — PASS/clean.

- [ ] **Step 7:** Commit: `git add internal/cp/registry internal/cp/scheduler internal/cp/server.go && git commit --no-verify -m "feat(cp): placement-constrained PickFor + Provision (sp-t5p)"`

---

## Task 4: CreateSpawn routing policy

**Files:** Modify `internal/cp/server.go`; create `internal/cp/routing_test.go`.

> Context: `CreateSpawn` resolves `ver` then (slice 3) rejects non-reviewed explicit versions with `FailedPrecondition`. Replace that rejection with placement computation. `s.st.Apps().Creator(appID) (string, error)` returns the app's creator. `registry.Placement` from Task 3.

- [ ] **Step 1: Failing test** — create `internal/cp/routing_test.go`. Reuse the `createActive`/`capSender` pattern from `version_select_test.go` but set `Class`/`Owner` on the registered node. Tests:
```go
package cp

import (
	"context"
	"testing"
	"time"

	"connectrpc.com/connect"

	cpv1 "spawnery/gen/cp/v1"
	nodev1 "spawnery/gen/node/v1"
	"spawnery/internal/cp/auth"
	"spawnery/internal/cp/registry"
	"spawnery/internal/cp/store"
)

// register an unverified version of an app owned by `creator`, return appID.
func seedUnverified(t *testing.T, s *Server, creator, appID string) {
	t.Helper()
	ctx := auth.WithOwner(context.Background(), creator)
	if _, err := s.RegisterAppVersion(ctx, connect.NewRequest(&cpv1.RegisterAppVersionRequest{
		Manifest: &cpv1.AppManifest{ApiVersion: "spawnery/v1", Id: appID, Title: "T", Visibility: "open", Mounts: []*cpv1.ManifestMount{{Name: "main", Path: "data", Seed: "seed"}}},
		Version: "0.1.0", Ref: appID + "@sha",
	})); err != nil {
		t.Fatal(err)
	}
}

// drive the single spawn on a node with the given class/owner to ACTIVE; return the persisted Spawn.
func createActiveOn(t *testing.T, s *Server, reg *registry.Registry, caller, appID, version, nodeClass, nodeOwner string) (store.Spawn, error) {
	t.Helper()
	sender := &capSender{}
	reg.Add(&registry.Node{ID: "n1", Sender: sender, Max: 1, Free: 1, Class: nodeClass, Owner: nodeOwner})
	go func() {
		for {
			if st := sender.firstStart(); st != nil {
				s.sched.OnStatus(st.GetSpawnId(), nodev1.SpawnPhase_ACTIVE)
				return
			}
			time.Sleep(time.Millisecond)
		}
	}()
	resp, err := s.CreateSpawn(auth.WithOwner(context.Background(), caller), connect.NewRequest(&cpv1.CreateSpawnRequest{AppId: appID, Model: "m", Version: version}))
	if err != nil {
		return store.Spawn{}, err
	}
	sp, gerr := s.st.Spawns().Get(context.Background(), resp.Msg.SpawnId)
	if gerr != nil {
		t.Fatal(gerr)
	}
	return sp, nil
}

func TestUnverifiedSpawnsOnAuthorSelfHostedNode(t *testing.T) {
	s, reg, _ := newTestServer(t)
	seedUnverified(t, s, "alice", "alice/dev")
	sp, err := createActiveOn(t, s, reg, "alice", "alice/dev", "0.1.0", "self-hosted", "alice")
	if err != nil || sp.AppVersion != "0.1.0" {
		t.Fatalf("author should spawn unverified on own self-hosted node: sp=%+v err=%v", sp, err)
	}
}

func TestUnverifiedRejectedForNonCreator(t *testing.T) {
	s, reg, _ := newTestServer(t)
	seedUnverified(t, s, "alice", "alice/dev")
	_, err := createActiveOn(t, s, reg, "mallory", "alice/dev", "0.1.0", "self-hosted", "mallory")
	if connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Fatalf("non-creator unverified spawn want PermissionDenied, got %v", err)
	}
}

func TestUnverifiedRejectedWithoutSelfHostedNode(t *testing.T) {
	s, reg, _ := newTestServer(t)
	seedUnverified(t, s, "alice", "alice/dev")
	// only a CLOUD node available -> no eligible node for an unverified app.
	_, err := createActiveOn(t, s, reg, "alice", "alice/dev", "0.1.0", "cloud", "")
	if connect.CodeOf(err) != connect.CodeResourceExhausted {
		t.Fatalf("unverified w/o self-hosted node want ResourceExhausted, got %v", err)
	}
}
```
(NOTE: the existing `version_select_test.go` defines `createActive`/`capSender`/`seedVersions` in package `cp`; make sure `createActiveOn`/`seedUnverified` don't collide. `capSender`/`firstStart` are shared — reuse, don't redefine.)

- [ ] **Step 2: Confirm failure:** `go test ./internal/cp/ -run 'TestUnverified' 2>&1 | head` (currently the explicit unverified version is rejected with FailedPrecondition, not the new behavior).

- [ ] **Step 3:** In `server.go` `CreateSpawn`, REPLACE the tier-gate block:
```go
		if ver.Tier != store.TierReviewed {
			return nil, connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("version %s@%s is tier %q, not spawnable", appID, v, ver.Tier))
		}
```
with: nothing here (allow any tier through resolution). Then AFTER the version is resolved (both explicit + empty branches) and before/around the `Provision` call, compute placement:
```go
	placement := registry.Placement{}
	if ver.Tier != store.TierReviewed && ver.Tier != store.TierScanned {
		// unverified (or unknown): author-self-host rule.
		creator, cerr := s.st.Apps().Creator(ctx, appID)
		if cerr != nil {
			return nil, connect.NewError(connect.CodeInternal, cerr)
		}
		if creator != owner {
			return nil, connect.NewError(connect.CodePermissionDenied, fmt.Errorf("only the author can run an unverified version of %s", appID))
		}
		placement = registry.Placement{Class: "self-hosted", Owner: owner}
	}
```
Place this computation before the `Provision` call. Then change the Provision call:
```go
	nodeID, err := s.sched.Provision(ctx, spawnID, ver.Ref, req.Msg.Model, placement)
```
(Ensure `placement` is in scope at the Provision call — declare it before the lock/tx block, or right before Provision. The spawn row is created regardless; if Provision fails with ResourceExhausted the existing SetError path runs — acceptable. If you prefer to reject BEFORE minting the spawn, compute the PermissionDenied check early; but the no-eligible-node ResourceExhausted naturally comes from Provision. Keeping the creator-check early (before minting) avoids an orphan row for the PermissionDenied case — RECOMMENDED: do the creator/permission check right after resolving `ver`, before the lock/Create; set `placement` there too.)

> Implementer: prefer computing `placement` + the creator/PermissionDenied check immediately after version resolution (before `uuid.NewV7()`/lock/Create), so a denied request never mints a spawn row. The ResourceExhausted (no node) still surfaces from Provision after the row is created → the existing `SetError` compensation handles it.

- [ ] **Step 4:** `go test ./internal/cp/ -run 'TestUnverified|TestCreateSpawn|TestPerUserSpawn' && go test ./internal/cp/ -race` — PASS (new routing + existing version-select/quota tests; the slice-3 `TestCreateSpawnVersionErrors` asserted FailedPrecondition for an unverified version — UPDATE that test: an unverified explicit version is now allowed-but-placement-gated, so that sub-assertion changes. Find `TestCreateSpawnVersionErrors` in `version_select_test.go` and adjust the `3.0.0-rc1` (unverified) case: it should NO LONGER expect FailedPrecondition; instead, with no self-hosted node owned by the caller it expects `ResourceExhausted` (or, if the caller isn't the creator of secret-app, `PermissionDenied`). secret-app's creator is "spawnery" (seeded), caller is "alice" → expect `PermissionDenied`. Update the assertion accordingly.)

- [ ] **Step 5:** `go build ./... && go vet ./...` — clean.

- [ ] **Step 6:** Commit: `git add internal/cp/server.go internal/cp/routing_test.go internal/cp/version_select_test.go && git commit --no-verify -m "feat(cp): route unverified apps to the author's self-hosted node (sp-t5p)"`

---

## Final Verification
- [ ] `export PATH="$PATH:$(go env GOPATH)/bin" && make gen` — no diff.
- [ ] `go build ./... && go build -tags e2e ./... && go vet ./...` — clean.
- [ ] `go test ./...` — pass; `go test ./internal/cp/... -race` — race-clean.

Then **superpowers:finishing-a-development-branch** (Option 1: merge locally).

---

## Self-Review Notes
- **Spec coverage:** §2 node owner → T1/T2; §3 placement → T3; §4 policy → T4. ✓
- **Types:** `registry.Placement{Class,Owner}`, `registry.PickFor`, `registry.Node.Owner`, `Provision(...,placement)`, `Register.NodeOwner` consistent. ✓
- **Behavior change to flag:** slice-3 `TestCreateSpawnVersionErrors` unverified-case assertion changes (FailedPrecondition → PermissionDenied for a non-creator); updated in T4 step 4. Empty-version default still `LatestReviewed` (normal UX unchanged). ✓
- **No orphan on PermissionDenied:** creator check before minting; ResourceExhausted (no node) still uses the existing post-Provision SetError path. ✓
