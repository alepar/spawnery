package cri

import (
	"context"
	"strings"
	"testing"

	runtimeapi "k8s.io/cri-api/pkg/apis/runtime/v1"
	"spawnery/internal/runtime"
)

func TestPingAndPreflight(t *testing.T) {
	c, f := newFakeCRI(t)
	b := NewCRIPodBackend(c, "runsc")
	ctx := context.Background()

	if err := b.Ping(ctx); err != nil {
		t.Fatalf("Ping: %v", err)
	}
	if err := b.Preflight(ctx); err != nil {
		t.Fatalf("Preflight (ready): %v", err)
	}

	f.setNetworkReady(false)
	if err := b.Preflight(ctx); err == nil {
		t.Fatal("Preflight must fail when NetworkReady is false")
	}
}

func TestStartPodSandboxSidecarAndHandle(t *testing.T) {
	c, f := newFakeCRI(t)
	b := NewCRIPodBackend(c, "runsc")
	ctx := context.Background()

	h, err := b.StartPod(ctx, runtime.PodSpec{
		ID:           "spawn-7",
		SidecarImage: "spawnery/sidecar:dev",
		SidecarEnv:   []string{"OPENROUTER_API_KEY=k", "SIDECAR_ADDR=127.0.0.1:8080"},
		Resources:    runtime.Resources{MemoryBytes: 512 << 20, NanoCPUs: 2_000_000_000, PidsLimit: 128},
		Runtime:      "runsc",
	})
	if err != nil {
		t.Fatalf("StartPod: %v", err)
	}
	if h.SandboxID != "sandbox-1" {
		t.Fatalf("SandboxID = %q", h.SandboxID)
	}
	if h.PodIP != "10.244.0.7" {
		t.Fatalf("PodIP = %q", h.PodIP)
	}
	if h.NetnsPath != "/proc/4242/ns/net" {
		t.Fatalf("NetnsPath = %q", h.NetnsPath)
	}
	if h.SidecarID != "ctr-1" {
		t.Fatalf("SidecarID = %q", h.SidecarID)
	}
	if h.AgentID != "" {
		t.Fatalf("AgentID must be empty after StartPod, got %q", h.AgentID)
	}
	if len(f.created) != 1 || f.createdNames[0] != "sidecar" || f.createSandbox[0] != "sandbox-1" {
		t.Fatalf("sidecar create wrong: names=%v sandbox=%v", f.createdNames, f.createSandbox)
	}
	if len(f.started) != 1 || f.started[0] != "ctr-1" {
		t.Fatalf("started = %v", f.started)
	}
	if len(f.pulled) != 1 || f.pulled[0] != "spawnery/sidecar:dev" {
		t.Fatalf("pulled = %v", f.pulled)
	}
	sc := f.created[0]
	if sc.Linux.Resources.MemoryLimitInBytes != 512<<20 {
		t.Fatalf("mem = %d", sc.Linux.Resources.MemoryLimitInBytes)
	}
	if sc.Linux.Resources.CpuPeriod != 100000 || sc.Linux.Resources.CpuQuota != 200000 {
		t.Fatalf("cpu period/quota = %d/%d", sc.Linux.Resources.CpuPeriod, sc.Linux.Resources.CpuQuota)
	}
	if sc.Linux.Resources.Unified["pids.max"] != "128" {
		t.Fatalf("pids.max = %q", sc.Linux.Resources.Unified["pids.max"])
	}
}

func TestStartPodCleansUpSandboxOnFailure(t *testing.T) {
	c, f := newFakeCRI(t)
	f.failCreate = true // sidecar CreateContainer fails -> StartPod must tear down the sandbox
	b := NewCRIPodBackend(c, "runsc")

	_, err := b.StartPod(context.Background(), runtime.PodSpec{ID: "spawn-9", SidecarImage: "s"})
	if err == nil {
		t.Fatal("StartPod must fail when CreateContainer fails")
	}
	if len(f.stopSandbox) != 1 || f.stopSandbox[0] != "sandbox-1" {
		t.Fatalf("sandbox must be stopped on failure; stopSandbox=%v", f.stopSandbox)
	}
	if len(f.removeSandbox) != 1 || f.removeSandbox[0] != "sandbox-1" {
		t.Fatalf("sandbox must be removed on failure; removeSandbox=%v", f.removeSandbox)
	}
}

