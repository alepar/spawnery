package spawnlet

// adopt_test.go: Manager.Adopt — rebuilding the in-memory Spawn for a pod that survived a spawnlet
// restart (SE3 §4.2). "Restart" here is literal and hermetic: a SECOND Manager over the SAME fakepod
// backend, whose container state outlives the first Manager.

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"spawnery/internal/runtime"
	"spawnery/internal/runtime/fakepod"
	"spawnery/internal/spawnlet/firewall"
)

// recordingApplier records the egress-floor calls in order, so a test can pin Remove-then-Apply.
type recordingApplier struct{ calls []string }

func (a *recordingApplier) Apply(_ context.Context, rules []firewall.Rule) error {
	a.calls = append(a.calls, "apply")
	return nil
}
func (a *recordingApplier) Remove(_ context.Context, rules []firewall.Rule) error {
	a.calls = append(a.calls, "remove")
	return nil
}

// restart returns a NEW Manager over the same backend + data root — the hermetic equivalent of
// `systemctl restart spawnery-spawnlet`: fresh in-memory store, same containers, same node-local dirs.
func restart(t *testing.T, be runtime.PodBackend, fw firewall.Applier, dataRoot string) *Manager {
	t.Helper()
	return NewManagerWithBackend(be, fw, ManagerConfig{
		AgentImage: "a", SidecarImage: "s", DataRoot: dataRoot, NodeID: "n1",
	})
}

// The adopted Spawn must be complete enough to drive every later lifecycle op: the container ids and pod
// IP from the runtime, the mount table re-derived from the manifest + the CP's spec, and — critically —
// the mount host dirs must still hold the agent's data (Adopt must never re-Prepare them).
func TestAdoptRebuildsSpawnWithoutRepreparingMounts(t *testing.T) {
	ctx := context.Background()
	be := fakepod.New()
	t.Cleanup(be.Close)
	dataRoot := t.TempDir()

	m1 := restart(t, be, &recordingApplier{}, dataRoot)
	sp, err := m1.Create(ctx, "sp1", "../../examples/secret-app", "model", "friendly", "app1", 3)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if len(sp.MountDirs) == 0 {
		t.Fatal("setup: the app has no mounts — pick an app that does")
	}
	// The agent's work, on the host side of a live bind mount.
	work := filepath.Join(sp.MountDirs[len(sp.MountDirs)-1], "work.txt")
	if err := os.WriteFile(work, []byte("AGENT WORK"), 0o644); err != nil {
		t.Fatal(err)
	}

	// --- the restart ---
	m2 := restart(t, be, &recordingApplier{}, dataRoot)
	pods, err := m2.UntrackedPods(ctx)
	if err != nil || len(pods) != 1 {
		t.Fatalf("UntrackedPods = %+v, %v; want the one surviving pod", pods, err)
	}

	got, err := m2.Adopt(ctx, pods[0], AdoptSpec{
		AppRef: "../../examples/secret-app", Model: "model", Name: "friendly", AppID: "app1",
	})
	if err != nil {
		t.Fatalf("Adopt: %v", err)
	}

	if got.ID != "sp1" || got.Generation != 3 {
		t.Fatalf("adopted spawn = %s gen %d, want sp1 gen 3", got.ID, got.Generation)
	}
	if got.AgentID != pods[0].AgentID || got.SidecarID != pods[0].SidecarID || got.PodIP != pods[0].PodIP {
		t.Fatalf("adopted ids/IP = %+v, want %+v", got, pods[0])
	}
	if len(got.MountDirs) != len(sp.MountDirs) {
		t.Fatalf("adopted MountDirs = %v, want the same table as Create's %v", got.MountDirs, sp.MountDirs)
	}
	for i := range sp.MountDirs {
		if got.MountDirs[i] != sp.MountDirs[i] {
			t.Fatalf("adopted MountDirs[%d] = %q, want %q", i, got.MountDirs[i], sp.MountDirs[i])
		}
	}
	if len(got.MountTargets) != len(sp.MountTargets) {
		t.Fatalf("adopted MountTargets = %v, want Create's %v (the delta-scrub guard reads these)",
			got.MountTargets, sp.MountTargets)
	}
	if len(got.MountFinalizers) != len(sp.MountFinalizers) {
		t.Fatalf("adopted MountFinalizers = %d, want %d (teardown finalizes through them)",
			len(got.MountFinalizers), len(sp.MountFinalizers))
	}
	// The agent's data is UNTOUCHED — Adopt must not have re-Prepared (re-seeded) the mount.
	b, err := os.ReadFile(work)
	if err != nil || string(b) != "AGENT WORK" {
		t.Fatalf("mount data after adopt = %q, %v — Adopt re-prepared a live mount", b, err)
	}
	// The pod was never touched.
	for _, role := range []string{"sandbox", "sidecar", "agent"} {
		if st := be.State("sp1", role); st != fakepod.StateRunning {
			t.Fatalf("after Adopt, sp1/%s = %v, want running", role, st)
		}
	}
	// And the Manager tracks it again: the CP's next heartbeat carries it.
	if inv := m2.RunningInventory(); len(inv) != 1 || inv[0].SpawnID != "sp1" || inv[0].Generation != 3 {
		t.Fatalf("RunningInventory after adopt = %+v", inv)
	}
}

