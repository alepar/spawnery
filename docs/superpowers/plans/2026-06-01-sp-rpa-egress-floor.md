# Egress Allowlist Floor (sp-rpa) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax.

**Goal:** Enforce a per-pod egress block-floor — drop cloud-metadata (169.254/16) + RFC1918 from the spawn pod's network namespace — applied fail-closed at spawn creation.

**Architecture:** Node-only. A `firewall` package builds static iptables rules + applies them to the sidecar container's netns via `nsenter` (the agent shares that netns). `spawnlet/manager.go` applies the floor after the sidecar starts and before the agent starts; if it fails, the spawn aborts. No CP/proto/manifest changes (the floor is static).

**Tech Stack:** Go, Docker SDK, `nsenter`+`iptables` (host tools), os/exec.

**Source spec:** `docs/superpowers/specs/2026-06-01-egress-floor-sp-rpa.md`

**ENVIRONMENT REALITY (read this):** the dev sandbox has Docker but **no `iptables` and is non-root**, so the real packet-drop test (`egress_e2e` build tag) **cannot run here** — it must only *compile*. Verify the rule-builder + fail-closed wiring hermetically; the actual enforcement runs on the privileged node host. Do NOT weaken the e2e to pass here; gate it behind the build tag.

**Conventions:** commit `--no-verify`; bead `sp-rpa`; branch `sp-rpa-egress-floor` off master. No codegen in this slice.

---

## File Structure

| File | Responsibility | Action |
|------|----------------|--------|
| `internal/spawnlet/firewall/firewall.go` | `Rule`, `Rules(allowCIDRs)`, `Applier` iface, `NsenterApplier` | Create |
| `internal/spawnlet/firewall/firewall_test.go` | rule-builder unit test (hermetic) | Create |
| `internal/spawnlet/firewall/egress_e2e_test.go` | `//go:build egress_e2e` real-enforcement test | Create |
| `internal/runtime/runtime.go` | add `ContainerPID` to iface + `FakeRuntime` | Modify |
| `internal/runtime/docker.go` | `ContainerPID` via `ContainerInspect` | Modify |
| `internal/spawnlet/manager.go` | wire firewall (fail-closed); `Config`/`Manager` fields | Modify |
| `internal/spawnlet/manager_egress_test.go` | fail-closed unit test | Create |
| `cmd/spawnlet/main.go` | `EGRESS_ENFORCE` (default true) + `EGRESS_ALLOW_CIDRS` env | Modify |

---

## Task 1: `firewall` package — rules + applier

**Files:** Create `internal/spawnlet/firewall/firewall.go`, `internal/spawnlet/firewall/firewall_test.go`.

- [ ] **Step 1: Failing test** — `internal/spawnlet/firewall/firewall_test.go`:
```go
package firewall

import (
	"strings"
	"testing"
)

func TestRulesBlockFloor(t *testing.T) {
	rules := Rules(nil)
	joined := make([]string, len(rules))
	for i, r := range rules {
		joined[i] = strings.Join(r.Args, " ")
	}
	all := strings.Join(joined, "\n")

	// the four mandatory drops
	for _, cidr := range []string{"169.254.0.0/16", "10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16"} {
		if !strings.Contains(all, "-d "+cidr+" -j DROP") {
			t.Fatalf("missing DROP for %s in:\n%s", cidr, all)
		}
	}
	// loopback accept must come BEFORE any DROP
	loIdx, firstDrop := -1, -1
	for i, j := range joined {
		if strings.Contains(j, "-o lo -j ACCEPT") && loIdx == -1 {
			loIdx = i
		}
		if strings.Contains(j, "-j DROP") && firstDrop == -1 {
			firstDrop = i
		}
	}
	if loIdx == -1 || firstDrop == -1 || loIdx > firstDrop {
		t.Fatalf("lo ACCEPT (%d) must precede first DROP (%d)", loIdx, firstDrop)
	}
}

func TestRulesAllowCIDRsBeforeDrops(t *testing.T) {
	rules := Rules([]string{"192.168.50.0/24"})
	acceptIdx, dropIdx := -1, -1
	for i, r := range rules {
		j := strings.Join(r.Args, " ")
		if strings.Contains(j, "-d 192.168.50.0/24 -j ACCEPT") {
			acceptIdx = i
		}
		if strings.Contains(j, "-j DROP") && dropIdx == -1 {
			dropIdx = i
		}
	}
	if acceptIdx == -1 {
		t.Fatal("allow-CIDR ACCEPT rule missing")
	}
	if acceptIdx > dropIdx {
		t.Fatalf("allow-CIDR ACCEPT (%d) must precede DROPs (%d)", acceptIdx, dropIdx)
	}
}
```

- [ ] **Step 2: Confirm failure:** `go test ./internal/spawnlet/firewall/ 2>&1 | head` (no package/`Rules`).