func TestStartAgentAndStopLifecycle(t *testing.T) {
	c, f := newFakeCRI(t)
	b := NewCRIPodBackend(c, "runsc")
	ctx := context.Background()

	h, err := b.StartPod(ctx, runtime.PodSpec{ID: "spawn-7", SidecarImage: "sidecar:dev", Resources: runtime.Resources{MemoryBytes: 1 << 20}})
	if err != nil {
		t.Fatalf("StartPod: %v", err)
	}

	err = b.StartAgent(ctx, h, runtime.AgentSpec{
		Image:       "goose:dev",
		Env:         []string{"SPAWN_MODEL=m"},
		Mounts:      []runtime.Mount{{HostPath: "/h", ContainerPath: "/app", ReadOnly: true}},
		Resources:   runtime.Resources{MemoryBytes: 1 << 20},
		DropAllCaps: true,
	})
	if err != nil {
		t.Fatalf("StartAgent: %v", err)
	}
	if h.AgentID != "ctr-2" {
		t.Fatalf("AgentID = %q", h.AgentID)
	}
	if len(f.created) != 2 || f.createdNames[1] != "agent" || f.createSandbox[1] != "sandbox-1" {
		t.Fatalf("agent create wrong: names=%v", f.createdNames)
	}
	ag := f.created[1]
	// ReadonlyRootfs is retired (spec §6); only cap-drop=ALL is checked.
	if ag.Linux.SecurityContext == nil || len(ag.Linux.SecurityContext.Capabilities.DropCapabilities) != 1 ||
		ag.Linux.SecurityContext.Capabilities.DropCapabilities[0] != "ALL" {
		t.Fatalf("agent hardening wrong: %+v", ag.Linux.SecurityContext)
	}
	if ag.Linux.SecurityContext.ReadonlyRootfs {
		t.Fatal("ReadonlyRootfs must not be set (retired by spec §6)")
	}
	if len(ag.Mounts) != 1 || ag.Mounts[0].HostPath != "/h" || ag.Mounts[0].ContainerPath != "/app" || !ag.Mounts[0].Readonly {
		t.Fatalf("agent mount wrong: %+v", ag.Mounts)
	}
	var hasAdapter bool
	var acpListen string
	for _, kv := range ag.Envs {
		if kv.Key == "ACP_ADAPTER" && kv.Value == "1" {
			hasAdapter = true
		}
		if kv.Key == "ACP_LISTEN" {
			acpListen = kv.Value
		}
	}
	if !hasAdapter {
		t.Fatalf("CRI agent must set ACP_ADAPTER=1; envs=%+v", ag.Envs)
	}
	if acpListen != "tcp://0.0.0.0:7000" {
		t.Fatalf("CRI agent must listen for ACP over TCP (gVisor isolates the abstract UDS); ACP_LISTEN=%q", acpListen)
	}

	if err := b.Stop(ctx, h); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if len(f.stopped) != 2 || f.stopped[0] != "ctr-2" || f.stopped[1] != "ctr-1" {
		t.Fatalf("stopped order = %v", f.stopped)
	}
	if len(f.stopSandbox) != 1 || f.stopSandbox[0] != "sandbox-1" || len(f.removeSandbox) != 1 || f.removeSandbox[0] != "sandbox-1" {
		t.Fatalf("sandbox teardown wrong: stop=%v remove=%v", f.stopSandbox, f.removeSandbox)
	}
}

func TestStartAgentCapPolicyEmission(t *testing.T) {
	cases := []struct {
		name        string
		dropAllCaps bool
		wantDropAll bool
	}{
		{
			name:        "DropAll=true emits DropCapabilities=ALL",
			dropAllCaps: true,
			wantDropAll: true,
		},
		{
			name:        "DropAll=false (DefaultSet) emits no SecurityContext (no cap mods)",
			dropAllCaps: false,
			wantDropAll: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, f := newFakeCRI(t)
			b := NewCRIPodBackend(c, "runsc")
			ctx := context.Background()

			h, err := b.StartPod(ctx, runtime.PodSpec{ID: "spawn-cap", SidecarImage: "sidecar:dev"})
			if err != nil {
				t.Fatalf("StartPod: %v", err)
			}
			if err := b.StartAgent(ctx, h, runtime.AgentSpec{
				Image:       "agent:dev",
				DropAllCaps: tc.dropAllCaps,
			}); err != nil {
				t.Fatalf("StartAgent: %v", err)
			}

			// f.created[0] = sidecar, f.created[1] = agent
			if len(f.created) != 2 {
				t.Fatalf("expected 2 containers created, got %d", len(f.created))
			}

			// Sidecar must always use CapDefaultSet (no cap mods).
			sc := f.created[0]
			if sc.Linux.GetSecurityContext().GetCapabilities().GetDropCapabilities() != nil {
				t.Errorf("sidecar must not have DropCapabilities; got %v",
					sc.Linux.GetSecurityContext().GetCapabilities().GetDropCapabilities())
			}

			ag := f.created[1]
			if tc.wantDropAll {
				// DropAll path: SecurityContext with DropCapabilities=["ALL"]
				if ag.Linux.GetSecurityContext() == nil {
					t.Fatal("DropAll: expected non-nil SecurityContext")
				}
				got := ag.Linux.GetSecurityContext().GetCapabilities().GetDropCapabilities()
				if len(got) != 1 || got[0] != "ALL" {
					t.Errorf("DropAll: DropCapabilities = %v, want [ALL]", got)
				}
				if ag.Linux.GetSecurityContext().GetReadonlyRootfs() {
					t.Error("ReadonlyRootfs must not be set (retired by spec §6)")
				}
			} else {
				// DefaultSet path: NO SecurityContext → NO capability modifications.
				if ag.Linux.GetSecurityContext() != nil {
					t.Errorf("DefaultSet: expected nil SecurityContext (no cap mods), got %+v",
						ag.Linux.GetSecurityContext())
				}
			}
		})
	}
}

