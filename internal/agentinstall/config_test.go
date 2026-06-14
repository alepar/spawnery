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

// applyConfig applies a single Config artifact and returns the single Report.
func applyConfig(t *testing.T, home, agent string, a agentinstall.Artifact) agentinstall.Report {
	t.Helper()
	env := agentinstall.MapEnviron{"HOME": home}
	reg := agentinstall.NewRegistry(env)
	opts := agentinstall.Options{HomeDir: home}
	res := agentinstall.Apply(reg, agentinstall.Manifest{Artifacts: []agentinstall.Artifact{a}}, opts, env)
	if len(res.Reports) != 1 {
		t.Fatalf("want 1 report, got %d: %+v", len(res.Reports), res.Reports)
	}
	return res.Reports[0]
}

// configArtifact builds a basic config artifact with given normalized keys.
func configArtifact(name string, targets []string, norm map[string]interface{}) agentinstall.Artifact {
	return agentinstall.Artifact{
		Kind:    agentinstall.KindConfig,
		Name:    name,
		Targets: targets,
		Config: &agentinstall.ConfigPayload{
			Normalized: norm,
		},
	}
}

// configArtifactWithNative builds a config artifact with normalized keys and native passthrough.
func configArtifactWithNative(name string, targets []string, norm map[string]interface{}, native map[string]interface{}) agentinstall.Artifact {
	return agentinstall.Artifact{
		Kind:    agentinstall.KindConfig,
		Name:    name,
		Targets: targets,
		Config: &agentinstall.ConfigPayload{
			Normalized: norm,
			Native:     native,
		},
	}
}

// ---- Claude tests -----------------------------------------------------------

// TestApplyConfig_ClaudeNativePassthroughSurvival pre-seeds settings.json with existing content,
// applies a native passthrough, and asserts all pre-existing entries survive.
func TestApplyConfig_ClaudeNativePassthroughSurvival(t *testing.T) {
	home := t.TempDir()
	claudeDir := filepath.Join(home, ".claude")
	if err := os.MkdirAll(claudeDir, 0o755); err != nil {
		t.Fatal(err)
	}

	// Pre-seed with existing config
	existing := map[string]interface{}{
		"permissions": map[string]interface{}{
			"allow": []interface{}{"Bash(*)", "Read(*)"},
		},
		"otherKey": "existingValue",
	}
	seed, _ := json.MarshalIndent(existing, "", "  ")
	if err := os.WriteFile(filepath.Join(claudeDir, "settings.json"), seed, 0o644); err != nil {
		t.Fatal(err)
	}

	// Apply native passthrough that adds permissions.defaultMode and a new top-level key
	a := configArtifactWithNative("myconfig", []string{"claude"}, nil, map[string]interface{}{
		"claude": map[string]interface{}{
			"permissions": map[string]interface{}{
				"defaultMode": "bypassPermissions",
			},
			"newKey": "newValue",
		},
	})

	r := applyConfig(t, home, "claude", a)
	if r.Status != agentinstall.StatusApplied {
		t.Fatalf("expected applied, got %q (reason: %q)", r.Status, r.Reason)
	}

	data, err := os.ReadFile(filepath.Join(claudeDir, "settings.json"))
	if err != nil {
		t.Fatalf("read settings.json: %v", err)
	}
	var root map[string]interface{}
	if err := json.Unmarshal(data, &root); err != nil {
		t.Fatalf("parse settings.json: %v", err)
	}

	// Pre-existing otherKey survives
	if root["otherKey"] != "existingValue" {
		t.Errorf("otherKey was lost or changed: got %v", root["otherKey"])
	}

	// New key was added
	if root["newKey"] != "newValue" {
		t.Errorf("newKey not added: got %v", root["newKey"])
	}

	// permissions.allow survives (deep merge)
	perms, ok := root["permissions"].(map[string]interface{})
	if !ok {
		t.Fatalf("permissions missing or wrong type: %T", root["permissions"])
	}
	allow, ok := perms["allow"].([]interface{})
	if !ok {
		t.Fatalf("permissions.allow missing or wrong type: %T", perms["allow"])
	}
	if len(allow) != 2 {
		t.Errorf("permissions.allow: got %v, want 2 entries", allow)
	}

	// New permissions.defaultMode was added
	if perms["defaultMode"] != "bypassPermissions" {
		t.Errorf("permissions.defaultMode: got %v, want bypassPermissions", perms["defaultMode"])
	}
}