- [ ] **Step 3: Implement `internal/spawnlet/firewall/firewall.go`:**
```go
// Package firewall builds + applies the per-pod egress block-floor (sp-rpa): drop cloud-metadata
// and RFC1918 from a spawn pod's network namespace, allowing public egress otherwise. Static rules
// (same every spawn); applied from the host into the sidecar container's netns via nsenter.
package firewall

import (
	"context"
	"fmt"
	"os/exec"
)

// Rule is one iptables invocation's arguments (everything after "iptables").
type Rule struct{ Args []string }

// Rules returns the OUTPUT-chain block-floor. allowCIDRs are ACCEPTed before the drops (for an
// operator whose model upstream / DNS resolver is on a LAN). Order matters: ACCEPTs precede DROPs.
func Rules(allowCIDRs []string) []Rule {
	rules := []Rule{
		{Args: []string{"-A", "OUTPUT", "-o", "lo", "-j", "ACCEPT"}},
	}
	for _, c := range allowCIDRs {
		rules = append(rules, Rule{Args: []string{"-A", "OUTPUT", "-d", c, "-j", "ACCEPT"}})
	}
	for _, c := range []string{"169.254.0.0/16", "10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16"} {
		rules = append(rules, Rule{Args: []string{"-A", "OUTPUT", "-d", c, "-j", "DROP"}})
	}
	return rules
}

// Applier applies firewall rules to the netns of the process with the given pid.
type Applier interface {
	Apply(ctx context.Context, pid int, rules []Rule) error
}

// NsenterApplier runs the host's iptables inside the target pid's network namespace via nsenter.
// Requires nsenter + iptables on the host and CAP_NET_ADMIN/root for this process.
type NsenterApplier struct{}

func (NsenterApplier) Apply(ctx context.Context, pid int, rules []Rule) error {
	for _, r := range rules {
		args := append([]string{"-t", fmt.Sprint(pid), "-n", "--", "iptables"}, r.Args...)
		out, err := exec.CommandContext(ctx, "nsenter", args...).CombinedOutput()
		if err != nil {
			return fmt.Errorf("nsenter iptables %v: %w (%s)", r.Args, err, out)
		}
	}
	return nil
}
```

- [ ] **Step 4: Run:** `go test ./internal/spawnlet/firewall/` — PASS.

- [ ] **Step 5: Commit:**
```bash
git add internal/spawnlet/firewall
git commit --no-verify -m "feat(spawnlet): egress block-floor rule builder + nsenter applier (sp-rpa)"
```

---

## Task 2: Runtime `ContainerPID` + manager wiring (fail-closed)

**Files:** Modify `internal/runtime/runtime.go`, `internal/runtime/docker.go`, `internal/spawnlet/manager.go`, `cmd/spawnlet/main.go`; create `internal/spawnlet/manager_egress_test.go`.

> Capable model — multi-file integration + fail-closed control flow.

- [ ] **Step 1: Failing fail-closed test** — `internal/spawnlet/manager_egress_test.go`:
```go
package spawnlet

import (
	"context"
	"errors"
	"testing"

	"spawnery/internal/runtime"
	"spawnery/internal/spawnlet/firewall"
)

type failApplier struct{ called bool }

func (f *failApplier) Apply(ctx context.Context, pid int, rules []firewall.Rule) error {
	f.called = true
	return errors.New("boom")
}

func TestCreateFailClosedWhenFirewallFails(t *testing.T) {
	rt := runtime.NewFake()
	m := NewManager(rt, ManagerConfig{AgentImage: "a", SidecarImage: "s", DataRoot: t.TempDir(), EgressEnforce: true})
	fa := &failApplier{}
	m.fw = fa // inject failing applier (same package)

	_, err := m.Create(context.Background(), "spawn1", "../../examples/secret-app", "model")
	if err == nil {
		t.Fatal("Create must fail-closed when the firewall can't be applied")
	}
	if !fa.called {
		t.Fatal("firewall applier was not called")
	}
	// the sidecar (first started container, fake-1) must have been stopped — no leak, no agent.
	if !rt.Stopped["fake-1"] {
		t.Fatalf("sidecar not stopped after fail-closed; stopped=%v", rt.Stopped)
	}
	if len(rt.Started) != 1 {
		t.Fatalf("agent must NOT start after firewall failure; started=%d", len(rt.Started))
	}
}

func TestCreateSkipsFirewallWhenDisabled(t *testing.T) {
	rt := runtime.NewFake()
	m := NewManager(rt, ManagerConfig{AgentImage: "a", SidecarImage: "s", DataRoot: t.TempDir(), EgressEnforce: false})
	fa := &failApplier{}
	m.fw = fa
	if _, err := m.Create(context.Background(), "spawn2", "../../examples/secret-app", "model"); err != nil {
		t.Fatalf("Create with enforce=false should succeed: %v", err)
	}
	if fa.called {
		t.Fatal("firewall must NOT be applied when EgressEnforce=false")
	}
}
```

