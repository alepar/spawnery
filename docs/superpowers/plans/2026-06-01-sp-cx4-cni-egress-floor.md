# CRI Egress Floor (SPAWNLET-EGRESS) + Host Ops (sp-cx4) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add a `CNIFloorApplier` that enforces the per-pod egress floor on a spawnlet-owned `SPAWNLET-EGRESS` iptables chain (jumped from `FORWARD`), for CRI/CNI-bridge pods where `DOCKER-USER` is not in the forwarding path — plus the containerd/CNI host-ops docs.

**Architecture:** `CNIFloorApplier` implements the existing `firewall.Applier` interface and **reuses the existing `firewall.Rules(ip, allowCIDRs)`** (the same per-pod, source-IP-scoped block-floor content). The only differences from the Docker `HostFloorApplier`: (1) it targets a `SPAWNLET-EGRESS` chain instead of `DOCKER-USER`, and (2) it **creates that chain + inserts a `FORWARD` jump** (idempotently), since nothing else provides it. An injectable command runner makes the iptables invocations hermetically testable. The Docker floor is untouched.

**Tech Stack:** Go 1.26, the existing `internal/spawnlet/firewall` package, iptables (host-gated e2e only).

**Scope:** Node only, the `firewall` package + docs. No CP/proto/manifest/web changes, and **no Manager wiring** — selecting `CNIFloorApplier` vs `HostFloorApplier` by `CONTAINER_RUNTIME` is slice 5 (`sp-ghx`). This slice builds the applier standalone (like slice 3 built the CRI backend standalone). This dev sandbox has **no iptables/root/docker** — hermetic tests (command construction via a fake runner) run here; the real-iptables enforcement test is build-tagged + host-gated. Commits use `--no-verify` (the `.beads` export hook dirties commits).

---

## Background facts (verified against the current code)

- `firewall.Rule` = `struct{ Args []string }` (an iptables arg-list, everything after `iptables`).
- `firewall.Rules(ip string, allowCIDRs []string) []Rule` returns the per-pod floor scoped `-s ip`: DNS udp/tcp `:53` ACCEPT, allowCIDRs ACCEPT, then DROP `169.254.0.0/16` + RFC1918. **Reused as-is.**
- `firewall.Applier` interface = `Apply(ctx, []Rule) error` + `Remove(ctx, []Rule) error`.
- `HostFloorApplier` (Docker) inserts with `-I DOCKER-USER` in **reverse** so the final top-to-bottom order matches `Rules()`; removes with `-D DOCKER-USER`. Uses a package-level `run(ctx, args)` that shells out to `iptables`.
- The chosen design is **per-pod scoped rules** (Option A): `SPAWNLET-EGRESS` holds only `-s <podIP>` rules; non-spawn forwarded traffic doesn't match any and falls through to the rest of `FORWARD`. (No unscoped static drops — keeps the floor pod-scoped and reuses `Rules()`.)

---

## File Structure

**New files:**
- `internal/spawnlet/firewall/cni.go` — `CNIFloorApplier` (`EnsureChain`/`Apply`/`Remove`) on `SPAWNLET-EGRESS`, with an injectable runner.
- `internal/spawnlet/firewall/cni_test.go` — hermetic tests (command construction via a fake runner: chain create/jump idempotency, reversed inserts, removes).
- `internal/spawnlet/firewall/cni_egress_e2e_test.go` — build-tagged (`cni_egress_e2e`) host-gated real-iptables enforcement test.

**Modified files:**
- `Justfile` — a `test-cni-egress` recipe (mirrors `test-egress`).
- `deployment.md` — §5 CRI-pod floor (`SPAWNLET-EGRESS`) + containerd runsc-handler + CNI conflist host prereqs.
- `ISOLATION.md` — §3.1 note the floor chain differs per backend (DOCKER-USER vs SPAWNLET-EGRESS).

---

## Task 1: CNIFloorApplier + tests

**Files:**
- Create: `internal/spawnlet/firewall/cni.go`
- Test: `internal/spawnlet/firewall/cni_test.go`
- Create: `internal/spawnlet/firewall/cni_egress_e2e_test.go`
- Modify: `Justfile`

- [ ] **Step 1: Write the failing hermetic test**

Create `internal/spawnlet/firewall/cni_test.go` (package `firewall`, white-box — it sets the unexported `run` field):