// TestApplyConfig_ClaudeApprovalPostureNever tests approvalPosture=never -> permissions.defaultMode=bypassPermissions.
func TestApplyConfig_ClaudeApprovalPostureNever(t *testing.T) {
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}

	a := configArtifact("cfg", []string{"claude"}, map[string]interface{}{
		"approvalPosture": "never",
	})

	r := applyConfig(t, home, "claude", a)
	if r.Status != agentinstall.StatusApplied {
		t.Fatalf("expected applied, got %q (reason: %q)", r.Status, r.Reason)
	}

	data, err := os.ReadFile(filepath.Join(home, ".claude", "settings.json"))
	if err != nil {
		t.Fatalf("read settings.json: %v", err)
	}
	var root map[string]interface{}
	if err := json.Unmarshal(data, &root); err != nil {
		t.Fatalf("parse settings.json: %v", err)
	}
	perms, ok := root["permissions"].(map[string]interface{})
	if !ok {
		t.Fatalf("permissions missing or wrong type: %T", root["permissions"])
	}
	if perms["defaultMode"] != "bypassPermissions" {
		t.Errorf("permissions.defaultMode: got %v, want bypassPermissions", perms["defaultMode"])
	}
}

// TestApplyConfig_ClaudeApprovalPostureUntrusted tests approvalPosture=untrusted -> permissions.defaultMode=default.
func TestApplyConfig_ClaudeApprovalPostureUntrusted(t *testing.T) {
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}

	a := configArtifact("cfg", []string{"claude"}, map[string]interface{}{
		"approvalPosture": "untrusted",
	})

	r := applyConfig(t, home, "claude", a)
	if r.Status != agentinstall.StatusApplied {
		t.Fatalf("expected applied, got %q (reason: %q)", r.Status, r.Reason)
	}

	data, _ := os.ReadFile(filepath.Join(home, ".claude", "settings.json"))
	var root map[string]interface{}
	_ = json.Unmarshal(data, &root)
	perms, ok := root["permissions"].(map[string]interface{})
	if !ok {
		t.Fatalf("permissions missing or wrong type: %T", root["permissions"])
	}
	if perms["defaultMode"] != "default" {
		t.Errorf("permissions.defaultMode: got %v, want default", perms["defaultMode"])
	}
}

// TestApplyConfig_ClaudeApprovalPostureOnRequestSkipped tests that on-request is skipped with a reason.
func TestApplyConfig_ClaudeApprovalPostureOnRequestSkipped(t *testing.T) {
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}

	a := configArtifact("cfg", []string{"claude"}, map[string]interface{}{
		"approvalPosture": "on-request",
	})

	r := applyConfig(t, home, "claude", a)
	if r.Status != agentinstall.StatusSkipped {
		t.Fatalf("expected skipped (no applicable additions), got %q (reason: %q)", r.Status, r.Reason)
	}
	if !strings.Contains(r.Reason, "not mappable") {
		t.Errorf("reason should mention not mappable, got: %q", r.Reason)
	}
	// settings.json must not be created (nothing to write)
	if _, err := os.Stat(filepath.Join(home, ".claude", "settings.json")); !os.IsNotExist(err) {
		t.Error("settings.json should not be created when all keys are skipped")
	}
}

// TestApplyConfig_ClaudeApprovalPostureOnFailureSkipped tests that on-failure is also skipped.
func TestApplyConfig_ClaudeApprovalPostureOnFailureSkipped(t *testing.T) {
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}

	a := configArtifact("cfg", []string{"claude"}, map[string]interface{}{
		"approvalPosture": "on-failure",
	})

	r := applyConfig(t, home, "claude", a)
	if r.Status != agentinstall.StatusSkipped {
		t.Fatalf("expected skipped, got %q (reason: %q)", r.Status, r.Reason)
	}
	if !strings.Contains(r.Reason, "not mappable") {
		t.Errorf("reason should mention not mappable, got: %q", r.Reason)
	}
}