- [ ] **Step 2: Confirm failure:** `go test ./internal/spawnlet/ -run TestCreateFailClosed 2>&1 | head` (no `EgressEnforce`, no `m.fw`).

- [ ] **Step 3: Add `ContainerPID` to the runtime interface** (`internal/runtime/runtime.go`):
  - In `ContainerRuntime`, add: `ContainerPID(ctx context.Context, id string) (int, error)`.
  - On `FakeRuntime`, add: `func (f *FakeRuntime) ContainerPID(_ context.Context, id string) (int, error) { return 4242, nil }`.

- [ ] **Step 4: Implement on Docker** (`internal/runtime/docker.go`):
```go
func (d *Docker) ContainerPID(ctx context.Context, id string) (int, error) {
	j, err := d.cli.ContainerInspect(ctx, id)
	if err != nil {
		return 0, err
	}
	if j.State == nil || j.State.Pid == 0 {
		return 0, fmt.Errorf("container %s has no pid (not running)", id)
	}
	return j.State.Pid, nil
}
```
Add `"fmt"` to docker.go imports if not present.

- [ ] **Step 5: Wire into `manager.go`:**
  - Add to `ManagerConfig`: `EgressEnforce bool` and `EgressAllowCIDRs []string`.
  - Add to `Manager` struct: `fw firewall.Applier`.
  - In `NewManager`, set `fw: firewall.NsenterApplier{}` (default real applier). Add import `"spawnery/internal/spawnlet/firewall"`.
  - In `Create`, AFTER the sidecar `StartContainer` succeeds (the `sidecarID` block) and BEFORE the agent `StartContainer`, insert:
```go
	if m.cfg.EgressEnforce {
		pid, err := m.rt.ContainerPID(ctx, sidecarID)
		if err == nil {
			err = m.fw.Apply(ctx, pid, firewall.Rules(m.cfg.EgressAllowCIDRs))
		}
		if err != nil {
			_ = m.rt.StopContainer(ctx, sidecarID)
			finalizeAll()
			return nil, fmt.Errorf("egress floor (fail-closed): %w", err)
		}
	}
```

- [ ] **Step 6: Wire `cmd/spawnlet/main.go`** — read env into the config (default enforce TRUE):
```go
		EgressEnforce:    getenvBool("EGRESS_ENFORCE", true),
		EgressAllowCIDRs: splitCSV(os.Getenv("EGRESS_ALLOW_CIDRS")),
```
Add small helpers if not present:
```go
func getenvBool(k string, def bool) bool {
	v := os.Getenv(k)
	if v == "" {
		return def
	}
	return v == "1" || v == "true" || v == "TRUE"
}
func splitCSV(s string) []string {
	if s == "" {
		return nil
	}
	return strings.Split(s, ",")
}
```
Add `"strings"` import if needed. (Read `cmd/spawnlet/main.go` to slot these into the existing `ManagerConfig{...}` literal and imports.)

- [ ] **Step 7: Run:** `go test ./internal/spawnlet/ -run 'TestCreateFailClosed|TestCreateSkipsFirewall'` — PASS. Then `go build ./...` clean (FakeRuntime now satisfies the extended interface; any other ContainerRuntime impls — grep `ContainerRuntime` / `StartContainer` impls — must gain `ContainerPID` too; there should only be `Docker` + `FakeRuntime`).

- [ ] **Step 8: Full spawnlet package (non-tagged):** `go test ./internal/spawnlet/ ./internal/runtime/` — PASS (existing tests use FakeRuntime with zero-value `EgressEnforce=false`, so they skip the firewall and are unaffected).

- [ ] **Step 9: Commit:**
```bash
git add internal/runtime internal/spawnlet/manager.go internal/spawnlet/manager_egress_test.go cmd/spawnlet/main.go
git commit --no-verify -m "feat(spawnlet): apply egress floor fail-closed in Create + ContainerPID (sp-rpa)"
```

---

## Task 3: Build-tagged real-enforcement e2e

**Files:** Create `internal/spawnlet/firewall/egress_e2e_test.go`.

> This test needs privileged Docker + iptables + root. It **will not run in this sandbox** (no iptables, non-root). It must COMPILE (so it stays green under the build tag) and run on the node host. Do NOT weaken it to pass here.

