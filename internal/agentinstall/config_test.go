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

// TestApplyConfig_ClaudeNativePassthroughSurvival pre-seeds settings.json with existing
// content, applies a native passthrough with non-forbidden keys, and asserts all
// pre-existing entries survive (deep merge).
func TestApplyConfig_ClaudeNativePassthroughSurvival(t *testing.T) {
	home := t.TempDir()
	claudeDir := filepath.Join(home, ".claude")
	if err := os.MkdirAll(claudeDir, 0o755); err != nil {
		t.Fatal(err)
	}

	// Pre-seed with existing config (non-forbidden keys only).
	existing := map[string]interface{}{
		"existingMap": map[string]interface{}{
			"nested": "existingVal",
		},
		"otherKey": "existingValue",
	}
	seed, _ := json.MarshalIndent(existing, "", "  ")
	if err := os.WriteFile(filepath.Join(claudeDir, "settings.json"), seed, 0o644); err != nil {
		t.Fatal(err)
	}

	// Apply native passthrough that deep-merges existingMap and adds a new key.
	a := configArtifactWithNative("myconfig", []string{"claude"}, nil, map[string]interface{}{
		"claude": map[string]interface{}{
			"existingMap": map[string]interface{}{
				"added": "newVal",
			},
			"newTopKey": "newValue",
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

	// Pre-existing otherKey survives.
	if root["otherKey"] != "existingValue" {
		t.Errorf("otherKey was lost or changed: got %v", root["otherKey"])
	}
	// New top-level key was added.
	if root["newTopKey"] != "newValue" {
		t.Errorf("newTopKey not added: got %v", root["newTopKey"])
	}
	// existingMap.nested survives (deep merge).
	existingMap, ok := root["existingMap"].(map[string]interface{})
	if !ok {
		t.Fatalf("existingMap missing or wrong type: %T", root["existingMap"])
	}
	if existingMap["nested"] != "existingVal" {
		t.Errorf("existingMap.nested was lost: got %v", existingMap["nested"])
	}
	if existingMap["added"] != "newVal" {
		t.Errorf("existingMap.added not added: got %v", existingMap["added"])
	}
}

// TestApplyConfig_ClaudeApprovalPostureAlwaysAsk tests always-ask → permissions.defaultMode=default.
func TestApplyConfig_ClaudeApprovalPostureAlwaysAsk(t *testing.T) {
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}
	a := configArtifact("cfg", []string{"claude"}, map[string]interface{}{
		"approvalPosture": "always-ask",
	})
	r := applyConfig(t, home, "claude", a)
	if r.Status != agentinstall.StatusApplied {
		t.Fatalf("expected applied, got %q (reason: %q)", r.Status, r.Reason)
	}
	if r.Capability != agentinstall.CapabilityApplied {
		t.Errorf("Capability: got %q, want applied", r.Capability)
	}
	data, _ := os.ReadFile(filepath.Join(home, ".claude", "settings.json"))
	var root map[string]interface{}
	_ = json.Unmarshal(data, &root)
	perms, ok := root["permissions"].(map[string]interface{})
	if !ok {
		t.Fatalf("permissions missing or wrong type")
	}
	if perms["defaultMode"] != "default" {
		t.Errorf("permissions.defaultMode: got %v, want default", perms["defaultMode"])
	}
}

// TestApplyConfig_ClaudeApprovalPostureAskRisky tests ask-risky → permissions.defaultMode=acceptEdits.
func TestApplyConfig_ClaudeApprovalPostureAskRisky(t *testing.T) {
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}
	a := configArtifact("cfg", []string{"claude"}, map[string]interface{}{
		"approvalPosture": "ask-risky",
	})
	r := applyConfig(t, home, "claude", a)
	if r.Status != agentinstall.StatusApplied {
		t.Fatalf("expected applied, got %q (reason: %q)", r.Status, r.Reason)
	}
	if r.Capability != agentinstall.CapabilityApplied {
		t.Errorf("Capability: got %q, want applied", r.Capability)
	}
	data, _ := os.ReadFile(filepath.Join(home, ".claude", "settings.json"))
	var root map[string]interface{}
	_ = json.Unmarshal(data, &root)
	perms, ok := root["permissions"].(map[string]interface{})
	if !ok {
		t.Fatalf("permissions missing or wrong type")
	}
	if perms["defaultMode"] != "acceptEdits" {
		t.Errorf("permissions.defaultMode: got %v, want acceptEdits", perms["defaultMode"])
	}
}

// TestApplyConfig_ClaudeApprovalPostureYolo tests yolo → permissions.defaultMode=bypassPermissions.
func TestApplyConfig_ClaudeApprovalPostureYolo(t *testing.T) {
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}
	a := configArtifact("cfg", []string{"claude"}, map[string]interface{}{
		"approvalPosture": "yolo",
	})
	r := applyConfig(t, home, "claude", a)
	if r.Status != agentinstall.StatusApplied {
		t.Fatalf("expected applied, got %q (reason: %q)", r.Status, r.Reason)
	}
	if r.Capability != agentinstall.CapabilityApplied {
		t.Errorf("Capability: got %q, want applied", r.Capability)
	}
	data, _ := os.ReadFile(filepath.Join(home, ".claude", "settings.json"))
	var root map[string]interface{}
	_ = json.Unmarshal(data, &root)
	perms, ok := root["permissions"].(map[string]interface{})
	if !ok {
		t.Fatalf("permissions missing or wrong type")
	}
	if perms["defaultMode"] != "bypassPermissions" {
		t.Errorf("permissions.defaultMode: got %v, want bypassPermissions", perms["defaultMode"])
	}
}