// TestApplyConfig_ClaudeForbiddenModelDropped tests that native "model" is not written.
func TestApplyConfig_ClaudeForbiddenModelDropped(t *testing.T) {
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}

	a := configArtifactWithNative("cfg", []string{"claude"}, nil, map[string]interface{}{
		"claude": map[string]interface{}{
			"model":   "gpt-4",
			"someKey": "someVal",
		},
	})

	r := applyConfig(t, home, "claude", a)
	if r.Status != agentinstall.StatusApplied {
		t.Fatalf("expected applied (someKey should be written), got %q (reason: %q)", r.Status, r.Reason)
	}
	if !strings.Contains(r.Reason, "model") {
		t.Errorf("reason should mention forbidden model key, got: %q", r.Reason)
	}

	data, _ := os.ReadFile(filepath.Join(home, ".claude", "settings.json"))
	var root map[string]interface{}
	_ = json.Unmarshal(data, &root)

	// "model" must NOT be written
	if root["model"] != nil {
		t.Errorf("forbidden key 'model' was written to settings.json: %v", root["model"])
	}
	// "someKey" must be written
	if root["someKey"] != "someVal" {
		t.Errorf("someKey: got %v, want someVal", root["someKey"])
	}
}

// TestApplyConfig_ClaudeNormalizedWinsOverNative tests that normalized-derived keys win over native passthrough.
func TestApplyConfig_ClaudeNormalizedWinsOverNative(t *testing.T) {
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}

	// Native says permissions.defaultMode=default, normalized says never->bypassPermissions
	a := configArtifactWithNative("cfg", []string{"claude"},
		map[string]interface{}{
			"approvalPosture": "never", // translates to permissions.defaultMode=bypassPermissions
		},
		map[string]interface{}{
			"claude": map[string]interface{}{
				"permissions": map[string]interface{}{
					"defaultMode": "default", // should be overwritten by normalized
				},
			},
		})

	r := applyConfig(t, home, "claude", a)
	if r.Status != agentinstall.StatusApplied {
		t.Fatalf("expected applied, got %q (reason: %q)", r.Status, r.Reason)
	}

	data, _ := os.ReadFile(filepath.Join(home, ".claude", "settings.json"))
	var root map[string]interface{}
	_ = json.Unmarshal(data, &root)
	perms, ok := root["permissions"].(map[string]interface{})
	if !ok {
		t.Fatalf("permissions missing")
	}
	// normalized wins: bypassPermissions, not "default"
	if perms["defaultMode"] != "bypassPermissions" {
		t.Errorf("permissions.defaultMode: got %v, want bypassPermissions (normalized should win)", perms["defaultMode"])
	}
}

// TestApplyConfig_ClaudeIdempotent tests that applying twice results in a stable settings.json.
func TestApplyConfig_ClaudeIdempotent(t *testing.T) {
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}
	a := configArtifact("cfg", []string{"claude"}, map[string]interface{}{
		"approvalPosture": "never",
	})

	for i := 1; i <= 2; i++ {
		r := applyConfig(t, home, "claude", a)
		if r.Status != agentinstall.StatusApplied {
			t.Fatalf("apply #%d: expected applied, got %q (reason: %q)", i, r.Status, r.Reason)
		}
	}

	data, _ := os.ReadFile(filepath.Join(home, ".claude", "settings.json"))
	var root map[string]interface{}
	_ = json.Unmarshal(data, &root)
	perms := root["permissions"].(map[string]interface{})
	if perms["defaultMode"] != "bypassPermissions" {
		t.Errorf("idempotent check: permissions.defaultMode: got %v", perms["defaultMode"])
	}
}

// TestApplyConfig_ClaudeNilConfigSkipped tests that nil Config returns Skipped.
func TestApplyConfig_ClaudeNilConfigSkipped(t *testing.T) {
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}

	a := agentinstall.Artifact{
		Kind:    agentinstall.KindConfig,
		Name:    "cfg",
		Targets: []string{"claude"},
		Config:  nil,
	}
	r := applyConfig(t, home, "claude", a)
	if r.Status != agentinstall.StatusSkipped {
		t.Fatalf("expected skipped, got %q (reason: %q)", r.Status, r.Reason)
	}
}

