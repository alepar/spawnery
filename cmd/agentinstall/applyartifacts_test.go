package main

// Tests for deploy/agent/apply-artifacts.sh via hermetic sh invocations.
// Uses runtime.Caller to locate the helper relative to this test file,
// builds agentinstall onto a temp PATH, and exercises the emitter-map and
// old-image-guard behaviours without a real container.

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// helperScript returns the absolute path to apply-artifacts.sh relative to this test file.
func helperScript(t *testing.T) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	// thisFile: <repo>/cmd/agentinstall/applyartifacts_test.go
	// helper:   <repo>/deploy/agent/apply-artifacts.sh
	repoRoot := filepath.Dir(filepath.Dir(filepath.Dir(thisFile)))
	helper := filepath.Join(repoRoot, "deploy", "agent", "apply-artifacts.sh")
	if _, err := os.Stat(helper); err != nil {
		t.Fatalf("apply-artifacts.sh not found at %s: %v", helper, err)
	}
	return helper
}

// buildAgentinstallToDir builds the agentinstall binary into dir and returns its path.
func buildAgentinstallToDir(t *testing.T, dir string) string {
	t.Helper()
	binPath := filepath.Join(dir, "agentinstall")
	cmd := exec.Command("go", "build", "-o", binPath, "spawnery/cmd/agentinstall")
	cmd.Env = append(os.Environ(), "CGO_ENABLED=1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("build agentinstall: %v\n%s", err, out)
	}
	return binPath
}

// runHelper runs apply-artifacts.sh with the given runnable and env overrides,
// returning stdout+stderr combined and the exit code.
func runHelper(t *testing.T, helper, runnable string, env []string) (string, int) {
	t.Helper()
	cmd := exec.Command("sh", helper, runnable)
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	code := 0
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			code = ee.ExitCode()
		} else {
			t.Fatalf("run helper: %v", err)
		}
	}
	return string(out), code
}

// TestApplyArtifacts_ClaudeTUIWritesConfig verifies that with a valid manifest targeting
// claude, apply-artifacts.sh for claude-tui writes ~/.claude.json.
func TestApplyArtifacts_ClaudeTUIWritesConfig(t *testing.T) {
	helper := helperScript(t)
	home := t.TempDir()
	binDir := t.TempDir()
	buildAgentinstallToDir(t, binDir)

	// Build staging dir with a manifest targeting claude.
	artifactsDir := t.TempDir()
	manifest := `{"artifacts":[{"kind":"mcp","name":"test-mcp","targets":["claude"],"mcp":{"http":{"url":"http://localhost:9999"}}}]}`
	if err := os.WriteFile(filepath.Join(artifactsDir, "manifest.json"), []byte(manifest), 0o600); err != nil {
		t.Fatal(err)
	}
	secretsDir := t.TempDir()

	env := []string{
		"HOME=" + home,
		"PATH=" + binDir + ":/usr/bin:/bin",
		"SPAWNERY_ARTIFACTS_DIR=" + artifactsDir,
		"SPAWNERY_SECRETS_DIR=" + secretsDir,
		"SECRET_WAIT_TIMEOUT=1s",
	}

	out, code := runHelper(t, helper, "claude-tui", env)
	if code != 0 {
		t.Fatalf("expected exit 0, got %d\noutput:\n%s", code, out)
	}

	// ~/.claude.json should have been written by agentinstall.
	claudeJSON := filepath.Join(home, ".claude.json")
	data, err := os.ReadFile(claudeJSON)
	if err != nil {
		t.Fatalf("~/.claude.json not written (out=%q): %v", out, err)
	}
	var root map[string]interface{}
	if err := json.Unmarshal(data, &root); err != nil {
		t.Fatalf("parse .claude.json: %v", err)
	}
	servers, ok := root["mcpServers"].(map[string]interface{})
	if !ok || servers["test-mcp"] == nil {
		t.Errorf("test-mcp not found in .claude.json mcpServers: %+v", root)
	}

	// apply-report.json should have been written.
	report := filepath.Join(artifactsDir, "apply-report.json")
	if _, err := os.Stat(report); err != nil {
		t.Errorf("apply-report.json not written: %v", err)
	}
}

// TestApplyArtifacts_NoOpRunnable verifies that a no-op runnable (e.g. goose-tui) exits 0
// and writes nothing, even with a real agentinstall in PATH and a valid manifest.
func TestApplyArtifacts_NoOpRunnable(t *testing.T) {
	helper := helperScript(t)
	home := t.TempDir()
	binDir := t.TempDir()
	buildAgentinstallToDir(t, binDir)

	artifactsDir := t.TempDir()
	manifest := `{"artifacts":[{"kind":"mcp","name":"test-mcp","targets":["claude"],"mcp":{"http":{"url":"http://localhost:9999"}}}]}`
	if err := os.WriteFile(filepath.Join(artifactsDir, "manifest.json"), []byte(manifest), 0o600); err != nil {
		t.Fatal(err)
	}

	env := []string{
		"HOME=" + home,
		"PATH=" + binDir + ":/usr/bin:/bin",
		"SPAWNERY_ARTIFACTS_DIR=" + artifactsDir,
		"SPAWNERY_SECRETS_DIR=" + t.TempDir(),
		"SECRET_WAIT_TIMEOUT=1s",
	}

	_, code := runHelper(t, helper, "goose-tui", env)
	if code != 0 {
		t.Fatalf("expected exit 0 for no-op runnable, got %d", code)
	}

	// No config should have been written (goose-tui maps to no-op).
	claudeJSON := filepath.Join(home, ".claude.json")
	if _, err := os.Stat(claudeJSON); !os.IsNotExist(err) {
		t.Errorf("unexpected write to ~/.claude.json for goose-tui no-op: err=%v", err)
	}
}