// TestApplyConfig_ClaudeApprovalPostureAuto tests auto → permissions.defaultMode=acceptEdits
// with Capability=best-effort and a model-tier reason.
func TestApplyConfig_ClaudeApprovalPostureAuto(t *testing.T) {
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}
	a := configArtifact("cfg", []string{"claude"}, map[string]interface{}{
		"approvalPosture": "auto",
	})
	r := applyConfig(t, home, "claude", a)
	if r.Status != agentinstall.StatusApplied {
		t.Fatalf("expected applied, got %q (reason: %q)", r.Status, r.Reason)
	}
	if r.Capability != agentinstall.CapabilityBestEffort {
		t.Errorf("Capability: got %q, want best-effort", r.Capability)
	}
	if !strings.Contains(r.Reason, "model-tier") {
		t.Errorf("reason should mention model-tier, got: %q", r.Reason)
	}
	data, _ := os.ReadFile(filepath.Join(home, ".claude", "settings.json"))
	var root map[string]interface{}
	_ = json.Unmarshal(data, &root)
	perms, ok := root["permissions"].(map[string]interface{})
	if !ok {
		t.Fatalf("permissions missing or wrong type")
	}
	if perms["defaultMode"] != "acceptEdits" {
		t.Errorf("permissions.defaultMode: got %v, want acceptEdits (auto best-effort)", perms["defaultMode"])
	}
}

// TestApplyConfig_ClaudeApprovalPostureUnrecognized tests that an unrecognized value
// results in StatusSkipped with capability=unsupported.
func TestApplyConfig_ClaudeApprovalPostureUnrecognized(t *testing.T) {
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}
	a := configArtifact("cfg", []string{"claude"}, map[string]interface{}{
		"approvalPosture": "bogus-value",
	})
	r := applyConfig(t, home, "claude", a)
	if r.Status != agentinstall.StatusSkipped {
		t.Fatalf("expected skipped, got %q (reason: %q)", r.Status, r.Reason)
	}
	if r.Capability != agentinstall.CapabilityUnsupported {
		t.Errorf("Capability: got %q, want unsupported", r.Capability)
	}
	if !strings.Contains(r.Reason, "not recognized") {
		t.Errorf("reason should mention not recognized, got: %q", r.Reason)
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

	if root["model"] != nil {
		t.Errorf("forbidden key 'model' was written to settings.json: %v", root["model"])
	}
	if root["someKey"] != "someVal" {
		t.Errorf("someKey: got %v, want someVal", root["someKey"])
	}
}

