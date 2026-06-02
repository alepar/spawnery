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
	rec := &recRunner{}
	a := &CNIFloorApplier{run: rec.run}
	rules := Rules("10.244.0.7", nil)
	if err := a.Apply(context.Background(), rules); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	lines := rec.joined()
	var inserts []string
	for _, l := range lines {
		if strings.HasPrefix(l, "-I SPAWNLET-EGRESS ") {
			inserts = append(inserts, l)
		}
	}
	if len(inserts) != len(rules) {
		t.Fatalf("want %d rule inserts, got %d (%v)", len(rules), len(inserts), inserts)
	}
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
