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

// Starts a real container, applies the block-floor to the HOST's DOCKER-USER chain matched by the
// container's bridge IP, and asserts metadata + an RFC1918 host are unreachable while a public host
// is reachable. This is the gVisor/runsc-correct approach: in-netns iptables is a no-op under runsc.
// Requires Docker + iptables + root. Build/run on the node host with:
// go test -tags egress_e2e ./internal/spawnlet/firewall/ -run TestEgressFloorEnforced
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
	id, err := rt.StartContainer(ctx, runtime.ContainerSpec{
		Image: "curlimages/curl:latest",
		Cmd:   []string{"sleep", "120"},
	})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	defer rt.StopContainer(context.Background(), id)

	// Get the container's bridge IP. The runtime's ContainerIP helper arrives in Task 2; shell out to
	// docker inspect here to keep this test self-contained.
	ipOut, err := exec.CommandContext(ctx, "docker", "inspect", "-f", "{{.NetworkSettings.IPAddress}}", id).CombinedOutput()
	if err != nil {
		t.Fatalf("inspect ip: %v (%s)", err, ipOut)
	}
	ip := strings.TrimSpace(string(ipOut))
	if ip == "" {
		t.Fatalf("empty container IP")
	}

	rules := firewall.Rules(ip, nil)
	if err := (firewall.HostFloorApplier{}).Apply(ctx, rules); err != nil {
		t.Fatalf("apply (needs iptables+root): %v", err)
	}
	defer (firewall.HostFloorApplier{}).Remove(context.Background(), rules)

	if out, err := exec.CommandContext(ctx, "docker", "exec", id, "curl", "-sS", "--max-time", "3", "http://169.254.169.254/").CombinedOutput(); err == nil {
		t.Fatalf("metadata reachable after floor: %s", out)
	}
	if out, err := exec.CommandContext(ctx, "docker", "exec", id, "curl", "-sS", "--max-time", "3", "http://10.0.0.1/").CombinedOutput(); err == nil {
		t.Fatalf("RFC1918 reachable after floor: %s", out)
	}
	// Public egress must still work. Hit a public IP directly (1.1.1.1:443) rather than a hostname so
	// the check doesn't depend on DNS resolution. A curl connect failure (exit 7) or timeout (exit 28)
	// means the floor blocked public egress; a TLS/HTTP error from a *reached* server is fine.
	out, perr := exec.CommandContext(ctx, "docker", "exec", id, "curl", "-sS", "--max-time", "10", "-o", "/dev/null", "https://1.1.1.1/").CombinedOutput()
	if ee, ok := perr.(*exec.ExitError); ok && (ee.ExitCode() == 7 || ee.ExitCode() == 28) {
		t.Fatalf("public IP unreachable after floor (floor too strict): exit %d (%s)", ee.ExitCode(), strings.TrimSpace(string(out)))
	}
}