// TestApplyConfig_ClaudeForbiddenPermissionsPassthrough tests that native "permissions"
// passthrough is dropped and noted in reason, while other native keys still apply.
func TestApplyConfig_ClaudeForbiddenPermissionsPassthrough(t *testing.T) {
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}

	a := configArtifactWithNative("cfg", []string{"claude"}, nil, map[string]interface{}{
		"claude": map[string]interface{}{
			"permissions": map[string]interface{}{"defaultMode": "bypassPermissions"},
			"otherKey":    "otherVal",
		},
	})

	r := applyConfig(t, home, "claude", a)
	if r.Status != agentinstall.StatusApplied {
		t.Fatalf("expected applied (otherKey should be written), got %q (reason: %q)", r.Status, r.Reason)
	}
	if !strings.Contains(r.Reason, "permissions") {
		t.Errorf("reason should mention forbidden permissions key, got: %q", r.Reason)
	}

	data, _ := os.ReadFile(filepath.Join(home, ".claude", "settings.json"))
	var root map[string]interface{}
	_ = json.Unmarshal(data, &root)

	// permissions must NOT be written via passthrough (it's forbidden).
	if root["permissions"] != nil {
		t.Errorf("forbidden 'permissions' key was written: %v", root["permissions"])
	}
	if root["otherKey"] != "otherVal" {
		t.Errorf("otherKey: got %v, want otherVal", root["otherKey"])
	}
}

// TestApplyConfig_ClaudeForbiddenNormalizedPermissionsKey tests that a normalized key
// named "permissions" (a forbidden key for claude) is rejected without being written,
// resulting in StatusSkipped with Capability=unsupported.
func TestApplyConfig_ClaudeForbiddenNormalizedPermissionsKey(t *testing.T) {
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}

	// Attempt to inject "permissions" via the normalized pathway — should be rejected.
	a := configArtifact("cfg", []string{"claude"}, map[string]interface{}{
		"permissions": map[string]interface{}{"allow": []interface{}{"*"}},
	})

	r := applyConfig(t, home, "claude", a)
	if r.Status != agentinstall.StatusSkipped {
		t.Fatalf("expected skipped (forbidden normalized key), got %q (reason: %q)", r.Status, r.Reason)
	}
	if r.Capability != agentinstall.CapabilityUnsupported {
		t.Errorf("Capability: got %q, want unsupported", r.Capability)
	}
	if !strings.Contains(r.Reason, "forbidden") {
		t.Errorf("reason should mention forbidden, got: %q", r.Reason)
	}
	// settings.json must NOT be created.
	if _, err := os.Stat(filepath.Join(home, ".claude", "settings.json")); !os.IsNotExist(err) {
		t.Error("settings.json should not be created for forbidden-only normalized key")
	}
}

// TestApplyConfig_ClaudeNormalizedWinsOverNative shows that native "permissions" passthrough
// is forbidden and dropped, while normalized approvalPosture still sets permissions.defaultMode.
// This demonstrates that the normalized pathway applies even when the native pathway is blocked.
func TestApplyConfig_ClaudeNormalizedWinsOverNative(t *testing.T) {
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}

	// Native tries to set permissions.defaultMode=default via forbidden passthrough.
	// Normalized sets approvalPosture=yolo → permissions.defaultMode=bypassPermissions.
	a := configArtifactWithNative("cfg", []string{"claude"},
		map[string]interface{}{
			"approvalPosture": "yolo",
		},
		map[string]interface{}{
			"claude": map[string]interface{}{
				"permissions": map[string]interface{}{
					"defaultMode": "default", // forbidden — dropped
				},
			},
		})

	r := applyConfig(t, home, "claude", a)
	if r.Status != agentinstall.StatusApplied {
		t.Fatalf("expected applied, got %q (reason: %q)", r.Status, r.Reason)
	}
	// The forbidden native "permissions" should be noted in reason.
	if !strings.Contains(r.Reason, "permissions") {
		t.Errorf("reason should mention forbidden permissions key, got: %q", r.Reason)
	}

	data, _ := os.ReadFile(filepath.Join(home, ".claude", "settings.json"))
	var root map[string]interface{}
	_ = json.Unmarshal(data, &root)
	perms, ok := root["permissions"].(map[string]interface{})
	if !ok {
		t.Fatalf("permissions missing (normalized translation should have written it)")
	}
	// Normalized wins: bypassPermissions from yolo, not "default" from forbidden passthrough.
	if perms["defaultMode"] != "bypassPermissions" {
		t.Errorf("permissions.defaultMode: got %v, want bypassPermissions (normalized applies even with native forbidden)", perms["defaultMode"])
	}
}

