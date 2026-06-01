# Phase 1 Isolation Hardening + runsc Readiness — Implementation Plan

> REQUIRED SUB-SKILL: superpowers:subagent-driven-development. Checkbox steps.

**Spec:** `docs/superpowers/specs/2026-06-01-phase1-isolation-hardening.md`. Branch `sp-ff2-host-floor` off master. Beads: `sp-ff2` (P0 floor), `sp-s9u` (hardening), runsc-preflight. Commits `--no-verify`. **The host-floor mechanism is empirically host-verified** — DOCKER-USER `-s <ip>` rules enforce under runc+runsc.

---

## Task 1: `firewall` package → host `DOCKER-USER`, IP-matched, with Remove

**Files:** `internal/spawnlet/firewall/firewall.go`, `firewall_test.go`.

- [ ] **Step 1: Rewrite `Rules` to take an IP + emit DOCKER-USER args** (`firewall.go`):
```go
// Rules returns the per-pod egress block-floor for the given pod bridge IP, as DOCKER-USER chain
// arg-lists (everything after "iptables"). Matched by source IP so multiple pods coexist in the
// shared chain. Order (top-to-bottom once applied): DNS + allowCIDRs ACCEPT, then metadata + RFC1918
// DROP. No loopback rule — agent<->sidecar (127.0.0.1) is never forwarded, so the host chain never
// sees it. Applied on the HOST (works under runc AND gVisor/runsc, where in-netns iptables is a no-op).
func Rules(ip string, allowCIDRs []string) []Rule {
	var rules []Rule
	add := func(args ...string) { rules = append(rules, Rule{Args: append([]string{"-s", ip}, args...)}) }
	add("-p", "udp", "--dport", "53", "-j", "ACCEPT")
	add("-p", "tcp", "--dport", "53", "-j", "ACCEPT")
	for _, c := range allowCIDRs {
		add("-d", c, "-j", "ACCEPT")
	}
	for _, c := range []string{"169.254.0.0/16", "10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16"} {
		add("-d", c, "-j", "DROP")
	}
	return rules
}
```

- [ ] **Step 2: Replace the `Applier` interface + `NsenterApplier` with a host applier:**
```go
const chain = "DOCKER-USER"

// Applier installs/removes egress-floor rules on the host's DOCKER-USER chain.
type Applier interface {
	Apply(ctx context.Context, rules []Rule) error
	Remove(ctx context.Context, rules []Rule) error
}

// HostFloorApplier runs the host's iptables against the DOCKER-USER chain. Requires iptables + root
// (CAP_NET_ADMIN). Inserts rules in REVERSE with -I so the final top-to-bottom order matches Rules().
type HostFloorApplier struct{}

func (HostFloorApplier) Apply(ctx context.Context, rules []Rule) error {
	for i := len(rules) - 1; i >= 0; i-- {
		if err := run(ctx, append([]string{"-I", chain}, rules[i].Args...)); err != nil {
			return err
		}
	}
	return nil
}

func (HostFloorApplier) Remove(ctx context.Context, rules []Rule) error {
	var firstErr error
	for _, r := range rules {
		if err := run(ctx, append([]string{"-D", chain}, r.Args...)); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

func run(ctx context.Context, args []string) error {
	out, err := exec.CommandContext(ctx, "iptables", args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("iptables %v: %w (%s)", args, err, out)
	}
	return nil
}
```
(`Rule` stays `struct{ Args []string }`. Drop the pid/`nsenter` code. Update the package doc comment to "applied on the host DOCKER-USER chain, matched by pod IP".)

