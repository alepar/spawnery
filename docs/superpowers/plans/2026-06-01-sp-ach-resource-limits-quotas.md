# Resource Limits + Per-User Quota + Isolation Runtime Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development. Steps use checkbox (`- [ ]`) syntax.

**Goal:** Cap each spawn's mem/CPU/pids, allow an optional gVisor runtime, and cap concurrent spawns per user.

**Architecture:** Node-side per-spawn cgroup limits + runtime via Docker `HostConfig` (extract a testable `buildHostConfig`); CP-side per-user concurrency cap in `CreateSpawn`. Mostly hermetic; real cgroup/gVisor effect verifies on a privileged host.

**Source spec:** `docs/superpowers/specs/2026-06-01-resource-limits-quotas-sp-ach.md`

**Conventions:** commit `--no-verify`; bead `sp-ach`; branch `sp-ach-limits` off master. No codegen.

---

## Task 1: `ContainerSpec` limits/runtime + `buildHostConfig`

**Files:** Modify `internal/runtime/runtime.go`, `internal/runtime/docker.go`; create `internal/runtime/hostconfig_test.go`.

> Context: `docker.go` `StartContainer` currently builds `host := &container.HostConfig{}`, sets `host.NetworkMode` (if `NetnsOf`), and appends `host.Binds` from mounts, then `ContainerCreate(ctx, cfg, host, ...)`. Imports `github.com/docker/docker/api/types/container`. In that package: `HostConfig.Runtime string`, `HostConfig.Resources` is a `container.Resources` with `Memory int64`, `NanoCPUs int64`, `PidsLimit *int64`.

- [ ] **Step 1: Add fields to `ContainerSpec`** (`runtime.go`):
```go
	MemoryBytes int64  // 0 = unlimited
	NanoCPUs    int64  // 0 = unlimited; 1 CPU = 1_000_000_000
	PidsLimit   int64  // 0 = unlimited
	Runtime     string // "" = Docker default; e.g. "runsc"
```

- [ ] **Step 2: Failing test** — create `internal/runtime/hostconfig_test.go`:
```go
package runtime

import "testing"

func TestBuildHostConfigLimits(t *testing.T) {
	h := buildHostConfig(ContainerSpec{
		MemoryBytes: 512 << 20, NanoCPUs: 1_500_000_000, PidsLimit: 200, Runtime: "runsc",
		Mounts: []Mount{{HostPath: "/h", ContainerPath: "/app", ReadOnly: true}},
	})
	if h.Resources.Memory != 512<<20 {
		t.Fatalf("Memory = %d", h.Resources.Memory)
	}
	if h.Resources.NanoCPUs != 1_500_000_000 {
		t.Fatalf("NanoCPUs = %d", h.Resources.NanoCPUs)
	}
	if h.Resources.PidsLimit == nil || *h.Resources.PidsLimit != 200 {
		t.Fatalf("PidsLimit = %v", h.Resources.PidsLimit)
	}
	if h.Runtime != "runsc" {
		t.Fatalf("Runtime = %q", h.Runtime)
	}
	if len(h.Binds) != 1 || h.Binds[0] != "/h:/app:ro" {
		t.Fatalf("Binds = %v", h.Binds)
	}
}

func TestBuildHostConfigZeroValuesOmitted(t *testing.T) {
	h := buildHostConfig(ContainerSpec{NetnsOf: "sidecar123"})
	if h.Resources.Memory != 0 || h.Resources.NanoCPUs != 0 || h.Resources.PidsLimit != nil {
		t.Fatalf("zero limits must be unset: %+v", h.Resources)
	}
	if h.Runtime != "" {
		t.Fatalf("Runtime should be empty, got %q", h.Runtime)
	}
	if string(h.NetworkMode) != "container:sidecar123" {
		t.Fatalf("NetworkMode = %q", h.NetworkMode)
	}
}
```

- [ ] **Step 3: Confirm failure:** `go test ./internal/runtime/ -run TestBuildHostConfig 2>&1 | head` (no `buildHostConfig`, no fields).

- [ ] **Step 4: Extract + implement `buildHostConfig`** in `docker.go`. Replace the inline `host := &container.HostConfig{}` ... construction in `StartContainer` with `host := buildHostConfig(s)`, and add:
```go
func buildHostConfig(s ContainerSpec) *container.HostConfig {
	host := &container.HostConfig{}
	if s.NetnsOf != "" {
		host.NetworkMode = container.NetworkMode("container:" + s.NetnsOf)
	}
	for _, m := range s.Mounts {
		host.Binds = append(host.Binds, bind(m))
	}
	if s.MemoryBytes > 0 {
		host.Resources.Memory = s.MemoryBytes
	}
	if s.NanoCPUs > 0 {
		host.Resources.NanoCPUs = s.NanoCPUs
	}
	if s.PidsLimit > 0 {
		p := s.PidsLimit
		host.Resources.PidsLimit = &p
	}
	if s.Runtime != "" {
		host.Runtime = s.Runtime
	}
	return host
}
```