// TestApplyConfig_ClaudeDisabledTools tests disabledTools → permissions.deny for claude.
func TestApplyConfig_ClaudeDisabledTools(t *testing.T) {
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}
	a := configArtifact("cfg", []string{"claude"}, map[string]interface{}{
		"disabledTools": []interface{}{"WebFetch"},
	})
	r := applyConfig(t, home, "claude", a)
	if r.Status != agentinstall.StatusApplied {
		t.Fatalf("expected applied, got %q (reason: %q)", r.Status, r.Reason)
	}
	if r.Capability != agentinstall.CapabilityApplied {
		t.Errorf("Capability: got %q, want applied", r.Capability)
	}
	data, _ := os.ReadFile(filepath.Join(home, ".claude", "settings.json"))
	var root map[string]interface{}
	_ = json.Unmarshal(data, &root)
	perms, ok := root["permissions"].(map[string]interface{})
	if !ok {
		t.Fatalf("permissions missing or wrong type")
	}
	deny, ok := perms["deny"].([]interface{})
	if !ok {
		t.Fatalf("permissions.deny missing or wrong type: %T", perms["deny"])
	}
	found := false
	for _, e := range deny {
		if e == "WebFetch" {
			found = true
		}
	}
	if !found {
		t.Errorf("permissions.deny does not contain WebFetch: %v", deny)
	}
}

// TestApplyConfig_ClaudeAllowedDeniedCommands tests allowedCommands → permissions.allow
// and deniedCommands → permissions.deny for claude (Bash-wrapped).
func TestApplyConfig_ClaudeAllowedDeniedCommands(t *testing.T) {
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}
	a := configArtifact("cfg", []string{"claude"}, map[string]interface{}{
		"allowedCommands": []interface{}{"git status"},
		"deniedCommands":  []interface{}{"rm -rf *"},
	})
	r := applyConfig(t, home, "claude", a)
	if r.Status != agentinstall.StatusApplied {
		t.Fatalf("expected applied, got %q (reason: %q)", r.Status, r.Reason)
	}
	if r.Capability != agentinstall.CapabilityApplied {
		t.Errorf("Capability: got %q, want applied", r.Capability)
	}
	data, _ := os.ReadFile(filepath.Join(home, ".claude", "settings.json"))
	var root map[string]interface{}
	_ = json.Unmarshal(data, &root)
	perms, ok := root["permissions"].(map[string]interface{})
	if !ok {
		t.Fatalf("permissions missing or wrong type")
	}
	allow, _ := perms["allow"].([]interface{})
	deny, _ := perms["deny"].([]interface{})

	foundAllow := false
	for _, e := range allow {
		if e == "Bash(git status)" {
			foundAllow = true
		}
	}
	if !foundAllow {
		t.Errorf("permissions.allow does not contain Bash(git status): %v", allow)
	}

	foundDeny := false
	for _, e := range deny {
		if e == "Bash(rm -rf *)" {
			foundDeny = true
		}
	}
	if !foundDeny {
		t.Errorf("permissions.deny does not contain Bash(rm -rf *): %v", deny)
	}
}

