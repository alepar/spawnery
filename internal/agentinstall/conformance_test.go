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

// allAgents is the canonical set of registered emitters exercised by the matrix.
var allAgents = []string{"claude", "codex", "opencode", "hermes", "goose"}

// allKinds is the set of artifact kinds the conformance matrix exercises.
var allKinds = []agentinstall.Kind{
	agentinstall.KindSkill,
	agentinstall.KindMCP,
	agentinstall.KindConfig,
}

// expectApplied[agent][kind] is true when the (agent×kind) cell is expected to
// produce StatusApplied; false means StatusSkipped (no-op / deferred).
var expectApplied = map[string]map[agentinstall.Kind]bool{
	"claude":   {agentinstall.KindSkill: true, agentinstall.KindMCP: true, agentinstall.KindConfig: true},
	"codex":    {agentinstall.KindSkill: true, agentinstall.KindMCP: true, agentinstall.KindConfig: true},
	"opencode": {agentinstall.KindSkill: false, agentinstall.KindMCP: true, agentinstall.KindConfig: true},
	"hermes":   {agentinstall.KindSkill: false, agentinstall.KindMCP: false, agentinstall.KindConfig: false},
	"goose":    {agentinstall.KindSkill: false, agentinstall.KindMCP: false, agentinstall.KindConfig: false},
}

const confName = "ctx7"

// confArtifact builds the canonical artifact for a (kind, agent) conformance cell.
// Config uses a per-agent native passthrough fragment so it applies uniformly
// across claude (JSON), codex (TOML), and opencode (JSONC) without relying on a
// normalized key that is lossy for some agents.
func confArtifact(kind agentinstall.Kind, agent string) agentinstall.Artifact {
	switch kind {
	case agentinstall.KindSkill:
		return agentinstall.Artifact{
			Kind:    kind,
			Name:    confName,
			Targets: []string{agent},
			Skill:   &agentinstall.SkillPayload{Dir: "skillsrc"},
		}
	case agentinstall.KindMCP:
		return agentinstall.Artifact{
			Kind:    kind,
			Name:    confName,
			Targets: []string{agent},
			MCP: &agentinstall.MCPPayload{
				Stdio: &agentinstall.MCPTransportStdio{Command: "npx", Args: []string{"-y", "ctx7"}},
			},
		}
	case agentinstall.KindConfig:
		return agentinstall.Artifact{
			Kind:    kind,
			Name:    "cfg",
			Targets: []string{agent},
			Config: &agentinstall.ConfigPayload{
				Native: map[string]interface{}{
					agent: map[string]interface{}{"customKey": "customValue"},
				},
			},
		}
	}
	return agentinstall.Artifact{}
}

// confApply applies a single artifact through the public engine and returns the report.
func confApply(t *testing.T, home, artifactsDir string, a agentinstall.Artifact) agentinstall.Report {
	t.Helper()
	env := agentinstall.MapEnviron{"HOME": home}
	reg := agentinstall.NewRegistry(env)
	opts := agentinstall.Options{HomeDir: home, ArtifactsDir: artifactsDir}
	res := agentinstall.Apply(reg, agentinstall.Manifest{Artifacts: []agentinstall.Artifact{a}}, opts, env)
	if len(res.Reports) != 1 {
		t.Fatalf("want 1 report, got %d", len(res.Reports))
	}
	return res.Reports[0]
}

// confLayout returns the resolved AgentLayout for an agent under the given home.
func confLayout(t *testing.T, home, agent string) agentinstall.AgentLayout {
	t.Helper()
	env := agentinstall.MapEnviron{"HOME": home}
	reg := agentinstall.NewRegistry(env)
	e, ok := reg.Lookup(agent)
	if !ok {
		t.Fatalf("agent %q not in registry", agent)
	}
	return e.Layout()
}

// parseBack reads path and parses it according to the agent's declared format.
func parseBack(t *testing.T, format agentinstall.Format, path string) map[string]interface{} {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var root map[string]interface{}
	switch format {
	case agentinstall.FormatJSON:
		if err := json.Unmarshal(data, &root); err != nil {
			t.Fatalf("parse JSON %s: %v", path, err)
		}
	case agentinstall.FormatJSONC:
		std, err := hujson.Standardize(data)
		if err != nil {
			t.Fatalf("standardize JSONC %s: %v", path, err)
		}
		if err := json.Unmarshal(std, &root); err != nil {
			t.Fatalf("parse JSONC %s: %v", path, err)
		}
	case agentinstall.FormatTOML:
		if err := toml.Unmarshal(data, &root); err != nil {
			t.Fatalf("parse TOML %s: %v", path, err)
		}
	default:
		t.Fatalf("unsupported parse-back format %q", format)
	}
	return root
}