// TestApplyConfig_ClaudeUnknownNormalizedKeySkipped tests that unknown normalized keys are skipped with reason.
func TestApplyConfig_ClaudeUnknownNormalizedKeySkipped(t *testing.T) {
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}

	// Mix: unknown key (skipped) + valid approvalPosture (applied)
	a := configArtifact("cfg", []string{"claude"}, map[string]interface{}{
		"approvalPosture": "never",
		"unknownKey":      "val",
	})

	r := applyConfig(t, home, "claude", a)
	if r.Status != agentinstall.StatusApplied {
		t.Fatalf("expected applied (approvalPosture:never is valid), got %q (reason: %q)", r.Status, r.Reason)
	}
	if !strings.Contains(r.Reason, "unknownKey") {
		t.Errorf("reason should mention unknownKey, got: %q", r.Reason)
	}

	// settings.json should have permissions.defaultMode
	data, _ := os.ReadFile(filepath.Join(home, ".claude", "settings.json"))
	var root map[string]interface{}
	_ = json.Unmarshal(data, &root)
	perms, ok := root["permissions"].(map[string]interface{})
	if !ok {
		t.Fatalf("permissions missing")
	}
	if perms["defaultMode"] != "bypassPermissions" {
		t.Errorf("permissions.defaultMode: got %v, want bypassPermissions", perms["defaultMode"])
	}
	// unknownKey itself should not be written to the file
	if root["unknownKey"] != nil {
		t.Errorf("unknownKey should not appear in settings.json, got: %v", root["unknownKey"])
	}
}

// ---- Codex tests ------------------------------------------------------------