// TestApplyConfig_ClaudeIdempotent tests that applying twice results in a stable settings.json.
func TestApplyConfig_ClaudeIdempotent(t *testing.T) {
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}
	a := configArtifact("cfg", []string{"claude"}, map[string]interface{}{
		"approvalPosture": "yolo",
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

// TestApplyConfig_ClaudeUnknownNormalizedKeySkipped tests that unknown normalized keys are
// skipped with reason while valid keys are still applied.
func TestApplyConfig_ClaudeUnknownNormalizedKeySkipped(t *testing.T) {
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}

	// Mix: unknown key (skipped) + valid approvalPosture (applied).
	a := configArtifact("cfg", []string{"claude"}, map[string]interface{}{
		"approvalPosture": "yolo",
		"unknownKey":      "val",
	})

	r := applyConfig(t, home, "claude", a)
	if r.Status != agentinstall.StatusApplied {
		t.Fatalf("expected applied (approvalPosture:yolo is valid), got %q (reason: %q)", r.Status, r.Reason)
	}
	if !strings.Contains(r.Reason, "unknownKey") {
		t.Errorf("reason should mention unknownKey, got: %q", r.Reason)
	}

	// settings.json should have permissions.defaultMode.
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
	// unknownKey itself should not be written to the file.
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

	// Simulate launcher-generated base config.
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
				"approvalPosture": "yolo",
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

	// New entry: approval_policy = "never" (yolo → never).
	if doc["approval_policy"] != "never" {
		t.Errorf("approval_policy: got %v, want never", doc["approval_policy"])
	}

	// Base-gen keys survive.
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

// TestApplyConfig_CodexApprovalPostureAlwaysAsk tests always-ask → approval_policy=untrusted.
func TestApplyConfig_CodexApprovalPostureAlwaysAsk(t *testing.T) {
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
			Normalized: map[string]interface{}{"approvalPosture": "always-ask"},
		},
	}
	res := agentinstall.Apply(reg, agentinstall.Manifest{Artifacts: []agentinstall.Artifact{a}}, opts, env)
	r := res.Reports[0]
	if r.Status != agentinstall.StatusApplied {
		t.Fatalf("expected applied, got %q (reason: %q)", r.Status, r.Reason)
	}
	if r.Capability != agentinstall.CapabilityApplied {
		t.Errorf("Capability: got %q, want applied", r.Capability)
	}
	data, _ := os.ReadFile(filepath.Join(codexHome, "config.toml"))
	var doc map[string]interface{}
	_ = toml.Unmarshal(data, &doc)
	if doc["approval_policy"] != "untrusted" {
		t.Errorf("approval_policy: got %v, want untrusted", doc["approval_policy"])
	}
}

// TestApplyConfig_CodexApprovalPostureAskRisky tests ask-risky → approval_policy=on-request.
func TestApplyConfig_CodexApprovalPostureAskRisky(t *testing.T) {
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
			Normalized: map[string]interface{}{"approvalPosture": "ask-risky"},
		},
	}
	res := agentinstall.Apply(reg, agentinstall.Manifest{Artifacts: []agentinstall.Artifact{a}}, opts, env)
	r := res.Reports[0]
	if r.Status != agentinstall.StatusApplied {
		t.Fatalf("expected applied, got %q (reason: %q)", r.Status, r.Reason)
	}
	if r.Capability != agentinstall.CapabilityApplied {
		t.Errorf("Capability: got %q, want applied", r.Capability)
	}
	data, _ := os.ReadFile(filepath.Join(codexHome, "config.toml"))
	var doc map[string]interface{}
	_ = toml.Unmarshal(data, &doc)
	if doc["approval_policy"] != "on-request" {
		t.Errorf("approval_policy: got %v, want on-request", doc["approval_policy"])
	}
}

// TestApplyConfig_CodexApprovalPostureYolo tests yolo → approval_policy=never.
func TestApplyConfig_CodexApprovalPostureYolo(t *testing.T) {
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
			Normalized: map[string]interface{}{"approvalPosture": "yolo"},
		},
	}
	res := agentinstall.Apply(reg, agentinstall.Manifest{Artifacts: []agentinstall.Artifact{a}}, opts, env)
	r := res.Reports[0]
	if r.Status != agentinstall.StatusApplied {
		t.Fatalf("expected applied, got %q (reason: %q)", r.Status, r.Reason)
	}
	if r.Capability != agentinstall.CapabilityApplied {
		t.Errorf("Capability: got %q, want applied", r.Capability)
	}
	data, _ := os.ReadFile(filepath.Join(codexHome, "config.toml"))
	var doc map[string]interface{}
	_ = toml.Unmarshal(data, &doc)
	if doc["approval_policy"] != "never" {
		t.Errorf("approval_policy: got %v, want never", doc["approval_policy"])
	}
}