// The egress floor is re-established as Remove-then-Apply: the previous process never removed its rules
// (DetachAll deliberately doesn't), and both appliers insert with `-I`, so a bare re-Apply would leave a
// DUPLICATE rule set behind after teardown's single `-D` pass (plan decision 2). FloorIP must be recorded
// so teardown can remove it at all.
func TestAdoptReestablishesEgressFloorExactlyOnce(t *testing.T) {
	ctx := context.Background()
	be := fakepod.New()
	t.Cleanup(be.Close)
	dataRoot := t.TempDir()
	fw := &recordingApplier{}

	m1 := NewManagerWithBackend(be, fw, ManagerConfig{
		AgentImage: "a", SidecarImage: "s", DataRoot: dataRoot, NodeID: "n1",
		NodeClass: "cloud", EgressEnforce: true,
	})
	if _, err := m1.Create(ctx, "sp1", "../../examples/secret-app", "model", "", "", 1); err != nil {
		t.Fatalf("Create: %v", err)
	}
	fw2 := &recordingApplier{}
	m2 := NewManagerWithBackend(be, fw2, ManagerConfig{
		AgentImage: "a", SidecarImage: "s", DataRoot: dataRoot, NodeID: "n1",
		NodeClass: "cloud", EgressEnforce: true,
	})
	pods, err := m2.UntrackedPods(ctx)
	if err != nil || len(pods) != 1 {
		t.Fatalf("UntrackedPods = %+v, %v", pods, err)
	}
	sp, err := m2.Adopt(ctx, pods[0], AdoptSpec{AppRef: "../../examples/secret-app", Model: "model"})
	if err != nil {
		t.Fatalf("Adopt: %v", err)
	}
	if got := fw2.calls; len(got) != 2 || got[0] != "remove" || got[1] != "apply" {
		t.Fatalf("floor calls on adopt = %v, want [remove apply] (exactly one rule set survives)", got)
	}
	if sp.FloorIP != pods[0].PodIP || sp.FloorIP == "" {
		t.Fatalf("FloorIP = %q, want the pod IP %q (teardown removes by it)", sp.FloorIP, pods[0].PodIP)
	}
}

// A rebuild that cannot complete leaves NOTHING behind: no store entry, no floor. The caller then
// capture-before-reaps. (Here the app ref is gone — the manifest cannot be parsed.)
func TestAdoptFailureLeavesNoHalfAdoptedState(t *testing.T) {
	ctx := context.Background()
	be := fakepod.New()
	t.Cleanup(be.Close)
	dataRoot := t.TempDir()
	fw := &recordingApplier{}

	m1 := restart(t, be, fw, dataRoot)
	if _, err := m1.Create(ctx, "sp1", "../../examples/secret-app", "model", "", "", 1); err != nil {
		t.Fatalf("Create: %v", err)
	}
	m2 := restart(t, be, fw, dataRoot)
	pods, _ := m2.UntrackedPods(ctx)

	if _, err := m2.Adopt(ctx, pods[0], AdoptSpec{AppRef: filepath.Join(dataRoot, "no-such-app"), Model: "m"}); err == nil {
		t.Fatal("Adopt with a missing app ref must fail")
	}
	if _, ok := m2.Store().Get("sp1"); ok {
		t.Fatal("a failed Adopt left the spawn in the store — half-adopted state")
	}
	if inv := m2.RunningInventory(); len(inv) != 0 {
		t.Fatalf("RunningInventory after a failed adopt = %+v, want empty", inv)
	}
}

