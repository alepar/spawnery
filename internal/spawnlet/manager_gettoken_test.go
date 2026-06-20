package spawnlet

import (
	"context"
	"os"
	"strings"
	"sync"
	"testing"
)

// mockGitHubControlServer records Serve/Stop/SpawnCACert calls for assertions.
type mockGitHubControlServer struct {
	mu        sync.Mutex
	serveArgs []ControlTransport
	stopArgs  []string
	serveErr  error
}

func (m *mockGitHubControlServer) Serve(t ControlTransport) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.serveArgs = append(m.serveArgs, t)
	return m.serveErr
}

func (m *mockGitHubControlServer) Stop(spawnID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.stopArgs = append(m.stopArgs, spawnID)
}

func (m *mockGitHubControlServer) SpawnCACert(spawnID string) ([]byte, error) {
	return []byte("fake-ca-cert"), nil
}

func (m *mockGitHubControlServer) lastServe() (ControlTransport, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.serveArgs) == 0 {
		return ControlTransport{}, false
	}
	return m.serveArgs[len(m.serveArgs)-1], true
}

func (m *mockGitHubControlServer) stopCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.stopArgs)
}

// sidecarEnvVal extracts the value for key from the sidecar env, or "" if missing.
func sidecarEnvVal(env []string, key string) string {
	prefix := key + "="
	for _, e := range env {
		if strings.HasPrefix(e, prefix) {
			return strings.TrimPrefix(e, prefix)
		}
	}
	return ""
}

// TestManagerGetTokenUDSLane verifies that when UsernsMode="remap", CreateWithSelection:
//   - creates the host control dir with mode 0711
//   - adds SIDECAR_GETTOKEN_UDS env var to the sidecar
//   - adds a SidecarMount for the control dir
//   - calls ghControl.Serve with Network="unix"
func TestManagerGetTokenUDSLane(t *testing.T) {
	fb := &fakePodBackend{}
	mock := &mockGitHubControlServer{}
	overrideSidecarReadyProbe(t, nil) // sp-n7iy.5: probe added; stub so test doesn't dial 10.0.0.5
	m := NewManagerWithBackend(fb, &fakeApplier{}, ManagerConfig{
		AgentImage:   "a",
		SidecarImage: "s",
		DataRoot:     t.TempDir(),
		UsernsMode:   "remap",
	})
	m.SetGitHubControlServer(mock)

	_, err := m.Create(context.Background(), "sp-uds", writeApp(t), "model", "", "", 0)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Verify SIDECAR_GETTOKEN_UDS is in sidecar env.
	if sidecarEnvVal(fb.podSpec.SidecarEnv, SidecarGetTokenUDSEnv) == "" {
		t.Fatalf("SIDECAR_GETTOKEN_UDS missing from sidecar env; env=%v", fb.podSpec.SidecarEnv)
	}

	// Verify NO TCP vars.
	if sidecarEnvVal(fb.podSpec.SidecarEnv, SidecarGetTokenAddrEnv) != "" {
		t.Fatalf("SIDECAR_GETTOKEN_ADDR must not be set in UDS lane; env=%v", fb.podSpec.SidecarEnv)
	}
	if sidecarEnvVal(fb.podSpec.SidecarEnv, SidecarGetTokenBearerEnv) != "" {
		t.Fatalf("SIDECAR_GETTOKEN_BEARER must not be set in UDS lane; env=%v", fb.podSpec.SidecarEnv)
	}

	// Verify a sidecar mount was added.
	if len(fb.podSpec.SidecarMounts) == 0 {
		t.Fatal("SidecarMounts empty in UDS lane; expected control dir bind-mount")
	}
	found := false
	for _, mn := range fb.podSpec.SidecarMounts {
		if mn.ContainerPath == SidecarControlMountPath {
			found = true
			// The host control dir must exist and be 0711.
			fi, ferr := os.Stat(mn.HostPath)
			if ferr != nil {
				t.Fatalf("control dir %q does not exist: %v", mn.HostPath, ferr)
			}
			if fi.Mode().Perm() != 0o711 {
				t.Fatalf("control dir perm = %o, want 0711", fi.Mode().Perm())
			}
		}
	}
	if !found {
		t.Fatalf("no SidecarMount with ContainerPath=%s; mounts=%v", SidecarControlMountPath, fb.podSpec.SidecarMounts)
	}

	// Verify Serve was called with Network="unix".
	st, ok := mock.lastServe()
	if !ok {
		t.Fatal("ghControl.Serve was not called")
	}
	if st.Network != "unix" {
		t.Fatalf("Serve Network = %q, want unix", st.Network)
	}
	if st.SpawnID != "sp-uds" {
		t.Fatalf("Serve SpawnID = %q, want sp-uds", st.SpawnID)
	}
}

