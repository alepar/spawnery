package agentinstall_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pelletier/go-toml/v2"
	"github.com/tailscale/hujson"

	"spawnery/internal/agentinstall"
)

// applyMCP applies a single MCP artifact and returns the single Report.
func applyMCP(t *testing.T, home, secretsDir, agent string, a agentinstall.Artifact) agentinstall.Report {
	t.Helper()
	env := agentinstall.MapEnviron{"HOME": home}
	reg := agentinstall.NewRegistry(env)
	opts := agentinstall.Options{HomeDir: home, SecretsDir: secretsDir}
	res := agentinstall.Apply(reg, agentinstall.Manifest{Artifacts: []agentinstall.Artifact{a}}, opts, env)
	if len(res.Reports) != 1 {
		t.Fatalf("want 1 report, got %d", len(res.Reports))
	}
	return res.Reports[0]
}

// stdioArtifact builds a basic stdio MCP artifact.
func stdioArtifact(name string, targets []string) agentinstall.Artifact {
	return agentinstall.Artifact{
		Kind:    agentinstall.KindMCP,
		Name:    name,
		Targets: targets,
		MCP: &agentinstall.MCPPayload{
			Stdio: &agentinstall.MCPTransportStdio{
				Command: "npx",
				Args:    []string{"-y", "ctx7"},
			},
		},
	}
}

// TestInstallMCP_ClaudeStdioApplied tests that claude stdio MCP is applied and parses back correctly.
func TestInstallMCP_ClaudeStdioApplied(t *testing.T) {
	home := t.TempDir()
	r := applyMCP(t, home, "", "claude", stdioArtifact("ctx7", []string{"claude"}))
	if r.Status != agentinstall.StatusApplied {
		t.Fatalf("expected applied, got %q (reason: %q)", r.Status, r.Reason)
	}

	data, err := os.ReadFile(filepath.Join(home, ".claude.json"))
	if err != nil {
		t.Fatalf("read .claude.json: %v", err)
	}
	var root map[string]interface{}
	if err := json.Unmarshal(data, &root); err != nil {
		t.Fatalf("parse .claude.json: %v", err)
	}
	servers, ok := root["mcpServers"].(map[string]interface{})
	if !ok {
		t.Fatalf("mcpServers missing or wrong type: %T", root["mcpServers"])
	}
	server, ok := servers["ctx7"].(map[string]interface{})
	if !ok {
		t.Fatalf("ctx7 entry missing or wrong type")
	}
	if server["command"] != "npx" {
		t.Errorf("command: got %v want npx", server["command"])
	}
	args, ok := server["args"].([]interface{})
	if !ok {
		t.Fatalf("args: missing or wrong type: %T", server["args"])
	}
	if len(args) != 2 || args[0] != "-y" || args[1] != "ctx7" {
		t.Errorf("args: got %v want [-y ctx7]", args)
	}
}

// TestInstallMCP_ClaudeHTTPApplied tests that claude http MCP is applied correctly.
func TestInstallMCP_ClaudeHTTPApplied(t *testing.T) {
	home := t.TempDir()
	a := agentinstall.Artifact{
		Kind:    agentinstall.KindMCP,
		Name:    "ctx7",
		Targets: []string{"claude"},
		MCP: &agentinstall.MCPPayload{
			HTTP: &agentinstall.MCPTransportHTTP{
				URL:     "https://x/mcp",
				Headers: map[string]string{"X-A": "b"},
			},
		},
	}
	r := applyMCP(t, home, "", "claude", a)
	if r.Status != agentinstall.StatusApplied {
		t.Fatalf("expected applied, got %q (reason: %q)", r.Status, r.Reason)
	}

	data, err := os.ReadFile(filepath.Join(home, ".claude.json"))
	if err != nil {
		t.Fatalf("read .claude.json: %v", err)
	}
	var root map[string]interface{}
	if err := json.Unmarshal(data, &root); err != nil {
		t.Fatalf("parse .claude.json: %v", err)
	}
	servers := root["mcpServers"].(map[string]interface{})
	server := servers["ctx7"].(map[string]interface{})
	if server["type"] != "http" {
		t.Errorf("type: got %v want http", server["type"])
	}
	if server["url"] != "https://x/mcp" {
		t.Errorf("url: got %v want https://x/mcp", server["url"])
	}
	headers, ok := server["headers"].(map[string]interface{})
	if !ok {
		t.Fatalf("headers: missing or wrong type: %T", server["headers"])
	}
	if headers["X-A"] != "b" {
		t.Errorf("X-A header: got %v want b", headers["X-A"])
	}
}

