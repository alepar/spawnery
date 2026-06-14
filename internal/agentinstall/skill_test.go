package agentinstall_test

import (
	"os"
	"path/filepath"
	"testing"

	"spawnery/internal/agentinstall"
)

// stageSkillTree creates a skill directory at <artifactsDir>/<relPath> with:
//   - SKILL.md (content "# skill\n")
//   - sub/nested.txt (content "nested\n", mode 0o644)
//   - exec.sh (content "#!/bin/sh\n", mode 0o755)
func stageSkillTree(t *testing.T, artifactsDir, relPath string) {
	t.Helper()
	skillDir := filepath.Join(artifactsDir, relPath)
	if err := os.MkdirAll(filepath.Join(skillDir, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	write := func(path, content string, mode os.FileMode) {
		if err := os.WriteFile(path, []byte(content), mode); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(skillDir, "SKILL.md"), "# skill\n", 0o644)
	write(filepath.Join(skillDir, "sub", "nested.txt"), "nested\n", 0o644)
	write(filepath.Join(skillDir, "exec.sh"), "#!/bin/sh\n", 0o755)
}

// applySkill is a convenience wrapper to apply a single skill artifact.
func applySkill(home, artifactsDir, agentName, skillName, skillDir string) (agentinstall.Report, []agentinstall.Report) {
	env := agentinstall.MapEnviron{"HOME": home}
	reg := agentinstall.NewRegistry(env)
	m := agentinstall.Manifest{
		Artifacts: []agentinstall.Artifact{
			{
				Kind:    agentinstall.KindSkill,
				Name:    skillName,
				Targets: []string{agentName},
				Skill:   &agentinstall.SkillPayload{Dir: skillDir},
			},
		},
	}
	opts := agentinstall.Options{
		HomeDir:      home,
		ArtifactsDir: artifactsDir,
	}
	result := agentinstall.Apply(reg, m, opts, env)
	if len(result.Reports) == 0 {
		return agentinstall.Report{}, result.Reports
	}
	return result.Reports[0], result.Reports
}

func TestInstallSkillApplied(t *testing.T) {
	// Table: emitter name → expected skill root relative to home
	tests := []struct {
		agent       string
		skillSubDir string // relative to home
	}{
		{"claude", ".claude/skills"},
		{"codex", ".codex/skills"},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.agent, func(t *testing.T) {
			home := t.TempDir()
			artifacts := t.TempDir()
			stageSkillTree(t, artifacts, "payloads/my-skill")

			r, all := applySkill(home, artifacts, tc.agent, "my-skill", "payloads/my-skill")
			if len(all) != 1 {
				t.Fatalf("expected 1 report, got %d", len(all))
			}
			if r.Status != agentinstall.StatusApplied {
				t.Errorf("status: got %q want %q (reason: %q)", r.Status, agentinstall.StatusApplied, r.Reason)
			}
			if r.Agent != tc.agent {
				t.Errorf("agent: got %q want %q", r.Agent, tc.agent)
			}
			if r.Kind != agentinstall.KindSkill {
				t.Errorf("kind: got %q want %q", r.Kind, agentinstall.KindSkill)
			}
			if r.Name != "my-skill" {
				t.Errorf("name: got %q want %q", r.Name, "my-skill")
			}

			// Verify SKILL.md content
			destSkillMD := filepath.Join(home, tc.skillSubDir, "my-skill", "SKILL.md")
			got, err := os.ReadFile(destSkillMD)
			if err != nil {
				t.Fatalf("read SKILL.md: %v", err)
			}
			if string(got) != "# skill\n" {
				t.Errorf("SKILL.md content: got %q want %q", string(got), "# skill\n")
			}

			// Verify nested file
			nestedPath := filepath.Join(home, tc.skillSubDir, "my-skill", "sub", "nested.txt")
			nestedGot, err := os.ReadFile(nestedPath)
			if err != nil {
				t.Fatalf("read nested.txt: %v", err)
			}
			if string(nestedGot) != "nested\n" {
				t.Errorf("nested.txt content: got %q want %q", string(nestedGot), "nested\n")
			}
		})
	}
}

func TestInstallSkillModePreservation(t *testing.T) {
	tests := []struct {
		agent string
	}{
		{"claude"},
		{"codex"},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.agent, func(t *testing.T) {
			home := t.TempDir()
			artifacts := t.TempDir()
			stageSkillTree(t, artifacts, "payloads/my-skill")

			r, _ := applySkill(home, artifacts, tc.agent, "my-skill", "payloads/my-skill")
			if r.Status != agentinstall.StatusApplied {
				t.Fatalf("expected applied, got %q (reason: %q)", r.Status, r.Reason)
			}

			env := agentinstall.MapEnviron{"HOME": home}
			reg := agentinstall.NewRegistry(env)
			e, ok := reg.Lookup(tc.agent)
			if !ok {
				t.Fatalf("agent %q not in registry", tc.agent)
			}
			lay := e.Layout()

			// exec.sh must be 0o755
			execPath := filepath.Join(lay.SkillPath, "my-skill", "exec.sh")
			info, err := os.Stat(execPath)
			if err != nil {
				t.Fatalf("stat exec.sh: %v", err)
			}
			if info.Mode().Perm() != 0o755 {
				t.Errorf("exec.sh perm: got %o want %o", info.Mode().Perm(), 0o755)
			}

			// SKILL.md must be 0o644
			mdPath := filepath.Join(lay.SkillPath, "my-skill", "SKILL.md")
			info2, err := os.Stat(mdPath)
			if err != nil {
				t.Fatalf("stat SKILL.md: %v", err)
			}
			if info2.Mode().Perm() != 0o644 {
				t.Errorf("SKILL.md perm: got %o want %o", info2.Mode().Perm(), 0o644)
			}
		})
	}
}