// TestApplyConfig_CodexApprovalPostureMergeAfterBaseGen tests codex approvalPosture with base-gen survival.
func TestApplyConfig_CodexApprovalPostureMergeAfterBaseGen(t *testing.T) {
	home := t.TempDir()
	codexHome := filepath.Join(home, ".codex")
	if err := os.MkdirAll(codexHome, 0o755); err != nil {
		t.Fatal(err)
	}

	// Simulate launcher-generated base config (same as MCP test)
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
		Kind:    agentinstall.KindConfig,
		Name:    "cfg",
		Targets: []string{"codex"},
		Config: &agentinstall.ConfigPayload{
			Normalized: map[string]interface{}{
				"approvalPosture": "never",
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

	// New entry: approval_policy = "never"
	if doc["approval_policy"] != "never" {
		t.Errorf("approval_policy: got %v, want never", doc["approval_policy"])
	}

	// Base-gen keys survive
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

// TestApplyConfig_CodexApprovalPostureAllValues tests all four codex approval_policy values.
func TestApplyConfig_CodexApprovalPostureAllValues(t *testing.T) {
	values := []string{"untrusted", "on-failure", "on-request", "never"}
	for _, v := range values {
		v := v
		t.Run(v, func(t *testing.T) {
			home := t.TempDir()
			codexHome := filepath.Join(home, ".codex")
			if err := os.MkdirAll(codexHome, 0o755); err != nil {
				t.Fatal(err)
			}
			env := agentinstall.MapEnviron{"HOME": home, "CODEX_HOME": codexHome}
			reg := agentinstall.NewRegistry(env)
			opts := agentinstall.Options{HomeDir: home}
			a := agentinstall.Artifact{
				Kind:    agentinstall.KindConfig,
				Name:    "cfg",
				Targets: []string{"codex"},
				Config: &agentinstall.ConfigPayload{
					Normalized: map[string]interface{}{
						"approvalPosture": v,
					},
				},
			}
			res := agentinstall.Apply(reg, agentinstall.Manifest{Artifacts: []agentinstall.Artifact{a}}, opts, env)
			r := res.Reports[0]
			if r.Status != agentinstall.StatusApplied {
				t.Fatalf("approvalPosture=%q: expected applied, got %q (reason: %q)", v, r.Status, r.Reason)
			}

			data, _ := os.ReadFile(filepath.Join(codexHome, "config.toml"))
			var doc map[string]interface{}
			_ = toml.Unmarshal(data, &doc)
			if doc["approval_policy"] != v {
				t.Errorf("approval_policy: got %v, want %v", doc["approval_policy"], v)
			}
		})
	}
}

// TestApplyConfig_CodexNativePassthroughAndForbiddenModel tests native merge + forbidden model drop.
func TestApplyConfig_CodexNativePassthroughAndForbiddenModel(t *testing.T) {
	home := t.TempDir()
	codexHome := filepath.Join(home, ".codex")
	if err := os.MkdirAll(codexHome, 0o755); err != nil {
		t.Fatal(err)
	}

	env := agentinstall.MapEnviron{"HOME": home, "CODEX_HOME": codexHome}
	reg := agentinstall.NewRegistry(env)
	opts := agentinstall.Options{HomeDir: home}
	a := agentinstall.Artifact{
		Kind:    agentinstall.KindConfig,
		Name:    "cfg",
		Targets: []string{"codex"},
		Config: &agentinstall.ConfigPayload{
			Native: map[string]interface{}{
				"codex": map[string]interface{}{
					"model":    "gpt-4", // forbidden
					"some_key": "val",   // allowed
				},
			},
		},
	}
	res := agentinstall.Apply(reg, agentinstall.Manifest{Artifacts: []agentinstall.Artifact{a}}, opts, env)
	r := res.Reports[0]
	if r.Status != agentinstall.StatusApplied {
		t.Fatalf("expected applied, got %q (reason: %q)", r.Status, r.Reason)
	}
	if !strings.Contains(r.Reason, "model") {
		t.Errorf("reason should mention forbidden model key, got: %q", r.Reason)
	}

	data, _ := os.ReadFile(filepath.Join(codexHome, "config.toml"))
	var doc map[string]interface{}
	_ = toml.Unmarshal(data, &doc)

	if doc["model"] != nil {
		t.Errorf("forbidden key 'model' was written: %v", doc["model"])
	}
	if doc["some_key"] != "val" {
		t.Errorf("some_key: got %v, want val", doc["some_key"])
	}
}

// TestApplyConfig_CodexIdempotent tests that applying twice produces a stable config.toml.
func TestApplyConfig_CodexIdempotent(t *testing.T) {
	home := t.TempDir()
	codexHome := filepath.Join(home, ".codex")
	if err := os.MkdirAll(codexHome, 0o755); err != nil {
		t.Fatal(err)
	}
	env := agentinstall.MapEnviron{"HOME": home, "CODEX_HOME": codexHome}
	reg := agentinstall.NewRegistry(env)
	opts := agentinstall.Options{HomeDir: home}
	a := agentinstall.Artifact{
		Kind:    agentinstall.KindConfig,
		Name:    "cfg",
		Targets: []string{"codex"},
		Config: &agentinstall.ConfigPayload{
			Normalized: map[string]interface{}{
				"approvalPosture": "never",
			},
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
	if doc["approval_policy"] != "never" {
		t.Errorf("idempotency: approval_policy: got %v, want never", doc["approval_policy"])
	}
}

// ---- Opencode tests ---------------------------------------------------------

// TestApplyConfig_OpencodeApprovalPostureSkippedNativeApplied tests that approvalPosture is
// skipped with reason while native passthrough is still applied.
func TestApplyConfig_OpencodeApprovalPostureSkippedNativeApplied(t *testing.T) {
	home := t.TempDir()
	configDir := filepath.Join(home, ".config", "opencode")
	if err := os.MkdirAll(configDir, 0o755); err != nil {
		t.Fatal(err)
	}

	a := configArtifactWithNative("cfg", []string{"opencode"},
		map[string]interface{}{
			"approvalPosture": "never", // skipped for opencode
		},
		map[string]interface{}{
			"opencode": map[string]interface{}{
				"theme": "dark", // allowed native passthrough
			},
		})

	r := applyConfig(t, home, "opencode", a)
	if r.Status != agentinstall.StatusApplied {
		t.Fatalf("expected applied (native passthrough applies), got %q (reason: %q)", r.Status, r.Reason)
	}
	if !strings.Contains(r.Reason, "approvalPosture") {
		t.Errorf("reason should mention approvalPosture skip, got: %q", r.Reason)
	}

	data, err := os.ReadFile(filepath.Join(configDir, "opencode.json"))
	if err != nil {
		t.Fatalf("read opencode.json: %v", err)
	}
	std, _ := hujson.Standardize(data)
	var root map[string]interface{}
	_ = json.Unmarshal(std, &root)
	if root["theme"] != "dark" {
		t.Errorf("theme: got %v, want dark", root["theme"])
	}
}

// TestApplyConfig_OpencodeApprovalPostureOnlySkipped tests that only approvalPosture results in Skipped.
func TestApplyConfig_OpencodeApprovalPostureOnlySkipped(t *testing.T) {
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, ".config", "opencode"), 0o755); err != nil {
		t.Fatal(err)
	}

	a := configArtifact("cfg", []string{"opencode"}, map[string]interface{}{
		"approvalPosture": "never",
	})

	r := applyConfig(t, home, "opencode", a)
	if r.Status != agentinstall.StatusSkipped {
		t.Fatalf("expected skipped (no applicable additions), got %q (reason: %q)", r.Status, r.Reason)
	}
	if !strings.Contains(r.Reason, "approvalPosture") {
		t.Errorf("reason should mention approvalPosture, got: %q", r.Reason)
	}
}

// TestApplyConfig_OpencodeJSONCCommentTolerance tests that JSONC comments survive after config apply.
func TestApplyConfig_OpencodeJSONCCommentTolerance(t *testing.T) {
	home := t.TempDir()
	configDir := filepath.Join(home, ".config", "opencode")
	if err := os.MkdirAll(configDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// Pre-write a JSONC file with comments and existing entries
	existingJSONC := `{
  // This is a comment
  "existingKey": "existingVal",
  "nested": {
    "a": 1
  }
}`
	if err := os.WriteFile(filepath.Join(configDir, "opencode.json"), []byte(existingJSONC), 0o644); err != nil {
		t.Fatal(err)
	}

	a := configArtifactWithNative("cfg", []string{"opencode"}, nil, map[string]interface{}{
		"opencode": map[string]interface{}{
			"newKey": "newVal",
			"nested": map[string]interface{}{
				"b": 2, // should merge with existing nested, not replace
			},
		},
	})

	r := applyConfig(t, home, "opencode", a)
	if r.Status != agentinstall.StatusApplied {
		t.Fatalf("expected applied, got %q (reason: %q)", r.Status, r.Reason)
	}

	data, err := os.ReadFile(filepath.Join(configDir, "opencode.json"))
	if err != nil {
		t.Fatalf("read opencode.json: %v", err)
	}
	// Verify the file remains valid JSONC (hujson can parse it)
	_, err = hujson.Parse(data)
	if err != nil {
		t.Fatalf("result is not valid JSONC: %v\ncontent:\n%s", err, data)
	}

	std, _ := hujson.Standardize(data)
	var root map[string]interface{}
	_ = json.Unmarshal(std, &root)

	if root["existingKey"] != "existingVal" {
		t.Errorf("existingKey was lost: got %v", root["existingKey"])
	}
	if root["newKey"] != "newVal" {
		t.Errorf("newKey not added: got %v", root["newKey"])
	}

	// nested.a survives, nested.b added (deep merge)
	nested, ok := root["nested"].(map[string]interface{})
	if !ok {
		t.Fatalf("nested missing or wrong type: %T", root["nested"])
	}
	// Note: JSON numbers deserialize as float64
	if nested["a"] != float64(1) {
		t.Errorf("nested.a was lost: got %v", nested["a"])
	}
	if nested["b"] != float64(2) {
		t.Errorf("nested.b not added: got %v", nested["b"])
	}
}

// TestApplyConfig_OpencodeExistingKeyPreserved tests that existing entries survive unrelated config apply.
func TestApplyConfig_OpencodeExistingKeyPreserved(t *testing.T) {
	home := t.TempDir()
	configDir := filepath.Join(home, ".config", "opencode")
	if err := os.MkdirAll(configDir, 0o755); err != nil {
		t.Fatal(err)
	}
	existing := `{"otherKey": "otherVal"}`
	if err := os.WriteFile(filepath.Join(configDir, "opencode.json"), []byte(existing), 0o644); err != nil {
		t.Fatal(err)
	}

	a := configArtifactWithNative("cfg", []string{"opencode"}, nil, map[string]interface{}{
		"opencode": map[string]interface{}{
			"theme": "light",
		},
	})

	r := applyConfig(t, home, "opencode", a)
	if r.Status != agentinstall.StatusApplied {
		t.Fatalf("expected applied, got %q (reason: %q)", r.Status, r.Reason)
	}

	data, _ := os.ReadFile(filepath.Join(configDir, "opencode.json"))
	std, _ := hujson.Standardize(data)
	var root map[string]interface{}
	_ = json.Unmarshal(std, &root)
	if root["otherKey"] != "otherVal" {
		t.Errorf("otherKey was lost: got %v", root["otherKey"])
	}
	if root["theme"] != "light" {
		t.Errorf("theme not added: got %v", root["theme"])
	}
}

// TestApplyConfig_OpencodeForbiddenModelDropped tests that native "model" is not written for opencode.
func TestApplyConfig_OpencodeForbiddenModelDropped(t *testing.T) {
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, ".config", "opencode"), 0o755); err != nil {
		t.Fatal(err)
	}

	a := configArtifactWithNative("cfg", []string{"opencode"}, nil, map[string]interface{}{
		"opencode": map[string]interface{}{
			"model": "gpt-4", // forbidden
			"theme": "dark",  // allowed
		},
	})

	r := applyConfig(t, home, "opencode", a)
	if r.Status != agentinstall.StatusApplied {
		t.Fatalf("expected applied (theme should be written), got %q (reason: %q)", r.Status, r.Reason)
	}
	if !strings.Contains(r.Reason, "model") {
		t.Errorf("reason should mention forbidden model key, got: %q", r.Reason)
	}

	data, _ := os.ReadFile(filepath.Join(home, ".config", "opencode", "opencode.json"))
	std, _ := hujson.Standardize(data)
	var root map[string]interface{}
	_ = json.Unmarshal(std, &root)
	if root["model"] != nil {
		t.Errorf("forbidden key 'model' was written: %v", root["model"])
	}
	if root["theme"] != "dark" {
		t.Errorf("theme: got %v, want dark", root["theme"])
	}
}

// TestApplyConfig_OpencodeIdempotent tests that applying twice results in stable opencode.json.
func TestApplyConfig_OpencodeIdempotent(t *testing.T) {
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, ".config", "opencode"), 0o755); err != nil {
		t.Fatal(err)
	}
	a := configArtifactWithNative("cfg", []string{"opencode"}, nil, map[string]interface{}{
		"opencode": map[string]interface{}{
			"theme": "dark",
		},
	})

	for i := 1; i <= 2; i++ {
		r := applyConfig(t, home, "opencode", a)
		if r.Status != agentinstall.StatusApplied {
			t.Fatalf("apply #%d: expected applied, got %q (reason: %q)", i, r.Status, r.Reason)
		}
	}

	data, _ := os.ReadFile(filepath.Join(home, ".config", "opencode", "opencode.json"))
	std, _ := hujson.Standardize(data)
	var root map[string]interface{}
	_ = json.Unmarshal(std, &root)
	if root["theme"] != "dark" {
		t.Errorf("idempotency: theme: got %v, want dark", root["theme"])
	}
}

