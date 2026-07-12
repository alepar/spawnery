package agentinstall_test

import (
	"os"
	"path/filepath"
	"testing"

	"spawnery/internal/agentinstall"
)

func TestPiRegisteredInRegistry(t *testing.T) {
	env := agentinstall.MapEnviron{"HOME": "/h"}
	reg := agentinstall.NewRegistry(env)

	names := reg.Names()
	want := []string{"claude", "codex", "opencode", "hermes", "goose", "pi"}
	if len(names) != len(want) {
		t.Fatalf("Names() = %v, want %v", names, want)
	}
	for i := range want {
		if names[i] != want[i] {
			t.Errorf("names[%d] = %q, want %q", i, names[i], want[i])
		}
	}

	e, ok := reg.Lookup("pi")
	if !ok {
		t.Fatal("Lookup(pi) not found")
	}
	layout := e.Layout()
	want2 := agentinstall.AgentLayout{
		Name:       "pi",
		ConfigRoot: filepath.Join("/h", ".pi"),
		SkillPath:  "",
		MCPPath:    "",
		ConfigPath: "",
	}
	if layout.Name != want2.Name {
		t.Errorf("Name: got %q want %q", layout.Name, want2.Name)
	}
	if layout.ConfigRoot != want2.ConfigRoot {
		t.Errorf("ConfigRoot: got %q want %q", layout.ConfigRoot, want2.ConfigRoot)
	}
	if layout.SkillPath != want2.SkillPath {
		t.Errorf("SkillPath: got %q want %q (vestigial paths must stay blank per §4.3)", layout.SkillPath, want2.SkillPath)
	}
	if layout.MCPPath != want2.MCPPath {
		t.Errorf("MCPPath: got %q want %q", layout.MCPPath, want2.MCPPath)
	}
	if layout.ConfigPath != want2.ConfigPath {
		t.Errorf("ConfigPath: got %q want %q", layout.ConfigPath, want2.ConfigPath)
	}
}

func TestPiInstallSkillCanonicalOnly(t *testing.T) {
	home := t.TempDir()
	artifactsDir := t.TempDir()
	stageSkillTree(t, artifactsDir, "payloads/my-skill")

	r, all := applySkill(home, artifactsDir, "pi", "my-skill", "payloads/my-skill")
	if len(all) != 1 {
		t.Fatalf("expected 1 report, got %d", len(all))
	}
	if r.Status != agentinstall.StatusApplied {
		t.Fatalf("status: got %q want applied (reason %q)", r.Status, r.Reason)
	}

	canonicalDest := filepath.Join(agentinstall.CanonicalSkillsDir(home), "my-skill")
	if _, err := os.Stat(filepath.Join(canonicalDest, "SKILL.md")); err != nil {
		t.Fatalf("canonical SKILL.md not installed: %v", err)
	}

	// Must NOT land anywhere under ~/.pi.
	piRoot := filepath.Join(home, ".pi")
	if _, err := os.Stat(piRoot); err == nil {
		t.Errorf("skill install must not create anything under %s", piRoot)
	} else if !os.IsNotExist(err) {
		t.Fatalf("stat %s: %v", piRoot, err)
	}
}

func TestPiInstallSkillIdempotent(t *testing.T) {
	home := t.TempDir()
	artifactsDir := t.TempDir()
	stageSkillTree(t, artifactsDir, "payloads/my-skill")

	r1, _ := applySkill(home, artifactsDir, "pi", "my-skill", "payloads/my-skill")
	if r1.Status != agentinstall.StatusApplied {
		t.Fatalf("first apply: got %q want applied (reason %q)", r1.Status, r1.Reason)
	}
	r2, _ := applySkill(home, artifactsDir, "pi", "my-skill", "payloads/my-skill")
	if r2.Status != agentinstall.StatusApplied {
		t.Fatalf("second apply: got %q want applied (reason %q)", r2.Status, r2.Reason)
	}

	canonicalDir := agentinstall.CanonicalSkillsDir(home)
	entries, err := os.ReadDir(canonicalDir)
	if err != nil {
		t.Fatalf("read canonical dir: %v", err)
	}
	if len(entries) != 1 || entries[0].Name() != "my-skill" {
		t.Fatalf("canonical dir: got %v want exactly [my-skill]", entries)
	}
}

func TestPiCapabilities(t *testing.T) {
	env := agentinstall.MapEnviron{"HOME": "/h"}
	reg := agentinstall.NewRegistry(env)
	e, ok := reg.Lookup("pi")
	if !ok {
		t.Fatal("Lookup(pi) not found")
	}
	caps := e.Capabilities()
	if caps[agentinstall.KindSkill] != agentinstall.CapStatusSupported {
		t.Errorf("pi skill capability: got %q want supported", caps[agentinstall.KindSkill])
	}
	for _, k := range []agentinstall.Kind{agentinstall.KindMCP, agentinstall.KindConfig, agentinstall.KindPlugin, agentinstall.Kind("instructions")} {
		if caps[k] != agentinstall.CapStatusNoOp {
			t.Errorf("pi %s capability: got %q want no-op", k, caps[k])
		}
	}
}