```go
package firewall

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// recRunner records iptables arg-lists and fails any call whose joined args start with a failPrefix
// (used to simulate "chain/jump absent" so the check-then-create path runs).
type recRunner struct {
	calls        [][]string
	failPrefixes []string
}

func (r *recRunner) run(_ context.Context, args []string) error {
	r.calls = append(r.calls, args)
	joined := strings.Join(args, " ")
	for _, p := range r.failPrefixes {
		if strings.HasPrefix(joined, p) {
			return errors.New("simulated: absent")
		}
	}
	return nil
}

func (r *recRunner) joined() []string {
	out := make([]string, len(r.calls))
	for i, c := range r.calls {
		out[i] = strings.Join(c, " ")
	}
	return out
}

func contains(lines []string, want string) bool {
	for _, l := range lines {
		if l == want {
			return true
		}
	}
	return false
}

func TestCNIEnsureChainCreatesWhenAbsent(t *testing.T) {
	// chain absent (-nL fails) and jump absent (-C fails) -> must create chain + insert jump at pos 1.
	rec := &recRunner{failPrefixes: []string{"-nL SPAWNLET-EGRESS", "-C FORWARD -j SPAWNLET-EGRESS"}}
	a := &CNIFloorApplier{run: rec.run}
	if err := a.EnsureChain(context.Background()); err != nil {
		t.Fatalf("EnsureChain: %v", err)
	}
	lines := rec.joined()
	if !contains(lines, "-N SPAWNLET-EGRESS") {
		t.Fatalf("missing chain create; calls=%v", lines)
	}
	if !contains(lines, "-I FORWARD 1 -j SPAWNLET-EGRESS") {
		t.Fatalf("missing FORWARD jump at pos 1; calls=%v", lines)
	}
}

func TestCNIEnsureChainIdempotentWhenPresent(t *testing.T) {
	// chain + jump present (no failPrefixes) -> must NOT create/insert.
	rec := &recRunner{}
	a := &CNIFloorApplier{run: rec.run}
	if err := a.EnsureChain(context.Background()); err != nil {
		t.Fatalf("EnsureChain: %v", err)
	}
	for _, l := range rec.joined() {
		if l == "-N SPAWNLET-EGRESS" || strings.HasPrefix(l, "-I FORWARD") {
			t.Fatalf("must not create/insert when present; got %q", l)
		}
	}
}

func TestCNIApplyInsertsReversedIntoChain(t *testing.T) {
	rec := &recRunner{} // chain present
	a := &CNIFloorApplier{run: rec.run}
	rules := Rules("10.244.0.7", nil)
	if err := a.Apply(context.Background(), rules); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	lines := rec.joined()
	// Every rule insert targets the SPAWNLET-EGRESS chain via -I.
	var inserts []string
	for _, l := range lines {
		if strings.HasPrefix(l, "-I SPAWNLET-EGRESS ") {
			inserts = append(inserts, l)
		}
	}
	if len(inserts) != len(rules) {
		t.Fatalf("want %d rule inserts, got %d (%v)", len(rules), len(inserts), inserts)
	}
	// Reversed insertion: the LAST rule (a DROP) is inserted FIRST so the final order matches Rules().
	last := "-I SPAWNLET-EGRESS " + strings.Join(rules[len(rules)-1].Args, " ")
	if inserts[0] != last {
		t.Fatalf("first insert must be the last rule (reversed); got %q want %q", inserts[0], last)
	}
}

func TestCNIRemoveDeletesFromChain(t *testing.T) {
	rec := &recRunner{}
	a := &CNIFloorApplier{run: rec.run}
	rules := Rules("10.244.0.7", nil)
	if err := a.Remove(context.Background(), rules); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	for _, r := range rules {
		want := "-D SPAWNLET-EGRESS " + strings.Join(r.Args, " ")
		if !contains(rec.joined(), want) {
			t.Fatalf("missing delete %q", want)
		}
	}
}
```

- [ ] **Step 2: Run it to verify it fails**

Run: `go test ./internal/spawnlet/firewall/ -run TestCNI -v`
Expected: FAIL — `CNIFloorApplier` undefined.

- [ ] **Step 3: Implement the applier**

Create `internal/spawnlet/firewall/cni.go`:

