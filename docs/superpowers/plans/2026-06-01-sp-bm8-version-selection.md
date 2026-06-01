# E5 Slice 3 — Version Selection & Pinning Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax.

**Goal:** Let `CreateSpawn` select an app version (latest reviewed, or an explicit reviewed version) and record whether the spawn is pinned.

**Architecture:** Pure CP. Contracts (`version`+`pin` on `CreateSpawnRequest`) → handler resolution. No store schema change (`Spawn.pinned/app_version/app_ref` already exist).

**Tech Stack:** Go, ConnectRPC, buf, bun.

**Source spec:** `docs/superpowers/specs/2026-06-01-e5-version-selection-slice3.md`

**Conventions:** commit `--no-verify`; codegen via `export PATH="$PATH:$(go env GOPATH)/bin" && make gen` (install missing tools, never stub); bead `sp-bm8`; branch `sp-bm8-version-selection` off master.

---

## Task 1: Contracts — `version` + `pin` on `CreateSpawnRequest`

**Files:** Modify `proto/cp/v1/cp.proto`; regenerated `gen/cp/v1/*`.

- [ ] **Step 1:** In `proto/cp/v1/cp.proto`, change the `CreateSpawnRequest` line:
```proto
message CreateSpawnRequest  { string app_id = 1; string model = 2; repeated MountBinding mounts = 3; string version = 4; bool pin = 5; }
```

- [ ] **Step 2:** `export PATH="$PATH:$(go env GOPATH)/bin" && make gen`

- [ ] **Step 3:** Verify: `go build ./... && grep -c "GetVersion\|Pin\b" gen/cp/v1/cp.pb.go` — builds clean; the generated `CreateSpawnRequest` has `Version` and `Pin` accessors (`grep "func (x \*CreateSpawnRequest) GetVersion\|GetPin" gen/cp/v1/cp.pb.go` finds both).

- [ ] **Step 4:** Commit:
```bash
git add proto/cp/v1/cp.proto gen/cp/v1
git commit --no-verify -m "feat(cp): version + pin on CreateSpawnRequest (sp-bm8)"
```
(Pure-codegen; verification is the build + accessor grep.)

---

## Task 2: `CreateSpawn` version resolution + pinning

**Files:** Modify `internal/cp/server.go` (`CreateSpawn`); Test `internal/cp/version_select_test.go` (new).

> Context: `CreateSpawn` currently does `ver, err := s.st.Apps().LatestReviewed(ctx, appID)` and builds `store.Spawn{... AppVersion: ver.Version, AppRef: ver.Ref, ...}` with `Pinned` unset (false). `s.st.Apps().GetVersion(ctx, appID, version) (store.AppVersion, error)` exists and returns `store.ErrNotFound` when absent. `store.AppVersion` has `.Tier` (type `store.Tier`); `store.TierReviewed` is the spawnable tier. The handler already has `owner`, `err`, `appID` in scope and imports `connect`, `fmt`, `store`.

- [ ] **Step 1: Failing test** — create `internal/cp/version_select_test.go`. The success cases drive a spawn to ACTIVE using the EXACT pattern from the existing `TestCreateSpawnPersistsNodeID` (`server_test.go:127`): a `capSender` node added to `reg` + a goroutine that drives the first start to ACTIVE. One create per test so that single-start driver works verbatim. Error cases need no node (they return before provisioning).
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

// seedVersions adds a second reviewed version (2.0.0, newest) + one unverified (3.0.0-rc1)
// to the test server's seeded secret-app.
func seedVersions(t *testing.T, s *Server) {
	t.Helper()
	ctx := context.Background()
	if err := s.st.Apps().UpsertVersion(ctx,
		store.AppVersion{AppID: "secret-app", Version: "2.0.0", Ref: "examples/secret-app", Tier: store.TierReviewed, CreatedAt: 100}, nil); err != nil {
		t.Fatal(err)
	}
	if err := s.st.Apps().UpsertVersion(ctx,
		store.AppVersion{AppID: "secret-app", Version: "3.0.0-rc1", Ref: "examples/secret-app", Tier: store.TierUnverified, CreatedAt: 200}, nil); err != nil {
		t.Fatal(err)
	}
}

// createActive registers a fake node, drives the one spawn to ACTIVE, runs CreateSpawn, and
// returns the persisted Spawn. Mirrors TestCreateSpawnPersistsNodeID.
func createActive(t *testing.T, s *Server, reg *registry.Registry, req *cpv1.CreateSpawnRequest) store.Spawn {
	t.Helper()
	sender := &capSender{}
	reg.Add(&registry.Node{ID: "n1", Sender: sender, Max: 1, Free: 1})
	go func() {
		for {
			if st := sender.firstStart(); st != nil {
				s.sched.OnStatus(st.GetSpawnId(), nodev1.SpawnPhase_ACTIVE)
				return
			}
			time.Sleep(time.Millisecond)
		}
	}()
	ctx := auth.WithOwner(context.Background(), "alice")
	resp, err := s.CreateSpawn(ctx, connect.NewRequest(req))
	if err != nil {
		t.Fatalf("CreateSpawn: %v", err)
	}
	sp, err := s.st.Spawns().Get(ctx, resp.Msg.SpawnId)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	return sp
}