// mcpKeyFor returns the native top-level key holding MCP servers for an agent.
func mcpKeyFor(t *testing.T, agent string) string {
	t.Helper()
	switch agent {
	case "claude":
		return "mcpServers"
	case "codex":
		return "mcp_servers"
	case "opencode":
		return "mcp"
	default:
		t.Fatalf("no mcp key for agent %q", agent)
		return ""
	}
}

// mcpServers extracts the server map for an agent from a parsed config root.
func mcpServers(t *testing.T, agent string, root map[string]interface{}) map[string]interface{} {
	t.Helper()
	key := mcpKeyFor(t, agent)
	servers, ok := root[key].(map[string]interface{})
	if !ok {
		t.Fatalf("%s missing or wrong type: %T", key, root[key])
	}
	return servers
}

// TestConformance_PathFormatPresence asserts every applied cell lands at its
// Layout path, parses back in its Layout format, and contains the upserted entry.
// Skipped cells must report StatusSkipped.
func TestConformance_PathFormatPresence(t *testing.T) {
	for _, agent := range allAgents {
		for _, kind := range allKinds {
			agent, kind := agent, kind
			t.Run(agent+"/"+string(kind), func(t *testing.T) {
				home := t.TempDir()
				artifactsDir := t.TempDir()
				if kind == agentinstall.KindSkill {
					stageSkillTree(t, artifactsDir, "skillsrc")
				}
				r := confApply(t, home, artifactsDir, confArtifact(kind, agent))

				if !expectApplied[agent][kind] {
					if r.Status != agentinstall.StatusSkipped {
						t.Fatalf("status: got %q want skipped (reason %q)", r.Status, r.Reason)
					}
					return
				}
				if r.Status != agentinstall.StatusApplied {
					t.Fatalf("status: got %q want applied (reason %q)", r.Status, r.Reason)
				}

				layout := confLayout(t, home, agent)
				switch kind {
				case agentinstall.KindSkill:
					dest := filepath.Join(layout.SkillPath, confName)
					if _, err := os.Stat(filepath.Join(dest, "SKILL.md")); err != nil {
						t.Fatalf("skill SKILL.md not at %s: %v", dest, err)
					}
				case agentinstall.KindMCP:
					root := parseBack(t, layout.MCPFormat, layout.MCPPath)
					servers := mcpServers(t, agent, root)
					if _, ok := servers[confName].(map[string]interface{}); !ok {
						t.Fatalf("server %q missing under %s", confName, mcpKeyFor(t, agent))
					}
				case agentinstall.KindConfig:
					root := parseBack(t, layout.ConfigFormat, layout.ConfigPath)
					if root["customKey"] != "customValue" {
						t.Fatalf("config customKey: got %v want customValue", root["customKey"])
					}
				}
			})
		}
	}
}