// TestInstallMCP_ClaudeIdempotent tests that applying twice results in exactly one entry.
func TestInstallMCP_ClaudeIdempotent(t *testing.T) {
	home := t.TempDir()
	a := stdioArtifact("ctx7", []string{"claude"})

	r1 := applyMCP(t, home, "", "claude", a)
	if r1.Status != agentinstall.StatusApplied {
		t.Fatalf("first apply: expected applied, got %q (reason: %q)", r1.Status, r1.Reason)
	}

	r2 := applyMCP(t, home, "", "claude", a)
	if r2.Status != agentinstall.StatusApplied {
		t.Fatalf("second apply: expected applied, got %q (reason: %q)", r2.Status, r2.Reason)
	}

	data, _ := os.ReadFile(filepath.Join(home, ".claude.json"))
	var root map[string]interface{}
	_ = json.Unmarshal(data, &root)
	servers := root["mcpServers"].(map[string]interface{})
	if len(servers) != 1 {
		t.Errorf("expected exactly 1 server entry, got %d", len(servers))
	}
}

// TestInstallMCP_ClaudeMergeSurvival tests that existing top-level keys and other servers survive.
func TestInstallMCP_ClaudeMergeSurvival(t *testing.T) {
	home := t.TempDir()
	existing := map[string]interface{}{
		"numStartups": 3,
		"mcpServers": map[string]interface{}{
			"old": map[string]interface{}{"command": "x"},
		},
	}
	data, _ := json.MarshalIndent(existing, "", "  ")
	if err := os.WriteFile(filepath.Join(home, ".claude.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}

	r := applyMCP(t, home, "", "claude", stdioArtifact("ctx7", []string{"claude"}))
	if r.Status != agentinstall.StatusApplied {
		t.Fatalf("expected applied, got %q (reason: %q)", r.Status, r.Reason)
	}

	data2, _ := os.ReadFile(filepath.Join(home, ".claude.json"))
	var root map[string]interface{}
	_ = json.Unmarshal(data2, &root)

	if root["numStartups"] == nil {
		t.Error("numStartups was lost after apply")
	}
	servers := root["mcpServers"].(map[string]interface{})
	if servers["old"] == nil {
		t.Error("old server was lost after apply")
	}
	if servers["ctx7"] == nil {
		t.Error("new ctx7 server was not added")
	}
}

// TestInstallMCP_CodexStdioMergeAfterBaseGen tests codex stdio MCP with launcher base content survival.
func TestInstallMCP_CodexStdioMergeAfterBaseGen(t *testing.T) {
	home := t.TempDir()
	codexHome := filepath.Join(home, ".codex")
	if err := os.MkdirAll(codexHome, 0o755); err != nil {
		t.Fatal(err)
	}

	// Simulate launcher-generated base config
	baseToml := `model = "m"

[model_providers.spawnery]
name = "Spawnery"

[projects."/app"]
trust_level = "trusted"
`
	if err := os.WriteFile(filepath.Join(codexHome, "config.toml"), []byte(baseToml), 0o644); err != nil {
		t.Fatal(err)
	}

	env := agentinstall.MapEnviron{"HOME": home, "CODEX_HOME": codexHome}
	reg := agentinstall.NewRegistry(env)
	opts := agentinstall.Options{HomeDir: home}
	a := agentinstall.Artifact{
		Kind:    agentinstall.KindMCP,
		Name:    "ctx7",
		Targets: []string{"codex"},
		MCP: &agentinstall.MCPPayload{
			Stdio: &agentinstall.MCPTransportStdio{
				Command: "npx",
				Args:    []string{"-y", "ctx7"},
			},
		},
	}
	res := agentinstall.Apply(reg, agentinstall.Manifest{Artifacts: []agentinstall.Artifact{a}}, opts, env)
	if len(res.Reports) != 1 {
		t.Fatalf("want 1 report, got %d", len(res.Reports))
	}
	r := res.Reports[0]
	if r.Status != agentinstall.StatusApplied {
		t.Fatalf("expected applied, got %q (reason: %q)", r.Status, r.Reason)
	}

	data, err := os.ReadFile(filepath.Join(codexHome, "config.toml"))
	if err != nil {
		t.Fatalf("read config.toml: %v", err)
	}
	var doc map[string]interface{}
	if err := toml.Unmarshal(data, &doc); err != nil {
		t.Fatalf("parse config.toml: %v", err)
	}

	// Check new entry
	mcpServers, ok := doc["mcp_servers"].(map[string]interface{})
	if !ok {
		t.Fatalf("mcp_servers missing or wrong type: %T", doc["mcp_servers"])
	}
	server, ok := mcpServers["ctx7"].(map[string]interface{})
	if !ok {
		t.Fatalf("ctx7 entry missing or wrong type")
	}
	if server["command"] != "npx" {
		t.Errorf("command: got %v want npx", server["command"])
	}

	// Check survival of base fields
	if doc["model"] != "m" {
		t.Errorf("model was lost after apply: got %v", doc["model"])
	}
	modelProviders, ok := doc["model_providers"].(map[string]interface{})
	if !ok {
		t.Fatalf("model_providers was lost after apply")
	}
	if modelProviders["spawnery"] == nil {
		t.Error("model_providers.spawnery was lost after apply")
	}
	projects, ok := doc["projects"].(map[string]interface{})
	if !ok {
		t.Fatalf("projects was lost after apply")
	}
	if projects["/app"] == nil {
		t.Error("projects./app was lost after apply")
	}
}

// TestInstallMCP_CodexIdempotent tests that applying codex MCP twice results in one entry.
func TestInstallMCP_CodexIdempotent(t *testing.T) {
	home := t.TempDir()
	codexHome := filepath.Join(home, ".codex")
	if err := os.MkdirAll(codexHome, 0o755); err != nil {
		t.Fatal(err)
	}

	env := agentinstall.MapEnviron{"HOME": home, "CODEX_HOME": codexHome}
	reg := agentinstall.NewRegistry(env)
	opts := agentinstall.Options{HomeDir: home}
	a := agentinstall.Artifact{
		Kind:    agentinstall.KindMCP,
		Name:    "ctx7",
		Targets: []string{"codex"},
		MCP: &agentinstall.MCPPayload{
			Stdio: &agentinstall.MCPTransportStdio{Command: "npx"},
		},
	}

	for i := 1; i <= 2; i++ {
		res := agentinstall.Apply(reg, agentinstall.Manifest{Artifacts: []agentinstall.Artifact{a}}, opts, env)
		if len(res.Reports) != 1 || res.Reports[0].Status != agentinstall.StatusApplied {
			t.Fatalf("apply #%d: expected applied, got %+v", i, res.Reports)
		}
	}

	data, _ := os.ReadFile(filepath.Join(codexHome, "config.toml"))
	var doc map[string]interface{}
	_ = toml.Unmarshal(data, &doc)
	mcpServers := doc["mcp_servers"].(map[string]interface{})
	if len(mcpServers) != 1 {
		t.Errorf("expected exactly 1 mcp_server entry, got %d", len(mcpServers))
	}
}

// TestInstallMCP_OpencodeStdioShape tests the S2-validated opencode stdio shape.
func TestInstallMCP_OpencodeStdioShape(t *testing.T) {
	home := t.TempDir()
	a := agentinstall.Artifact{
		Kind:    agentinstall.KindMCP,
		Name:    "ctx7",
		Targets: []string{"opencode"},
		MCP: &agentinstall.MCPPayload{
			Stdio: &agentinstall.MCPTransportStdio{
				Command: "npx",
				Args:    []string{"-y", "ctx7"},
				Env:     map[string]string{"FOO": "bar"},
			},
		},
	}
	r := applyMCP(t, home, "", "opencode", a)
	if r.Status != agentinstall.StatusApplied {
		t.Fatalf("expected applied, got %q (reason: %q)", r.Status, r.Reason)
	}

	data, err := os.ReadFile(filepath.Join(home, ".config", "opencode", "opencode.json"))
	if err != nil {
		t.Fatalf("read opencode.json: %v", err)
	}
	// Standardize JSONC -> JSON
	std, err := hujson.Standardize(data)
	if err != nil {
		t.Fatalf("standardize JSONC: %v", err)
	}
	var root map[string]interface{}
	if err := json.Unmarshal(std, &root); err != nil {
		t.Fatalf("parse opencode.json: %v", err)
	}
	mcp, ok := root["mcp"].(map[string]interface{})
	if !ok {
		t.Fatalf("mcp missing or wrong type: %T", root["mcp"])
	}
	server, ok := mcp["ctx7"].(map[string]interface{})
	if !ok {
		t.Fatalf("ctx7 missing or wrong type")
	}
	if server["type"] != "local" {
		t.Errorf("type: got %v want local", server["type"])
	}
	if server["enabled"] != true {
		t.Errorf("enabled: got %v want true", server["enabled"])
	}
	cmd, ok := server["command"].([]interface{})
	if !ok {
		t.Fatalf("command: expected array, got %T", server["command"])
	}
	want := []string{"npx", "-y", "ctx7"}
	if len(cmd) != len(want) {
		t.Fatalf("command len: got %d want %d", len(cmd), len(want))
	}
	for i, v := range want {
		if cmd[i] != v {
			t.Errorf("command[%d]: got %v want %s", i, cmd[i], v)
		}
	}
	env, ok := server["environment"].(map[string]interface{})
	if !ok {
		t.Fatalf("environment: expected map, got %T", server["environment"])
	}
	if env["FOO"] != "bar" {
		t.Errorf("environment.FOO: got %v want bar", env["FOO"])
	}
}

// TestInstallMCP_OpencodeHTTPShape tests the S2-validated opencode http shape.
func TestInstallMCP_OpencodeHTTPShape(t *testing.T) {
	home := t.TempDir()
	a := agentinstall.Artifact{
		Kind:    agentinstall.KindMCP,
		Name:    "ctx7",
		Targets: []string{"opencode"},
		MCP: &agentinstall.MCPPayload{
			HTTP: &agentinstall.MCPTransportHTTP{
				URL:     "https://x/mcp",
				Headers: map[string]string{"X-A": "b"},
			},
		},
	}
	r := applyMCP(t, home, "", "opencode", a)
	if r.Status != agentinstall.StatusApplied {
		t.Fatalf("expected applied, got %q (reason: %q)", r.Status, r.Reason)
	}

	data, err := os.ReadFile(filepath.Join(home, ".config", "opencode", "opencode.json"))
	if err != nil {
		t.Fatalf("read opencode.json: %v", err)
	}
	std, _ := hujson.Standardize(data)
	var root map[string]interface{}
	_ = json.Unmarshal(std, &root)
	mcp := root["mcp"].(map[string]interface{})
	server := mcp["ctx7"].(map[string]interface{})
	if server["type"] != "remote" {
		t.Errorf("type: got %v want remote", server["type"])
	}
	if server["enabled"] != true {
		t.Errorf("enabled: got %v want true", server["enabled"])
	}
	if server["url"] != "https://x/mcp" {
		t.Errorf("url: got %v want https://x/mcp", server["url"])
	}
	headers, ok := server["headers"].(map[string]interface{})
	if !ok {
		t.Fatalf("headers: expected map, got %T", server["headers"])
	}
	if headers["X-A"] != "b" {
		t.Errorf("X-A header: got %v want b", headers["X-A"])
	}
}

// TestInstallMCP_OpencodeJSONCTolerance tests that JSONC comments and existing entries survive.
func TestInstallMCP_OpencodeJSONCTolerance(t *testing.T) {
	home := t.TempDir()
	configDir := filepath.Join(home, ".config", "opencode")
	if err := os.MkdirAll(configDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// Pre-write a JSONC file with a comment and an existing mcp entry
	existingJSONC := `{
  // This is a comment
  "mcp": {
    "old": {
      "type": "local",
      "enabled": true,
      "command": ["old-cmd"]
    }
  }
}`
	if err := os.WriteFile(filepath.Join(configDir, "opencode.json"), []byte(existingJSONC), 0o644); err != nil {
		t.Fatal(err)
	}

	r := applyMCP(t, home, "", "opencode", stdioArtifact("ctx7", []string{"opencode"}))
	if r.Status != agentinstall.StatusApplied {
		t.Fatalf("expected applied, got %q (reason: %q)", r.Status, r.Reason)
	}

	data, err := os.ReadFile(filepath.Join(configDir, "opencode.json"))
	if err != nil {
		t.Fatalf("read opencode.json: %v", err)
	}
	std, err := hujson.Standardize(data)
	if err != nil {
		t.Fatalf("standardize JSONC: %v", err)
	}
	var root map[string]interface{}
	if err := json.Unmarshal(std, &root); err != nil {
		t.Fatalf("parse opencode.json: %v", err)
	}
	mcp, ok := root["mcp"].(map[string]interface{})
	if !ok {
		t.Fatalf("mcp missing")
	}
	if mcp["old"] == nil {
		t.Error("old entry was lost after apply")
	}
	if mcp["ctx7"] == nil {
		t.Error("ctx7 entry was not added")
	}
}

// TestInstallMCP_SecretInjectionClaude tests that secret files are read and injected into env.
func TestInstallMCP_SecretInjectionClaude(t *testing.T) {
	home := t.TempDir()
	secretsDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(secretsDir, "CTX7_TOKEN"), []byte("s3cr3t"), 0o600); err != nil {
		t.Fatal(err)
	}

	a := agentinstall.Artifact{
		Kind:      agentinstall.KindMCP,
		Name:      "ctx7",
		Targets:   []string{"claude"},
		Sensitive: true,
		MCP: &agentinstall.MCPPayload{
			Stdio:      &agentinstall.MCPTransportStdio{Command: "npx"},
			SecretRefs: []string{"CTX7_TOKEN"},
		},
	}
	r := applyMCP(t, home, secretsDir, "claude", a)
	if r.Status != agentinstall.StatusApplied {
		t.Fatalf("expected applied, got %q (reason: %q)", r.Status, r.Reason)
	}

	data, err := os.ReadFile(filepath.Join(home, ".claude.json"))
	if err != nil {
		t.Fatalf("read .claude.json: %v", err)
	}
	var root map[string]interface{}
	_ = json.Unmarshal(data, &root)
	servers := root["mcpServers"].(map[string]interface{})
	server := servers["ctx7"].(map[string]interface{})
	env, ok := server["env"].(map[string]interface{})
	if !ok {
		t.Fatalf("env missing or wrong type: %T", server["env"])
	}
	if env["CTX7_TOKEN"] != "s3cr3t" {
		t.Errorf("CTX7_TOKEN: got %v want s3cr3t", env["CTX7_TOKEN"])
	}

	// File should be 0600 when secrets present
	info, err := os.Stat(filepath.Join(home, ".claude.json"))
	if err != nil {
		t.Fatalf("stat .claude.json: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("perm: got %o want 0600", info.Mode().Perm())
	}
}

// TestInstallMCP_MissingSecretFailed tests that a missing secret file results in StatusFailed.
func TestInstallMCP_MissingSecretFailed(t *testing.T) {
	home := t.TempDir()
	secretsDir := t.TempDir()
	// Don't write the secret file

	a := agentinstall.Artifact{
		Kind:      agentinstall.KindMCP,
		Name:      "ctx7",
		Targets:   []string{"claude"},
		Sensitive: true,
		MCP: &agentinstall.MCPPayload{
			Stdio:      &agentinstall.MCPTransportStdio{Command: "npx"},
			SecretRefs: []string{"MISSING_TOKEN"},
		},
	}
	r := applyMCP(t, home, secretsDir, "claude", a)
	if r.Status != agentinstall.StatusFailed {
		t.Fatalf("expected failed, got %q (reason: %q)", r.Status, r.Reason)
	}
	if !strings.Contains(r.Reason, "MISSING_TOKEN") {
		t.Errorf("reason should mention the secret ref, got: %q", r.Reason)
	}
	// Verify no partial write
	if _, err := os.Stat(filepath.Join(home, ".claude.json")); !os.IsNotExist(err) {
		t.Error("partial write should not occur on secret failure")
	}
}

// TestResolveStdioEnv_TrimsTrailingNewline verifies that a trailing newline in a secret
// file is stripped from the injected env value (typical of echo/printf output).
func TestResolveStdioEnv_TrimsTrailingNewline(t *testing.T) {
	home := t.TempDir()
	secretsDir := t.TempDir()
	// Write secret with trailing newline (common from shell `echo`).
	if err := os.WriteFile(filepath.Join(secretsDir, "TOK"), []byte("tok123\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	a := agentinstall.Artifact{
		Kind:    agentinstall.KindMCP,
		Name:    "ctx7",
		Targets: []string{"claude"},
		MCP: &agentinstall.MCPPayload{
			Stdio:      &agentinstall.MCPTransportStdio{Command: "npx"},
			SecretRefs: []string{"TOK"},
		},
	}
	r := applyMCP(t, home, secretsDir, "claude", a)
	if r.Status != agentinstall.StatusApplied {
		t.Fatalf("expected applied, got %q (reason: %q)", r.Status, r.Reason)
	}

	data, _ := os.ReadFile(filepath.Join(home, ".claude.json"))
	var root map[string]interface{}
	_ = json.Unmarshal(data, &root)
	servers := root["mcpServers"].(map[string]interface{})
	server := servers["ctx7"].(map[string]interface{})
	env, ok := server["env"].(map[string]interface{})
	if !ok {
		t.Fatalf("env missing or wrong type: %T", server["env"])
	}
	if env["TOK"] != "tok123" {
		t.Errorf("TOK: got %q, want %q (trailing newline should be trimmed)", env["TOK"], "tok123")
	}
}

// TestInstallMCP_ClaudeForces0600OnExistingFile verifies that applying a non-secret stdio
// MCP to claude always sets .claude.json to 0600, even if the file existed with 0644.
func TestInstallMCP_ClaudeForces0600OnExistingFile(t *testing.T) {
	home := t.TempDir()
	// Pre-create ~/.claude.json with 0644.
	if err := os.WriteFile(filepath.Join(home, ".claude.json"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Apply a non-secret stdio MCP (no SecretRefs, no Sensitive flag).
	r := applyMCP(t, home, "", "claude", stdioArtifact("ctx7", []string{"claude"}))
	if r.Status != agentinstall.StatusApplied {
		t.Fatalf("expected applied, got %q (reason: %q)", r.Status, r.Reason)
	}

	info, err := os.Stat(filepath.Join(home, ".claude.json"))
	if err != nil {
		t.Fatalf("stat .claude.json: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf(".claude.json perm: got %o, want 0600 (claude always 0600)", info.Mode().Perm())
	}
}

// TestInstallMCP_CodexHTTPSensitiveIs0600 verifies that a codex HTTP MCP with Sensitive=true
// results in config.toml written at 0600.
func TestInstallMCP_CodexHTTPSensitiveIs0600(t *testing.T) {
	home := t.TempDir()
	codexHome := filepath.Join(home, ".codex")
	if err := os.MkdirAll(codexHome, 0o755); err != nil {
		t.Fatal(err)
	}

	env := agentinstall.MapEnviron{"HOME": home, "CODEX_HOME": codexHome}
	reg := agentinstall.NewRegistry(env)
	opts := agentinstall.Options{HomeDir: home}
	a := agentinstall.Artifact{
		Kind:      agentinstall.KindMCP,
		Name:      "ctx7",
		Targets:   []string{"codex"},
		Sensitive: true,
		MCP: &agentinstall.MCPPayload{
			HTTP: &agentinstall.MCPTransportHTTP{
				URL:     "https://api.example.com/mcp",
				Headers: map[string]string{"Authorization": "Bearer secret"},
			},
		},
	}
	res := agentinstall.Apply(reg, agentinstall.Manifest{Artifacts: []agentinstall.Artifact{a}}, opts, env)
	if len(res.Reports) != 1 {
		t.Fatalf("want 1 report, got %d", len(res.Reports))
	}
	r := res.Reports[0]
	if r.Status != agentinstall.StatusApplied {
		t.Fatalf("expected applied, got %q (reason: %q)", r.Status, r.Reason)
	}

	info, err := os.Stat(filepath.Join(codexHome, "config.toml"))
	if err != nil {
		t.Fatalf("stat config.toml: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("config.toml perm: got %o, want 0600 (Sensitive HTTP MCP)", info.Mode().Perm())
	}
}

// TestInstallMCP_OpencodeHTTPHeadersIs0600 verifies that an opencode HTTP MCP with headers
// (but no Sensitive flag) results in opencode.json written at 0600.
func TestInstallMCP_OpencodeHTTPHeadersIs0600(t *testing.T) {
	home := t.TempDir()
	a := agentinstall.Artifact{
		Kind:    agentinstall.KindMCP,
		Name:    "ctx7",
		Targets: []string{"opencode"},
		MCP: &agentinstall.MCPPayload{
			HTTP: &agentinstall.MCPTransportHTTP{
				URL:     "https://api.example.com/mcp",
				Headers: map[string]string{"X-Auth-Token": "secret"},
			},
		},
	}
	r := applyMCP(t, home, "", "opencode", a)
	if r.Status != agentinstall.StatusApplied {
		t.Fatalf("expected applied, got %q (reason: %q)", r.Status, r.Reason)
	}

	info, err := os.Stat(filepath.Join(home, ".config", "opencode", "opencode.json"))
	if err != nil {
		t.Fatalf("stat opencode.json: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("opencode.json perm: got %o, want 0600 (HTTP MCP with headers)", info.Mode().Perm())
	}
}

// TestInstallMCP_OpencodeIdempotentApplyTwice verifies that applying the same opencode stdio
// MCP twice results in exactly one entry in opencode.json (no duplication).
func TestInstallMCP_OpencodeIdempotentApplyTwice(t *testing.T) {
	home := t.TempDir()
	a := agentinstall.Artifact{
		Kind:    agentinstall.KindMCP,
		Name:    "ctx7",
		Targets: []string{"opencode"},
		MCP: &agentinstall.MCPPayload{
			Stdio: &agentinstall.MCPTransportStdio{
				Command: "npx",
				Args:    []string{"-y", "ctx7"},
			},
		},
	}

	r1 := applyMCP(t, home, "", "opencode", a)
	if r1.Status != agentinstall.StatusApplied {
		t.Fatalf("first apply: expected applied, got %q (reason: %q)", r1.Status, r1.Reason)
	}

	r2 := applyMCP(t, home, "", "opencode", a)
	if r2.Status != agentinstall.StatusApplied {
		t.Fatalf("second apply: expected applied, got %q (reason: %q)", r2.Status, r2.Reason)
	}

	data, err := os.ReadFile(filepath.Join(home, ".config", "opencode", "opencode.json"))
	if err != nil {
		t.Fatalf("read opencode.json: %v", err)
	}
	std, err := hujson.Standardize(data)
	if err != nil {
		t.Fatalf("standardize JSONC: %v", err)
	}
	var root map[string]interface{}
	if err := json.Unmarshal(std, &root); err != nil {
		t.Fatalf("parse opencode.json: %v", err)
	}
	mcp, ok := root["mcp"].(map[string]interface{})
	if !ok {
		t.Fatalf("mcp missing or wrong type: %T", root["mcp"])
	}
	if len(mcp) != 1 {
		t.Errorf("expected exactly 1 mcp entry after two applies, got %d", len(mcp))
	}
	server, ok := mcp["ctx7"].(map[string]interface{})
	if !ok {
		t.Fatalf("ctx7 missing or wrong type: %T", mcp["ctx7"])
	}
	if server["type"] != "local" {
		t.Errorf("type: got %v, want local", server["type"])
	}
	if server["enabled"] != true {
		t.Errorf("enabled: got %v, want true", server["enabled"])
	}
	cmd, ok := server["command"].([]interface{})
	if !ok {
		t.Fatalf("command: expected array, got %T", server["command"])
	}
	want := []string{"npx", "-y", "ctx7"}
	if len(cmd) != len(want) {
		t.Fatalf("command len: got %d want %d", len(cmd), len(want))
	}
	for i, v := range want {
		if cmd[i] != v {
			t.Errorf("command[%d]: got %v want %s", i, cmd[i], v)
		}
	}
}

// TestInstallMCP_GooseSkipped tests that goose MCP is always skipped (deferred).
func TestInstallMCP_GooseSkipped(t *testing.T) {
	home := t.TempDir()
	r := applyMCP(t, home, "", "goose", stdioArtifact("ctx7", []string{"goose"}))
	if r.Status != agentinstall.StatusSkipped {
		t.Fatalf("expected skipped, got %q (reason: %q)", r.Status, r.Reason)
	}
	if !strings.Contains(r.Reason, "deferred") {
		t.Errorf("reason should mention deferred, got: %q", r.Reason)
	}
}

// TestInstallMCP_NilPayloadSkipped tests that a KindMCP artifact with nil MCP is skipped.
func TestInstallMCP_NilPayloadSkipped(t *testing.T) {
	home := t.TempDir()
	a := agentinstall.Artifact{
		Kind:    agentinstall.KindMCP,
		Name:    "ctx7",
		Targets: []string{"claude"},
		MCP:     nil,
	}
	r := applyMCP(t, home, "", "claude", a)
	if r.Status != agentinstall.StatusSkipped {
		t.Fatalf("expected skipped, got %q (reason: %q)", r.Status, r.Reason)
	}
}

// TestInstallMCP_PathConfinement tests that dangerous names are rejected.
func TestInstallMCP_PathConfinement(t *testing.T) {
	badNames := []string{
		"../evil",
		"sub/dir",
		"",
		".",
		"..",
	}
	for _, agent := range []string{"claude", "codex", "opencode"} {
		for _, name := range badNames {
			agent, name := agent, name
			t.Run(agent+"/"+name, func(t *testing.T) {
				home := t.TempDir()
				a := agentinstall.Artifact{
					Kind:    agentinstall.KindMCP,
					Name:    name,
					Targets: []string{agent},
					MCP:     &agentinstall.MCPPayload{Stdio: &agentinstall.MCPTransportStdio{Command: "npx"}},
				}
				r := applyMCP(t, home, "", agent, a)
				if r.Status != agentinstall.StatusSkipped && r.Status != agentinstall.StatusFailed {
					t.Errorf("name=%q agent=%q: expected skipped or failed, got %q (reason: %q)", name, agent, r.Status, r.Reason)
				}
				if r.Reason == "" {
					t.Errorf("name=%q agent=%q: expected non-empty reason", name, agent)
				}
				// Ensure no file was written at a path that would result from
				// treating the name as a path component (filesystem escape check).
				evilPath := filepath.Join(home, "evil")
				if _, err := os.Stat(evilPath); !os.IsNotExist(err) {
					t.Errorf("name=%q agent=%q: escape file exists at %s", name, agent, evilPath)
				}
			})
		}
	}
}
