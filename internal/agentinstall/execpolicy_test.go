package agentinstall_test

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"spawnery/internal/agentinstall"
)

// TestRenderCodexRules verifies that renderCodexRules produces a version-pinned,
// deterministic rules file with allow and deny entries in input order.
func TestRenderCodexRules(t *testing.T) {
	cp := agentinstall.ExportCommandPolicy(
		[]string{"git status", "npm test"},
		[]string{"rm -rf *"},
	)

	out1 := agentinstall.ExportRenderCodexRules(cp)
	out2 := agentinstall.ExportRenderCodexRules(cp)

	// (a) Must start with a version-pinned header containing CodexExecPolicyVersion.
	if !strings.HasPrefix(string(out1), "# codex exec policy rules (format version ") {
		t.Errorf("expected version header, got: %q", string(out1[:min(len(out1), 80)]))
	}
	if !strings.Contains(string(out1), agentinstall.ExportCodexExecPolicyVersion) {
		t.Errorf("header does not contain version %q:\n%s", agentinstall.ExportCodexExecPolicyVersion, out1)
	}

	// (b) Must contain deterministic allow/deny entries.
	if !strings.Contains(string(out1), "allow git status\n") {
		t.Errorf("allow git status not found:\n%s", out1)
	}
	if !strings.Contains(string(out1), "allow npm test\n") {
		t.Errorf("allow npm test not found:\n%s", out1)
	}
	if !strings.Contains(string(out1), "deny rm -rf *\n") {
		t.Errorf("deny rm -rf * not found:\n%s", out1)
	}

	// (c) Byte-stable across two calls (idempotent render).
	if !bytes.Equal(out1, out2) {
		t.Errorf("renderCodexRules is not idempotent:\ncall1:\n%s\ncall2:\n%s", out1, out2)
	}

	// Allow entries appear before deny entries.
	allowIdx := strings.Index(string(out1), "allow git status")
	denyIdx := strings.Index(string(out1), "deny rm -rf *")
	if allowIdx < 0 || denyIdx < 0 || allowIdx >= denyIdx {
		t.Errorf("allow entries should appear before deny entries; allowIdx=%d denyIdx=%d\n%s", allowIdx, denyIdx, out1)
	}
}

// TestWriteCodexExecPolicy verifies that writeCodexExecPolicy creates the rules file
// at <rulesDir>/default.rules with mode 0644, mkdir-p's the directory, and that a
// second call overwrites atomically (no .tmp leftover).
func TestWriteCodexExecPolicy(t *testing.T) {
	rulesDir := filepath.Join(t.TempDir(), "nested", "rules")

	cp := agentinstall.ExportCommandPolicy(
		[]string{"git status", "npm test"},
		[]string{"rm -rf *"},
	)

	// First write — should create the dir and file.
	gotPath, err := agentinstall.ExportWriteCodexExecPolicy(rulesDir, cp)
	if err != nil {
		t.Fatalf("writeCodexExecPolicy: %v", err)
	}

	wantPath := filepath.Join(rulesDir, "default.rules")
	if gotPath != wantPath {
		t.Errorf("path: got %q, want %q", gotPath, wantPath)
	}

	data, err := os.ReadFile(wantPath)
	if err != nil {
		t.Fatalf("read default.rules: %v", err)
	}
	want := agentinstall.ExportRenderCodexRules(cp)
	if !bytes.Equal(data, want) {
		t.Errorf("content mismatch:\ngot:\n%s\nwant:\n%s", data, want)
	}

	// File mode must be 0644.
	info, err := os.Stat(wantPath)
	if err != nil {
		t.Fatalf("stat default.rules: %v", err)
	}
	if info.Mode().Perm() != 0o644 {
		t.Errorf("mode: got %o, want 644", info.Mode().Perm())
	}

	// Second call — must overwrite without leftover .tmp files.
	cp2 := agentinstall.ExportCommandPolicy([]string{"git log"}, nil)
	if _, err := agentinstall.ExportWriteCodexExecPolicy(rulesDir, cp2); err != nil {
		t.Fatalf("second writeCodexExecPolicy: %v", err)
	}

	data2, err := os.ReadFile(wantPath)
	if err != nil {
		t.Fatalf("read default.rules after second write: %v", err)
	}
	want2 := agentinstall.ExportRenderCodexRules(cp2)
	if !bytes.Equal(data2, want2) {
		t.Errorf("content after second write mismatch:\ngot:\n%s\nwant:\n%s", data2, want2)
	}

	// No .tmp files should remain.
	entries, _ := os.ReadDir(rulesDir)
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".tmp") {
			t.Errorf("leftover .tmp file: %s", e.Name())
		}
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