// TestApplyConfig_CodexApprovalPostureAuto tests auto → approval_policy=on-failure with best-effort.
func TestApplyConfig_CodexApprovalPostureAuto(t *testing.T) {
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
			Normalized: map[string]interface{}{"approvalPosture": "auto"},
		},
	}
	res := agentinstall.Apply(reg, agentinstall.Manifest{Artifacts: []agentinstall.Artifact{a}}, opts, env)
	r := res.Reports[0]
	if r.Status != agentinstall.StatusApplied {
		t.Fatalf("expected applied, got %q (reason: %q)", r.Status, r.Reason)
	}
	if r.Capability != agentinstall.CapabilityBestEffort {
		t.Errorf("Capability: got %q, want best-effort", r.Capability)
	}
	if !strings.Contains(r.Reason, "model-tier") {
		t.Errorf("reason should mention model-tier gating, got: %q", r.Reason)
	}
	data, _ := os.ReadFile(filepath.Join(codexHome, "config.toml"))
	var doc map[string]interface{}
	_ = toml.Unmarshal(data, &doc)
	if doc["approval_policy"] != "on-failure" {
		t.Errorf("approval_policy: got %v, want on-failure (auto best-effort)", doc["approval_policy"])
	}
}

// TestApplyConfig_CodexDisabledToolsUnsupported tests that codex returns capability=unsupported
// for disabledTools since codex has no per-tool disable.
func TestApplyConfig_CodexDisabledToolsUnsupported(t *testing.T) {
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
				"disabledTools": []interface{}{"SomeTool"},
			},
		},
	}
	res := agentinstall.Apply(reg, agentinstall.Manifest{Artifacts: []agentinstall.Artifact{a}}, opts, env)
	r := res.Reports[0]
	if r.Status != agentinstall.StatusSkipped {
		t.Fatalf("expected skipped, got %q (reason: %q)", r.Status, r.Reason)
	}
	if r.Capability != agentinstall.CapabilityUnsupported {
		t.Errorf("Capability: got %q, want unsupported", r.Capability)
	}
	if !strings.Contains(r.Reason, "codex has no per-tool disable") {
		t.Errorf("reason should mention 'codex has no per-tool disable', got: %q", r.Reason)
	}
}

// TestApplyConfig_CodexForbiddenNormalizedSandboxMode tests that normalized "sandbox_mode"
// (a forbidden key for codex) is rejected without being written.
func TestApplyConfig_CodexForbiddenNormalizedSandboxMode(t *testing.T) {
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
				"sandbox_mode": "danger-full-access",
			},
		},
	}
	res := agentinstall.Apply(reg, agentinstall.Manifest{Artifacts: []agentinstall.Artifact{a}}, opts, env)
	r := res.Reports[0]
	if r.Status != agentinstall.StatusSkipped {
		t.Fatalf("expected skipped, got %q (reason: %q)", r.Status, r.Reason)
	}
	if r.Capability != agentinstall.CapabilityUnsupported {
		t.Errorf("Capability: got %q, want unsupported", r.Capability)
	}
	if !strings.Contains(r.Reason, "forbidden") {
		t.Errorf("reason should mention forbidden, got: %q", r.Reason)
	}
	// config.toml must NOT be created.
	if _, err := os.Stat(filepath.Join(codexHome, "config.toml")); !os.IsNotExist(err) {
		t.Error("config.toml should not be created for forbidden-only normalized key")
	}
}

// TestApplyConfig_CodexNativePassthroughAndForbiddenModel tests native merge + forbidden
// model drop.  Now codex also forbids approval_policy, sandbox_mode,
// sandbox_workspace_write from native passthrough.
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