// The node re-checks the CP's fence: a decision for a generation that is not the one on the pod is not
// adoptable (plan decision 4).
func TestAdoptRejectsGenerationMismatch(t *testing.T) {
	ctx := context.Background()
	be := fakepod.New()
	t.Cleanup(be.Close)
	dataRoot := t.TempDir()

	m1 := restart(t, be, &recordingApplier{}, dataRoot)
	if _, err := m1.Create(ctx, "sp1", "../../examples/secret-app", "model", "", "", 2); err != nil {
		t.Fatalf("Create: %v", err)
	}
	m2 := restart(t, be, &recordingApplier{}, dataRoot)
	pods, _ := m2.UntrackedPods(ctx)
	mp := pods[0]
	mp.Generation = 99 // the pod's labels say 2; pretend the caller handed us a mismatched fence

	if _, err := m2.Adopt(ctx, mp, AdoptSpec{AppRef: "../../examples/secret-app", Model: "model", Generation: 2}); err == nil {
		t.Fatal("Adopt must reject a spec whose generation disagrees with the observed pod")
	}
}

// --- sp-2tx8.3.5: control token + GitHub control server re-adoption -----------------------------

// The sidecar never stopped: Adopt must read SIDECAR_CONTROL_TOKEN back out of its still-running
// env (not mint a new one — the sidecar is still enforcing the OLD token on /control/model).
func TestAdoptReadsControlTokenFromSidecarEnv(t *testing.T) {
	ctx := context.Background()
	be := fakepod.New()
	t.Cleanup(be.Close)
	dataRoot := t.TempDir()

	m1 := restart(t, be, &recordingApplier{}, dataRoot)
	if _, err := m1.Create(ctx, "sp1", "../../examples/secret-app", "model", "", "", 1); err != nil {
		t.Fatalf("Create: %v", err)
	}
	wantToken := sidecarEnvVal(be.PodSpec("sp1").SidecarEnv, SidecarControlTokenEnv)
	if wantToken == "" {
		t.Fatal("setup: sidecar env has no control token")
	}

	m2 := restart(t, be, &recordingApplier{}, dataRoot)
	pods, err := m2.UntrackedPods(ctx)
	if err != nil || len(pods) != 1 {
		t.Fatalf("UntrackedPods = %+v, %v", pods, err)
	}

	sp, err := m2.Adopt(ctx, pods[0], AdoptSpec{AppRef: "../../examples/secret-app", Model: "model", Generation: 1})
	if err != nil {
		t.Fatalf("Adopt: %v", err)
	}
	if sp.ControlToken != wantToken {
		t.Fatalf("ControlToken = %q, want %q (read back from the still-running sidecar's env)", sp.ControlToken, wantToken)
	}
	if sp.ControlURL == "" {
		t.Fatal("ControlURL is empty after adopt")
	}
}

// A missing control token in the sidecar's env means the node cannot recover the control plane's
// bearer — Adopt must fail closed rather than leave a spawn with an empty (useless) token.
func TestAdoptFailsClosedWhenControlTokenMissing(t *testing.T) {
	ctx := context.Background()
	be := fakepod.New()
	t.Cleanup(be.Close)
	dataRoot := t.TempDir()

	labels := map[string]string{
		runtime.LabelManaged: "true", runtime.LabelSpawnID: "sp1",
		runtime.LabelGeneration: "1", runtime.LabelNodeID: "n1",
	}
	h, err := be.StartPod(ctx, runtime.PodSpec{ID: "sp1", SidecarImage: "sidecar:test", Labels: labels})
	if err != nil {
		t.Fatalf("StartPod: %v", err)
	}
	if err := be.StartAgent(ctx, h, runtime.AgentSpec{Image: "agent:base"}); err != nil {
		t.Fatalf("StartAgent: %v", err)
	}
	mp := runtime.ManagedPod{
		SpawnID: "sp1", Generation: 1, NodeID: "n1",
		SidecarID: h.SidecarID, AgentID: h.AgentID, PodIP: h.PodIP,
	}

	m := restart(t, be, &recordingApplier{}, dataRoot)
	if _, err := m.Adopt(ctx, mp, AdoptSpec{AppRef: "../../examples/secret-app", Model: "model", Generation: 1}); err == nil {
		t.Fatal("Adopt must fail closed when the sidecar env has no control token")
	}
	if _, ok := m.Store().Get("sp1"); ok {
		t.Fatal("a failed Adopt left the spawn in the store")
	}
}