func TestInstallSkillIdempotentUpsert(t *testing.T) {
	tests := []struct {
		agent string
	}{
		{"claude"},
		{"codex"},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.agent, func(t *testing.T) {
			home := t.TempDir()
			artifacts := t.TempDir()
			stageSkillTree(t, artifacts, "payloads/my-skill")

			// First apply
			r1, _ := applySkill(home, artifacts, tc.agent, "my-skill", "payloads/my-skill")
			if r1.Status != agentinstall.StatusApplied {
				t.Fatalf("first apply: expected applied, got %q (reason: %q)", r1.Status, r1.Reason)
			}

			// Plant a stale file that should be gone after second apply (full tree replace)
			env := agentinstall.MapEnviron{"HOME": home}
			reg := agentinstall.NewRegistry(env)
			e, ok := reg.Lookup(tc.agent)
			if !ok {
				t.Fatalf("agent %q not in registry", tc.agent)
			}
			lay := e.Layout()
			stalePath := filepath.Join(lay.SkillPath, "my-skill", "stale-file.txt")
			if err := os.WriteFile(stalePath, []byte("stale"), 0o644); err != nil {
				t.Fatal(err)
			}

			// Second apply with updated SKILL.md content
			updatedSkillDir := filepath.Join(artifacts, "payloads/my-skill-v2")
			if err := os.MkdirAll(updatedSkillDir, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(updatedSkillDir, "SKILL.md"), []byte("# skill v2\n"), 0o644); err != nil {
				t.Fatal(err)
			}

			r2, _ := applySkill(home, artifacts, tc.agent, "my-skill", "payloads/my-skill-v2")
			if r2.Status != agentinstall.StatusApplied {
				t.Fatalf("second apply: expected applied, got %q (reason: %q)", r2.Status, r2.Reason)
			}

			// Stale file must be gone
			if _, err := os.Stat(stalePath); !os.IsNotExist(err) {
				t.Error("stale-file.txt should have been removed by upsert")
			}

			// Content from second apply
			got, err := os.ReadFile(filepath.Join(lay.SkillPath, "my-skill", "SKILL.md"))
			if err != nil {
				t.Fatalf("read SKILL.md after second apply: %v", err)
			}
			if string(got) != "# skill v2\n" {
				t.Errorf("SKILL.md after upsert: got %q want %q", string(got), "# skill v2\n")
			}
		})
	}
}

func TestInstallSkillPathConfinement(t *testing.T) {
	badNames := []string{
		"../evil",
		"../",
		"sub/dir",
		"",
		".",
		"..",
	}

	for _, tc := range []string{"claude", "codex"} {
		for _, name := range badNames {
			tc, name := tc, name
			t.Run(tc+"/"+name, func(t *testing.T) {
				home := t.TempDir()
				artifacts := t.TempDir()
				stageSkillTree(t, artifacts, "payloads/skill")

				env := agentinstall.MapEnviron{"HOME": home}
				reg := agentinstall.NewRegistry(env)
				m := agentinstall.Manifest{
					Artifacts: []agentinstall.Artifact{
						{
							Kind:    agentinstall.KindSkill,
							Name:    name,
							Targets: []string{tc},
							Skill:   &agentinstall.SkillPayload{Dir: "payloads/skill"},
						},
					},
				}
				opts := agentinstall.Options{
					HomeDir:      home,
					ArtifactsDir: artifacts,
				}
				result := agentinstall.Apply(reg, m, opts, env)
				if len(result.Reports) != 1 {
					t.Fatalf("expected 1 report, got %d", len(result.Reports))
				}
				r := result.Reports[0]
				if r.Status != agentinstall.StatusSkipped && r.Status != agentinstall.StatusFailed {
					t.Errorf("name=%q: expected skipped or failed, got %q (reason: %q)", name, r.Status, r.Reason)
				}
				if r.Reason == "" {
					t.Errorf("name=%q: expected non-empty reason", name)
				}
			})
		}
	}
}

