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
//
//	go test -tags cni_egress_e2e ./internal/spawnlet/firewall/ -run TestCNIEgressFloorEnforced
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
