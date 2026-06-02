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
	// A non-nil result from the -nL/-C checks means "absent" -> create. A genuinely broken iptables
	// still surfaces an error from the -N/-I below, so failures aren't swallowed.
	if r(ctx, []string{"-nL", cniChain}) != nil {
		if err := r(ctx, []string{"-N", cniChain}); err != nil {
			return fmt.Errorf("create %s chain: %w", cniChain, err)
		}
	}
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

var _ Applier = (*CNIFloorApplier)(nil)
