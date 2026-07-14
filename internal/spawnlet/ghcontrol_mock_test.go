package spawnlet

import (
	"context"
	"strings"
	"sync"
	"testing"
)

// mockGitHubControlServer records Stop/SpawnCACert/PushCredentials calls for assertions. There is no
// Serve: sp-2tx8.9.5 deleted the node's inbound control listener.
type mockGitHubControlServer struct {
	mu       sync.Mutex
	stopArgs []string

	pushArgs []pushCall
	pushErr  error
	// onPush, if set, is called synchronously from PushCredentials before it returns — a test hook
	// for asserting push-then-StartAgent ordering against the fake backend's op log.
	onPush func()
}

// pushCall records one PushCredentials invocation.
type pushCall struct {
	spawnID      string
	controlURL   string
	controlToken string
}

func (m *mockGitHubControlServer) Stop(spawnID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.stopArgs = append(m.stopArgs, spawnID)
}

func (m *mockGitHubControlServer) SpawnCACert(spawnID string) ([]byte, error) {
	return []byte("fake-ca-cert"), nil
}

func (m *mockGitHubControlServer) PushCredentials(_ context.Context, spawnID, controlURL, controlToken string) error {
	m.mu.Lock()
	m.pushArgs = append(m.pushArgs, pushCall{spawnID: spawnID, controlURL: controlURL, controlToken: controlToken})
	err := m.pushErr
	onPush := m.onPush
	m.mu.Unlock()
	if onPush != nil {
		onPush()
	}
	return err
}

func (m *mockGitHubControlServer) pushCalls() []pushCall {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]pushCall(nil), m.pushArgs...)
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

// TestManagerStopCallsGhControlStop verifies that Stop triggers ghControl.Stop for the spawn (which
// cancels its push loop + rejection watch and purges its CA).
func TestManagerStopCallsGhControlStop(t *testing.T) {
	fb := fakeBackend(t)
	mock := &mockGitHubControlServer{}
	overrideSidecarReadyProbe(t, nil)
	m := NewManagerWithBackend(fb, &fakeApplier{}, ManagerConfig{
		AgentImage:   "a",
		SidecarImage: "s",
		DataRoot:     t.TempDir(),
		UsernsMode:   "remap",
	})
	m.SetGitHubControlServer(mock)

	if _, err := m.Create(context.Background(), "sp-stop", writeApp(t), "model", "", "", 0); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := m.Stop(context.Background(), "sp-stop"); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if mock.stopCount() == 0 {
		t.Fatal("ghControl.Stop was not called on spawn Stop")
	}
}