// A ContainerEnv fault (the backend cannot read the sidecar's env at all) must fail Adopt closed,
// exactly like a missing token — the caller cannot distinguish "no control plane" from "no idea".
func TestAdoptFailsClosedWhenContainerEnvErrors(t *testing.T) {
	ctx := context.Background()
	be := fakepod.New()
	t.Cleanup(be.Close)
	dataRoot := t.TempDir()

	m1 := restart(t, be, &recordingApplier{}, dataRoot)
	if _, err := m1.Create(ctx, "sp1", "../../examples/secret-app", "model", "", "", 1); err != nil {
		t.Fatalf("Create: %v", err)
	}

	be.FailOn(fakepod.OpContainerEnv, errors.New("boom"))
	m2 := restart(t, be, &recordingApplier{}, dataRoot)
	pods, err := m2.UntrackedPods(ctx)
	if err != nil || len(pods) != 1 {
		t.Fatalf("UntrackedPods = %+v, %v", pods, err)
	}

	if _, err := m2.Adopt(ctx, pods[0], AdoptSpec{AppRef: "../../examples/secret-app", Model: "model", Generation: 1}); err == nil {
		t.Fatal("Adopt must fail closed when ContainerEnv errors")
	}
	if _, ok := m2.Store().Get("sp1"); ok {
		t.Fatal("a failed Adopt left the spawn in the store")
	}
}

// The UDS lane: Adopt reconstructs the transport from the sidecar's own env (SIDECAR_GETTOKEN_UDS),
// re-Serves on the HOST control-dir path (not the in-container path), and re-renders the agent's
// git-env trust bundle from the persisted CA — proving the whole re-adoption chain (D2's healing
// included, since this test's node has no persisted CA before Adopt runs).
func TestAdoptReservesUDSControlTransportAndRerendersGitEnv(t *testing.T) {
	ctx := context.Background()
	be := fakepod.New()
	t.Cleanup(be.Close)
	dataRoot := t.TempDir()
	overrideSidecarReadyProbe(t, nil)

	mock1 := &mockGitHubControlServer{}
	m1 := NewManagerWithBackend(be, &recordingApplier{}, ManagerConfig{
		AgentImage: "a", SidecarImage: "s", DataRoot: dataRoot, NodeID: "n1", UsernsMode: "remap",
	})
	m1.SetGitHubControlServer(mock1)
	if _, err := m1.Create(ctx, "sp1", "../../examples/secret-app", "model", "", "", 1); err != nil {
		t.Fatalf("Create: %v", err)
	}

	mock2 := &mockGitHubControlServer{}
	m2 := NewManagerWithBackend(be, &recordingApplier{}, ManagerConfig{
		AgentImage: "a", SidecarImage: "s", DataRoot: dataRoot, NodeID: "n1", UsernsMode: "remap",
	})
	m2.SetGitHubControlServer(mock2)
	pods, err := m2.UntrackedPods(ctx)
	if err != nil || len(pods) != 1 {
		t.Fatalf("UntrackedPods = %+v, %v", pods, err)
	}

	if _, err := m2.Adopt(ctx, pods[0], AdoptSpec{AppRef: "../../examples/secret-app", Model: "model", Generation: 1}); err != nil {
		t.Fatalf("Adopt: %v", err)
	}

	st, ok := mock2.lastServe()
	if !ok {
		t.Fatal("Adopt did not call Serve")
	}
	if st.Network != "unix" {
		t.Fatalf("Serve Network = %q, want unix", st.Network)
	}
	wantAddr := filepath.Join(m2.controlDirFor("sp1"), SidecarControlSocketName)
	if st.Address != wantAddr {
		t.Fatalf("Serve Address = %q, want %q (the HOST path)", st.Address, wantAddr)
	}
	if st.Bearer != "" {
		t.Fatalf("Serve Bearer = %q, want empty for the UDS lane", st.Bearer)
	}

	gitEnvDir := filepath.Join(dataRoot, "git-env", "sp1")
	caCert, err := os.ReadFile(filepath.Join(gitEnvDir, SpawnCACertName))
	if err != nil {
		t.Fatalf("read spawn-ca.crt: %v", err)
	}
	if string(caCert) != "fake-ca-cert" {
		t.Fatalf("spawn-ca.crt = %q, want the CA the mock control server returned", caCert)
	}
	bundle, err := os.ReadFile(filepath.Join(gitEnvDir, CABundleName))
	if err != nil {
		t.Fatalf("read ca-bundle.crt: %v", err)
	}
	if !strings.HasSuffix(strings.TrimRight(string(bundle), "\n"), "fake-ca-cert") {
		t.Fatalf("ca-bundle.crt = %q, want it to end with the spawn CA", bundle)
	}
}

