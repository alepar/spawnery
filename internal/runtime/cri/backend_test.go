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
		SidecarMounts: []runtime.Mount{{
			HostPath:      "/etc/spawnery/sidecar-ca-bundle/ca-bundle.crt",
			ContainerPath: "/run/spawnery/sidecar-ca/ca-bundle.crt",
			ReadOnly:      true,
		}},
		Resources: runtime.Resources{MemoryBytes: 512 << 20, NanoCPUs: 2_000_000_000, PidsLimit: 128},
		Runtime:   "runsc",
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
	if len(sc.Mounts) != 1 {
		t.Fatalf("sidecar mounts = %v, want exactly one", sc.Mounts)
	}
	mount := sc.Mounts[0]
	if mount.HostPath != "/etc/spawnery/sidecar-ca-bundle/ca-bundle.crt" ||
		mount.ContainerPath != "/run/spawnery/sidecar-ca/ca-bundle.crt" || !mount.Readonly {
		t.Fatalf("sidecar mount = %+v", mount)
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
	var tmuxTmpDir string
	for _, kv := range ag.Envs {
		if kv.Key == "ACP_ADAPTER" && kv.Value == "1" {
			hasAdapter = true
		}
		if kv.Key == "ACP_LISTEN" {
			acpListen = kv.Value
		}
		if kv.Key == "TMUX_TMPDIR" {
			tmuxTmpDir = kv.Value
		}
	}
	if !hasAdapter {
		t.Fatalf("CRI agent must set ACP_ADAPTER=1; envs=%+v", ag.Envs)
	}
	if acpListen != "tcp://0.0.0.0:7000" {
		t.Fatalf("CRI agent must listen for ACP over TCP (gVisor isolates the abstract UDS); ACP_LISTEN=%q", acpListen)
	}
	if tmuxTmpDir != "/dev/shm" {
		t.Fatalf("CRI agent TMUX_TMPDIR = %q, want /dev/shm", tmuxTmpDir)
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

func TestStartAgentPassesRunnableAsImageEntrypointArgs(t *testing.T) {
	c, f := newFakeCRI(t)
	b := NewCRIPodBackend(c, "runsc")
	ctx := context.Background()

	h, err := b.StartPod(ctx, runtime.PodSpec{ID: "spawn-runnable", SidecarImage: "sidecar:dev"})
	if err != nil {
		t.Fatalf("StartPod: %v", err)
	}
	if err := b.StartAgent(ctx, h, runtime.AgentSpec{Image: "agent:dev", Cmd: []string{"goose-acp"}}); err != nil {
		t.Fatalf("StartAgent: %v", err)
	}

	agent := f.created[1]
	if len(agent.Command) != 0 {
		t.Fatalf("CRI Command = %v, want empty so the image ENTRYPOINT remains active", agent.Command)
	}
	if len(agent.Args) != 1 || agent.Args[0] != "goose-acp" {
		t.Fatalf("CRI Args = %v, want [goose-acp]", agent.Args)
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

// TestCRIListManagedReportsIDsAndPodIP pins what re-adoption reads: the sidecar id, the agent id, the
// sandbox id and the pod IP (SE3 §4.5).
func TestCRIListManagedReportsIDsAndPodIP(t *testing.T) {
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
	if err := b.StartAgent(ctx, h, runtime.AgentSpec{Image: "a", Labels: labels}); err != nil {
		t.Fatalf("StartAgent: %v", err)
	}

	mps, err := b.ListManaged(ctx)
	if err != nil {
		t.Fatalf("ListManaged: %v", err)
	}
	if len(mps) != 1 {
		t.Fatalf("ListManaged = %d pods, want 1", len(mps))
	}
	mp := mps[0]
	if mp.SandboxID != f.sandboxID {
		t.Errorf("SandboxID = %q, want %q", mp.SandboxID, f.sandboxID)
	}
	if mp.SidecarID != h.SidecarID {
		t.Errorf("SidecarID = %q, want %q", mp.SidecarID, h.SidecarID)
	}
	if mp.AgentID != h.AgentID {
		t.Errorf("AgentID = %q, want %q", mp.AgentID, h.AgentID)
	}
	if mp.PodIP != f.podIP {
		t.Errorf("PodIP = %q, want %q", mp.PodIP, f.podIP)
	}
	if mp.Generation != 3 || mp.NodeID != "node-2" {
		t.Errorf("Generation/NodeID = %d/%q, want 3/node-2", mp.Generation, mp.NodeID)
	}
}

// TestCRIListManagedResolvesLegacyPodWithoutRoleLabels: a pod created by an OLDER node binary has
// containers with NO spawnery.role label — exactly the pods re-adoption exists to save after a node
// UPGRADE. Role must still resolve via the CRI container Metadata.Name.
func TestCRIListManagedResolvesLegacyPodWithoutRoleLabels(t *testing.T) {
	c, f := newFakeCRI(t)
	b := NewCRIPodBackend(c, "runsc")
	ctx := context.Background()
	labels := map[string]string{
		runtime.LabelManaged: "true", runtime.LabelSpawnID: "spawn-legacy",
		runtime.LabelGeneration: "1", runtime.LabelNodeID: "node-2",
	}
	// RunPodSandbox to register the sandbox's labels, but discard the sidecar StartPod created (it
	// stamps the role label — Task 5's own change) and inject containers directly with NO role label,
	// to model the pre-sp-2tx8.3.1 binary that never stamped spawnery.role.
	if _, err := b.StartPod(ctx, runtime.PodSpec{ID: "spawn-legacy", SidecarImage: "s", Labels: labels}); err != nil {
		t.Fatalf("StartPod: %v", err)
	}
	f.mu.Lock()
	f.containers = nil
	f.mu.Unlock()
	sidecarID := f.addContainer(f.sandboxID, "sidecar", map[string]string{runtime.LabelSpawnID: "spawn-legacy"}, runtimeapi.ContainerState_CONTAINER_RUNNING)
	agentID := f.addContainer(f.sandboxID, "agent", map[string]string{runtime.LabelSpawnID: "spawn-legacy"}, runtimeapi.ContainerState_CONTAINER_RUNNING)

	mps, err := b.ListManaged(ctx)
	if err != nil {
		t.Fatalf("ListManaged: %v", err)
	}
	if len(mps) != 1 {
		t.Fatalf("ListManaged = %d pods, want 1", len(mps))
	}
	mp := mps[0]
	if mp.SidecarID != sidecarID {
		t.Errorf("SidecarID = %q, want %q (resolved via Metadata.Name fallback)", mp.SidecarID, sidecarID)
	}
	if mp.AgentID != agentID {
		t.Errorf("AgentID = %q, want %q (resolved via Metadata.Name fallback)", mp.AgentID, agentID)
	}
}

// TestCRIListManagedPrefersRunningAgent: a sandbox can legitimately hold a crashed predecessor
// containerd has not GC'd, alongside its running replacement. ListManaged must report the RUNNING
// container's id — addressing the dead predecessor would tear down the wrong container.
func TestCRIListManagedPrefersRunningAgent(t *testing.T) {
	c, f := newFakeCRI(t)
	b := NewCRIPodBackend(c, "runsc")
	ctx := context.Background()
	labels := map[string]string{
		runtime.LabelManaged: "true", runtime.LabelSpawnID: "spawn-crash",
		runtime.LabelGeneration: "1", runtime.LabelNodeID: "node-2",
	}
	h, err := b.StartPod(ctx, runtime.PodSpec{ID: "spawn-crash", SidecarImage: "s", Labels: labels})
	if err != nil {
		t.Fatalf("StartPod: %v", err)
	}
	// An EXITED predecessor agent, created before the running one.
	deadAgentID := f.addContainer(f.sandboxID, "agent", runtime.WithRole(labels, runtime.RoleAgent), runtimeapi.ContainerState_CONTAINER_EXITED)
	if err := b.StartAgent(ctx, h, runtime.AgentSpec{Image: "a", Labels: labels}); err != nil {
		t.Fatalf("StartAgent: %v", err)
	}

	mps, err := b.ListManaged(ctx)
	if err != nil {
		t.Fatalf("ListManaged: %v", err)
	}
	if len(mps) != 1 {
		t.Fatalf("ListManaged = %d pods, want 1", len(mps))
	}
	if mps[0].AgentID != h.AgentID {
		t.Errorf("AgentID = %q, want %q (the RUNNING agent, not the exited predecessor %q)",
			mps[0].AgentID, h.AgentID, deadAgentID)
	}
	if mps[0].AgentID == deadAgentID {
		t.Fatal("AgentID resolved to the exited predecessor")
	}
}

// TestCRIListManagedFailsOnSandboxStatusError: a swallowed PodSandboxStatus error here becomes "no pod
// IP" and then a destroyed spawn at reconcile time (spec §4.3) — it must fail the whole call instead.
func TestCRIListManagedFailsOnSandboxStatusError(t *testing.T) {
	c, f := newFakeCRI(t)
	b := NewCRIPodBackend(c, "runsc")
	ctx := context.Background()
	labels := map[string]string{
		runtime.LabelManaged: "true", runtime.LabelSpawnID: "spawn-7",
		runtime.LabelGeneration: "1", runtime.LabelNodeID: "node-2",
	}
	if _, err := b.StartPod(ctx, runtime.PodSpec{ID: "spawn-7", SidecarImage: "s", Labels: labels}); err != nil {
		t.Fatalf("StartPod: %v", err)
	}
	f.failSandboxStatus = true

	if _, err := b.ListManaged(ctx); err == nil {
		t.Fatal("ListManaged must fail when PodSandboxStatus errors, not report a partially-known pod")
	}
}

// TestCRIListManagedFailsOnListContainersError: same rationale as the sandbox-status case — a
// transient CRI blip must never be laundered into "this pod has no containers".
func TestCRIListManagedFailsOnListContainersError(t *testing.T) {
	c, f := newFakeCRI(t)
	b := NewCRIPodBackend(c, "runsc")
	ctx := context.Background()
	labels := map[string]string{
		runtime.LabelManaged: "true", runtime.LabelSpawnID: "spawn-7",
		runtime.LabelGeneration: "1", runtime.LabelNodeID: "node-2",
	}
	if _, err := b.StartPod(ctx, runtime.PodSpec{ID: "spawn-7", SidecarImage: "s", Labels: labels}); err != nil {
		t.Fatalf("StartPod: %v", err)
	}
	f.failListCtrs = true

	if _, err := b.ListManaged(ctx); err == nil {
		t.Fatal("ListManaged must fail when ListContainers errors, not report a partially-known pod")
	}
}

func TestEnvFromContainerInfoConfigEnvs(t *testing.T) {
	info := map[string]string{"info": `{"config":{"envs":[{"key":"A","value":"1"},{"key":"B","value":"2"}]}}`}
	env, err := envFromContainerInfo(info)
	if err != nil {
		t.Fatalf("envFromContainerInfo: %v", err)
	}
	if len(env) != 2 || env[0] != "A=1" || env[1] != "B=2" {
		t.Fatalf("env = %v, want [A=1 B=2]", env)
	}
}

func TestEnvFromContainerInfoRuntimeSpecFallback(t *testing.T) {
	info := map[string]string{"info": `{"config":{},"runtimeSpec":{"process":{"env":["A=1","B=2"]}}}`}
	env, err := envFromContainerInfo(info)
	if err != nil {
		t.Fatalf("envFromContainerInfo: %v", err)
	}
	if len(env) != 2 || env[0] != "A=1" || env[1] != "B=2" {
		t.Fatalf("env = %v, want [A=1 B=2]", env)
	}
}

func TestEnvFromContainerInfoMissingInfo(t *testing.T) {
	if _, err := envFromContainerInfo(map[string]string{}); err == nil {
		t.Fatal("envFromContainerInfo: want error for missing info key")
	}
}

func TestEnvFromContainerInfoUnparseableJSON(t *testing.T) {
	if _, err := envFromContainerInfo(map[string]string{"info": "not json"}); err == nil {
		t.Fatal("envFromContainerInfo: want error for unparseable info")
	}
}

func TestCRIContainerEnvRoundTrips(t *testing.T) {
	c, _ := newFakeCRI(t)
	b := NewCRIPodBackend(c, "runsc")
	ctx := context.Background()

	h, err := b.StartPod(ctx, runtime.PodSpec{
		ID:           "spawn-7",
		SidecarImage: "s",
		SidecarEnv:   []string{"SIDECAR_CONTROL_TOKEN=tok", "OPENROUTER_API_KEY=k"},
	})
	if err != nil {
		t.Fatalf("StartPod: %v", err)
	}

	env, err := b.ContainerEnv(ctx, h.SidecarID)
	if err != nil {
		t.Fatalf("ContainerEnv: %v", err)
	}
	want := map[string]bool{"SIDECAR_CONTROL_TOKEN=tok": true, "OPENROUTER_API_KEY=k": true}
	if len(env) != len(want) {
		t.Fatalf("env = %v, want %v", env, want)
	}
	for _, e := range env {
		if !want[e] {
			t.Fatalf("unexpected env entry %q (got %v)", e, env)
		}
	}
}

func TestCRIContainerEnvFailsOnContainerStatusError(t *testing.T) {
	c, f := newFakeCRI(t)
	b := NewCRIPodBackend(c, "runsc")
	ctx := context.Background()

	h, err := b.StartPod(ctx, runtime.PodSpec{ID: "spawn-7", SidecarImage: "s"})
	if err != nil {
		t.Fatalf("StartPod: %v", err)
	}
	f.failContainerStatus = true

	if _, err := b.ContainerEnv(ctx, h.SidecarID); err == nil {
		t.Fatal("ContainerEnv must fail (not return an empty env) when ContainerStatus errors")
	}
}