// TestConformance_Idempotency asserts a second apply produces no duplication:
// byte-identical config/mcp files and a single skill directory.
func TestConformance_Idempotency(t *testing.T) {
	for _, agent := range allAgents {
		for _, kind := range allKinds {
			if !expectApplied[agent][kind] {
				continue
			}
			agent, kind := agent, kind
			t.Run(agent+"/"+string(kind), func(t *testing.T) {
				home := t.TempDir()
				artifactsDir := t.TempDir()
				if kind == agentinstall.KindSkill {
					stageSkillTree(t, artifactsDir, "skillsrc")
				}
				a := confArtifact(kind, agent)

				if r := confApply(t, home, artifactsDir, a); r.Status != agentinstall.StatusApplied {
					t.Fatalf("first apply: got %q want applied (reason %q)", r.Status, r.Reason)
				}

				layout := confLayout(t, home, agent)
				if kind == agentinstall.KindSkill {
					if r := confApply(t, home, artifactsDir, a); r.Status != agentinstall.StatusApplied {
						t.Fatalf("second apply: got %q want applied (reason %q)", r.Status, r.Reason)
					}
					entries, err := os.ReadDir(layout.SkillPath)
					if err != nil {
						t.Fatalf("read skills dir: %v", err)
					}
					if len(entries) != 1 || entries[0].Name() != confName {
						t.Fatalf("skills dir: got %v want exactly [%s]", names(entries), confName)
					}
					if _, err := os.Stat(filepath.Join(layout.SkillPath, confName, "SKILL.md")); err != nil {
						t.Fatalf("SKILL.md missing after re-apply: %v", err)
					}
					return
				}

				// mcp/config share file machinery: assert byte-stable + single entry.
				path := layout.MCPPath
				format := layout.MCPFormat
				if kind == agentinstall.KindConfig {
					path = layout.ConfigPath
					format = layout.ConfigFormat
				}
				first, err := os.ReadFile(path)
				if err != nil {
					t.Fatalf("read after first apply: %v", err)
				}
				if r := confApply(t, home, artifactsDir, a); r.Status != agentinstall.StatusApplied {
					t.Fatalf("second apply: got %q want applied (reason %q)", r.Status, r.Reason)
				}
				second, err := os.ReadFile(path)
				if err != nil {
					t.Fatalf("read after second apply: %v", err)
				}
				if string(first) != string(second) {
					t.Fatalf("re-apply changed file bytes (not idempotent)\n--- first ---\n%s\n--- second ---\n%s", first, second)
				}
				if kind == agentinstall.KindMCP {
					servers := mcpServers(t, agent, parseBack(t, format, path))
					if len(servers) != 1 {
						t.Fatalf("expected exactly 1 mcp server entry, got %d", len(servers))
					}
				}
			})
		}
	}
}

// TestConformance_LauncherClobberSurvival pre-seeds a launcher-managed base file
// (or a sibling skill / a JSONC comment), applies the artifact, and asserts the
// base content survives AND the artifact lands [J31].
func TestConformance_LauncherClobberSurvival(t *testing.T) {
	type cell struct {
		agent string
		kind  agentinstall.Kind
	}
	cells := []cell{
		{"claude", agentinstall.KindSkill}, {"claude", agentinstall.KindMCP}, {"claude", agentinstall.KindConfig},
		{"codex", agentinstall.KindSkill}, {"codex", agentinstall.KindMCP}, {"codex", agentinstall.KindConfig},
		{"opencode", agentinstall.KindMCP}, {"opencode", agentinstall.KindConfig},
	}
	for _, c := range cells {
		c := c
		t.Run(c.agent+"/"+string(c.kind), func(t *testing.T) {
			home := t.TempDir()
			artifactsDir := t.TempDir()
			layout := confLayout(t, home, c.agent)
			seedClobberBase(t, layout, c.agent, c.kind)
			if c.kind == agentinstall.KindSkill {
				stageSkillTree(t, artifactsDir, "skillsrc")
			}
			if r := confApply(t, home, artifactsDir, confArtifact(c.kind, c.agent)); r.Status != agentinstall.StatusApplied {
				t.Fatalf("status: got %q want applied (reason %q)", r.Status, r.Reason)
			}
			assertClobberSurvival(t, layout, c.agent, c.kind)
		})
	}
}

