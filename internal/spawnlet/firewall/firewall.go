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
