package agentinstall_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"spawnery/internal/agentinstall"
	"spawnery/internal/agentinstall/spec"
)

func TestCanonicalSkillsDir(t *testing.T) {
	got := agentinstall.CanonicalSkillsDir("/h")
	want := filepath.Join("/h", ".agents", "skills")
	if got != want {
		t.Errorf("CanonicalSkillsDir(%q) = %q, want %q", "/h", got, want)
	}
}

// mustNotBeSymlink fails the test if path is a symlink — guards against #38051/#25367
// (claude-code stops loading user-level skills, or fails per-skill-dir with "Unknown
// skill", when the destination is a symlink).
func mustNotBeSymlink(t *testing.T, path string) {
	t.Helper()
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatalf("lstat %s: %v", path, err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		t.Fatalf("%s must be a real directory, not a symlink", path)
	}
}

func TestSkillInstallsToCanonicalAndClaudeCopy(t *testing.T) {
	home := t.TempDir()
	artifacts := t.TempDir()
	stageSkillTree(t, artifacts, "payloads/my-skill")

	r, all := applySkill(home, artifacts, "claude", "my-skill", "payloads/my-skill")
	if len(all) != 1 {
		t.Fatalf("expected 1 report, got %d", len(all))
	}
	if r.Status != agentinstall.StatusApplied {
		t.Fatalf("expected applied, got %q (reason: %q)", r.Status, r.Reason)
	}

	canonicalDest := filepath.Join(agentinstall.CanonicalSkillsDir(home), "my-skill")
	claudeDest := filepath.Join(home, ".claude", "skills", "my-skill")

	for _, dest := range []string{canonicalDest, claudeDest} {
		mustNotBeSymlink(t, dest)
		md, err := os.ReadFile(filepath.Join(dest, "SKILL.md"))
		if err != nil {
			t.Fatalf("read SKILL.md at %s: %v", dest, err)
		}
		if string(md) != "# skill\n" {
			t.Errorf("SKILL.md at %s: got %q want %q", dest, string(md), "# skill\n")
		}
		nested, err := os.ReadFile(filepath.Join(dest, "sub", "nested.txt"))
		if err != nil {
			t.Fatalf("read sub/nested.txt at %s: %v", dest, err)
		}
		if string(nested) != "nested\n" {
			t.Errorf("nested.txt at %s: got %q want %q", dest, string(nested), "nested\n")
		}
	}
}