// seedClobberBase writes a format-appropriate base file (with a launcher-managed
// sentinel) or a sibling skill before the artifact is applied.
func seedClobberBase(t *testing.T, layout agentinstall.AgentLayout, agent string, kind agentinstall.Kind) {
	t.Helper()
	switch kind {
	case agentinstall.KindSkill:
		other := filepath.Join(layout.SkillPath, "other")
		if err := os.MkdirAll(other, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(other, "SKILL.md"), []byte("# other\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	case agentinstall.KindMCP:
		writeBaseFile(t, layout.MCPPath, layout.MCPFormat)
	case agentinstall.KindConfig:
		writeBaseFile(t, layout.ConfigPath, layout.ConfigFormat)
	}
}

// writeBaseFile writes a base config file containing a "launcherKey" sentinel
// (plus model for TOML), in the declared format. JSONC also carries a comment.
func writeBaseFile(t *testing.T, path string, format agentinstall.Format) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	var content string
	switch format {
	case agentinstall.FormatJSON:
		content = "{\n  \"launcherKey\": \"keep\"\n}\n"
	case agentinstall.FormatTOML:
		content = "model = \"m\"\nlauncherKey = \"keep\"\n"
	case agentinstall.FormatJSONC:
		content = "{\n  // launcher comment\n  \"launcherKey\": \"keep\"\n}\n"
	default:
		t.Fatalf("writeBaseFile: unsupported format %q", format)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// assertClobberSurvival verifies the base content + the new artifact coexist.
func assertClobberSurvival(t *testing.T, layout agentinstall.AgentLayout, agent string, kind agentinstall.Kind) {
	t.Helper()
	switch kind {
	case agentinstall.KindSkill:
		if _, err := os.Stat(filepath.Join(layout.SkillPath, "other", "SKILL.md")); err != nil {
			t.Fatalf("sibling skill 'other' was clobbered: %v", err)
		}
		if _, err := os.Stat(filepath.Join(layout.SkillPath, confName, "SKILL.md")); err != nil {
			t.Fatalf("new skill %q not installed: %v", confName, err)
		}
	case agentinstall.KindMCP:
		root := parseBack(t, layout.MCPFormat, layout.MCPPath)
		if root["launcherKey"] != "keep" {
			t.Fatalf("launcherKey clobbered: got %v want keep", root["launcherKey"])
		}
		// TOML base files also seed model="m"; verify it survives.
		if layout.MCPFormat == agentinstall.FormatTOML {
			if root["model"] != "m" {
				t.Fatalf("TOML model key was clobbered after MCP apply: got %v want m", root["model"])
			}
		}
		servers := mcpServers(t, agent, root)
		if _, ok := servers[confName].(map[string]interface{}); !ok {
			t.Fatalf("server %q not added", confName)
		}
		if agent == "opencode" {
			assertJSONCCommentSurvived(t, layout.MCPPath)
		}
	case agentinstall.KindConfig:
		root := parseBack(t, layout.ConfigFormat, layout.ConfigPath)
		if root["launcherKey"] != "keep" {
			t.Fatalf("launcherKey clobbered: got %v want keep", root["launcherKey"])
		}
		// TOML base files also seed model="m"; verify it survives.
		if layout.ConfigFormat == agentinstall.FormatTOML {
			if root["model"] != "m" {
				t.Fatalf("TOML model key was clobbered after Config apply: got %v want m", root["model"])
			}
		}
		if root["customKey"] != "customValue" {
			t.Fatalf("config customKey: got %v want customValue", root["customKey"])
		}
		if agent == "opencode" {
			assertJSONCCommentSurvived(t, layout.ConfigPath)
		}
	}
}

// assertJSONCCommentSurvived confirms the format-preserving JSONC writer kept comments.
func assertJSONCCommentSurvived(t *testing.T, path string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if !strings.Contains(string(data), "launcher comment") {
		t.Fatalf("JSONC comment was lost (not format-preserving):\n%s", data)
	}
}

// TestConformance_SkippedCellsNoWrite asserts skipped cells report StatusSkipped
// and write no native artifact file.
func TestConformance_SkippedCellsNoWrite(t *testing.T) {
	for _, agent := range allAgents {
		for _, kind := range allKinds {
			if expectApplied[agent][kind] {
				continue
			}
			agent, kind := agent, kind
			t.Run(agent+"/"+string(kind), func(t *testing.T) {
				home := t.TempDir()
				artifactsDir := t.TempDir()
				if kind == agentinstall.KindSkill {
					stageSkillTree(t, artifactsDir, "skillsrc")
				}
				r := confApply(t, home, artifactsDir, confArtifact(kind, agent))
				if r.Status != agentinstall.StatusSkipped {
					t.Fatalf("status: got %q want skipped (reason %q)", r.Status, r.Reason)
				}
				if r.Reason == "" {
					t.Fatalf("skipped cell must carry a non-empty reason")
				}

				layout := confLayout(t, home, agent)
				switch kind {
				case agentinstall.KindSkill:
					if layout.SkillPath != "" {
						if _, err := os.Stat(filepath.Join(layout.SkillPath, confName)); !os.IsNotExist(err) {
							t.Fatalf("skipped skill must not be written at %s/%s", layout.SkillPath, confName)
						}
					}
				case agentinstall.KindMCP:
					if _, err := os.Stat(layout.MCPPath); !os.IsNotExist(err) {
						t.Fatalf("skipped mcp must not write %s", layout.MCPPath)
					}
				case agentinstall.KindConfig:
					if _, err := os.Stat(layout.ConfigPath); !os.IsNotExist(err) {
						t.Fatalf("skipped config must not write %s", layout.ConfigPath)
					}
				}
			})
		}
	}
}

// names returns the entry names for a dir listing (test diagnostics helper).
func names(entries []os.DirEntry) []string {
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		out = append(out, e.Name())
	}
	return out
}
