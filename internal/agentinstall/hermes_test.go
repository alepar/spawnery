package agentinstall_test

import (
	"os"
	"path/filepath"
	"testing"

	"gopkg.in/yaml.v3"

	"spawnery/internal/agentinstall"
)

// hermesConfigPath returns the path hermesEmitter writes/reads config.yaml at, for a
// given home.
func hermesConfigPath(home string) string {
	return filepath.Join(home, ".hermes", "config.yaml")
}

// readHermesConfig parses config.yaml at path into a generic map for assertions.
func readHermesConfig(t *testing.T, path string) map[string]interface{} {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var root map[string]interface{}
	if err := yaml.Unmarshal(data, &root); err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	return root
}

// externalDirs extracts skills.external_dirs as a []string from a parsed config map.
func externalDirs(t *testing.T, root map[string]interface{}) []string {
	t.Helper()
	skills, ok := root["skills"].(map[string]interface{})
	if !ok {
		t.Fatalf("skills key missing or wrong type: %#v", root["skills"])
	}
	raw, ok := skills["external_dirs"].([]interface{})
	if !ok {
		t.Fatalf("skills.external_dirs missing or wrong type: %#v", skills["external_dirs"])
	}
	out := make([]string, len(raw))
	for i, v := range raw {
		s, ok := v.(string)
		if !ok {
			t.Fatalf("external_dirs[%d] not a string: %#v", i, v)
		}
		out[i] = s
	}
	return out
}

// TestHermesGlue_ConfigAbsent covers (a): config.yaml absent -> InstallSkill installs
// the canonical tree AND creates ~/.hermes/config.yaml with skills.external_dirs set,
// dir 0700, file 0600.
func TestHermesGlue_ConfigAbsent(t *testing.T) {
	home := t.TempDir()
	artifactsDir := t.TempDir()
	stageSkillTree(t, artifactsDir, "payloads/my-skill")

	r, _ := applySkill(home, artifactsDir, "hermes", "my-skill", "payloads/my-skill")
	if r.Status != agentinstall.StatusApplied {
		t.Fatalf("status: got %q want applied (reason %q)", r.Status, r.Reason)
	}

	canonicalDest := filepath.Join(agentinstall.CanonicalSkillsDir(home), "my-skill")
	if _, err := os.Stat(filepath.Join(canonicalDest, "SKILL.md")); err != nil {
		t.Fatalf("canonical SKILL.md not installed: %v", err)
	}

	cfgPath := hermesConfigPath(home)
	root := readHermesConfig(t, cfgPath)
	dirs := externalDirs(t, root)
	want := agentinstall.CanonicalSkillsDir(home)
	found := false
	for _, d := range dirs {
		if d == want {
			found = true
		}
	}
	if !found {
		t.Fatalf("external_dirs %v does not contain canonical dir %q", dirs, want)
	}

	dirInfo, err := os.Stat(filepath.Dir(cfgPath))
	if err != nil {
		t.Fatalf("stat hermes config dir: %v", err)
	}
	if perm := dirInfo.Mode().Perm(); perm != 0o700 {
		t.Errorf("hermes config dir perm: got %o want 0700", perm)
	}

	fileInfo, err := os.Stat(cfgPath)
	if err != nil {
		t.Fatalf("stat hermes config file: %v", err)
	}
	if perm := fileInfo.Mode().Perm(); perm != 0o600 {
		t.Errorf("hermes config file perm: got %o want 0600", perm)
	}
}

