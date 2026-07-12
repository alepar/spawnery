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
				InstructionsPath:    filepath.Join(configRoot, "profile-instructions.md"),
			},
		},
	}
}

// InstallSkill installs a skill into the canonical ~/.agents/skills/<name>/ dir and
// copies it into ~/.claude/skills/<name>/ — a REAL COPY, never a symlink: claude-code
// does not read ~/.agents/skills (#31005), and a symlinked ~/.claude/skills stops
// user-level skills from loading at all (#38051) or fails per-skill-dir with "Unknown
// skill" (#25367).
func (e claudeEmitter) InstallSkill(a Artifact, opts Options) Report {
	return installSkill(e.layout, a, opts)
}

// Capabilities returns the full support matrix for claude: all kinds fully supported.
func (e claudeEmitter) Capabilities() map[Kind]CapabilityStatus {
	return map[Kind]CapabilityStatus{
		KindSkill:            CapStatusSupported,
		KindMCP:              CapStatusSupported,
		KindConfig:           CapStatusSupported,
		KindPlugin:           CapStatusSupported,
		Kind("instructions"): CapStatusSupported,
	}
}

// InstallMCP is implemented in mcp.go (sp-cywj). ApplyConfig is implemented in config.go (sp-g5x8).
// ForbiddenConfigKeys: ["model","permissions"] — model is launcher-regenerated; permissions is
// managed via normalized keys (approvalPosture, disabledTools, allowedCommands, deniedCommands)
// and must not be overridden wholesale via native passthrough.