- [ ] **Step 1: Write the tagged test** — `internal/spawnlet/firewall/egress_e2e_test.go`:
```go
//go:build egress_e2e

package firewall_test

import (
	"context"
	"os/exec"
	"strings"
	"testing"
	"time"

	"spawnery/internal/runtime"
	"spawnery/internal/spawnlet/firewall"
)

// Starts a real container, applies the block-floor to its netns, and asserts metadata + an RFC1918
// host are unreachable while a public host is reachable. Requires privileged Docker + iptables.
// Build/run with: go test -tags egress_e2e ./internal/spawnlet/firewall/ -run TestEgressFloorEnforced
func TestEgressFloorEnforced(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	rt, err := runtime.NewDocker()
	if err != nil {
		t.Fatalf("docker: %v", err)
	}
	if err := rt.Ping(ctx); err != nil {
		t.Fatalf("docker ping: %v", err)
	}
	// a long-lived container with curl available.
	id, err := rt.StartContainer(ctx, runtime.ContainerSpec{
		Image: "curlimages/curl:latest",
		Cmd:   []string{"sleep", "120"},
	})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	defer rt.StopContainer(context.Background(), id)

	pid, err := rt.ContainerPID(ctx, id)
	if err != nil {
		t.Fatalf("pid: %v", err)
	}
	if err := (firewall.NsenterApplier{}).Apply(ctx, pid, firewall.Rules(nil)); err != nil {
		t.Fatalf("apply (needs iptables+root): %v", err)
	}

	// metadata IP must be blocked (curl fails fast).
	if out, err := exec.CommandContext(ctx, "docker", "exec", id, "curl", "-sS", "--max-time", "3", "http://169.254.169.254/").CombinedOutput(); err == nil {
		t.Fatalf("metadata reachable after floor: %s", out)
	}
	// an RFC1918 host must be blocked.
	if out, err := exec.CommandContext(ctx, "docker", "exec", id, "curl", "-sS", "--max-time", "3", "http://10.0.0.1/").CombinedOutput(); err == nil {
		t.Fatalf("RFC1918 reachable after floor: %s", out)
	}
	// a public host must still be reachable (DNS + egress).
	if out, err := exec.CommandContext(ctx, "docker", "exec", id, "curl", "-sS", "--max-time", "10", "https://api.openrouter.ai/").CombinedOutput(); err != nil {
		t.Fatalf("public host unreachable after floor (floor too strict): %v (%s)", err, strings.TrimSpace(string(out)))
	}
}
```
> Note: this uses `rt.ContainerPID` (Task 2) + `firewall` (Task 1). The container joins no special netns here — we apply directly to its own netns, which is the same mechanism the manager uses on the sidecar's netns. `curlimages/curl` is a small public image with curl.

- [ ] **Step 2: Confirm it COMPILES (does not run here):**
```
go vet -tags egress_e2e ./internal/spawnlet/firewall/
go build -tags egress_e2e ./internal/spawnlet/firewall/
```
Both must succeed. Do NOT run `go test -tags egress_e2e` here (no iptables/root — it will fail, which is correct behavior, not something to "fix"). Confirm the NON-tagged `go test ./internal/spawnlet/firewall/` still passes (only the unit test runs).

- [ ] **Step 3: Commit:**
```bash
git add internal/spawnlet/firewall/egress_e2e_test.go
git commit --no-verify -m "test(spawnlet): build-tagged real egress-enforcement e2e (sp-rpa)"
```

---

## Final Verification

- [ ] `go build ./... && go build -tags e2e ./... && go build -tags egress_e2e ./... && go vet ./...` — all clean.
- [ ] `go test ./internal/spawnlet/... ./internal/runtime/...` — pass (hermetic; firewall rule-builder + fail-closed).
- [ ] `go test ./...` — pass (no regression).
- [ ] Confirm the `egress_e2e` test is NOT run here (documented: needs privileged host).

Then **superpowers:finishing-a-development-branch** (Option 1: merge to master locally). **At merge, call out explicitly that real packet-drop enforcement is unverified in this sandbox and must be validated on the node host with `go test -tags egress_e2e ./internal/spawnlet/firewall/`.**

---

## Self-Review Notes
- **Spec coverage:** §2 rules → T1; §3 mechanism (ContainerPID + nsenter) → T1/T2; §4 fail-closed → T2; §5 files → all; §6 testing (unit + fail-closed + tagged e2e) → T1/T2/T3. Out-of-scope (manifest/CP/proto, consent, IPv6, allow-list) absent. ✓
- **Types:** `firewall.Rule{Args}`, `Rules([]string) []Rule`, `Applier.Apply(ctx,pid,[]Rule)`, `NsenterApplier`, `Manager.fw`, `ManagerConfig.EgressEnforce/EgressAllowCIDRs`, `runtime.ContainerPID` consistent across tasks. ✓
- **Library default note:** `ManagerConfig.EgressEnforce` zero-value is `false` (keeps existing FakeRuntime tests untouched); the PRODUCTION default-true lives in `cmd/spawnlet` (`getenvBool("EGRESS_ENFORCE", true)`). Documented tradeoff: fail-closed holds for the real entrypoint; a library caller must opt in.
