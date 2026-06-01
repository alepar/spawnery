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
	m.fw = fa
	_, err := m.Create(context.Background(), "spawn1", "../../examples/secret-app", "model")
	if err == nil {
		t.Fatal("Create must fail-closed when the firewall can't be applied")
	}
	if !fa.called {
		t.Fatal("firewall applier was not called")
	}
	if !rt.Stopped["fake-1"] {
		t.Fatalf("sidecar not stopped after fail-closed; stopped=%v", rt.Stopped)
	}
	if len(rt.Started) != 1 {
		t.Fatalf("agent must NOT start after firewall failure; started=%d", len(rt.Started))
	}
}

func TestCreateSkipsFirewallSelfHostedDisabled(t *testing.T) {
	rt := runtime.NewFake()
	m := NewManager(rt, ManagerConfig{AgentImage: "a", SidecarImage: "s", DataRoot: t.TempDir(), NodeClass: "self-hosted", EgressEnforce: false})
	fa := &failApplier{}
	m.fw = fa
	if _, err := m.Create(context.Background(), "spawn2", "../../examples/secret-app", "model"); err != nil {
		t.Fatalf("Create self-hosted+enforce=false should succeed: %v", err)
	}
	if fa.called {
		t.Fatal("firewall must NOT be applied on self-hosted with EgressEnforce=false")
	}
}

func TestCreateCloudForcesEnforce(t *testing.T) {
	rt := runtime.NewFake()
	m := NewManager(rt, ManagerConfig{AgentImage: "a", SidecarImage: "s", DataRoot: t.TempDir(), NodeClass: "cloud", EgressEnforce: false})
	fa := &failApplier{}
	m.fw = fa
	if _, err := m.Create(context.Background(), "spawn3", "../../examples/secret-app", "model"); err == nil {
		t.Fatal("cloud node must fail-closed (firewall forced) even with EgressEnforce=false")
	}
	if !fa.called {
		t.Fatal("cloud node must apply the firewall regardless of EgressEnforce")
	}
}
