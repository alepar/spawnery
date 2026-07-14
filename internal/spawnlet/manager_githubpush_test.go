package spawnlet

import (
	"context"
	"errors"
	"strings"
	"testing"

	"spawnery/internal/runtime/fakepod"
)

// testSpawnID is the spawn id used by every test in this file.
const testSpawnID = "sp-ghpush"

// newGitHubTestManager builds a Manager wired to a fresh fakepod.Backend and mock, mirroring the
// fixture used by TestManagerGetTokenUDSLane (manager_gettoken_test.go): a fake pod backend, a
// t.TempDir() DataRoot, UsernsMode "remap", and the sidecar-ready probe stubbed out so Create
// never dials a real sidecar.
func newGitHubTestManager(t *testing.T, mock *mockGitHubControlServer) (*Manager, *fakepod.Backend) {
	t.Helper()
	fb := fakeBackend(t)
	overrideSidecarReadyProbe(t, nil)
	m := NewManagerWithBackend(fb, &fakeApplier{}, ManagerConfig{
		AgentImage:   "a",
		SidecarImage: "s",
		DataRoot:     t.TempDir(),
		UsernsMode:   "remap",
	})
	m.SetGitHubControlServer(mock)
	return m, fb
}

// createTestSpawn creates testSpawnID against m, mirroring the Create call shape used throughout
// manager_gettoken_test.go.
func createTestSpawn(t *testing.T, m *Manager) (*Spawn, error) {
	t.Helper()
	return m.Create(context.Background(), testSpawnID, writeApp(t), "model", "", "", 0)
}

// startAgentRanBefore reports whether ops (the fake backend's op log, taken at some point in time)
// already contains a "startagent:" entry for testSpawnID.
func startAgentRan(ops []string) bool {
	for _, op := range ops {
		if strings.HasPrefix(op, "startagent:") {
			return true
		}
	}
	return false
}

func stopRan(ops []string) bool {
	for _, op := range ops {
		if strings.HasPrefix(op, "stop:") {
			return true
		}
	}
	return false
}

// TestCreatePushesGitHubCredentialsBeforeAgent asserts the node pushes the spawn's CA + token into
// the sidecar AFTER the readiness probe and BEFORE the agent container starts (spec §3.1: "the agent
// must never start without a working proxy").
func TestCreatePushesGitHubCredentialsBeforeAgent(t *testing.T) {
	mock := &mockGitHubControlServer{}
	m, fb := newGitHubTestManager(t, mock)
	var agentStartedBeforePush bool
	mock.onPush = func() {
		agentStartedBeforePush = startAgentRan(fb.Ops())
	}

	if _, err := createTestSpawn(t, m); err != nil {
		t.Fatalf("create: %v", err)
	}
	pushes := mock.pushCalls()
	if len(pushes) != 1 {
		t.Fatalf("PushCredentials calls = %d, want 1", len(pushes))
	}
	if pushes[0].spawnID != testSpawnID {
		t.Fatalf("pushed spawn id = %q, want %q", pushes[0].spawnID, testSpawnID)
	}
	if pushes[0].controlURL == "" || pushes[0].controlToken == "" {
		t.Fatalf("push got empty control endpoint: %+v", pushes[0])
	}
	if agentStartedBeforePush {
		t.Fatalf("StartAgent ran before PushCredentials — the agent must never start without a working proxy")
	}
	if !startAgentRan(fb.Ops()) {
		t.Fatal("StartAgent never ran after a successful push")
	}
}

// TestCreatePushFailureTearsThePodDown asserts push-at-create is FAIL-CLOSED (spec §4).
func TestCreatePushFailureTearsThePodDown(t *testing.T) {
	mock := &mockGitHubControlServer{pushErr: errors.New("sidecar unreachable")}
	m, fb := newGitHubTestManager(t, mock)

	if _, err := createTestSpawn(t, m); err == nil {
		t.Fatal("create succeeded despite a failed credential push; want a fail-closed error")
	}
	if startAgentRan(fb.Ops()) {
		t.Fatal("StartAgent ran after a failed credential push")
	}
	if !stopRan(fb.Ops()) {
		t.Fatal("pod was not torn down after a failed credential push")
	}
}