// The TCP lane: Adopt reconstructs Address/Bearer/PodIP from the sidecar's own env
// (SIDECAR_GETTOKEN_ADDR/_BEARER), never re-derived from the CURRENT node config (D3) — a node
// whose GetTokenListenIP changed across the restart must not "fix" a running pod onto a lane the
// sidecar was never told about.
func TestAdoptReservesTCPControlTransportFromSidecarEnv(t *testing.T) {
	ctx := context.Background()
	be := fakepod.New()
	t.Cleanup(be.Close)
	dataRoot := t.TempDir()
	overrideSidecarReadyProbe(t, nil)

	mock1 := &mockGitHubControlServer{}
	m1 := NewManagerWithBackend(be, &recordingApplier{}, ManagerConfig{
		AgentImage: "a", SidecarImage: "s", DataRoot: dataRoot, NodeID: "n1",
		UsernsMode: "off", GetTokenListenIP: "127.0.0.1",
	})
	m1.SetGitHubControlServer(mock1)
	if _, err := m1.Create(ctx, "sp1", "../../examples/secret-app", "model", "", "", 1); err != nil {
		t.Fatalf("Create: %v", err)
	}
	wantAddr := sidecarEnvVal(be.PodSpec("sp1").SidecarEnv, SidecarGetTokenAddrEnv)
	wantBearer := sidecarEnvVal(be.PodSpec("sp1").SidecarEnv, SidecarGetTokenBearerEnv)
	if wantAddr == "" || wantBearer == "" {
		t.Fatal("setup: sidecar env missing TCP GetToken vars")
	}

	// A DIFFERENT GetTokenListenIP on the "restarted" node must NOT change the transport Adopt
	// re-serves — it must come from the sidecar's env, not this node's current config.
	mock2 := &mockGitHubControlServer{}
	m2 := NewManagerWithBackend(be, &recordingApplier{}, ManagerConfig{
		AgentImage: "a", SidecarImage: "s", DataRoot: dataRoot, NodeID: "n1",
		UsernsMode: "off", GetTokenListenIP: "10.9.9.9",
	})
	m2.SetGitHubControlServer(mock2)
	pods, err := m2.UntrackedPods(ctx)
	if err != nil || len(pods) != 1 {
		t.Fatalf("UntrackedPods = %+v, %v", pods, err)
	}

	if _, err := m2.Adopt(ctx, pods[0], AdoptSpec{AppRef: "../../examples/secret-app", Model: "model", Generation: 1}); err != nil {
		t.Fatalf("Adopt: %v", err)
	}

	st, ok := mock2.lastServe()
	if !ok {
		t.Fatal("Adopt did not call Serve")
	}
	if st.Network != "tcp" {
		t.Fatalf("Serve Network = %q, want tcp", st.Network)
	}
	if st.Address != wantAddr {
		t.Fatalf("Serve Address = %q, want %q (from the sidecar env, not the current node config)", st.Address, wantAddr)
	}
	if st.Bearer != wantBearer {
		t.Fatalf("Serve Bearer = %q, want %q", st.Bearer, wantBearer)
	}
	if st.PodIP != pods[0].PodIP {
		t.Fatalf("Serve PodIP = %q, want %q", st.PodIP, pods[0].PodIP)
	}
}