// TestApplyConfig_CodexForbiddenSandboxModePassthrough tests that native sandbox_mode
// is dropped from passthrough.
func TestApplyConfig_CodexForbiddenSandboxModePassthrough(t *testing.T) {
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
					"sandbox_mode": "danger-full-access", // forbidden
					"my_setting":   "val",                // allowed
				},
			},
		},
	}
	res := agentinstall.Apply(reg, agentinstall.Manifest{Artifacts: []agentinstall.Artifact{a}}, opts, env)
	r := res.Reports[0]
	if r.Status != agentinstall.StatusApplied {
		t.Fatalf("expected applied (my_setting should be written), got %q (reason: %q)", r.Status, r.Reason)
	}
	if !strings.Contains(r.Reason, "sandbox_mode") {
		t.Errorf("reason should mention sandbox_mode, got: %q", r.Reason)
	}
	data, _ := os.ReadFile(filepath.Join(codexHome, "config.toml"))
	var doc map[string]interface{}
	_ = toml.Unmarshal(data, &doc)
	if doc["sandbox_mode"] != nil {
		t.Errorf("sandbox_mode was written (should be forbidden): %v", doc["sandbox_mode"])
	}
	if doc["my_setting"] != "val" {
		t.Errorf("my_setting: got %v, want val", doc["my_setting"])
	}
}

// TestApplyConfig_CodexAllowedCommandsWritesRulesFile tests that allowedCommands and
// deniedCommands for codex are written to the execpolicy rules file (not config.toml).
func TestApplyConfig_CodexAllowedCommandsWritesRulesFile(t *testing.T) {
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
				"allowedCommands": []interface{}{"git status"},
				"deniedCommands":  []interface{}{"rm -rf *"},
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
	if r.Capability != agentinstall.CapabilityApplied {
		t.Errorf("Capability: got %q, want applied", r.Capability)
	}

	// rules/default.rules must exist with allow/deny entries.
	rulesPath := filepath.Join(codexHome, "rules", "default.rules")
	rulesData, err := os.ReadFile(rulesPath)
	if err != nil {
		t.Fatalf("read %s: %v", rulesPath, err)
	}
	rulesStr := string(rulesData)
	if !strings.Contains(rulesStr, "allow git status") {
		t.Errorf("rules file missing 'allow git status':\n%s", rulesStr)
	}
	if !strings.Contains(rulesStr, "deny rm -rf *") {
		t.Errorf("rules file missing 'deny rm -rf *':\n%s", rulesStr)
	}

	// config.toml must NOT contain the command patterns (they go to rules, not TOML).
	if _, err := os.Stat(filepath.Join(codexHome, "config.toml")); os.IsNotExist(err) {
		// config.toml not created at all — that's fine (commands-only artifact).
	} else {
		data, _ := os.ReadFile(filepath.Join(codexHome, "config.toml"))
		if strings.Contains(string(data), "git status") {
			t.Errorf("config.toml should not contain command patterns:\n%s", data)
		}
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
				"approvalPosture": "yolo",
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
			"approvalPosture": "yolo", // skipped for opencode
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

// TestApplyConfig_OpencodeApprovalPostureOnlySkipped tests that only approvalPosture results
// in Skipped with capability=unsupported.
func TestApplyConfig_OpencodeApprovalPostureOnlySkipped(t *testing.T) {
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, ".config", "opencode"), 0o755); err != nil {
		t.Fatal(err)
	}

	a := configArtifact("cfg", []string{"opencode"}, map[string]interface{}{
		"approvalPosture": "yolo",
	})

	r := applyConfig(t, home, "opencode", a)
	if r.Status != agentinstall.StatusSkipped {
		t.Fatalf("expected skipped (no applicable additions), got %q (reason: %q)", r.Status, r.Reason)
	}
	if r.Capability != agentinstall.CapabilityUnsupported {
		t.Errorf("Capability: got %q, want unsupported", r.Capability)
	}
	if !strings.Contains(r.Reason, "approvalPosture") {
		t.Errorf("reason should mention approvalPosture, got: %q", r.Reason)
	}
}

