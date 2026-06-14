package spec_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"spawnery/internal/agentinstall/spec"
)

func TestManifestRoundTripWithSchemaVersion(t *testing.T) {
	m := spec.Manifest{
		SchemaVersion: spec.CurrentSchemaVersion,
		Artifacts: []spec.Artifact{
			{
				Kind:    spec.KindSkill,
				Name:    "my-skill",
				Targets: []string{"claude", "codex"},
				Skill:   &spec.SkillPayload{Dir: "payloads/my-skill"},
			},
			{
				Kind:    spec.KindMCP,
				Name:    "my-mcp",
				Targets: []string{"all-detected"},
				MCP: &spec.MCPPayload{
					Stdio:      &spec.MCPTransportStdio{Command: "node", Args: []string{"server.js"}, Env: map[string]string{"KEY": "val"}},
					SecretRefs: []string{"MY_SECRET"},
				},
				Sensitive: true,
			},
			{
				Kind:    spec.KindConfig,
				Name:    "my-cfg",
				Targets: []string{"claude"},
				Config:  &spec.ConfigPayload{Normalized: map[string]interface{}{"approvalPosture": "strict"}},
			},
		},
	}
	data, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(data), `"schema_version":1`) {
		t.Errorf("expected schema_version in JSON, got %s", data)
	}
	var m2 spec.Manifest
	if err := json.Unmarshal(data, &m2); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if m2.SchemaVersion != spec.CurrentSchemaVersion {
		t.Errorf("SchemaVersion: got %d want %d", m2.SchemaVersion, spec.CurrentSchemaVersion)
	}
	if len(m2.Artifacts) != 3 || m2.Artifacts[1].MCP == nil || m2.Artifacts[1].MCP.Stdio == nil {
		t.Fatalf("artifacts not round-tripped: %+v", m2.Artifacts)
	}
}

func TestLoadManifestAcceptsCurrentVersion(t *testing.T) {
	dir := t.TempDir()
	content := `{"schema_version":1,"artifacts":[{"kind":"skill","name":"t","targets":["claude"],"skill":{"dir":"payloads/t"}}]}`
	writeManifest(t, dir, content)
	m, err := spec.LoadManifest(dir)
	if err != nil {
		t.Fatalf("LoadManifest: %v", err)
	}
	if len(m.Artifacts) != 1 || m.Artifacts[0].Kind != spec.KindSkill {
		t.Fatalf("unexpected manifest: %+v", m)
	}
}

func TestLoadManifestAcceptsMissingVersion(t *testing.T) {
	dir := t.TempDir()
	content := `{"artifacts":[{"kind":"mcp","name":"x","targets":["all-detected"],"mcp":{"http":{"url":"https://e"}}}]}`
	writeManifest(t, dir, content)
	m, err := spec.LoadManifest(dir)
	if err != nil {
		t.Fatalf("LoadManifest (legacy, no schema_version): %v", err)
	}
	if m.SchemaVersion != 0 {
		t.Errorf("expected SchemaVersion 0 for legacy manifest, got %d", m.SchemaVersion)
	}
}

func TestLoadManifestRejectsFutureMajor(t *testing.T) {
	dir := t.TempDir()
	content := `{"schema_version":999,"artifacts":[]}`
	writeManifest(t, dir, content)
	_, err := spec.LoadManifest(dir)
	if err == nil {
		t.Fatal("expected hard error for schema_version above CurrentSchemaVersion")
	}
	if !strings.Contains(err.Error(), "schema_version") {
		t.Errorf("error should mention schema_version, got: %v", err)
	}
}

func TestLoadManifestMissingFile(t *testing.T) {
	if _, err := spec.LoadManifest(t.TempDir()); err == nil {
		t.Error("expected error for missing manifest.json")
	}
}

func writeManifest(t *testing.T, dir, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, "manifest.json"), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}