func TestAssertNoAddedCaps(t *testing.T) {
	// nil SecurityContext → no error.
	if err := assertNoAddedCaps(nil); err != nil {
		t.Fatalf("nil sc: expected nil error, got %v", err)
	}

	// SecurityContext with nil Capabilities → no error.
	if err := assertNoAddedCaps(&runtimeapi.LinuxContainerSecurityContext{}); err != nil {
		t.Fatalf("nil Capabilities: expected nil error, got %v", err)
	}

	// SecurityContext with empty AddCapabilities → no error.
	if err := assertNoAddedCaps(&runtimeapi.LinuxContainerSecurityContext{
		Capabilities: &runtimeapi.Capability{AddCapabilities: []string{}},
	}); err != nil {
		t.Fatalf("empty AddCapabilities: expected nil error, got %v", err)
	}

	// Single CAP_NET_ADMIN → error mentioning CAP_NET_ADMIN.
	err := assertNoAddedCaps(&runtimeapi.LinuxContainerSecurityContext{
		Capabilities: &runtimeapi.Capability{AddCapabilities: []string{"CAP_NET_ADMIN"}},
	})
	if err == nil {
		t.Fatal("CAP_NET_ADMIN: expected error, got nil")
	}
	if !strings.Contains(err.Error(), "CAP_NET_ADMIN") {
		t.Errorf("error should mention CAP_NET_ADMIN, got: %v", err)
	}

	// Multiple caps → error.
	err2 := assertNoAddedCaps(&runtimeapi.LinuxContainerSecurityContext{
		Capabilities: &runtimeapi.Capability{AddCapabilities: []string{"CAP_SYS_ADMIN", "CAP_NET_ADMIN"}},
	})
	if err2 == nil {
		t.Fatal("multi-cap: expected error, got nil")
	}
	if !strings.Contains(err2.Error(), "CAP_SYS_ADMIN") {
		t.Errorf("error should mention CAP_SYS_ADMIN, got: %v", err2)
	}
}

func TestStartAgentUnknownSandbox(t *testing.T) {
	c, _ := newFakeCRI(t)
	b := NewCRIPodBackend(c, "runsc")
	err := b.StartAgent(context.Background(), &runtime.PodHandle{SandboxID: "nope"}, runtime.AgentSpec{Image: "x"})
	if err == nil {
		t.Fatal("StartAgent must error for an unknown sandbox")
	}
}

func TestAttachRequiresPodIP(t *testing.T) {
	c, _ := newFakeCRI(t)
	b := NewCRIPodBackend(c, "runsc")
	if _, err := b.Attach(context.Background(), &runtime.PodHandle{}); err == nil {
		t.Fatal("Attach must error when the pod has no IP")
	}
}

func TestStartPodLabelsAndListManaged(t *testing.T) {
	c, f := newFakeCRI(t)
	b := NewCRIPodBackend(c, "runsc")
	ctx := context.Background()
	labels := map[string]string{
		runtime.LabelManaged: "true", runtime.LabelSpawnID: "spawn-7",
		runtime.LabelGeneration: "3", runtime.LabelNodeID: "node-2",
	}
	h, err := b.StartPod(ctx, runtime.PodSpec{ID: "spawn-7", SidecarImage: "s", Labels: labels})
	if err != nil {
		t.Fatalf("StartPod: %v", err)
	}
	if f.sandboxLabels[runtime.LabelSpawnID] != "spawn-7" || f.sandboxLabels[runtime.LabelGeneration] != "3" {
		t.Fatalf("sandbox labels = %v", f.sandboxLabels)
	}
	if f.created[0].Labels[runtime.LabelManaged] != "true" {
		t.Fatalf("sidecar container labels = %v", f.created[0].Labels)
	}

	mps, err := b.ListManaged(ctx)
	if err != nil {
		t.Fatalf("ListManaged: %v", err)
	}
	if len(mps) != 1 || mps[0].SpawnID != "spawn-7" || mps[0].Generation != 3 || mps[0].SandboxID != "sandbox-1" {
		t.Fatalf("ListManaged = %+v", mps)
	}

	if err := b.Stop(ctx, h); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if mps, _ := b.ListManaged(ctx); len(mps) != 0 {
		t.Fatalf("ListManaged after Stop = %+v", mps)
	}
}