// TestApplyConfig_OpencodeDisabledTools tests disabledTools → tools map for opencode.
func TestApplyConfig_OpencodeDisabledTools(t *testing.T) {
	home := t.TempDir()
	configDir := filepath.Join(home, ".config", "opencode")
	if err := os.MkdirAll(configDir, 0o755); err != nil {
		t.Fatal(err)
	}
	a := configArtifact("cfg", []string{"opencode"}, map[string]interface{}{
		"disabledTools": []interface{}{"write"},
	})
	r := applyConfig(t, home, "opencode", a)
	if r.Status != agentinstall.StatusApplied {
		t.Fatalf("expected applied, got %q (reason: %q)", r.Status, r.Reason)
	}
	if r.Capability != agentinstall.CapabilityApplied {
		t.Errorf("Capability: got %q, want applied", r.Capability)
	}
	data, _ := os.ReadFile(filepath.Join(configDir, "opencode.json"))
	std, _ := hujson.Standardize(data)
	var root map[string]interface{}
	_ = json.Unmarshal(std, &root)
	tools, ok := root["tools"].(map[string]interface{})
	if !ok {
		t.Fatalf("tools missing or wrong type: %T", root["tools"])
	}
	if tools["write"] != false {
		t.Errorf("tools.write: got %v, want false", tools["write"])
	}
}

// TestApplyConfig_OpencodeAllowedDeniedCommands tests allowedCommands/deniedCommands →
// permission.bash map for opencode.
func TestApplyConfig_OpencodeAllowedDeniedCommands(t *testing.T) {
	home := t.TempDir()
	configDir := filepath.Join(home, ".config", "opencode")
	if err := os.MkdirAll(configDir, 0o755); err != nil {
		t.Fatal(err)
	}
	a := configArtifact("cfg", []string{"opencode"}, map[string]interface{}{
		"allowedCommands": []interface{}{"git status"},
		"deniedCommands":  []interface{}{"rm -rf *"},
	})
	r := applyConfig(t, home, "opencode", a)
	if r.Status != agentinstall.StatusApplied {
		t.Fatalf("expected applied, got %q (reason: %q)", r.Status, r.Reason)
	}
	if r.Capability != agentinstall.CapabilityApplied {
		t.Errorf("Capability: got %q, want applied", r.Capability)
	}
	data, _ := os.ReadFile(filepath.Join(configDir, "opencode.json"))
	std, _ := hujson.Standardize(data)
	var root map[string]interface{}
	_ = json.Unmarshal(std, &root)
	permission, ok := root["permission"].(map[string]interface{})
	if !ok {
		t.Fatalf("permission missing or wrong type: %T", root["permission"])
	}
	bash, ok := permission["bash"].(map[string]interface{})
	if !ok {
		t.Fatalf("permission.bash missing or wrong type: %T", permission["bash"])
	}
	if bash["git status"] != "allow" {
		t.Errorf("permission.bash[\"git status\"]: got %v, want \"allow\"", bash["git status"])
	}
	if bash["rm -rf *"] != "deny" {
		t.Errorf("permission.bash[\"rm -rf *\"]: got %v, want \"deny\"", bash["rm -rf *"])
	}
}

// TestApplyConfig_OpencodeJSONCCommentTolerance tests that JSONC comments survive after config apply.
func TestApplyConfig_OpencodeJSONCCommentTolerance(t *testing.T) {
	home := t.TempDir()
	configDir := filepath.Join(home, ".config", "opencode")
	if err := os.MkdirAll(configDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// Pre-write a JSONC file with comments and existing entries.
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
	// Verify the file remains valid JSONC.
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

	// nested.a survives, nested.b added (deep merge).
	nested, ok := root["nested"].(map[string]interface{})
	if !ok {
		t.Fatalf("nested missing or wrong type: %T", root["nested"])
	}
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
		"approvalPosture": "yolo",
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
		"approvalPosture": "yolo",
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
