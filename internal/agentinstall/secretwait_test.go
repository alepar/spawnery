package agentinstall

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestWaitForSecrets_AllPresent verifies that waitForSecrets returns nil when all secret files
// already exist before the call.
func TestWaitForSecrets_AllPresent(t *testing.T) {
	secretsDir := t.TempDir()
	refs := []string{"TOKEN_A", "TOKEN_B"}
	for _, ref := range refs {
		if err := os.WriteFile(filepath.Join(secretsDir, ref), []byte("val"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	missing := waitForSecrets(secretsDir, refs, 100*time.Millisecond)
	if len(missing) != 0 {
		t.Errorf("expected nil missing, got %v", missing)
	}
}

// TestWaitForSecrets_Timeout verifies that waitForSecrets returns the missing refs when files
// never appear before the timeout expires.
func TestWaitForSecrets_Timeout(t *testing.T) {
	secretsDir := t.TempDir()
	refs := []string{"NEVER_WRITTEN"}

	start := time.Now()
	missing := waitForSecrets(secretsDir, refs, 120*time.Millisecond)
	elapsed := time.Since(start)

	if len(missing) != 1 || missing[0] != "NEVER_WRITTEN" {
		t.Errorf("expected [NEVER_WRITTEN], got %v", missing)
	}
	// Should have waited at least ~100ms (one sleep + check cycle)
	if elapsed < 100*time.Millisecond {
		t.Errorf("returned too fast: %v (expected ~120ms)", elapsed)
	}
}

// TestWaitForSecrets_AppearsLate verifies that waitForSecrets succeeds when the secret file
// is written after a short delay (simulates async delivery).
func TestWaitForSecrets_AppearsLate(t *testing.T) {
	secretsDir := t.TempDir()
	ref := "LATE_TOKEN"

	// Write the file after 80ms in a goroutine.
	go func() {
		time.Sleep(80 * time.Millisecond)
		_ = os.WriteFile(filepath.Join(secretsDir, ref), []byte("secret"), 0o600)
	}()

	missing := waitForSecrets(secretsDir, []string{ref}, 500*time.Millisecond)
	if len(missing) != 0 {
		t.Errorf("expected nil missing after late delivery, got %v", missing)
	}
}

// TestWaitForSecrets_ZeroTimeout verifies that timeout<=0 performs a single check without
// sleeping, even when files are absent.
func TestWaitForSecrets_ZeroTimeout(t *testing.T) {
	secretsDir := t.TempDir()
	refs := []string{"NO_WAIT_TOKEN"}

	start := time.Now()
	missing := waitForSecrets(secretsDir, refs, 0)
	elapsed := time.Since(start)

	if len(missing) != 1 || missing[0] != "NO_WAIT_TOKEN" {
		t.Errorf("expected [NO_WAIT_TOKEN], got %v", missing)
	}
	// Single check should return in well under 50ms (no sleep).
	if elapsed >= 50*time.Millisecond {
		t.Errorf("zero-timeout check took too long: %v (expected <50ms)", elapsed)
	}
}

// TestWaitForSecrets_InvalidRefTreatedMissing verifies that refs failing validateMCPName
// are treated as permanently missing.
func TestWaitForSecrets_InvalidRefTreatedMissing(t *testing.T) {
	secretsDir := t.TempDir()
	refs := []string{"../escape"}

	missing := waitForSecrets(secretsDir, refs, 0)
	if len(missing) != 1 || missing[0] != "../escape" {
		t.Errorf("expected invalid ref to be reported missing, got %v", missing)
	}
}
