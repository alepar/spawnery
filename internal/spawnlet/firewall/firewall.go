// Package firewall builds + applies the per-pod egress block-floor (sp-rpa, sp-ff2): drop
// cloud-metadata and RFC1918 for a spawn pod, allowing public egress otherwise. Rules are applied on
// the HOST's DOCKER-USER chain, matched by the pod's bridge source IP. This works under both runc AND
// gVisor/runsc — applying iptables inside the spawn's container netns is a no-op under runsc, whose
// netstack bypasses host netfilter. DOCKER-USER persists across containers, so rules carry a Remove.
package firewall

import (
	"context"
	"fmt"
	"os/exec"
)

// Rule is one iptables invocation's arguments (everything after "iptables").
type Rule struct{ Args []string }

// Rules returns the per-pod egress block-floor for the given pod bridge IP, as DOCKER-USER chain
// arg-lists (everything after "iptables"). Matched by source IP so multiple pods coexist in the
// shared chain. Final applied order (top-to-bottom): DNS + allowCIDRs ACCEPT, then metadata + RFC1918
// DROP. No loopback rule — agent<->sidecar (127.0.0.1) is never forwarded. Applied on the HOST
// (works under runc AND gVisor/runsc, where in-netns iptables is a no-op).
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

const chain = "DOCKER-USER"

// Applier installs/removes egress-floor rules on the host's DOCKER-USER chain.
type Applier interface {
	Apply(ctx context.Context, rules []Rule) error
	Remove(ctx context.Context, rules []Rule) error
}

// HostFloorApplier runs the host's iptables against DOCKER-USER. Requires iptables + root
// (CAP_NET_ADMIN). Inserts in REVERSE with -I so the final order matches Rules().
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