// ---- Goose/Hermes deferred tests -------------------------------------------

// TestApplyConfig_GooseDeferred tests that goose ApplyConfig is always skipped (deferred).
func TestApplyConfig_GooseDeferred(t *testing.T) {
	home := t.TempDir()
	a := configArtifact("cfg", []string{"goose"}, map[string]interface{}{
		"approvalPosture": "never",
	})
	r := applyConfig(t, home, "goose", a)
	if r.Status != agentinstall.StatusSkipped {
		t.Fatalf("expected skipped, got %q (reason: %q)", r.Status, r.Reason)
	}
	if !strings.Contains(r.Reason, "deferred") {
		t.Errorf("reason should mention deferred, got: %q", r.Reason)
	}
}

// TestApplyConfig_HermesDeferred tests that hermes ApplyConfig is always skipped (deferred).
func TestApplyConfig_HermesDeferred(t *testing.T) {
	home := t.TempDir()
	a := configArtifact("cfg", []string{"hermes"}, map[string]interface{}{
		"approvalPosture": "never",
	})
	r := applyConfig(t, home, "hermes", a)
	if r.Status != agentinstall.StatusSkipped {
		t.Fatalf("expected skipped, got %q (reason: %q)", r.Status, r.Reason)
	}
	if !strings.Contains(r.Reason, "deferred") {
		t.Errorf("reason should mention deferred, got: %q", r.Reason)
	}
}

// TestApplyConfig_NilConfigAlwaysSkipped tests nil Config for all agents.
func TestApplyConfig_NilConfigAlwaysSkipped(t *testing.T) {
	agents := []string{"claude", "codex", "opencode", "hermes", "goose"}
	for _, agent := range agents {
		agent := agent
		t.Run(agent, func(t *testing.T) {
			home := t.TempDir()
			// For agents that need their config dir to be detected, we don't create it —
			// the nil payload check happens before any IO.
			a := agentinstall.Artifact{
				Kind:    agentinstall.KindConfig,
				Name:    "cfg",
				Targets: []string{agent},
				Config:  nil,
			}
			r := applyConfig(t, home, agent, a)
			if r.Status != agentinstall.StatusSkipped {
				t.Errorf("%s: nil Config: expected skipped, got %q (reason: %q)", agent, r.Status, r.Reason)
			}
		})
	}
}
