package spawnlet

import (
	"context"
	"errors"
	"testing"

	"spawnery/internal/runtime"
	"spawnery/internal/spawnlet/firewall"
)

type fakeApplier struct {
	applied   bool
	removed   bool
	failApply bool
}

func (f *fakeApplier) Apply(ctx context.Context, rules []firewall.Rule) error {
	f.applied = true
	if f.failApply {
		return errors.New("boom")
	}
	return nil
}

func (f *fakeApplier) Remove(ctx context.Context, rules []firewall.Rule) error {
	f.removed = true
	return nil
}

func TestCreateFailClosedWhenFirewallFails(t *testing.T) {
	rt := runtime.NewFake()
	m := NewManager(rt, ManagerConfig{AgentImage: "a", SidecarImage: "s", DataRoot: t.TempDir(), EgressEnforce: true})
	fa := &fakeApplier{failApply: true}
	m.fw = fa
	_, err := m.Create(context.Background(), "spawn1", "../../examples/secret-app", "model", "", 0)
	if err == nil {
		t.Fatal("Create must fail-closed when the firewall can't be applied")
	}
	if !fa.applied {
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
	fa := &fakeApplier{}
	m.fw = fa
	if _, err := m.Create(context.Background(), "spawn2", "../../examples/secret-app", "model", "", 0); err != nil {
		t.Fatalf("Create self-hosted+enforce=false should succeed: %v", err)
	}
	if fa.applied {
		t.Fatal("firewall must NOT be applied on self-hosted with EgressEnforce=false")
	}
}

func TestCreateCloudForcesEnforce(t *testing.T) {
	rt := runtime.NewFake()
	m := NewManager(rt, ManagerConfig{AgentImage: "a", SidecarImage: "s", DataRoot: t.TempDir(), NodeClass: "cloud", EgressEnforce: false})
	fa := &fakeApplier{failApply: true}
	m.fw = fa
	if _, err := m.Create(context.Background(), "spawn3", "../../examples/secret-app", "model", "", 0); err == nil {
		t.Fatal("cloud node must fail-closed (firewall forced) even with EgressEnforce=false")
	}
	if !fa.applied {
		t.Fatal("cloud node must apply the firewall regardless of EgressEnforce")
	}
}

func TestStopRemovesFloor(t *testing.T) {
	rt := runtime.NewFake()
	m := NewManager(rt, ManagerConfig{AgentImage: "a", SidecarImage: "s", DataRoot: t.TempDir(), EgressEnforce: true})
	fa := &fakeApplier{}
	m.fw = fa
	sp, err := m.Create(context.Background(), "spawn4", "../../examples/secret-app", "model", "", 0)
	if err != nil {
		t.Fatalf("Create should succeed: %v", err)
	}
	if !fa.applied {
		t.Fatal("firewall floor must be applied on Create")
	}
	if err := m.Stop(context.Background(), sp.ID); err != nil {
		t.Fatalf("Stop should succeed: %v", err)
	}
	if !fa.removed {
		t.Fatal("egress floor must be removed on Stop")
	}
}

type emptyIPRuntime struct{ *runtime.FakeRuntime }

func (emptyIPRuntime) ContainerIP(context.Context, string) (string, error) { return "", nil }

func TestCreateEmptyIPFailsClosedWhenEnforced(t *testing.T) {
	rt := emptyIPRuntime{runtime.NewFake()}
	m := NewManager(rt, ManagerConfig{AgentImage: "a", SidecarImage: "s", DataRoot: t.TempDir(), EgressEnforce: true})
	if _, err := m.Create(context.Background(), "sp1", "../../examples/secret-app", "model", "", 0); err == nil {
		t.Fatal("enforcing spawn with no pod IP must fail closed")
	}
}

func TestCreateEmptyIPSucceedsWhenNotEnforced(t *testing.T) {
	rt := emptyIPRuntime{runtime.NewFake()}
	m := NewManager(rt, ManagerConfig{AgentImage: "a", SidecarImage: "s", DataRoot: t.TempDir(), NodeClass: "self-hosted", EgressEnforce: false})
	if _, err := m.Create(context.Background(), "sp2", "../../examples/secret-app", "model", "", 0); err != nil {
		t.Fatalf("non-enforcing spawn with no pod IP should succeed: %v", err)
	}
}
