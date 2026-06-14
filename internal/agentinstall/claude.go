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
				ForbiddenConfigKeys: []string{"model"},
			},
		},
	}
}

// InstallSkill, InstallMCP, ApplyConfig are all base placeholders (sp-w5aa/cywj/g5x8 fill).