- [ ] **Step 3: Rewrite `firewall_test.go`** for the new shape:
```go
func TestRulesHostFloor(t *testing.T) {
	rules := Rules("172.17.0.5", []string{"10.9.9.0/24"})
	var lines []string
	for _, r := range rules {
		lines = append(lines, strings.Join(r.Args, " "))
	}
	all := strings.Join(lines, "\n")
	// every rule scoped to the source IP
	for _, l := range lines {
		if !strings.HasPrefix(l, "-s 172.17.0.5 ") {
			t.Fatalf("rule not scoped to -s ip: %q", l)
		}
	}
	for _, cidr := range []string{"169.254.0.0/16", "10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16"} {
		if !strings.Contains(all, "-d "+cidr+" -j DROP") {
			t.Fatalf("missing DROP for %s", cidr)
		}
	}
	if !strings.Contains(all, "-d 10.9.9.0/24 -j ACCEPT") {
		t.Fatal("missing allow-CIDR ACCEPT")
	}
	// DNS + allow ACCEPTs precede the first DROP
	udp, firstDrop := -1, -1
	for i, l := range lines {
		if strings.Contains(l, "--dport 53 -j ACCEPT") && udp == -1 { udp = i }
		if strings.Contains(l, "-j DROP") && firstDrop == -1 { firstDrop = i }
	}
	if udp == -1 || firstDrop == -1 || udp > firstDrop {
		t.Fatalf("DNS ACCEPT (%d) must precede first DROP (%d)", udp, firstDrop)
	}
}
```

- [ ] **Step 3b: Update the build-tagged `egress_e2e_test.go`** (same package — the interface change breaks it under `-tags egress_e2e`). Rewrite it to the host approach: start a `curlimages/curl` container, `docker inspect -f '{{.NetworkSettings.IPAddress}}'` for its IP (or `runtime.NewDocker().ContainerIP` once Task 2 lands — but to keep this task self-contained, shell `docker inspect`), apply `firewall.HostFloorApplier{}.Apply(ctx, firewall.Rules(ip, nil))`, `defer ...Remove(...)`, then assert (via `docker exec ... curl`) metadata + RFC1918 blocked + public-IP `1.1.1.1` reachable. Mirror the prior structure; it stays `//go:build egress_e2e` + host-gated. Ensure it at least compiles (`go vet -tags egress_e2e ./internal/spawnlet/firewall/`).
- [ ] **Step 4:** `go test ./internal/spawnlet/firewall/` PASS (delete the old `TestRulesBlockFloor`/`TestRulesAllowCIDRsBeforeDrops`/`TestRulesAllowDNSBeforeDrops` — superseded by `TestRulesHostFloor`). `go vet -tags egress_e2e ./internal/spawnlet/firewall/` compiles. `go build ./...` will fail until Task 2 (manager + FakeRuntime use the new interface) — expected; note it, don't try to fix outside this package here.

- [ ] **Step 5: Commit:** `git add internal/spawnlet/firewall && git commit --no-verify -m "feat(spawnlet): host DOCKER-USER egress floor (IP-matched, +Remove) replacing in-netns nsenter (sp-ff2)"`

---

## Task 2: Runtime `ContainerIP` + manager apply/remove by IP (fail-closed)

**Files:** `internal/runtime/runtime.go`, `internal/runtime/docker.go`, `internal/spawnlet/manager.go`, `internal/spawnlet/store.go`, `internal/spawnlet/manager_egress_test.go`.

- [ ] **Step 1: Runtime `ContainerIP`** — add to `ContainerRuntime` interface: `ContainerIP(ctx context.Context, id string) (string, error)`. Docker (`docker.go`):
```go
func (d *Docker) ContainerIP(ctx context.Context, id string) (string, error) {
	j, err := d.cli.ContainerInspect(ctx, id)
	if err != nil {
		return "", err
	}
	ip := ""
	if j.NetworkSettings != nil {
		ip = j.NetworkSettings.DefaultNetworkSettings.IPAddress
	}
	if ip == "" {
		return "", fmt.Errorf("container %s has no bridge IP", id)
	}
	return ip, nil
}
```
(Confirm the field path: `types.ContainerJSON.NetworkSettings.DefaultNetworkSettings.IPAddress` — the top-level `.IPAddress` for the default bridge. If the SDK version exposes it as `.NetworkSettings.IPAddress`, use that.) `FakeRuntime`: `func (f *FakeRuntime) ContainerIP(_ context.Context, id string) (string, error) { return "172.17.0.99", nil }`.

