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
// host are unreachable while a public host is reachable. Requires privileged Docker + iptables + root.
// Build/run on the node host with: go test -tags egress_e2e ./internal/spawnlet/firewall/ -run TestEgressFloorEnforced
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

	pid, err := rt.ContainerPID(ctx, id)
	if err != nil {
		t.Fatalf("pid: %v", err)
	}
	if err := (firewall.NsenterApplier{}).Apply(ctx, pid, firewall.Rules(nil)); err != nil {
		t.Fatalf("apply (needs iptables+root): %v", err)
	}

	if out, err := exec.CommandContext(ctx, "docker", "exec", id, "curl", "-sS", "--max-time", "3", "http://169.254.169.254/").CombinedOutput(); err == nil {
		t.Fatalf("metadata reachable after floor: %s", out)
	}
	if out, err := exec.CommandContext(ctx, "docker", "exec", id, "curl", "-sS", "--max-time", "3", "http://10.0.0.1/").CombinedOutput(); err == nil {
		t.Fatalf("RFC1918 reachable after floor: %s", out)
	}
	// Public egress must still work. Hit a public IP directly (1.1.1.1:443) rather than a hostname so
	// the check doesn't depend on DNS resolution — some hosts/CI block outbound DNS entirely, which
	// would mask the floor's behavior. The DNS carve-out (udp/tcp :53 ACCEPT before the drops) is
	// covered by the firewall unit test + verified at the packet level on a real host. A curl connect
	// failure (exit 7) or timeout (exit 28) means the floor blocked public egress; a TLS/HTTP error
	// from a *reached* server is fine.
	out, perr := exec.CommandContext(ctx, "docker", "exec", id, "curl", "-sS", "--max-time", "10", "-o", "/dev/null", "https://1.1.1.1/").CombinedOutput()
	if ee, ok := perr.(*exec.ExitError); ok && (ee.ExitCode() == 7 || ee.ExitCode() == 28) {
		t.Fatalf("public IP unreachable after floor (floor too strict): exit %d (%s)", ee.ExitCode(), strings.TrimSpace(string(out)))
	}
}