```go
package firewall

import (
	"context"
	"fmt"
)

// cniChain is the spawnlet-owned filter chain for CRI/CNI-bridge pods, jumped from FORWARD position 1
// (in front of CNI's own CNI-FORWARD). DOCKER-USER is Docker-specific and not in a CNI pod's
// forwarding path, so the CRI floor needs its own host chain. Same per-pod Rules() content; enforces
// under runsc because the host veth still sees the pod's frames.
const cniChain = "SPAWNLET-EGRESS"

// CNIFloorApplier installs the egress floor on the SPAWNLET-EGRESS chain. Unlike HostFloorApplier
// (DOCKER-USER, provided by Docker), it creates the chain + the FORWARD jump itself. Requires
// iptables + root (CAP_NET_ADMIN). The run field is injectable for tests; nil = the real iptables.
type CNIFloorApplier struct {
	run func(ctx context.Context, args []string) error
}

// NewCNIFloorApplier returns an applier backed by the host's iptables.
func NewCNIFloorApplier() *CNIFloorApplier { return &CNIFloorApplier{} }

func (a *CNIFloorApplier) runner() func(context.Context, []string) error {
	if a.run != nil {
		return a.run
	}
	return run
}

// EnsureChain creates SPAWNLET-EGRESS and the FORWARD jump if absent (idempotent). Call at boot and
// periodically — CNI may rebuild FORWARD on a containerd restart, which would orphan the floor.
func (a *CNIFloorApplier) EnsureChain(ctx context.Context) error {
	r := a.runner()
	// Create the chain only if it doesn't exist (a check via -nL; -N errors if it already exists).
	if r(ctx, []string{"-nL", cniChain}) != nil {
		if err := r(ctx, []string{"-N", cniChain}); err != nil {
			return fmt.Errorf("create %s chain: %w", cniChain, err)
		}
	}
	// Insert the FORWARD jump at position 1 only if it isn't already present.
	if r(ctx, []string{"-C", "FORWARD", "-j", cniChain}) != nil {
		if err := r(ctx, []string{"-I", "FORWARD", "1", "-j", cniChain}); err != nil {
			return fmt.Errorf("insert FORWARD -> %s jump: %w", cniChain, err)
		}
	}
	return nil
}

// Apply ensures the chain exists, then inserts the per-pod rules. Inserted in REVERSE with -I so the
// final top-to-bottom order matches Rules() (DNS/allow ACCEPT before the DROPs).
func (a *CNIFloorApplier) Apply(ctx context.Context, rules []Rule) error {
	if err := a.EnsureChain(ctx); err != nil {
		return err
	}
	r := a.runner()
	for i := len(rules) - 1; i >= 0; i-- {
		if err := r(ctx, append([]string{"-I", cniChain}, rules[i].Args...)); err != nil {
			return err
		}
	}
	return nil
}

// Remove deletes the per-pod rules from the chain (the chain + FORWARD jump persist for other pods).
func (a *CNIFloorApplier) Remove(ctx context.Context, rules []Rule) error {
	r := a.runner()
	var firstErr error
	for _, rule := range rules {
		if err := r(ctx, append([]string{"-D", cniChain}, rule.Args...)); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}
```

- [ ] **Step 4: Run the hermetic tests to verify they pass**

Run: `go test ./internal/spawnlet/firewall/ -run TestCNI -race -v`
Expected: PASS (all 4 CNI tests). Also `go test ./internal/spawnlet/firewall/ -race -count=1` to confirm the existing `TestRulesHostFloor` still passes.

- [ ] **Step 5: Add the interface-satisfaction assertion**

Append to `internal/spawnlet/firewall/cni.go`:

```go
var _ Applier = (*CNIFloorApplier)(nil)
```

Run: `go build ./... && go vet ./internal/spawnlet/firewall/`
Expected: exit 0 (proves `CNIFloorApplier` satisfies `firewall.Applier`).

- [ ] **Step 6: Write the host-gated enforcement test (compile-only here)**

Create `internal/spawnlet/firewall/cni_egress_e2e_test.go`. It mirrors `egress_e2e_test.go` but uses `CNIFloorApplier` + `SPAWNLET-EGRESS`, and **cleans up its own chain** afterward (unlike DOCKER-USER, SPAWNLET-EGRESS is spawnlet-owned). It uses a Docker container (whose FORWARD traffic the chain matches by source IP) to prove the chain+jump+rules enforce on real iptables — the actual CRI-pod case is slice 5.