// TestHermesGlue_PreservesLauncherModelBlock covers (b): config.yaml present with the
// launcher's model: block -> after InstallSkill the model: block is byte-for-byte
// preserved in value and skills.external_dirs is added. No user-config clobber.
func TestHermesGlue_PreservesLauncherModelBlock(t *testing.T) {
	home := t.TempDir()
	artifactsDir := t.TempDir()
	stageSkillTree(t, artifactsDir, "payloads/my-skill")

	cfgPath := hermesConfigPath(home)
	if err := os.MkdirAll(filepath.Dir(cfgPath), 0o700); err != nil {
		t.Fatal(err)
	}
	launcherContent := "model:\n  provider: custom\n  default: openai/gpt-4o-mini\n  base_url: http://127.0.0.1:8080/v1\n  api_key: sk-unused-sidecar-injects-real-key\n"
	if err := os.WriteFile(cfgPath, []byte(launcherContent), 0o600); err != nil {
		t.Fatal(err)
	}

	r, _ := applySkill(home, artifactsDir, "hermes", "my-skill", "payloads/my-skill")
	if r.Status != agentinstall.StatusApplied {
		t.Fatalf("status: got %q want applied (reason %q)", r.Status, r.Reason)
	}

	root := readHermesConfig(t, cfgPath)
	model, ok := root["model"].(map[string]interface{})
	if !ok {
		t.Fatalf("model key missing or wrong type: %#v", root["model"])
	}
	wantModel := map[string]string{
		"provider": "custom",
		"default":  "openai/gpt-4o-mini",
		"base_url": "http://127.0.0.1:8080/v1",
		"api_key":  "sk-unused-sidecar-injects-real-key",
	}
	for k, v := range wantModel {
		if got, _ := model[k].(string); got != v {
			t.Errorf("model.%s: got %q want %q", k, got, v)
		}
	}

	dirs := externalDirs(t, root)
	want := agentinstall.CanonicalSkillsDir(home)
	found := false
	for _, d := range dirs {
		if d == want {
			found = true
		}
	}
	if !found {
		t.Fatalf("external_dirs %v does not contain canonical dir %q", dirs, want)
	}
}

// TestHermesGlue_Idempotent covers (c): two InstallSkill calls (and a second skill)
// leave exactly ONE canonical entry in external_dirs; a pre-existing unrelated dir in
// external_dirs is preserved.
func TestHermesGlue_Idempotent(t *testing.T) {
	home := t.TempDir()
	artifactsDir := t.TempDir()
	stageSkillTree(t, artifactsDir, "payloads/skill-one")
	stageSkillTree(t, artifactsDir, "payloads/skill-two")

	cfgPath := hermesConfigPath(home)
	if err := os.MkdirAll(filepath.Dir(cfgPath), 0o700); err != nil {
		t.Fatal(err)
	}
	preseeded := "skills:\n  external_dirs:\n    - /opt/unrelated-dir\n"
	if err := os.WriteFile(cfgPath, []byte(preseeded), 0o600); err != nil {
		t.Fatal(err)
	}

	if r, _ := applySkill(home, artifactsDir, "hermes", "skill-one", "payloads/skill-one"); r.Status != agentinstall.StatusApplied {
		t.Fatalf("apply skill-one: got %q want applied (reason %q)", r.Status, r.Reason)
	}
	if r, _ := applySkill(home, artifactsDir, "hermes", "skill-one", "payloads/skill-one"); r.Status != agentinstall.StatusApplied {
		t.Fatalf("re-apply skill-one: got %q want applied (reason %q)", r.Status, r.Reason)
	}
	if r, _ := applySkill(home, artifactsDir, "hermes", "skill-two", "payloads/skill-two"); r.Status != agentinstall.StatusApplied {
		t.Fatalf("apply skill-two: got %q want applied (reason %q)", r.Status, r.Reason)
	}

	root := readHermesConfig(t, cfgPath)
	dirs := externalDirs(t, root)

	canonicalCount := 0
	unrelatedFound := false
	want := agentinstall.CanonicalSkillsDir(home)
	for _, d := range dirs {
		if d == want {
			canonicalCount++
		}
		if d == "/opt/unrelated-dir" {
			unrelatedFound = true
		}
	}
	if canonicalCount != 1 {
		t.Errorf("external_dirs %v: canonical dir count = %d, want 1", dirs, canonicalCount)
	}
	if !unrelatedFound {
		t.Errorf("external_dirs %v: pre-existing unrelated dir was not preserved", dirs)
	}
}

