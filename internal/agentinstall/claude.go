package agentinstall

import "path/filepath"

// claudeEmitter handles artifact installation for the claude-code agent.
type claudeEmitter struct {
	baseEmitter
}

func newClaudeEmitter(homeDir string) claudeEmitter {
	configRoot := filepath.Join(homeDir, ".claude")
	return claudeEmitter{
		baseEmitter: baseEmitter{
			layout: AgentLayout{
				Name:                "claude",
				ConfigRoot:          configRoot,
				SkillPath:           filepath.Join(configRoot, "skills"),
				MCPPath:             filepath.Join(homeDir, ".claude.json"),
				MCPFormat:           FormatJSON,
				ConfigPath:          filepath.Join(configRoot, "settings.json"),
				ConfigFormat:        FormatJSON,
				SchemaVersion:       "claude-2.1",
				ForbiddenConfigKeys: []string{"model", "permissions"},
			},
		},
	}
}

// InstallSkill installs a skill directory tree into ~/.claude/skills/<name>/.
func (e claudeEmitter) InstallSkill(a Artifact, opts Options) Report {
	return installSkillTree(e.layout, a, opts)
}

// InstallMCP is implemented in mcp.go (sp-cywj). ApplyConfig is implemented in config.go (sp-g5x8).
// ForbiddenConfigKeys: ["model","permissions"] — model is launcher-regenerated; permissions is
// managed via normalized keys (approvalPosture, disabledTools, allowedCommands, deniedCommands)
// and must not be overridden wholesale via native passthrough.
