package agentinstall_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"spawnery/internal/agentinstall"
)

func TestManagedIndexRecordsProvenance(t *testing.T) {
	home := t.TempDir()
	env := agentinstall.MapEnviron{"HOME": home}
	os.MkdirAll(filepath.Join(home, ".claude"), 0o755) //nolint:errcheck
	idxPath := filepath.Join(home, ".spawnery", "managed.json")
	m := agentinstall.Manifest{Artifacts: []agentinstall.Artifact{
		{Kind: agentinstall.KindMCP, Name: "srv", Targets: []string{"claude"},
			MCP: &agentinstall.MCPPayload{Stdio: &agentinstall.MCPTransportStdio{Command: "node"}}},
		{Kind: agentinstall.KindConfig, Name: "instr", Targets: []string{"claude"},
			Config: &agentinstall.ConfigPayload{Instructions: "Be terse."}},
	}}
	opts := agentinstall.Options{
		HomeDir:          home,
		ManagedIndexPath: idxPath,
		ProfileID:        "p1",
		ProfileVersion:   "3",
	}
	agentinstall.Apply(agentinstall.NewRegistry(env), m, opts, env)
	raw, err := os.ReadFile(idxPath)
	if err != nil {
		t.Fatalf("managed.json not written: %v", err)
	}
	var ix agentinstall.ManagedIndex
	json.Unmarshal(raw, &ix) //nolint:errcheck
	byKind := map[string]agentinstall.ManagedEntry{}
	for _, e := range ix.Entries {
		byKind[string(e.Kind)] = e
	}
	if e := byKind["mcp"]; e.NativeKey != "mcpServers.srv" || e.ProfileID != "p1" || e.ProfileVersion != "3" {
		t.Fatalf("mcp entry wrong: %+v", e)
	}
	if e := byKind["instructions"]; e.File != filepath.Join(home, ".claude", "profile-instructions.md") {
		t.Fatalf("instructions entry wrong: %+v", e)
	}
}

func TestManagedIndexUpsertReplaceOnApply(t *testing.T) {
	home := t.TempDir()
	env := agentinstall.MapEnviron{"HOME": home}
	os.MkdirAll(filepath.Join(home, ".claude"), 0o755) //nolint:errcheck
	idxPath := filepath.Join(home, ".spawnery", "managed.json")
	mk := func(v string) agentinstall.Manifest {
		return agentinstall.Manifest{Artifacts: []agentinstall.Artifact{{
			Kind: agentinstall.KindConfig, Name: "instr", Targets: []string{"claude"},
			Config: &agentinstall.ConfigPayload{Instructions: v}}}}
	}
	opts := agentinstall.Options{HomeDir: home, ManagedIndexPath: idxPath, ProfileID: "p1", ProfileVersion: "1"}
	reg := agentinstall.NewRegistry(env)
	agentinstall.Apply(reg, mk("A"), opts, env)
	opts.ProfileVersion = "2"
	agentinstall.Apply(reg, mk("B"), opts, env)
	raw, _ := os.ReadFile(idxPath)
	var ix agentinstall.ManagedIndex
	json.Unmarshal(raw, &ix) //nolint:errcheck
	if len(ix.Entries) != 1 || ix.Entries[0].ProfileVersion != "2" {
		t.Fatalf("expected single upserted entry at v2, got %+v", ix.Entries)
	}
}

func TestManagedIndexNotWrittenWithoutPath(t *testing.T) {
	home := t.TempDir()
	env := agentinstall.MapEnviron{"HOME": home}
	os.MkdirAll(filepath.Join(home, ".claude"), 0o755) //nolint:errcheck
	m := agentinstall.Manifest{Artifacts: []agentinstall.Artifact{{
		Kind: agentinstall.KindConfig, Name: "instr", Targets: []string{"claude"},
		Config: &agentinstall.ConfigPayload{Instructions: "x"}}}}
	agentinstall.Apply(agentinstall.NewRegistry(env), m, agentinstall.Options{HomeDir: home}, env)
	if _, err := os.Stat(filepath.Join(home, ".spawnery", "managed.json")); !os.IsNotExist(err) {
		t.Fatal("managed.json must not be written when ManagedIndexPath is unset")
	}
}