```go
//go:build cni_egress_e2e

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

// Starts a real container, ensures the SPAWNLET-EGRESS chain + FORWARD jump, applies the block-floor
// matched by the container's bridge IP, and asserts metadata + RFC1918 are unreachable while public
// egress works. Proves the spawnlet-owned chain enforces on real iptables (the CRI-pod case itself is
// slice 5). Requires Docker + iptables + root. Run on the node host:
//   go test -tags cni_egress_e2e ./internal/spawnlet/firewall/ -run TestCNIEgressFloorEnforced
func TestCNIEgressFloorEnforced(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	rt, err := runtime.NewDocker()
	if err != nil {
		t.Fatalf("docker: %v", err)
	}
	if err := rt.Ping(ctx); err != nil {
		t.Fatalf("docker ping: %v", err)
	}
	id, err := rt.StartContainer(ctx, runtime.ContainerSpec{Image: "curlimages/curl:latest", Cmd: []string{"sleep", "120"}})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	defer rt.StopContainer(context.Background(), id)

	ip, err := rt.ContainerIP(ctx, id)
	if err != nil {
		t.Fatalf("container ip: %v", err)
	}

	a := firewall.NewCNIFloorApplier()
	rules := firewall.Rules(ip, nil)
	if err := a.Apply(ctx, rules); err != nil {
		t.Fatalf("apply (needs iptables+root): %v", err)
	}
	defer cleanupSpawnletChain(t, rules, a)

	if out, err := exec.CommandContext(ctx, "docker", "exec", id, "curl", "-sS", "--max-time", "3", "http://169.254.169.254/").CombinedOutput(); err == nil {
		t.Fatalf("metadata reachable after floor: %s", out)
	}
	if out, err := exec.CommandContext(ctx, "docker", "exec", id, "curl", "-sS", "--max-time", "3", "http://10.0.0.1/").CombinedOutput(); err == nil {
		t.Fatalf("RFC1918 reachable after floor: %s", out)
	}
	out, perr := exec.CommandContext(ctx, "docker", "exec", id, "curl", "-sS", "--max-time", "10", "-o", "/dev/null", "https://1.1.1.1/").CombinedOutput()
	if ee, ok := perr.(*exec.ExitError); ok && (ee.ExitCode() == 7 || ee.ExitCode() == 28) {
		t.Fatalf("public IP unreachable after floor (floor too strict): exit %d (%s)", ee.ExitCode(), strings.TrimSpace(string(out)))
	}
}

// cleanupSpawnletChain removes the per-pod rules then tears down the spawnlet-owned chain + jump so
// the host is left clean (the chain is ours, unlike Docker's DOCKER-USER).
func cleanupSpawnletChain(t *testing.T, rules []firewall.Rule, a *firewall.CNIFloorApplier) {
	bg := context.Background()
	_ = a.Remove(bg, rules)
	for _, args := range [][]string{
		{"-D", "FORWARD", "-j", "SPAWNLET-EGRESS"},
		{"-F", "SPAWNLET-EGRESS"},
		{"-X", "SPAWNLET-EGRESS"},
	} {
		if out, err := exec.CommandContext(bg, "iptables", args...).CombinedOutput(); err != nil {
			t.Logf("cleanup iptables %v: %v (%s)", args, err, out)
		}
	}
}
```

- [ ] **Step 7: Verify the host-gated test compiles (does NOT run here — no iptables/root)**

Run: `go vet -tags cni_egress_e2e ./internal/spawnlet/firewall/`
Expected: exit 0 (compiles; the test runs only on a privileged host via the tag).

- [ ] **Step 8: Add the Justfile recipe**

In `Justfile`, after the existing `test-egress` recipe, add:

```makefile
# CRI/CNI egress-floor enforcement e2e — REAL iptables on a privileged host (needs Docker + iptables + root).
test-cni-egress:
    docker pull -q curlimages/curl:latest
    go test -tags cni_egress_e2e -c -o /tmp/cni-egress.test ./internal/spawnlet/firewall/
    sudo env "PATH=/sbin:/usr/sbin:/usr/bin:/bin:$(dirname $(command -v docker))" /tmp/cni-egress.test -test.run TestCNIEgressFloorEnforced -test.v -test.count=1
```

- [ ] **Step 9: Commit**

```bash
git add internal/spawnlet/firewall/cni.go internal/spawnlet/firewall/cni_test.go internal/spawnlet/firewall/cni_egress_e2e_test.go Justfile
git commit --no-verify -m "feat(firewall): CNIFloorApplier on SPAWNLET-EGRESS for CRI pods (sp-cx4)"
```

---

## Task 2: Host-ops docs (containerd runsc handler + CNI + the CRI floor)

Document the CRI-pod floor and the containerd/CNI host prerequisites the runsc path needs.

**Files:**
- Modify: `ISOLATION.md`
- Modify: `deployment.md`

- [ ] **Step 1: Note the per-backend floor chain in ISOLATION.md**

In `ISOLATION.md` §3.1 (Network egress), add a bullet after the "Why host-side, not in-netns" bullet:

