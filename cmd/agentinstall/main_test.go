package main_test

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"spawnery/internal/agentinstall"
)

// buildAgentinstall builds the agentinstall binary and returns its path.
// It is cached per test run via a temp directory.
func buildAgentinstall(t *testing.T) string {
	t.Helper()
	// Build the binary into a temp dir.
	tmpDir := t.TempDir()
	binPath := filepath.Join(tmpDir, "agentinstall")
	cmd := exec.Command("go", "build", "-o", binPath, "spawnery/cmd/agentinstall")
	cmd.Env = append(os.Environ(), "CGO_ENABLED=1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("build agentinstall: %v\n%s", err, out)
	}
	return binPath
}

func TestListAgentsSmoke(t *testing.T) {
	bin := buildAgentinstall(t)
	home := t.TempDir()

	cmd := exec.Command(bin, "list-agents")
	cmd.Env = append(os.Environ(), "HOME="+home)
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("list-agents: %v", err)
	}

	output := string(out)
	for _, agent := range []string{"claude", "codex", "opencode", "hermes", "goose"} {
		if !strings.Contains(output, agent) {
			t.Errorf("list-agents output missing %q\noutput:\n%s", agent, output)
		}
	}
	// None should be detected since no config roots exist
	if strings.Contains(output, "detected\n") && !strings.Contains(output, "not detected") {
		t.Errorf("expected 'not detected' for all agents with empty home\noutput:\n%s", output)
	}
}