func TestInstallSkillMissingSkillMD(t *testing.T) {
	for _, agent := range []string{"claude", "codex"} {
		agent := agent
		t.Run(agent, func(t *testing.T) {
			home := t.TempDir()
			artifacts := t.TempDir()
			// Stage a dir without SKILL.md
			noMDDir := filepath.Join(artifacts, "payloads", "no-skill-md")
			if err := os.MkdirAll(noMDDir, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(noMDDir, "other.txt"), []byte("hi"), 0o644); err != nil {
				t.Fatal(err)
			}

			r, _ := applySkill(home, artifacts, agent, "no-skill-md", "payloads/no-skill-md")
			if r.Status != agentinstall.StatusSkipped && r.Status != agentinstall.StatusFailed {
				t.Errorf("expected skipped or failed, got %q (reason: %q)", r.Status, r.Reason)
			}
			if r.Reason == "" {
				t.Errorf("expected non-empty reason for missing SKILL.md")
			}
		})
	}
}

func TestInstallSkillMissingSourceDir(t *testing.T) {
	for _, agent := range []string{"claude", "codex"} {
		agent := agent
		t.Run(agent, func(t *testing.T) {
			home := t.TempDir()
			artifacts := t.TempDir()
			// Point to a non-existent dir
			r, _ := applySkill(home, artifacts, agent, "ghost-skill", "payloads/does-not-exist")
			if r.Status != agentinstall.StatusSkipped && r.Status != agentinstall.StatusFailed {
				t.Errorf("expected skipped or failed, got %q (reason: %q)", r.Status, r.Reason)
			}
			if r.Reason == "" {
				t.Errorf("expected non-empty reason for missing source dir")
			}
		})
	}
}

func TestInstallSkillAbsoluteSourceDir(t *testing.T) {
	// Verify that an absolute Skill.Dir works (bypasses ArtifactsDir resolution).
	home := t.TempDir()
	artifacts := t.TempDir()
	// Stage at an absolute path elsewhere
	absSkillDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(absSkillDir, "SKILL.md"), []byte("# abs\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	env := agentinstall.MapEnviron{"HOME": home}
	reg := agentinstall.NewRegistry(env)
	m := agentinstall.Manifest{
		Artifacts: []agentinstall.Artifact{
			{
				Kind:    agentinstall.KindSkill,
				Name:    "abs-skill",
				Targets: []string{"claude"},
				Skill:   &agentinstall.SkillPayload{Dir: absSkillDir},
			},
		},
	}
	opts := agentinstall.Options{
		HomeDir:      home,
		ArtifactsDir: artifacts, // relative dirs would resolve here, but Dir is absolute
	}
	result := agentinstall.Apply(reg, m, opts, env)
	if len(result.Reports) != 1 {
		t.Fatalf("expected 1 report, got %d", len(result.Reports))
	}
	r := result.Reports[0]
	if r.Status != agentinstall.StatusApplied {
		t.Errorf("expected applied, got %q (reason: %q)", r.Status, r.Reason)
	}

	dest := filepath.Join(home, ".claude", "skills", "abs-skill", "SKILL.md")
	got, err := os.ReadFile(dest)
	if err != nil {
		t.Fatalf("read dest SKILL.md: %v", err)
	}
	if string(got) != "# abs\n" {
		t.Errorf("content: got %q want %q", string(got), "# abs\n")
	}
}

func TestInstallSkillNoOpAgentsUnchanged(t *testing.T) {
	// opencode and goose must remain permanent no-ops; guard against regression.
	home := t.TempDir()
	artifacts := t.TempDir()
	stageSkillTree(t, artifacts, "payloads/my-skill")

	tests := []struct {
		agent  string
		reason string
	}{
		{"opencode", "opencode skills layout unconfirmed (S6)"},
		{"goose", "deferred"},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.agent, func(t *testing.T) {
			r, all := applySkill(home, artifacts, tc.agent, "my-skill", "payloads/my-skill")
			if len(all) != 1 {
				t.Fatalf("expected 1 report, got %d", len(all))
			}
			if r.Status != agentinstall.StatusSkipped {
				t.Errorf("expected skipped, got %q (reason: %q)", r.Status, r.Reason)
			}
			if r.Reason != tc.reason {
				t.Errorf("reason: got %q want %q", r.Reason, tc.reason)
			}
		})
	}
}