func TestSkillInstallIdempotent(t *testing.T) {
	home := t.TempDir()
	artifacts := t.TempDir()
	stageSkillTree(t, artifacts, "payloads/my-skill")

	r1, _ := applySkill(home, artifacts, "claude", "my-skill", "payloads/my-skill")
	if r1.Status != agentinstall.StatusApplied {
		t.Fatalf("first apply: expected applied, got %q (reason: %q)", r1.Status, r1.Reason)
	}

	// Second apply with changed body.
	v2Dir := filepath.Join(artifacts, "payloads", "my-skill-v2")
	if err := os.MkdirAll(v2Dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(v2Dir, "SKILL.md"), []byte("# v2\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	r2, _ := applySkill(home, artifacts, "claude", "my-skill", "payloads/my-skill-v2")
	if r2.Status != agentinstall.StatusApplied {
		t.Fatalf("second apply: expected applied, got %q (reason: %q)", r2.Status, r2.Reason)
	}

	canonicalDest := filepath.Join(agentinstall.CanonicalSkillsDir(home), "my-skill")
	claudeDest := filepath.Join(home, ".claude", "skills", "my-skill")

	for _, dest := range []string{canonicalDest, claudeDest} {
		md, err := os.ReadFile(filepath.Join(dest, "SKILL.md"))
		if err != nil {
			t.Fatalf("read SKILL.md at %s: %v", dest, err)
		}
		if string(md) != "# v2\n" {
			t.Errorf("SKILL.md at %s: got %q want %q (not updated)", dest, string(md), "# v2\n")
		}
		entries, err := os.ReadDir(filepath.Dir(dest))
		if err != nil {
			t.Fatalf("read dir %s: %v", filepath.Dir(dest), err)
		}
		for _, e := range entries {
			if strings.HasPrefix(e.Name(), ".tmp-") {
				t.Errorf("leftover temp dir in %s: %s", filepath.Dir(dest), e.Name())
			}
		}
	}
}

func TestCanonicalHonorsTargets_ScopedSkillAbsentForNonTargetedAgent(t *testing.T) {
	home := t.TempDir()
	artifacts := t.TempDir()
	stageSkillTree(t, artifacts, "payloads/my-skill")

	env := agentinstall.MapEnviron{"HOME": home}
	reg := agentinstall.NewRegistry(env)
	m := agentinstall.Manifest{
		Artifacts: []agentinstall.Artifact{
			{
				Kind:    agentinstall.KindSkill,
				Name:    "my-skill",
				Targets: []string{"claude"},
				Skill:   &agentinstall.SkillPayload{Dir: "payloads/my-skill"},
			},
		},
	}
	opts := agentinstall.Options{HomeDir: home, ArtifactsDir: artifacts}

	result := agentinstall.ApplyFiltered(reg, m, opts, env, "codex")
	if len(result.Reports) != 0 {
		t.Fatalf("expected 0 reports for non-targeted agent, got %d: %+v", len(result.Reports), result.Reports)
	}

	canonicalDest := filepath.Join(agentinstall.CanonicalSkillsDir(home), "my-skill")
	if _, err := os.Stat(canonicalDest); !os.IsNotExist(err) {
		t.Errorf("canonical dir must NOT exist for a Targets-scoped skill applied to a non-targeted agent: %s", canonicalDest)
	}
	codexDest := filepath.Join(home, ".codex", "skills", "my-skill")
	if _, err := os.Stat(codexDest); !os.IsNotExist(err) {
		t.Errorf("codex native dir must NOT exist: %s", codexDest)
	}

	// Mirror case: filtering on the targeted agent installs both canonical + native copy.
	result2 := agentinstall.ApplyFiltered(reg, m, opts, env, "claude")
	if len(result2.Reports) != 1 || result2.Reports[0].Status != agentinstall.StatusApplied {
		t.Fatalf("expected 1 applied report for targeted agent, got %+v", result2.Reports)
	}
	if _, err := os.Stat(filepath.Join(canonicalDest, "SKILL.md")); err != nil {
		t.Errorf("canonical copy missing for targeted agent: %v", err)
	}
	claudeDest := filepath.Join(home, ".claude", "skills", "my-skill", "SKILL.md")
	if _, err := os.Stat(claudeDest); err != nil {
		t.Errorf("claude copy missing for targeted agent: %v", err)
	}
}

func TestAllDetectedTargetsInstallCanonical(t *testing.T) {
	home := t.TempDir()
	artifacts := t.TempDir()
	stageSkillTree(t, artifacts, "payloads/my-skill")

	// goose must be "detected" (its ConfigRoot exists) for ApplyFiltered's all-detected
	// handling to target it.
	if err := os.MkdirAll(filepath.Join(home, ".config", "goose"), 0o755); err != nil {
		t.Fatal(err)
	}

	env := agentinstall.MapEnviron{"HOME": home}
	reg := agentinstall.NewRegistry(env)
	m := agentinstall.Manifest{
		Artifacts: []agentinstall.Artifact{
			{
				Kind:    agentinstall.KindSkill,
				Name:    "my-skill",
				Targets: []string{"all-detected"},
				Skill:   &agentinstall.SkillPayload{Dir: "payloads/my-skill"},
			},
		},
	}
	opts := agentinstall.Options{HomeDir: home, ArtifactsDir: artifacts}

	result := agentinstall.ApplyFiltered(reg, m, opts, env, "goose")
	if len(result.Reports) != 1 || result.Reports[0].Status != agentinstall.StatusApplied {
		t.Fatalf("expected 1 applied report, got %+v", result.Reports)
	}

	canonicalDest := filepath.Join(agentinstall.CanonicalSkillsDir(home), "my-skill", "SKILL.md")
	if _, err := os.Stat(canonicalDest); err != nil {
		t.Errorf("canonical copy missing: %v", err)
	}
	claudeSkillsDir := filepath.Join(home, ".claude", "skills")
	if _, err := os.Stat(claudeSkillsDir); !os.IsNotExist(err) {
		t.Errorf("~/.claude/skills must be untouched when filtering on goose: %s", claudeSkillsDir)
	}
}

func TestNameSquatRejected_Canonical(t *testing.T) {
	home := t.TempDir()
	artifacts := t.TempDir()
	stageSkillTree(t, artifacts, "payloads/beads")

	// Pre-create an unmanaged (no marker) skill dir under the canonical path.
	squatDir := filepath.Join(agentinstall.CanonicalSkillsDir(home), "beads")
	if err := os.MkdirAll(squatDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(squatDir, "SKILL.md"), []byte("# pre-existing\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	r, all := applySkill(home, artifacts, "goose", "beads", "payloads/beads")
	if len(all) != 1 {
		t.Fatalf("expected 1 report, got %d", len(all))
	}
	if r.Status != agentinstall.StatusFailed {
		t.Fatalf("expected failed, got %q (reason: %q)", r.Status, r.Reason)
	}
	if !containsAll(r.Reason, "already exists", squatDir, "not installed by spawnery") {
		t.Errorf("reason missing expected substrings: %q", r.Reason)
	}

	// Pre-existing content must be untouched.
	got, err := os.ReadFile(filepath.Join(squatDir, "SKILL.md"))
	if err != nil {
		t.Fatalf("read pre-existing SKILL.md: %v", err)
	}
	if string(got) != "# pre-existing\n" {
		t.Errorf("pre-existing SKILL.md was clobbered: got %q", string(got))
	}
}

func TestNameSquatRejected_ClaudeCopyBlocksBothDestinations(t *testing.T) {
	home := t.TempDir()
	artifacts := t.TempDir()
	stageSkillTree(t, artifacts, "payloads/beads")

	// Pre-create an unmanaged skill dir under claude's native copy path only.
	squatDir := filepath.Join(home, ".claude", "skills", "beads")
	if err := os.MkdirAll(squatDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(squatDir, "SKILL.md"), []byte("# pre-existing\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	r, all := applySkill(home, artifacts, "claude", "beads", "payloads/beads")
	if len(all) != 1 {
		t.Fatalf("expected 1 report, got %d", len(all))
	}
	if r.Status != agentinstall.StatusFailed {
		t.Fatalf("expected failed, got %q (reason: %q)", r.Status, r.Reason)
	}
	if !containsAll(r.Reason, "already exists", squatDir, "not installed by spawnery") {
		t.Errorf("reason missing expected substrings: %q", r.Reason)
	}

	// Nothing must have been written to the canonical dir either — the guard checks
	// both destinations before writing either (all-or-nothing across the two copies).
	canonicalDest := filepath.Join(agentinstall.CanonicalSkillsDir(home), "beads")
	if _, err := os.Stat(canonicalDest); !os.IsNotExist(err) {
		t.Errorf("canonical dir must NOT be written when the claude copy destination is squatted: %s", canonicalDest)
	}

	got, err := os.ReadFile(filepath.Join(squatDir, "SKILL.md"))
	if err != nil {
		t.Fatalf("read pre-existing SKILL.md: %v", err)
	}
	if string(got) != "# pre-existing\n" {
		t.Errorf("pre-existing SKILL.md was clobbered: got %q", string(got))
	}
}

func TestManagedSkillOverwriteReported(t *testing.T) {
	home := t.TempDir()
	artifacts := t.TempDir()
	stageSkillTree(t, artifacts, "payloads/my-skill")

	r1, _ := applySkill(home, artifacts, "claude", "my-skill", "payloads/my-skill")
	if r1.Status != agentinstall.StatusApplied {
		t.Fatalf("first apply: expected applied, got %q (reason: %q)", r1.Status, r1.Reason)
	}

	r2, _ := applySkill(home, artifacts, "claude", "my-skill", "payloads/my-skill")
	if r2.Status != agentinstall.StatusApplied {
		t.Fatalf("second apply: expected applied, got %q (reason: %q)", r2.Status, r2.Reason)
	}
	if !containsAll(r2.Reason, "overwrote previously-managed skill install") {
		t.Errorf("second apply's reason should record the overwrite, got: %q", r2.Reason)
	}
}

func TestSkillProvenanceCanonicalAndNativeRows(t *testing.T) {
	home := t.TempDir()
	artifacts := t.TempDir()
	stageSkillTree(t, artifacts, "payloads/my-skill")

	env := agentinstall.MapEnviron{"HOME": home}
	reg := agentinstall.NewRegistry(env)
	mk := func(agent string) agentinstall.Manifest {
		return agentinstall.Manifest{Artifacts: []agentinstall.Artifact{
			{
				Kind:    agentinstall.KindSkill,
				Name:    "my-skill",
				Targets: []string{agent},
				Skill:   &agentinstall.SkillPayload{Dir: "payloads/my-skill"},
			},
		}}
	}

	idxPath := filepath.Join(home, ".spawnery", "managed.json")
	opts := agentinstall.Options{HomeDir: home, ArtifactsDir: artifacts, ManagedIndexPath: idxPath}

	if res := agentinstall.Apply(reg, mk("claude"), opts, env); res.Reports[0].Status != agentinstall.StatusApplied {
		t.Fatalf("claude apply failed: %+v", res.Reports)
	}
	if res := agentinstall.Apply(reg, mk("goose"), opts, env); res.Reports[0].Status != agentinstall.StatusApplied {
		t.Fatalf("goose apply failed: %+v", res.Reports)
	}

	raw, err := os.ReadFile(idxPath)
	if err != nil {
		t.Fatalf("read managed index: %v", err)
	}
	var ix agentinstall.ManagedIndex
	if err := json.Unmarshal(raw, &ix); err != nil {
		t.Fatalf("unmarshal managed index: %v", err)
	}

	var claudeRows, gooseRows int
	for _, e := range ix.Entries {
		if e.Kind != agentinstall.KindSkill {
			continue
		}
		switch e.Agent {
		case "claude":
			claudeRows++
		case "goose":
			gooseRows++
		}
	}
	if claudeRows != 2 {
		t.Errorf("expected 2 skill rows for claude (canonical + native copy), got %d: %+v", claudeRows, ix.Entries)
	}
	if gooseRows != 1 {
		t.Errorf("expected 1 skill row for goose (canonical only), got %d: %+v", gooseRows, ix.Entries)
	}
}

// TestSkillMarkerCarriesSource verifies the ownership marker (.spawnery-skill.json) records the
// skill's source provenance in BOTH the canonical and native install copies (sp-mwco.2.8 §4.6).
func TestSkillMarkerCarriesSource(t *testing.T) {
	home := t.TempDir()
	artifacts := t.TempDir()
	stageSkillTree(t, artifacts, "payloads/my-skill")

	src := &spec.SkillSource{
		URL:    "https://github.com/obra/superpowers",
		Ref:    "main",
		Commit: "1111111111111111111111111111111111aaaa",
		Subdir: "skills/x",
	}
	env := agentinstall.MapEnviron{"HOME": home}
	reg := agentinstall.NewRegistry(env)
	m := agentinstall.Manifest{
		Artifacts: []agentinstall.Artifact{
			{
				Kind:    agentinstall.KindSkill,
				Name:    "my-skill",
				Targets: []string{"claude"},
				Skill:   &agentinstall.SkillPayload{Dir: "payloads/my-skill", Source: src},
			},
		},
	}
	opts := agentinstall.Options{HomeDir: home, ArtifactsDir: artifacts}
	result := agentinstall.Apply(reg, m, opts, env)
	if len(result.Reports) != 1 || result.Reports[0].Status != agentinstall.StatusApplied {
		t.Fatalf("expected applied, got %+v", result.Reports)
	}

	canonicalMarker := filepath.Join(agentinstall.CanonicalSkillsDir(home), "my-skill", ".spawnery-skill.json")
	nativeMarker := filepath.Join(home, ".claude", "skills", "my-skill", ".spawnery-skill.json")
	for _, path := range []string{canonicalMarker, nativeMarker} {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		var got map[string]any
		if err := json.Unmarshal(raw, &got); err != nil {
			t.Fatalf("unmarshal %s: %v", path, err)
		}
		if got["sourceUrl"] != src.URL || got["sourceRef"] != src.Ref ||
			got["sourceCommit"] != src.Commit || got["sourceSubdir"] != src.Subdir {
			t.Errorf("%s: source fields not recorded, got %+v", path, got)
		}
	}
}

// TestSkillMarkerNoSourceOmitsKeys verifies a no-source install's marker marshals with no
// source keys at all (omitempty), not empty-string keys.
func TestSkillMarkerNoSourceOmitsKeys(t *testing.T) {
	home := t.TempDir()
	artifacts := t.TempDir()
	stageSkillTree(t, artifacts, "payloads/my-skill")

	r, _ := applySkill(home, artifacts, "claude", "my-skill", "payloads/my-skill")
	if r.Status != agentinstall.StatusApplied {
		t.Fatalf("expected applied, got %q (reason: %q)", r.Status, r.Reason)
	}

	markerPath := filepath.Join(agentinstall.CanonicalSkillsDir(home), "my-skill", ".spawnery-skill.json")
	raw, err := os.ReadFile(markerPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"sourceUrl", "sourceRef", "sourceCommit", "sourceSubdir"} {
		if strings.Contains(string(raw), key) {
			t.Errorf("marker should have no %q key for a no-source install, got %s", key, raw)
		}
	}
}

func containsAll(s string, subs ...string) bool {
	for _, sub := range subs {
		if !strings.Contains(s, sub) {
			return false
		}
	}
	return true
}