- [ ] **Step 5: Run:** `go test ./internal/runtime/ -run TestBuildHostConfig` — PASS. Then `go build ./...` clean.

- [ ] **Step 6: Commit:**
```bash
git add internal/runtime
git commit --no-verify -m "feat(runtime): ContainerSpec cgroup limits + runtime via buildHostConfig (sp-ach)"
```

---

## Task 2: Manager applies limits + runtime

**Files:** Modify `internal/spawnlet/manager.go`, `cmd/spawnlet/main.go`; create `internal/spawnlet/limits_test.go`.

> Context: `manager.Create` starts the sidecar (`runtime.ContainerSpec{Image, Env}`) then the agent (`runtime.ContainerSpec{Image, NetnsOf, Env, Mounts, AttachStdio}`). `ManagerConfig` has `AgentImage, SidecarImage, OpenRouterKey, DataRoot string; SidecarPort int; NodeClass string; EgressEnforce bool; EgressAllowCIDRs []string`. `NewManager` defaults SidecarPort + sets `fw`. `FakeRuntime.Started []ContainerSpec` records every StartContainer spec.

- [ ] **Step 1: Failing test** — create `internal/spawnlet/limits_test.go`:
```go
package spawnlet

import (
	"context"
	"testing"

	"spawnery/internal/runtime"
)

func TestCreateAppliesResourceLimits(t *testing.T) {
	rt := runtime.NewFake()
	m := NewManager(rt, ManagerConfig{
		AgentImage: "a", SidecarImage: "s", DataRoot: t.TempDir(),
		MemLimitMB: 512, CPULimit: 2.0, PidsLimit: 128, ContainerRuntime: "runsc",
		// EgressEnforce defaults false (NodeClass ""), so no firewall in this test
	})
	if _, err := m.Create(context.Background(), "sp1", "../../examples/secret-app", "model"); err != nil {
		t.Fatal(err)
	}
	if len(rt.Started) != 2 {
		t.Fatalf("want sidecar+agent started, got %d", len(rt.Started))
	}
	for i, spec := range rt.Started {
		if spec.MemoryBytes != 512<<20 {
			t.Fatalf("spec[%d] Memory = %d (want %d)", i, spec.MemoryBytes, 512<<20)
		}
		if spec.NanoCPUs != 2_000_000_000 {
			t.Fatalf("spec[%d] NanoCPUs = %d", i, spec.NanoCPUs)
		}
		if spec.PidsLimit != 128 {
			t.Fatalf("spec[%d] PidsLimit = %d", i, spec.PidsLimit)
		}
		if spec.Runtime != "runsc" {
			t.Fatalf("spec[%d] Runtime = %q", i, spec.Runtime)
		}
	}
}

func TestNewManagerLimitDefaults(t *testing.T) {
	rt := runtime.NewFake()
	m := NewManager(rt, ManagerConfig{AgentImage: "a", SidecarImage: "s", DataRoot: t.TempDir()})
	if _, err := m.Create(context.Background(), "sp1", "../../examples/secret-app", "model"); err != nil {
		t.Fatal(err)
	}
	// defaults: 1024MB, 1 CPU, 256 pids, empty runtime
	s := rt.Started[0]
	if s.MemoryBytes != 1024<<20 || s.NanoCPUs != 1_000_000_000 || s.PidsLimit != 256 || s.Runtime != "" {
		t.Fatalf("default limits wrong: mem=%d cpu=%d pids=%d rt=%q", s.MemoryBytes, s.NanoCPUs, s.PidsLimit, s.Runtime)
	}
}
```

- [ ] **Step 2: Confirm failure:** `go test ./internal/spawnlet/ -run 'TestCreateAppliesResourceLimits|TestNewManagerLimitDefaults' 2>&1 | head`.

