package agentinstall_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"spawnery/internal/agentinstall"
)

func TestInstructionsPathByAgent(t *testing.T) {
	env := agentinstall.MapEnviron{"HOME": "/h"}
	reg := agentinstall.NewRegistry(env)
	cases := map[string]string{
		"claude":   "/h/.claude/profile-instructions.md",
		"codex":    "/h/.codex/profile-instructions.md",
		"opencode": "/h/.config/opencode/profile-instructions.md",
		"hermes":   "",
		"goose":    "",
	}
	for name, want := range cases {
		e, _ := reg.Lookup(name)
		if got := e.Layout().InstructionsPath; got != want {
			t.Errorf("%s InstructionsPath = %q want %q", name, got, want)
		}
	}
}

func TestClaudeInstructionsWritesManagedFile(t *testing.T) {
	home := t.TempDir()
	env := agentinstall.MapEnviron{"HOME": home}
	if err := os.MkdirAll(filepath.Join(home, ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}
	m := agentinstall.Manifest{Artifacts: []agentinstall.Artifact{{
		Kind: agentinstall.KindConfig, Name: "instr", Targets: []string{"claude"},
		Config: &agentinstall.ConfigPayload{Instructions: "Be terse."}}}}
	res := agentinstall.Apply(agentinstall.NewRegistry(env), m, agentinstall.Options{HomeDir: home}, env)
	if res.Reports[0].Status != agentinstall.StatusApplied {
		t.Fatalf("status=%s reason=%s", res.Reports[0].Status, res.Reports[0].Reason)
	}
	got, err := os.ReadFile(filepath.Join(home, ".claude", "profile-instructions.md"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "Be terse." {
		t.Fatalf("content=%q", got)
	}
	// CLAUDE.md memory must NOT be touched.
	if _, err := os.Stat(filepath.Join(home, ".claude", "CLAUDE.md")); !os.IsNotExist(err) {
		t.Fatal("CLAUDE.md must not be created")
	}
}

func TestInstructionsReplaceOnApply(t *testing.T) {
	home := t.TempDir()
	env := agentinstall.MapEnviron{"HOME": home}
	os.MkdirAll(filepath.Join(home, ".claude"), 0o755)
	mk := func(s string) agentinstall.Manifest {
		return agentinstall.Manifest{Artifacts: []agentinstall.Artifact{{
			Kind: agentinstall.KindConfig, Name: "instr", Targets: []string{"claude"},
			Config: &agentinstall.ConfigPayload{Instructions: s}}}}
	}
	reg := agentinstall.NewRegistry(env)
	agentinstall.Apply(reg, mk("V1"), agentinstall.Options{HomeDir: home}, env)
	agentinstall.Apply(reg, mk("V2"), agentinstall.Options{HomeDir: home}, env)
	got, _ := os.ReadFile(filepath.Join(home, ".claude", "profile-instructions.md"))
	if string(got) != "V2" {
		t.Fatalf("expected replace-on-apply, got %q", got)
	}
}

func TestOpencodeInstructionsReferencedInConfig(t *testing.T) {
	home := t.TempDir()
	env := agentinstall.MapEnviron{"HOME": home}
	ocDir := filepath.Join(home, ".config", "opencode")
	os.MkdirAll(ocDir, 0o755)
	m := agentinstall.Manifest{Artifacts: []agentinstall.Artifact{{
		Kind: agentinstall.KindConfig, Name: "instr", Targets: []string{"opencode"},
		Config: &agentinstall.ConfigPayload{Instructions: "Hello."}}}}
	res := agentinstall.Apply(agentinstall.NewRegistry(env), m, agentinstall.Options{HomeDir: home}, env)
	if res.Reports[0].Status != agentinstall.StatusApplied {
		t.Fatalf("status=%s", res.Reports[0].Status)
	}
	raw, _ := os.ReadFile(filepath.Join(ocDir, "opencode.json"))
	var cfg map[string]interface{}
	json.Unmarshal(raw, &cfg) //nolint:errcheck
	arr, _ := cfg["instructions"].([]interface{})
	want := filepath.Join(ocDir, "profile-instructions.md")
	found := false
	for _, v := range arr {
		if v == want {
			found = true
		}
	}
	if !found {
		t.Fatalf("instructions array missing %q: %v", want, arr)
	}
}

func TestInstructionsNoopForHermesGoose(t *testing.T) {
	home := t.TempDir()
	env := agentinstall.MapEnviron{"HOME": home}
	os.MkdirAll(filepath.Join(home, ".hermes"), 0o755)
	m := agentinstall.Manifest{Artifacts: []agentinstall.Artifact{{
		Kind: agentinstall.KindConfig, Name: "instr", Targets: []string{"hermes"},
		Config: &agentinstall.ConfigPayload{Instructions: "X."}}}}
	res := agentinstall.Apply(agentinstall.NewRegistry(env), m, agentinstall.Options{HomeDir: home}, env)
	if res.Reports[0].Status != agentinstall.StatusSkipped {
		t.Fatalf("hermes instructions should be skipped, got %s", res.Reports[0].Status)
	}
}