// TestApplyArtifacts_OldImageGuard verifies that when agentinstall is absent from PATH,
// the helper exits 0 and prints a diagnostic to stderr.
func TestApplyArtifacts_OldImageGuard(t *testing.T) {
	helper := helperScript(t)
	home := t.TempDir()
	artifactsDir := t.TempDir()

	// Write a manifest so we don't hit the "no manifest" early-exit.
	manifest := `{"artifacts":[{"kind":"mcp","name":"test-mcp","targets":["claude"],"mcp":{"http":{"url":"http://localhost:9999"}}}]}`
	if err := os.WriteFile(filepath.Join(artifactsDir, "manifest.json"), []byte(manifest), 0o600); err != nil {
		t.Fatal(err)
	}

	// PATH has NO agentinstall — deliberately omit binDir.
	env := []string{
		"HOME=" + home,
		"PATH=/usr/bin:/bin", // no agentinstall here
		"SPAWNERY_ARTIFACTS_DIR=" + artifactsDir,
		"SPAWNERY_SECRETS_DIR=" + t.TempDir(),
	}

	out, code := runHelper(t, helper, "claude-tui", env)
	if code != 0 {
		t.Fatalf("expected exit 0 for old-image guard, got %d\noutput:\n%s", code, out)
	}
	// Diagnostic message should mention agentinstall or old image.
	if !strings.Contains(out, "agentinstall") {
		t.Errorf("expected diagnostic mentioning agentinstall, got:\n%s", out)
	}

	// No config written (guard fired before invoking agentinstall).
	if _, err := os.Stat(filepath.Join(home, ".claude.json")); !os.IsNotExist(err) {
		t.Errorf("unexpected write to ~/.claude.json when guard fired: err=%v", err)
	}
}

// TestApplyArtifacts_NoManifest verifies that the helper exits 0 silently when there is no
// manifest.json in the artifacts dir.
func TestApplyArtifacts_NoManifest(t *testing.T) {
	helper := helperScript(t)
	home := t.TempDir()
	binDir := t.TempDir()
	buildAgentinstallToDir(t, binDir)

	artifactsDir := t.TempDir() // deliberately empty — no manifest.json

	env := []string{
		"HOME=" + home,
		"PATH=" + binDir + ":/usr/bin:/bin",
		"SPAWNERY_ARTIFACTS_DIR=" + artifactsDir,
		"SPAWNERY_SECRETS_DIR=" + t.TempDir(),
	}

	out, code := runHelper(t, helper, "claude-tui", env)
	if code != 0 {
		t.Fatalf("expected exit 0 for missing manifest, got %d\noutput:\n%s", code, out)
	}
}

// TestApplyArtifacts_SecretWaitRoundTrip verifies that a pre-written secret file is picked up
// by agentinstall and the value lands in ~/.claude.json.
func TestApplyArtifacts_SecretWaitRoundTrip(t *testing.T) {
	helper := helperScript(t)
	home := t.TempDir()
	binDir := t.TempDir()
	buildAgentinstallToDir(t, binDir)

	artifactsDir := t.TempDir()
	secretsDir := t.TempDir()

	// Write the secret file BEFORE running the helper (simulates a pre-start sync delivery).
	if err := os.WriteFile(filepath.Join(secretsDir, "MY_TOKEN"), []byte("s3cr3t-value"), 0o600); err != nil {
		t.Fatal(err)
	}

	// Manifest: claude MCP with a secretRef.
	manifest := `{"artifacts":[{"kind":"mcp","name":"secret-mcp","targets":["claude"],"sensitive":true,"mcp":{"stdio":{"command":"npx"},"secretRefs":["MY_TOKEN"]}}]}`
	if err := os.WriteFile(filepath.Join(artifactsDir, "manifest.json"), []byte(manifest), 0o600); err != nil {
		t.Fatal(err)
	}

	env := []string{
		"HOME=" + home,
		"PATH=" + binDir + ":/usr/bin:/bin",
		"SPAWNERY_ARTIFACTS_DIR=" + artifactsDir,
		"SPAWNERY_SECRETS_DIR=" + secretsDir,
		"SECRET_WAIT_TIMEOUT=5s",
	}

	out, code := runHelper(t, helper, "claude-tui", env)
	if code != 0 {
		t.Fatalf("expected exit 0, got %d\noutput:\n%s", code, out)
	}

	// ~/.claude.json must carry the secret value in the env map.
	claudeJSON := filepath.Join(home, ".claude.json")
	data, err := os.ReadFile(claudeJSON)
	if err != nil {
		t.Fatalf("~/.claude.json not written: %v", err)
	}
	var root map[string]interface{}
	if err := json.Unmarshal(data, &root); err != nil {
		t.Fatalf("parse .claude.json: %v\ncontent: %s", err, data)
	}
	servers, _ := root["mcpServers"].(map[string]interface{})
	server, _ := servers["secret-mcp"].(map[string]interface{})
	envMap, _ := server["env"].(map[string]interface{})
	if envMap["MY_TOKEN"] != "s3cr3t-value" {
		t.Errorf("MY_TOKEN not injected: got %v\n.claude.json: %s", envMap["MY_TOKEN"], data)
	}

	// File should be 0600 (secrets present → filePerm returns 0600).
	fi, err := os.Stat(claudeJSON)
	if err != nil {
		t.Fatalf("stat .claude.json: %v", err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("perm: got %o, want 0600", fi.Mode().Perm())
	}
}
