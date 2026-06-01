package runtime

import (
	"strings"
	"testing"
)

func TestDialOnceFailsWithoutPrivilege(t *testing.T) {
	// Dialing into our own netns still requires CAP_SYS_ADMIN for setns; under
	// the unprivileged test runner this must error (not hang, not panic).
	_, err := dialOnce("/proc/self/ns/net", "@spawnery-acp-test-nonexistent")
	if err == nil {
		t.Fatal("expected error dialing without privilege / missing socket")
	}
	if strings.Contains(err.Error(), "panic") {
		t.Fatalf("unexpected: %v", err)
	}
}

func TestDialOnceBogusNetnsPath(t *testing.T) {
	_, err := dialOnce("/proc/nonexistent-pid/ns/net", "@x")
	if err == nil {
		t.Fatal("expected error opening bogus netns path")
	}
}