```markdown
- **Floor chain differs by backend:** the Docker/runc path applies the floor on **`DOCKER-USER`**
  (provided by Docker). The CRI/runsc path applies the *same* per-pod `Rules()` on a spawnlet-owned
  **`SPAWNLET-EGRESS`** chain, jumped from `FORWARD` position 1 (in front of CNI's `CNI-FORWARD`),
  because `DOCKER-USER` is Docker-specific and not in a CNI-bridge pod's forwarding path. The
  spawnlet creates the chain + jump (idempotently) and re-asserts them (CNI may rebuild `FORWARD` on
  a containerd restart). Pod IP comes from the CRI `PodSandboxStatus.Network.Ip`. (Backend selection
  by `CONTAINER_RUNTIME` lands with the runsc wire-up.)
```

- [ ] **Step 2: Add the CRI host-ops section to deployment.md**

In `deployment.md` §5 (the egress floor section), after the existing "Host requirements" paragraph, add:

```markdown
**CRI/runsc pods — additional host prerequisites (the `CONTAINER_RUNTIME=runsc` path):**

- **containerd** with the CRI plugin, plus the **`runsc` runtime handler** registered. In
  `/etc/containerd/config.toml`:
  ```toml
  [plugins."io.containerd.grpc.v1.cri".containerd.runtimes.runsc]
    runtime_type = "io.containerd.runsc.v1"
  ```
  then restart containerd. (`containerd-shim-runsc-v1` + `runsc` must be on `PATH`.)
- **CNI** for pod-sandbox networking: the reference plugins (`bridge`, `host-local`, `loopback`,
  `firewall`, `portmap`) in `/opt/cni/bin/`, and a conflist at `/etc/cni/net.d/` (e.g. a `bridge` +
  `firewall` + `portmap` chain). Without CNI, `RunPodSandbox` fails.
- **The egress floor uses `SPAWNLET-EGRESS`** (not `DOCKER-USER`) for these pods — a spawnlet-owned
  `filter` chain jumped from `FORWARD` position 1. Same `iptables` + root requirement; verify with
  `just test-cni-egress` (Docker + iptables + root). The actual goose+sidecar runsc pod end-to-end is
  validated in the runsc wire-up slice.
- **Images** for the runsc path live in containerd's **`k8s.io`** namespace (separate from Docker's
  `moby`). Pull there (the node pulls via the CRI `ImageService`); bridge a locally-built image with
  `docker save <img> | ctr -n k8s.io images import -`.
```

- [ ] **Step 3: Commit**

```bash
git add ISOLATION.md deployment.md
git commit --no-verify -m "docs: SPAWNLET-EGRESS CRI floor + containerd/CNI host ops (sp-cx4)"
```

---

## Self-Review

**1. Spec coverage (spec §3.4 + slice 4):**
- `CNIFloorApplier` on a spawnlet-owned `SPAWNLET-EGRESS` chain, `-I FORWARD 1` before CNI-FORWARD, per-pod `-s <podIP>` rules (reusing `Rules()`) → Task 1. ✓
- Boot/reconcile of the chain + jump (idempotent `EnsureChain`, called in `Apply` + exported for slice-5 periodic use) → Task 1. ✓
- Docker floor (`DOCKER-USER`) untouched → confirmed (separate type, `HostFloorApplier` unchanged). ✓
- Host-gated `egress_e2e`-style enforcement test → Task 1 (`cni_egress_e2e`). ✓
- containerd runsc-handler + CNI conflist + image-store bridge docs → Task 2. ✓
- Floor *selection* by `CONTAINER_RUNTIME` is slice 5 — deliberately not wired here. ✓

**2. Placeholder scan:** No TBD/TODO; every code step is complete; every run step has an exact command + expected result.

**3. Type consistency:** `CNIFloorApplier{run func(ctx, []string) error}`, `EnsureChain`/`Apply`/`Remove`/`runner()`, and `cniChain = "SPAWNLET-EGRESS"` are used consistently across `cni.go`, the hermetic tests (which set the unexported `run` field), and the host-gated test (which uses `NewCNIFloorApplier()`). `Apply` reuses `firewall.Rules` and the reverse-insert pattern from `HostFloorApplier`. `var _ Applier = (*CNIFloorApplier)(nil)` proves interface conformance.

**4. Design note (Option A vs the research's Option B):** the floor uses **per-pod source-IP-scoped rules** (reusing `Rules()`), not unscoped static drops in the chain. This keeps the floor pod-scoped (non-spawn forwarded traffic isn't affected), reuses the proven rule content, and stays consistent with the Docker floor — the only delta is the chain name + the self-managed jump. Real-iptables enforcement under an actual CRI/runsc pod (vs the Docker-container host-gated proxy here) is validated in slice 5 (`sp-ghx`).