- [ ] **Step 2: `Spawn.FloorIP`** — add `FloorIP string` to the `Spawn` struct (`store.go`).

- [ ] **Step 3: Manager `Create`** — replace the firewall block:
```go
	var floorIP string
	if m.egressEnforced() {
		ip, ferr := m.rt.ContainerIP(ctx, sidecarID)
		if ferr == nil {
			floorIP = ip
			ferr = m.fw.Apply(ctx, firewall.Rules(ip, m.cfg.EgressAllowCIDRs))
		}
		if ferr != nil {
			_ = m.rt.StopContainer(ctx, sidecarID)
			finalizeAll()
			return nil, fmt.Errorf("egress floor (fail-closed): %w", ferr)
		}
	}
```
Set `FloorIP: floorIP` on the `Spawn` struct built near the end of `Create`. `NewManager` default applier → `firewall.HostFloorApplier{}`.

- [ ] **Step 4: Manager `Stop`** — after stopping containers, remove the floor (best-effort):
```go
	if sp.FloorIP != "" {
		if err := m.fw.Remove(ctx, firewall.Rules(sp.FloorIP, m.cfg.EgressAllowCIDRs)); err != nil {
			log.Printf("egress floor cleanup for %s (ip %s): %v", id, sp.FloorIP, err)
		}
	}
```
(add `"log"` import to manager.go if missing.)

- [ ] **Step 5: Update `manager_egress_test.go`** — the `failApplier` now implements the new interface:
```go
type fakeApplier struct{ applied, removed bool; failApply bool }
func (f *fakeApplier) Apply(ctx context.Context, rules []firewall.Rule) error { f.applied = true; if f.failApply { return errors.New("boom") }; return nil }
func (f *fakeApplier) Remove(ctx context.Context, rules []firewall.Rule) error { f.removed = true; return nil }
```
Update the three existing tests: fail-closed (failApply=true → Create errors + sidecar "fake-1" stopped + agent not started); self-hosted-disabled (no Apply); cloud-forces (Apply called even with EgressEnforce=false). Add a test: a successful Create then `Stop` calls `Remove` (`fa.removed == true`). The fake applier is injected via `m.fw = fa` (same package).

- [ ] **Step 6:** `go test ./internal/spawnlet/ ./internal/runtime/ && go build ./...` — PASS/clean.

- [ ] **Step 7: Commit:** `git add internal/runtime internal/spawnlet && git commit --no-verify -m "feat(spawnlet): apply/remove host egress floor by pod IP; ContainerIP (sp-ff2)"`

---

## Task 3: runsc startup preflight (fail-fast)

**Files:** `internal/spawnlet/manager.go` (or `runtime`), `cmd/spawnlet/main.go`, a test.

- [ ] **Step 1: `Manager.PreflightRuntime(ctx)`** (manager.go): if `m.cfg.ContainerRuntime == ""` return nil; else start a smoke container under it and stop it:
```go
// PreflightRuntime validates a configured non-default container runtime (e.g. runsc) at startup by
// running a throwaway smoke container. Returns an error if the runtime can't run a container —
// callers should fail hard rather than discover this at first CreateSpawn.
func (m *Manager) PreflightRuntime(ctx context.Context) error {
	if m.cfg.ContainerRuntime == "" {
		return nil
	}
	id, err := m.rt.StartContainer(ctx, runtime.ContainerSpec{Image: m.cfg.AgentImage, Cmd: []string{"true"}, Runtime: m.cfg.ContainerRuntime})
	if err != nil {
		return fmt.Errorf("runtime %q preflight: %w", m.cfg.ContainerRuntime, err)
	}
	_ = m.rt.StopContainer(context.WithoutCancel(ctx), id)
	return nil
}
```

- [ ] **Step 2: Wire `cmd/spawnlet/main.go`** — after `NewManager(...)` and before serving (both standalone + CP-attached paths, or just before the CP-attached `node.Run`): 
```go
	if err := mgr.PreflightRuntime(ctx); err != nil {
		log.Fatalf("container runtime preflight failed: %v", err)
	}
```
(place it where `mgr` is in scope, before the node attaches / listener starts.)

