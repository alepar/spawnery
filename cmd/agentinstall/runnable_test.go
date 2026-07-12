package main_test

// Tests for `agentinstall apply --runnable <x>` — the runnable->emitter resolution added by
// sp-mwco.2.6. buildAgentinstall/runApp helpers live in main_test.go.

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func writeSkillManifest(t *testing.T, stagingDir, skillName string, targets []string) {
	t.Helper()
	skillPayloadDir := filepath.Join(stagingDir, "payloads", skillName)
	if err := os.MkdirAll(skillPayloadDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(skillPayloadDir, "SKILL.md"), []byte("# "+skillName+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	targetsJSON, err := json.Marshal(targets)
	if err != nil {
		t.Fatal(err)
	}
	manifest := `{"artifacts":[{"kind":"skill","name":"` + skillName + `","targets":` + string(targetsJSON) + `,"skill":{"dir":"payloads/` + skillName + `"}}]}`
	if err := os.WriteFile(filepath.Join(stagingDir, "manifest.json"), []byte(manifest), 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestApplyRunnableClaudeTUI verifies `apply --runnable claude-tui` resolves to the claude
// emitter: the skill lands in both the canonical ~/.agents/skills dir and claude's native
// ~/.claude/skills copy, a report is written, and the process exits 0.
func TestApplyRunnableClaudeTUI(t *testing.T) {
	bin := buildAgentinstall(t)
	home := t.TempDir()
	stagingDir := t.TempDir()
	writeSkillManifest(t, stagingDir, "test-skill", []string{"claude"})
	reportPath := filepath.Join(stagingDir, "report", "apply-report.json")

	cmd := exec.Command(bin, "apply", "--artifacts", stagingDir, "--runnable", "claude-tui", "--report", reportPath)
	cmd.Env = append(os.Environ(), "HOME="+home)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("apply --runnable claude-tui: %v\noutput: %s", err, out)
	}

	canonical := filepath.Join(home, ".agents", "skills", "test-skill", "SKILL.md")
	if _, err := os.Stat(canonical); err != nil {
		t.Errorf("canonical skill not installed at %s: %v", canonical, err)
	}
	claudeCopy := filepath.Join(home, ".claude", "skills", "test-skill", "SKILL.md")
	if _, err := os.Stat(claudeCopy); err != nil {
		t.Errorf("claude skill copy not installed at %s: %v", claudeCopy, err)
	}
	if _, err := os.Stat(reportPath); err != nil {
		t.Errorf("report not written: %v", err)
	}
}

// TestApplyRunnableGooseTUI verifies `apply --runnable goose-tui` resolves to the goose
// emitter and actually installs — the regression this bead fixes (today, via the deleted
// shell case, goose-tui is a no-op).
func TestApplyRunnableGooseTUI(t *testing.T) {
	bin := buildAgentinstall(t)
	home := t.TempDir()
	stagingDir := t.TempDir()
	writeSkillManifest(t, stagingDir, "goose-skill", []string{"goose"})
	reportPath := filepath.Join(stagingDir, "report", "apply-report.json")

	cmd := exec.Command(bin, "apply", "--artifacts", stagingDir, "--runnable", "goose-tui", "--report", reportPath)
	cmd.Env = append(os.Environ(), "HOME="+home)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("apply --runnable goose-tui: %v\noutput: %s", err, out)
	}

	canonical := filepath.Join(home, ".agents", "skills", "goose-skill", "SKILL.md")
	if _, err := os.Stat(canonical); err != nil {
		t.Errorf("canonical skill not installed at %s: %v", canonical, err)
	}
	data, err := os.ReadFile(reportPath)
	if err != nil {
		t.Fatalf("report not written: %v", err)
	}
	if !strings.Contains(string(data), `"outcome":"ok"`) {
		t.Errorf("expected outcome ok, got: %s", data)
	}
}

// TestApplyRunnableShellNoOp verifies `apply --runnable shell` (a runnable with no
// agentcaps/emitter mapping) exits 0, writes no report, and installs nothing.
func TestApplyRunnableShellNoOp(t *testing.T) {
	bin := buildAgentinstall(t)
	home := t.TempDir()
	stagingDir := t.TempDir()
	writeSkillManifest(t, stagingDir, "test-skill", []string{"claude"})
	reportPath := filepath.Join(stagingDir, "report", "apply-report.json")

	cmd := exec.Command(bin, "apply", "--artifacts", stagingDir, "--runnable", "shell", "--report", reportPath)
	cmd.Env = append(os.Environ(), "HOME="+home)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("apply --runnable shell: expected exit 0, got %v\noutput: %s", err, out)
	}
	if _, err := os.Stat(reportPath); !os.IsNotExist(err) {
		t.Errorf("expected no report file for shell no-op, err=%v", err)
	}
	canonical := filepath.Join(home, ".agents", "skills", "test-skill")
	if _, err := os.Stat(canonical); !os.IsNotExist(err) {
		t.Errorf("expected nothing installed for shell no-op, but %s exists", canonical)
	}
}

// TestApplyRunnablePiTuiInstalls verifies `apply --runnable pi-tui` resolves to the pi
// emitter (registered in agentinstall.NewRegistry since sp-mwco.2.5) and actually installs:
// the skill lands in the canonical ~/.agents/skills dir (pi needs no per-agent glue), a
// report is written with no failed entries, and the process exits 0.
func TestApplyRunnablePiTuiInstalls(t *testing.T) {
	bin := buildAgentinstall(t)
	home := t.TempDir()
	stagingDir := t.TempDir()
	writeSkillManifest(t, stagingDir, "pi-skill", []string{"pi"})
	reportPath := filepath.Join(stagingDir, "report", "apply-report.json")

	cmd := exec.Command(bin, "apply", "--artifacts", stagingDir, "--runnable", "pi-tui", "--report", reportPath)
	cmd.Env = append(os.Environ(), "HOME="+home)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("apply --runnable pi-tui: %v\noutput: %s", err, out)
	}

	canonical := filepath.Join(home, ".agents", "skills", "pi-skill", "SKILL.md")
	if _, err := os.Stat(canonical); err != nil {
		t.Errorf("canonical skill not installed at %s: %v", canonical, err)
	}

	data, readErr := os.ReadFile(reportPath)
	if readErr != nil {
		t.Fatalf("report not written: %v", readErr)
	}
	var result struct {
		Reports []struct {
			Status string `json:"status"`
		} `json:"reports"`
	}
	if err := json.Unmarshal(data, &result); err != nil {
		t.Fatalf("parse report: %v\ncontent: %s", err, data)
	}
	for _, r := range result.Reports {
		if r.Status == "failed" {
			t.Errorf("unexpected failed report entry: %+v", result.Reports)
		}
	}
}

// TestApplyRunnableAndAgentMutuallyExclusive verifies `--runnable` and `--agent` together is a
// usage error (non-zero exit).
func TestApplyRunnableAndAgentMutuallyExclusive(t *testing.T) {
	bin := buildAgentinstall(t)
	home := t.TempDir()
	stagingDir := t.TempDir()
	writeSkillManifest(t, stagingDir, "test-skill", []string{"claude"})

	cmd := exec.Command(bin, "apply", "--artifacts", stagingDir, "--runnable", "claude-tui", "--agent", "claude")
	cmd.Env = append(os.Environ(), "HOME="+home)
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("expected non-zero exit when --runnable and --agent are both set, output: %s", out)
	}
	if !strings.Contains(string(out), "runnable") || !strings.Contains(string(out), "agent") {
		t.Errorf("expected usage error mentioning both flags, got: %s", out)
	}
}