// TestManagerGetTokenTCPLane verifies that when UsernsMode="off" and GetTokenListenIP is set,
// CreateWithSelection:
//   - adds SIDECAR_GETTOKEN_ADDR and SIDECAR_GETTOKEN_BEARER env vars
//   - does NOT add SidecarMounts for the control dir
//   - calls ghControl.Serve with Network="tcp" and the correct bearer
func TestManagerGetTokenTCPLane(t *testing.T) {
	fb := &fakePodBackend{}
	mock := &mockGitHubControlServer{}
	overrideSidecarReadyProbe(t, nil) // sp-n7iy.5: probe added; stub so test doesn't dial 10.0.0.5
	m := NewManagerWithBackend(fb, &fakeApplier{}, ManagerConfig{
		AgentImage:       "a",
		SidecarImage:     "s",
		DataRoot:         t.TempDir(),
		UsernsMode:       "off",
		GetTokenListenIP: "127.0.0.1",
	})
	m.SetGitHubControlServer(mock)

	_, err := m.Create(context.Background(), "sp-tcp", writeApp(t), "model", "", "", 0)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	addr := sidecarEnvVal(fb.podSpec.SidecarEnv, SidecarGetTokenAddrEnv)
	if addr == "" {
		t.Fatalf("SIDECAR_GETTOKEN_ADDR missing from sidecar env; env=%v", fb.podSpec.SidecarEnv)
	}
	if !strings.HasPrefix(addr, "127.0.0.1:") {
		t.Fatalf("SIDECAR_GETTOKEN_ADDR = %q, want 127.0.0.1:<port>", addr)
	}

	bearer := sidecarEnvVal(fb.podSpec.SidecarEnv, SidecarGetTokenBearerEnv)
	if bearer == "" {
		t.Fatalf("SIDECAR_GETTOKEN_BEARER missing from sidecar env; env=%v", fb.podSpec.SidecarEnv)
	}

	// Verify NO UDS env var.
	if sidecarEnvVal(fb.podSpec.SidecarEnv, SidecarGetTokenUDSEnv) != "" {
		t.Fatalf("SIDECAR_GETTOKEN_UDS must not be set in TCP lane; env=%v", fb.podSpec.SidecarEnv)
	}

	// Verify no SidecarMounts for the control dir.
	for _, mn := range fb.podSpec.SidecarMounts {
		if mn.ContainerPath == SidecarControlMountPath {
			t.Fatalf("SidecarMount for %s must not be present in TCP lane", SidecarControlMountPath)
		}
	}

	// Verify Serve called with Network="tcp" and matching bearer.
	st, ok := mock.lastServe()
	if !ok {
		t.Fatal("ghControl.Serve was not called")
	}
	if st.Network != "tcp" {
		t.Fatalf("Serve Network = %q, want tcp", st.Network)
	}
	if st.Bearer != bearer {
		t.Fatalf("Serve Bearer = %q, want %q (sidecar env value)", st.Bearer, bearer)
	}
	if st.SpawnID != "sp-tcp" {
		t.Fatalf("Serve SpawnID = %q, want sp-tcp", st.SpawnID)
	}
}

// TestManagerGetTokenNoServer verifies that without SetGitHubControlServer no SIDECAR_GETTOKEN_*
// env vars are injected and no panic occurs.
func TestManagerGetTokenNoServer(t *testing.T) {
	fb := &fakePodBackend{}
	m := NewManagerWithBackend(fb, &fakeApplier{}, ManagerConfig{
		AgentImage:   "a",
		SidecarImage: "s",
		DataRoot:     t.TempDir(),
		UsernsMode:   "remap",
	})
	// ghControl intentionally NOT set.

	_, err := m.Create(context.Background(), "sp-nil", writeApp(t), "model", "", "", 0)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	for _, e := range fb.podSpec.SidecarEnv {
		if strings.HasPrefix(e, "SIDECAR_GETTOKEN") {
			t.Fatalf("SIDECAR_GETTOKEN_* must not be injected without a control server; got %q", e)
		}
	}
}

// TestManagerStopCallsGhControlStop verifies that Stop triggers ghControl.Stop for the spawn.
func TestManagerStopCallsGhControlStop(t *testing.T) {
	fb := &fakePodBackend{}
	mock := &mockGitHubControlServer{}
	overrideSidecarReadyProbe(t, nil) // sp-n7iy.5: probe added; stub so test doesn't dial 10.0.0.5
	m := NewManagerWithBackend(fb, &fakeApplier{}, ManagerConfig{
		AgentImage:   "a",
		SidecarImage: "s",
		DataRoot:     t.TempDir(),
		UsernsMode:   "remap",
	})
	m.SetGitHubControlServer(mock)

	_, err := m.Create(context.Background(), "sp-stop", writeApp(t), "model", "", "", 0)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Stop must call ghControl.Stop.
	if err := m.Stop(context.Background(), "sp-stop"); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	if mock.stopCount() == 0 {
		t.Fatal("ghControl.Stop was not called on spawn Stop")
	}
}