func TestCreateSpawnExplicitVersionAndPin(t *testing.T) {
	s, reg, _ := newTestServer(t)
	seedVersions(t, s)
	sp := createActive(t, s, reg, &cpv1.CreateSpawnRequest{AppId: "secret-app", Model: "m", Version: "2.0.0", Pin: true})
	if sp.AppVersion != "2.0.0" || sp.AppRef != "examples/secret-app" || !sp.Pinned {
		t.Fatalf("explicit+pin spawn = %+v (want 2.0.0/examples/secret-app, pinned)", sp)
	}
}

func TestCreateSpawnLatestVersionNotPinned(t *testing.T) {
	s, reg, _ := newTestServer(t)
	seedVersions(t, s)
	sp := createActive(t, s, reg, &cpv1.CreateSpawnRequest{AppId: "secret-app", Model: "m"})
	if sp.AppVersion != "2.0.0" || sp.Pinned {
		t.Fatalf("latest spawn = %+v (want 2.0.0 newest reviewed, not pinned)", sp)
	}
}

func TestCreateSpawnVersionErrors(t *testing.T) {
	s, _, _ := newTestServer(t)
	seedVersions(t, s)
	ctx := auth.WithOwner(context.Background(), "alice")
	// these return BEFORE provisioning, so no node is needed.
	if _, err := s.CreateSpawn(ctx, connect.NewRequest(&cpv1.CreateSpawnRequest{AppId: "secret-app", Model: "m", Version: "9.9.9"})); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("unknown version: want InvalidArgument, got %v", err)
	}
	if _, err := s.CreateSpawn(ctx, connect.NewRequest(&cpv1.CreateSpawnRequest{AppId: "secret-app", Model: "m", Version: "3.0.0-rc1"})); connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Fatalf("unverified version: want FailedPrecondition, got %v", err)
	}
}
```
> The helper names `capSender`, `firstStart()`, `registry.Node{ID,Sender,Max,Free}`, and `s.sched.OnStatus(id, nodev1.SpawnPhase_ACTIVE)` are all used by the existing `TestCreateSpawnPersistsNodeID` — confirm them in `server_test.go` and match exactly. If the import path for `nodev1` differs, copy it from `server_test.go`'s import block.

- [ ] **Step 2: Confirm failure:** `go test ./internal/cp/ -run TestCreateSpawnExplicitVersion 2>&1 | head` (Version/Pin fields won't compile until Task 1 is merged into the branch — they are, since Task 1 committed the gen; so this should compile and FAIL on behavior: the handler ignores Version/Pin).

- [ ] **Step 3: Implement** — in `internal/cp/server.go` `CreateSpawn`, replace:
```go
	ver, err := s.st.Apps().LatestReviewed(ctx, appID)
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("unknown app: %s", appID))
	}
```
with:
```go
	var ver store.AppVersion
	if v := req.Msg.Version; v != "" {
		ver, err = s.st.Apps().GetVersion(ctx, appID, v)
		if err != nil {
			return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("unknown app version: %s@%s", appID, v))
		}
		if ver.Tier != store.TierReviewed {
			return nil, connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("version %s@%s is tier %q, not spawnable", appID, v, ver.Tier))
		}
	} else {
		ver, err = s.st.Apps().LatestReviewed(ctx, appID)
		if err != nil {
			return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("unknown app: %s", appID))
		}
	}
```
And in the `store.Spawn{...}` literal, add `Pinned: req.Msg.Pin,`.
> NOTE: `err` must already be declared in scope. If the function used `ver, err :=` (short decl) and there's no prior `err`, the new `var ver store.AppVersion` + `ver, err = ...` needs `err` declared — add `var err error` before the block if the compiler complains about undefined `err`.

- [ ] **Step 4: Run:** `go test ./internal/cp/ -run TestCreateSpawn` — PASS (new + existing CreateSpawn tests).

- [ ] **Step 5: Full package + race:** `go test ./internal/cp/ -race` — PASS.

- [ ] **Step 6: Commit:**
```bash
git add internal/cp/server.go internal/cp/version_select_test.go
git commit --no-verify -m "feat(cp): CreateSpawn version selection + pinning (sp-bm8)"
```

---

## Final Verification

- [ ] `export PATH="$PATH:$(go env GOPATH)/bin" && make gen` — no diff.
- [ ] `go build ./... && go build -tags e2e ./... && go vet ./...` — clean.
- [ ] `go test ./...` — pass; `go test ./internal/cp/ -race` — race-clean.

Then **superpowers:finishing-a-development-branch** (Option 1: merge to master locally).

---

## Self-Review Notes
- **Spec coverage:** §2 semantics → T1 (fields) + T2 (resolution/errors/pin); §4 testing → T2 tests. Out-of-scope (resume re-resolution, snapshot, non-reviewed spawning, repin RPC) absent. ✓
- **Types:** `req.Msg.Version`/`req.Msg.Pin`, `store.AppVersion`, `store.TierReviewed`, `GetVersion`, `Spawn.Pinned` consistent. ✓
- **Risk:** the test must mirror the existing CreateSpawn test's node/scheduler setup so provisioning succeeds — T2 Step 1 instructs reading `server_test.go` first.