func TestListAgentsDetected(t *testing.T) {
	bin := buildAgentinstall(t)
	home := t.TempDir()

	// Create claude config root
	if err := os.MkdirAll(filepath.Join(home, ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command(bin, "list-agents")
	cmd.Env = append(os.Environ(), "HOME="+home)
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("list-agents: %v", err)
	}

	output := string(out)
	if !strings.Contains(output, "claude") || !strings.Contains(output, "detected") {
		t.Errorf("expected 'claude detected' in output\n%s", output)
	}
}

func TestApplySmoke(t *testing.T) {
	bin := buildAgentinstall(t)
	home := t.TempDir()

	// Create a staging dir with a manifest and a real skill tree.
	stagingDir := t.TempDir()
	skillPayloadDir := filepath.Join(stagingDir, "payloads", "test-skill")
	if err := os.MkdirAll(skillPayloadDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(skillPayloadDir, "SKILL.md"), []byte("# test-skill\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	manifest := `{
		"artifacts": [
			{
				"kind": "skill",
				"name": "test-skill",
				"targets": ["claude"],
				"skill": {"dir": "payloads/test-skill"}
			},
			{
				"kind": "mcp",
				"name": "test-mcp",
				"targets": ["codex"],
				"mcp": {
					"http": {"url": "http://localhost:8080"}
				}
			}
		]
	}`
	if err := os.WriteFile(filepath.Join(stagingDir, "manifest.json"), []byte(manifest), 0o600); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command(bin, "apply", "--artifacts", stagingDir)
	cmd.Env = append(os.Environ(), "HOME="+home)
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("apply: %v\noutput: %s", err, out)
	}

	// Parse the JSON result
	var result struct {
		Reports []struct {
			Agent  string `json:"agent"`
			Kind   string `json:"kind"`
			Name   string `json:"name"`
			Status string `json:"status"`
		} `json:"reports"`
	}
	if err := json.Unmarshal(out, &result); err != nil {
		t.Fatalf("parse apply output: %v\noutput: %s", err, out)
	}

	if len(result.Reports) != 2 {
		t.Fatalf("expected 2 reports, got %d: %+v", len(result.Reports), result.Reports)
	}

	// First report: claude/skill → applied (InstallSkill implemented in sp-w5aa)
	r0 := result.Reports[0]
	if r0.Agent != "claude" || r0.Kind != "skill" || r0.Name != "test-skill" {
		t.Errorf("report[0]: got %+v, want claude/skill/test-skill", r0)
	}
	if r0.Status != "applied" {
		t.Errorf("report[0].Status: got %q, want %q", r0.Status, "applied")
	}

	// Second report: codex/mcp → applied (InstallMCP implemented in sp-cywj)
	r1 := result.Reports[1]
	if r1.Agent != "codex" || r1.Kind != "mcp" || r1.Name != "test-mcp" {
		t.Errorf("report[1]: got %+v, want codex/mcp/test-mcp", r1)
	}
	if r1.Status != "applied" {
		t.Errorf("report[1].Status: got %q, want %q", r1.Status, "applied")
	}
}

// TestInstallSkillByAgent exercises the `install --agent <name> skill` path,
// which was broken by parentCmd := cmd.Root() (the root has no --agent flag).
func TestInstallSkillByAgent(t *testing.T) {
	bin := buildAgentinstall(t)
	home := t.TempDir()
	skillDir := t.TempDir()

	cmd := exec.Command(bin,
		"install", "--agent", "claude",
		"skill", "--name", "my-skill", "--dir", skillDir,
	)
	cmd.Env = append(os.Environ(), "HOME="+home)
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("install --agent claude skill: %v\noutput: %s", err, out)
	}

	var result struct {
		Reports []struct {
			Agent  string `json:"agent"`
			Kind   string `json:"kind"`
			Name   string `json:"name"`
			Status string `json:"status"`
		} `json:"reports"`
	}
	if err := json.Unmarshal(out, &result); err != nil {
		t.Fatalf("parse install output: %v\noutput: %s", err, out)
	}
	if len(result.Reports) == 0 {
		t.Fatalf("expected at least 1 report, got 0\noutput: %s", out)
	}
	r := result.Reports[0]
	if r.Agent != "claude" {
		t.Errorf("report.Agent: got %q, want %q", r.Agent, "claude")
	}
	if r.Kind != "skill" {
		t.Errorf("report.Kind: got %q, want %q", r.Kind, "skill")
	}
	if r.Name != "my-skill" {
		t.Errorf("report.Name: got %q, want %q", r.Name, "my-skill")
	}
}

// TestInstallSkillAllDetected exercises the `install --all-detected skill` path.
func TestInstallSkillAllDetected(t *testing.T) {
	bin := buildAgentinstall(t)
	home := t.TempDir()
	skillDir := t.TempDir()

	// Create claude config root so at least one agent is detected.
	if err := os.MkdirAll(filepath.Join(home, ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command(bin,
		"install", "--all-detected",
		"skill", "--name", "auto-skill", "--dir", skillDir,
	)
	cmd.Env = append(os.Environ(), "HOME="+home)
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("install --all-detected skill: %v\noutput: %s", err, out)
	}

	var result struct {
		Reports []struct {
			Agent string `json:"agent"`
			Kind  string `json:"kind"`
			Name  string `json:"name"`
		} `json:"reports"`
	}
	if err := json.Unmarshal(out, &result); err != nil {
		t.Fatalf("parse install output: %v\noutput: %s", err, out)
	}
	if len(result.Reports) == 0 {
		t.Fatalf("expected at least 1 report (claude detected), got 0\noutput: %s", out)
	}
	// Verify at least one report is for claude.
	found := false
	for _, r := range result.Reports {
		if r.Agent == "claude" && r.Kind == "skill" && r.Name == "auto-skill" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected a claude/skill/auto-skill report, got %+v", result.Reports)
	}
}

func TestListAgentsCapabilitiesJSON(t *testing.T) {
	bin := buildAgentinstall(t)
	home := t.TempDir()

	cmd := exec.Command(bin, "list-agents", "--capabilities")
	cmd.Env = append(os.Environ(), "HOME="+home)
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("list-agents --capabilities: %v", err)
	}

	var entries []agentinstall.CapabilityEntry
	if err := json.Unmarshal(out, &entries); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, out)
	}
	found := false
	for _, e := range entries {
		if e.Kind == "plugin" && e.Agent == "opencode" && e.Status == "best-effort" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected opencode/plugin=best-effort in %s", out)
	}
}

func TestApplyMissingManifest(t *testing.T) {
	bin := buildAgentinstall(t)
	home := t.TempDir()
	stagingDir := t.TempDir() // no manifest.json inside

	cmd := exec.Command(bin, "apply", "--artifacts", stagingDir)
	cmd.Env = append(os.Environ(), "HOME="+home)
	err := cmd.Run()
	if err == nil {
		t.Fatal("expected non-zero exit for missing manifest")
	}
}

// TestInstallConfigSetFlag exercises the `install --agent codex config --set approvalPosture=yolo` CLI path.
// yolo is the canonical "bypass all approvals" value in the new 4-value grammar
// (always-ask | ask-risky | auto | yolo); it maps to codex approval_policy=never.
func TestInstallConfigSetFlag(t *testing.T) {
	bin := buildAgentinstall(t)
	home := t.TempDir()
	codexHome := filepath.Join(home, ".codex")
	if err := os.MkdirAll(codexHome, 0o755); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command(bin,
		"install", "--agent", "codex",
		"config", "--name", "myconfig", "--set", "approvalPosture=yolo",
	)
	cmd.Env = append(os.Environ(), "HOME="+home, "CODEX_HOME="+codexHome)
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("install --agent codex config --set approvalPosture=yolo: %v\noutput: %s", err, out)
	}

	var result struct {
		Reports []struct {
			Agent  string `json:"agent"`
			Kind   string `json:"kind"`
			Name   string `json:"name"`
			Status string `json:"status"`
			Reason string `json:"reason"`
		} `json:"reports"`
	}
	if err := json.Unmarshal(out, &result); err != nil {
		t.Fatalf("parse output: %v\noutput: %s", err, out)
	}
	if len(result.Reports) != 1 {
		t.Fatalf("expected 1 report, got %d: %+v", len(result.Reports), result.Reports)
	}
	r := result.Reports[0]
	if r.Agent != "codex" {
		t.Errorf("Agent: got %q, want codex", r.Agent)
	}
	if r.Kind != "config" {
		t.Errorf("Kind: got %q, want config", r.Kind)
	}
	if r.Name != "myconfig" {
		t.Errorf("Name: got %q, want myconfig", r.Name)
	}
	if r.Status != "applied" {
		t.Errorf("Status: got %q (reason: %q), want applied", r.Status, r.Reason)
	}

	// Verify config.toml contains approval_policy = "never"
	configPath := filepath.Join(codexHome, "config.toml")
	data, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("read config.toml: %v", err)
	}
	if !strings.Contains(string(data), "approval_policy") {
		t.Errorf("config.toml does not contain approval_policy\ncontent:\n%s", data)
	}
	if !strings.Contains(string(data), "never") {
		t.Errorf("config.toml does not contain 'never'\ncontent:\n%s", data)
	}
}

// TestApplyAgentFilter exercises `apply --agent claude` with a manifest that targets both
// claude and codex: only the claude report should appear and ~/.claude.json should be written.
func TestApplyAgentFilter(t *testing.T) {
	bin := buildAgentinstall(t)
	home := t.TempDir()
	codexHome := filepath.Join(home, ".codex")
	if err := os.MkdirAll(codexHome, 0o755); err != nil {
		t.Fatal(err)
	}

	// Build a staging dir with a manifest targeting both claude and codex.
	stagingDir := t.TempDir()
	manifest := `{
		"artifacts": [
			{
				"kind": "mcp",
				"name": "filter-test-mcp",
				"targets": ["claude", "codex"],
				"mcp": {
					"http": {"url": "http://localhost:9090"}
				}
			}
		]
	}`
	if err := os.WriteFile(filepath.Join(stagingDir, "manifest.json"), []byte(manifest), 0o600); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command(bin, "apply", "--artifacts", stagingDir, "--agent", "claude")
	cmd.Env = append(os.Environ(), "HOME="+home, "CODEX_HOME="+codexHome)
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("apply --agent claude: %v\noutput: %s", err, out)
	}

	var result struct {
		Reports []struct {
			Agent  string `json:"agent"`
			Kind   string `json:"kind"`
			Name   string `json:"name"`
			Status string `json:"status"`
		} `json:"reports"`
	}
	if err := json.Unmarshal(out, &result); err != nil {
		t.Fatalf("parse output: %v\noutput: %s", err, out)
	}

	// Only claude should appear (codex silently skipped).
	if len(result.Reports) != 1 {
		t.Fatalf("expected 1 report, got %d: %+v", len(result.Reports), result.Reports)
	}
	r := result.Reports[0]
	if r.Agent != "claude" {
		t.Errorf("agent: got %q, want claude", r.Agent)
	}
	if r.Kind != "mcp" {
		t.Errorf("kind: got %q, want mcp", r.Kind)
	}
	if r.Name != "filter-test-mcp" {
		t.Errorf("name: got %q, want filter-test-mcp", r.Name)
	}
	if r.Status != "applied" {
		t.Errorf("status: got %q, want applied", r.Status)
	}

	// ~/.claude.json should have been written.
	claudeJSON := filepath.Join(home, ".claude.json")
	if _, err := os.Stat(claudeJSON); err != nil {
		t.Errorf("~/.claude.json not written: %v", err)
	}

	// ~/.codex/config.toml should NOT have been written (filter excluded it).
	codexConfig := filepath.Join(codexHome, "config.toml")
	if _, err := os.Stat(codexConfig); !os.IsNotExist(err) {
		t.Errorf("codex config should not have been written when filter=claude: err=%v", err)
	}
}