- [ ] **Step 3: Test** (`internal/spawnlet/`): a `FakeRuntime` variant whose `StartContainer` errors when `spec.Runtime != ""` → `PreflightRuntime` returns an error with `ContainerRuntime` set; returns nil when `ContainerRuntime==""`. (Use the existing `runtime.NewFake()` for the empty case; for the error case, a tiny local fake or extend FakeRuntime with an `errOnRuntime` flag — keep it local to the test to avoid touching the shared fake.)

- [ ] **Step 4:** `go test ./internal/spawnlet/ && go build ./...` PASS/clean.

- [ ] **Step 5: Commit:** `git add internal/spawnlet/manager.go internal/spawnlet/*_test.go cmd/spawnlet/main.go && git commit --no-verify -m "feat(spawnlet): startup runtime preflight (fail-fast on broken runsc)"`

---

## Task 4: Container hardening — drop caps + (gated) ro-rootfs

**Files:** `internal/runtime/runtime.go`, `internal/runtime/docker.go`, `internal/spawnlet/manager.go`, `cmd/spawnlet/main.go`, `internal/runtime/hostconfig_test.go`.

- [ ] **Step 1:** `ContainerSpec` += `DropAllCaps bool`, `ReadonlyRootfs bool`. `buildHostConfig` (docker.go):
```go
	if s.DropAllCaps {
		host.CapDrop = []string{"ALL"}
	}
	if s.ReadonlyRootfs {
		host.ReadonlyRootfs = true
		if host.Tmpfs == nil {
			host.Tmpfs = map[string]string{}
		}
		host.Tmpfs["/tmp"] = ""
	}
```
(`host.CapDrop` is `strslice.StrSlice` — `[]string{"ALL"}` assigns fine.)

- [ ] **Step 2:** Manager `Create` — set on the **agent** spec: `DropAllCaps: true` (always) and `ReadonlyRootfs: m.cfg.HardenRootfs`. Add `HardenRootfs bool` to `ManagerConfig`. (Do NOT harden the sidecar.)

- [ ] **Step 3:** `cmd/spawnlet/main.go` — `HardenRootfs: getenvBool("HARDEN_ROOTFS", false)` in the ManagerConfig literal.

- [ ] **Step 4:** `hostconfig_test.go` — add a case: spec with `DropAllCaps:true, ReadonlyRootfs:true` → `host.CapDrop==["ALL"]`, `host.ReadonlyRootfs==true`, `host.Tmpfs["/tmp"]` present; without the flags they're unset.

- [ ] **Step 5:** `go test ./internal/runtime/ ./internal/spawnlet/ && go build ./...` PASS/clean.

- [ ] **Step 6: Commit:** `git add internal/runtime internal/spawnlet/manager.go cmd/spawnlet/main.go && git commit --no-verify -m "feat(spawnlet): drop-all-caps + gated read-only rootfs on the agent (sp-s9u)"`

---

## Final Verification
- [ ] `go build ./... && go build -tags egress_e2e ./... && go vet ./... && go test ./...` — clean/pass. (`egress_e2e` test will need its applier call updated to the host shape — update it to `firewall.HostFloorApplier{}.Apply(ctx, firewall.Rules(ip, nil))` using a real container IP; it's host-gated.)
- Controller then verifies under runsc locally (real spawn + host floor + preflight) before merge.

## Self-Review Notes
- Interface change (`Apply(ctx,pid)` → `Apply(ctx,rules)`+`Remove`) touches firewall + manager + fake + tests + egress_e2e — all listed.
- `ContainerIP` added to Docker + FakeRuntime (only impls).
- Floor lifecycle: apply on create (fail-closed), remove on stop (DOCKER-USER persists). `FloorIP` tracked on Spawn.
- ro-rootfs default OFF (`HARDEN_ROOTFS`) pending goose verification; caps-drop default ON (safe).
