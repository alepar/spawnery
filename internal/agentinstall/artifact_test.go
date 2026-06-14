package agentinstall_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"spawnery/internal/agentinstall"
)

func TestManifestRoundTrip(t *testing.T) {
	m := agentinstall.Manifest{
		Artifacts: []agentinstall.Artifact{
			{
				Kind:    agentinstall.KindSkill,
				Name:    "my-skill",
				Targets: []string{"claude", "codex"},
				Skill:   &agentinstall.SkillPayload{Dir: "payloads/my-skill"},
			},
			{
				Kind:    agentinstall.KindMCP,
				Name:    "my-mcp",
				Targets: []string{"all-detected"},
				MCP: &agentinstall.MCPPayload{
					Stdio: &agentinstall.MCPTransportStdio{
						Command: "node",
						Args:    []string{"server.js"},
						Env:     map[string]string{"KEY": "val"},
					},
					SecretRefs: []string{"MY_SECRET"},
				},
				Sensitive: true,
			},
			{
				Kind:    agentinstall.KindConfig,
				Name:    "my-cfg",
				Targets: []string{"claude"},
				Config: &agentinstall.ConfigPayload{
					Normalized: map[string]interface{}{"approvalPosture": "strict"},
					Native:     map[string]interface{}{"claude": map[string]interface{}{"foo": "bar"}},
				},
			},
		},
	}

	data, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var m2 agentinstall.Manifest
	if err := json.Unmarshal(data, &m2); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if len(m2.Artifacts) != 3 {
		t.Fatalf("expected 3 artifacts, got %d", len(m2.Artifacts))
	}
	if m2.Artifacts[0].Name != "my-skill" {
		t.Errorf("artifact[0].Name: got %q, want %q", m2.Artifacts[0].Name, "my-skill")
	}
	if m2.Artifacts[1].MCP == nil || m2.Artifacts[1].MCP.Stdio == nil {
		t.Error("artifact[1].MCP.Stdio is nil")
	}
	if !m2.Artifacts[1].Sensitive {
		t.Error("artifact[1].Sensitive expected true")
	}
	if m2.Artifacts[2].Config == nil {
		t.Error("artifact[2].Config is nil")
	}
}

func TestLoadManifest(t *testing.T) {
	dir := t.TempDir()
	content := `{"artifacts":[{"kind":"skill","name":"test","targets":["claude"],"skill":{"dir":"payloads/test"}}]}`
	if err := os.WriteFile(filepath.Join(dir, "manifest.json"), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}

	m, err := agentinstall.LoadManifest(dir)
	if err != nil {
		t.Fatalf("LoadManifest: %v", err)
	}
	if len(m.Artifacts) != 1 {
		t.Fatalf("expected 1 artifact, got %d", len(m.Artifacts))
	}
	if m.Artifacts[0].Kind != agentinstall.KindSkill {
		t.Errorf("kind: got %q, want %q", m.Artifacts[0].Kind, agentinstall.KindSkill)
	}
}

func TestLoadManifestMissing(t *testing.T) {
	_, err := agentinstall.LoadManifest(t.TempDir())
	if err == nil {
		t.Error("expected error for missing manifest.json")
	}
}

func TestReportResultJSON(t *testing.T) {
	r := agentinstall.Result{
		Reports: []agentinstall.Report{
			{
				Agent:  "claude",
				Kind:   agentinstall.KindSkill,
				Name:   "my-skill",
				Status: agentinstall.StatusApplied,
			},
			{
				Agent:  "codex",
				Kind:   agentinstall.KindMCP,
				Name:   "my-mcp",
				Status: agentinstall.StatusSkipped,
				Reason: "not implemented",
			},
			{
				Agent:             "opencode",
				Kind:              agentinstall.KindMCP,
				Name:              "my-mcp",
				Status:            agentinstall.StatusSkipped,
				RuntimeDepMissing: "node",
			},
		},
	}
	data, err := json.Marshal(r)
	if err != nil {
		t.Fatalf("marshal Result: %v", err)
	}
	var r2 agentinstall.Result
	if err := json.Unmarshal(data, &r2); err != nil {
		t.Fatalf("unmarshal Result: %v", err)
	}
	if len(r2.Reports) != 3 {
		t.Fatalf("expected 3 reports, got %d", len(r2.Reports))
	}
	if r2.Reports[2].RuntimeDepMissing != "node" {
		t.Errorf("RuntimeDepMissing: got %q, want %q", r2.Reports[2].RuntimeDepMissing, "node")
	}
}