// A spawn created with no GitHub wiring (no SIDECAR_GETTOKEN_* in its env) has nothing for Adopt to
// re-serve — Serve must not be called, and Adopt still succeeds.
func TestAdoptSkipsServeWhenNoTransportInSidecarEnv(t *testing.T) {
	ctx := context.Background()
	be := fakepod.New()
	t.Cleanup(be.Close)
	dataRoot := t.TempDir()
	overrideSidecarReadyProbe(t, nil)

	// UsernsMode "off" + no GetTokenListenIP: the TCP lane with nothing to inject (see Create's own
	// comment: "No GetTokenListenIP: SIDECAR_GETTOKEN_ADDR is not injected").
	mock1 := &mockGitHubControlServer{}
	m1 := NewManagerWithBackend(be, &recordingApplier{}, ManagerConfig{
		AgentImage: "a", SidecarImage: "s", DataRoot: dataRoot, NodeID: "n1", UsernsMode: "off",
	})
	m1.SetGitHubControlServer(mock1)
	if _, err := m1.Create(ctx, "sp1", "../../examples/secret-app", "model", "", "", 1); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if sidecarEnvVal(be.PodSpec("sp1").SidecarEnv, SidecarGetTokenAddrEnv) != "" {
		t.Fatal("setup: expected no SIDECAR_GETTOKEN_ADDR in sidecar env")
	}

	mock2 := &mockGitHubControlServer{}
	m2 := NewManagerWithBackend(be, &recordingApplier{}, ManagerConfig{
		AgentImage: "a", SidecarImage: "s", DataRoot: dataRoot, NodeID: "n1", UsernsMode: "off",
	})
	m2.SetGitHubControlServer(mock2)
	pods, err := m2.UntrackedPods(ctx)
	if err != nil || len(pods) != 1 {
		t.Fatalf("UntrackedPods = %+v, %v", pods, err)
	}

	if _, err := m2.Adopt(ctx, pods[0], AdoptSpec{AppRef: "../../examples/secret-app", Model: "model", Generation: 1}); err != nil {
		t.Fatalf("Adopt: %v", err)
	}
	if _, ok := mock2.lastServe(); ok {
		t.Fatal("Adopt called Serve with no transport in the sidecar env")
	}
}

// A Serve error on adopt is fail-closed: the spawn is left out of the store entirely (the caller
// then capture-before-reaps the pod).
func TestAdoptFailsClosedOnServeError(t *testing.T) {
	ctx := context.Background()
	be := fakepod.New()
	t.Cleanup(be.Close)
	dataRoot := t.TempDir()
	overrideSidecarReadyProbe(t, nil)

	mock1 := &mockGitHubControlServer{}
	m1 := NewManagerWithBackend(be, &recordingApplier{}, ManagerConfig{
		AgentImage: "a", SidecarImage: "s", DataRoot: dataRoot, NodeID: "n1", UsernsMode: "remap",
	})
	m1.SetGitHubControlServer(mock1)
	if _, err := m1.Create(ctx, "sp1", "../../examples/secret-app", "model", "", "", 1); err != nil {
		t.Fatalf("Create: %v", err)
	}

	mock2 := &mockGitHubControlServer{serveErr: errors.New("bind failed")}
	m2 := NewManagerWithBackend(be, &recordingApplier{}, ManagerConfig{
		AgentImage: "a", SidecarImage: "s", DataRoot: dataRoot, NodeID: "n1", UsernsMode: "remap",
	})
	m2.SetGitHubControlServer(mock2)
	pods, err := m2.UntrackedPods(ctx)
	if err != nil || len(pods) != 1 {
		t.Fatalf("UntrackedPods = %+v, %v", pods, err)
	}

	if _, err := m2.Adopt(ctx, pods[0], AdoptSpec{AppRef: "../../examples/secret-app", Model: "model", Generation: 1}); err == nil {
		t.Fatal("Adopt must fail closed when Serve errors")
	}
	if _, ok := m2.Store().Get("sp1"); ok {
		t.Fatal("a failed Adopt (Serve error) left the spawn in the store")
	}
}

// ghControl == nil: none of the GitHub control-server logic runs, and Adopt still recovers the
// control token from the sidecar env.
func TestAdoptWithoutGitHubControlServer(t *testing.T) {
	ctx := context.Background()
	be := fakepod.New()
	t.Cleanup(be.Close)
	dataRoot := t.TempDir()

	m1 := restart(t, be, &recordingApplier{}, dataRoot)
	if _, err := m1.Create(ctx, "sp1", "../../examples/secret-app", "model", "", "", 1); err != nil {
		t.Fatalf("Create: %v", err)
	}

	m2 := restart(t, be, &recordingApplier{}, dataRoot) // ghControl intentionally not set
	pods, err := m2.UntrackedPods(ctx)
	if err != nil || len(pods) != 1 {
		t.Fatalf("UntrackedPods = %+v, %v", pods, err)
	}

	sp, err := m2.Adopt(ctx, pods[0], AdoptSpec{AppRef: "../../examples/secret-app", Model: "model", Generation: 1})
	if err != nil {
		t.Fatalf("Adopt: %v", err)
	}
	if sp.ControlToken == "" {
		t.Fatal("adopted spawn has an empty ControlToken even without a GitHub control server")
	}
}