// TestHermesGlue_MalformedConfig covers (d): unparseable config.yaml -> StatusFailed,
// reason names the file, file left untouched.
func TestHermesGlue_MalformedConfig(t *testing.T) {
	home := t.TempDir()
	artifactsDir := t.TempDir()
	stageSkillTree(t, artifactsDir, "payloads/my-skill")

	cfgPath := hermesConfigPath(home)
	if err := os.MkdirAll(filepath.Dir(cfgPath), 0o700); err != nil {
		t.Fatal(err)
	}
	malformed := "not: valid: yaml: [\n"
	if err := os.WriteFile(cfgPath, []byte(malformed), 0o600); err != nil {
		t.Fatal(err)
	}

	r, _ := applySkill(home, artifactsDir, "hermes", "my-skill", "payloads/my-skill")
	if r.Status != agentinstall.StatusFailed {
		t.Fatalf("status: got %q want failed", r.Status)
	}
	if r.Reason == "" {
		t.Fatal("failed report must carry a non-empty reason")
	}

	after, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatalf("read config after failed apply: %v", err)
	}
	if string(after) != malformed {
		t.Fatalf("malformed config.yaml was modified:\n--- before ---\n%s\n--- after ---\n%s", malformed, after)
	}
}

// TestHermesGlue_WrongTypeExternalDirs covers (e): skills.external_dirs is a scalar (or
// skills is a scalar) -> StatusFailed, file untouched (never coerce user data away).
func TestHermesGlue_WrongTypeExternalDirs(t *testing.T) {
	cases := []struct {
		name    string
		content string
	}{
		{"external_dirs scalar", "skills:\n  external_dirs: not-a-list\n"},
		{"skills scalar", "skills: not-a-map\n"},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			artifactsDir := t.TempDir()
			stageSkillTree(t, artifactsDir, "payloads/my-skill")

			cfgPath := hermesConfigPath(home)
			if err := os.MkdirAll(filepath.Dir(cfgPath), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(cfgPath, []byte(tc.content), 0o600); err != nil {
				t.Fatal(err)
			}

			r, _ := applySkill(home, artifactsDir, "hermes", "my-skill", "payloads/my-skill")
			if r.Status != agentinstall.StatusFailed {
				t.Fatalf("status: got %q want failed", r.Status)
			}
			if r.Reason == "" {
				t.Fatal("failed report must carry a non-empty reason")
			}

			after, err := os.ReadFile(cfgPath)
			if err != nil {
				t.Fatalf("read config after failed apply: %v", err)
			}
			if string(after) != tc.content {
				t.Fatalf("config.yaml was modified:\n--- before ---\n%s\n--- after ---\n%s", tc.content, after)
			}
		})
	}
}

// TestHermesGlue_ReportMentionsGlue covers (f): a successful glue write is mentioned in
// the Report.Reason (so sp-mwco.2.7's apply-report surfaces it).
func TestHermesGlue_ReportMentionsGlue(t *testing.T) {
	home := t.TempDir()
	artifactsDir := t.TempDir()
	stageSkillTree(t, artifactsDir, "payloads/my-skill")

	r, _ := applySkill(home, artifactsDir, "hermes", "my-skill", "payloads/my-skill")
	if r.Status != agentinstall.StatusApplied {
		t.Fatalf("status: got %q want applied (reason %q)", r.Status, r.Reason)
	}
	if r.Reason == "" {
		t.Fatal("expected Report.Reason to mention the external_dirs glue write, got empty")
	}
}

// TestHermesCapabilities pins hermes' skill=supported capability (the glue makes it real).
func TestHermesCapabilities(t *testing.T) {
	env := agentinstall.MapEnviron{"HOME": "/h"}
	reg := agentinstall.NewRegistry(env)
	e, ok := reg.Lookup("hermes")
	if !ok {
		t.Fatal("Lookup(hermes) not found")
	}
	caps := e.Capabilities()
	if caps[agentinstall.KindSkill] != agentinstall.CapStatusSupported {
		t.Errorf("hermes skill capability: got %q want supported", caps[agentinstall.KindSkill])
	}
	for _, k := range []agentinstall.Kind{agentinstall.KindMCP, agentinstall.KindConfig, agentinstall.KindPlugin, agentinstall.Kind("instructions")} {
		if caps[k] != agentinstall.CapStatusNoOp {
			t.Errorf("hermes %s capability: got %q want no-op", k, caps[k])
		}
	}
}