- [ ] **Step 3: `ManagerConfig` + `NewManager` defaults** (`manager.go`): add fields `MemLimitMB int64`, `CPULimit float64`, `PidsLimit int64`, `ContainerRuntime string`. In `NewManager`, after the `SidecarPort` default, add:
```go
	if cfg.MemLimitMB == 0 {
		cfg.MemLimitMB = 1024
	}
	if cfg.CPULimit == 0 {
		cfg.CPULimit = 1.0
	}
	if cfg.PidsLimit == 0 {
		cfg.PidsLimit = 256
	}
```
(So an explicit limit of 0 still becomes the default — acceptable for the demo; "truly unlimited" isn't a needed config. Document in the struct comment.)

- [ ] **Step 4: Apply in `Create`** — compute once and set on BOTH specs:
```go
	mem := m.cfg.MemLimitMB << 20
	cpus := int64(m.cfg.CPULimit * 1e9)
	pids := m.cfg.PidsLimit
	rtName := m.cfg.ContainerRuntime
```
Add `MemoryBytes: mem, NanoCPUs: cpus, PidsLimit: pids, Runtime: rtName,` to the sidecar `runtime.ContainerSpec{...}` and the agent `runtime.ContainerSpec{...}`.

- [ ] **Step 5: `cmd/spawnlet/main.go`** — add to the `ManagerConfig{...}` literal (helpers `getenvInt64`/`getenvFloat` may need adding; reuse `env`):
```go
		MemLimitMB:       getenvInt64("MEM_LIMIT_MB", 1024),
		CPULimit:         getenvFloat("CPU_LIMIT", 1.0),
		PidsLimit:        getenvInt64("PIDS_LIMIT", 256),
		ContainerRuntime: os.Getenv("CONTAINER_RUNTIME"),
```
Add helpers (with `"strconv"` import):
```go
func getenvInt64(k string, def int64) int64 {
	if v := os.Getenv(k); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			return n
		}
	}
	return def
}
func getenvFloat(k string, def float64) float64 {
	if v := os.Getenv(k); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			return f
		}
	}
	return def
}
```

- [ ] **Step 6: Run:** `go test ./internal/spawnlet/ -run 'TestCreateAppliesResourceLimits|TestNewManagerLimitDefaults'` — PASS. Then `go test ./internal/spawnlet/` (existing tests unaffected — they don't assert on limits) + `go build ./...`.

- [ ] **Step 7: Commit:**
```bash
git add internal/spawnlet/manager.go internal/spawnlet/limits_test.go cmd/spawnlet/main.go
git commit --no-verify -m "feat(spawnlet): apply per-spawn mem/cpu/pids limits + runtime (sp-ach)"
```

---

## Task 3: Per-user concurrency cap (CP)

**Files:** Modify `internal/cp/server.go`, `cmd/cp/main.go`; create `internal/cp/quota_test.go`.

> Context: `CreateSpawn` (server.go) starts with `owner, ok := auth.OwnerFromContext(ctx)` then resolves the app version. `s.st.Spawns().ListByOwner(ctx, owner)` returns the owner's non-deleted spawns. `Server` struct + `NewServer(reg, rt, sched, st, tel)`.

- [ ] **Step 1: Failing test** — create `internal/cp/quota_test.go`:
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

func TestPerUserSpawnCap(t *testing.T) {
	s, _, _ := newTestServer(t)
	s.SetMaxSpawnsPerOwner(1)
	ctx := auth.WithOwner(context.Background(), "alice")
	// seed one existing non-deleted spawn for alice directly in the store.
	if err := s.st.Spawns().Create(ctx, store.Spawn{
		ID: "existing", OwnerID: "alice", AppID: "secret-app", AppVersion: "1.0.0", AppRef: "examples/secret-app",
		Model: "m", Status: store.Active, CreatedAt: 1, LastUsedAt: 1,
	}, nil); err != nil {
		t.Fatal(err)
	}
	// the next create must be rejected before provisioning.
	_, err := s.CreateSpawn(ctx, connect.NewRequest(&cpv1.CreateSpawnRequest{AppId: "secret-app", Model: "m"}))
	if connect.CodeOf(err) != connect.CodeResourceExhausted {
		t.Fatalf("want ResourceExhausted, got %v", err)
	}
}

func TestPerUserSpawnCapUnlimited(t *testing.T) {
	s, _, _ := newTestServer(t)
	// default cap 0 = unlimited: with a seeded spawn, a cap check must NOT reject.
	ctx := auth.WithOwner(context.Background(), "alice")
	if err := s.st.Spawns().Create(ctx, store.Spawn{
		ID: "existing", OwnerID: "alice", AppID: "secret-app", AppVersion: "1.0.0", AppRef: "examples/secret-app",
		Model: "m", Status: store.Active, CreatedAt: 1, LastUsedAt: 1,
	}, nil); err != nil {
		t.Fatal(err)
	}
	// We can't easily complete a real provision here, so assert the cap check specifically is NOT the
	// failure: call the (to-be-added) helper directly.
	if err := s.checkSpawnQuota(ctx, "alice"); err != nil {
		t.Fatalf("unlimited cap must not reject: %v", err)
	}
}
```
> The second test calls an unexported helper `checkSpawnQuota(ctx, owner) error` (returns a connect error or nil) — implement the cap as that helper so it's directly testable without provisioning. The first test exercises it through `CreateSpawn` (rejection happens before provisioning, so no node needed).

- [ ] **Step 2: Confirm failure:** `go test ./internal/cp/ -run TestPerUserSpawnCap 2>&1 | head` (no `SetMaxSpawnsPerOwner`/`checkSpawnQuota`).

- [ ] **Step 3: Implement on `Server`** (`server.go`): add field `maxSpawnsPerOwner int` to the struct; add:
```go
// SetMaxSpawnsPerOwner sets the per-owner concurrent-spawn cap (0 = unlimited).
func (s *Server) SetMaxSpawnsPerOwner(n int) { s.maxSpawnsPerOwner = n }

// checkSpawnQuota returns ResourceExhausted if the owner is at/over the per-owner spawn cap.
func (s *Server) checkSpawnQuota(ctx context.Context, owner string) error {
	if s.maxSpawnsPerOwner <= 0 {
		return nil
	}
	existing, err := s.st.Spawns().ListByOwner(ctx, owner)
	if err != nil {
		return connect.NewError(connect.CodeInternal, err)
	}
	if len(existing) >= s.maxSpawnsPerOwner {
		return connect.NewError(connect.CodeResourceExhausted, fmt.Errorf("spawn limit reached (%d)", s.maxSpawnsPerOwner))
	}
	return nil
}
```
In `CreateSpawn`, right after the `owner, ok := auth.OwnerFromContext(ctx)` check:
```go
	if err := s.checkSpawnQuota(ctx, owner); err != nil {
		return nil, err
	}
```

- [ ] **Step 4: `cmd/cp/main.go`** — after `NewServer(...)`, call `srv.SetMaxSpawnsPerOwner(envInt("CP_MAX_SPAWNS_PER_OWNER", 5))` (use the server var's actual name from main.go). Add an `envInt` helper:
```go
func envInt(k string, def int) int {
	if v := os.Getenv(k); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}
```
(add `"strconv"` import if missing).

- [ ] **Step 5: Run:** `go test ./internal/cp/ -run TestPerUserSpawnCap` — PASS.

- [ ] **Step 6: Full package + race + build:** `go test ./internal/cp/ -race && go build ./...` — PASS/clean (existing tests use the default cap 0 → no rejection).

- [ ] **Step 7: Commit:**
```bash
git add internal/cp/server.go internal/cp/quota_test.go cmd/cp/main.go
git commit --no-verify -m "feat(cp): per-user concurrent-spawn cap (sp-ach)"
```

---

## Final Verification
- [ ] `go build ./... && go build -tags e2e ./... && go vet ./...` — clean.
- [ ] `go test ./...` — pass; `go test ./internal/cp/ ./internal/spawnlet/... ./internal/runtime/ -race` — race-clean.

Then **superpowers:finishing-a-development-branch** (Option 1: merge to master locally). **At merge, note that real cgroup enforcement + gVisor isolation are unverified in this sandbox and must be validated on the node host.**

---

## Self-Review Notes
- **Spec coverage:** §2.1 ContainerSpec+buildHostConfig → T1; §2.2/2.3 manager+cmd → T2; §3 cap → T3. Out-of-scope (disk/GPU quotas, toolset, per-app) absent. ✓
- **Types:** `ContainerSpec.{MemoryBytes,NanoCPUs,PidsLimit,Runtime}`, `buildHostConfig`, `ManagerConfig.{MemLimitMB,CPULimit,PidsLimit,ContainerRuntime}`, `Server.maxSpawnsPerOwner`/`SetMaxSpawnsPerOwner`/`checkSpawnQuota` consistent. ✓
- **No-churn wiring:** `SetMaxSpawnsPerOwner` setter keeps `NewServer` + its callers (`newTestServer`, `e2e`) unchanged. ✓
- **Existing tests:** spawnlet tests don't assert limits (now defaulted — harmless); cp tests use cap 0 (no rejection). ✓
- **Verify-on-host:** cgroup/gVisor real effect is not hermetic — documented; the hermetic tests assert the spec/HostConfig carry the values. ✓
