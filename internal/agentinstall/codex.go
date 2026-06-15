package agentinstall

import "path/filepath"

// codexEmitter handles artifact installation for the codex agent.
type codexEmitter struct {
	baseEmitter
}

// newCodexEmitter creates a codex emitter. codexHome is resolved from $CODEX_HOME if set,
// else defaults to ~/.codex. The caller is responsible for environment resolution.
func newCodexEmitter(codexHome string) codexEmitter {
	return codexEmitter{
		baseEmitter: baseEmitter{
			layout: AgentLayout{
				Name:                "codex",
				ConfigRoot:          codexHome,
				SkillPath:           filepath.Join(codexHome, "skills"),
				MCPPath:             filepath.Join(codexHome, "config.toml"),
				MCPFormat:           FormatTOML,
				ConfigPath:          filepath.Join(codexHome, "config.toml"),
				ConfigFormat:        FormatTOML,
				SchemaVersion:       "codex-0.139",
				ForbiddenConfigKeys: []string{"model", "approval_policy", "sandbox_mode", "sandbox_workspace_write"},
				RulesDir:            filepath.Join(codexHome, "rules"),
				InstructionsPath:    filepath.Join(codexHome, "profile-instructions.md"),
			},
		},
	}
}

// InstallSkill installs a skill directory tree into <codexHome>/skills/<name>/.
func (e codexEmitter) InstallSkill(a Artifact, opts Options) Report {
	return installSkillTree(e.layout, a, opts)
}

// InstallMCP is implemented in mcp.go (sp-cywj). ApplyConfig is implemented in config.go (sp-g5x8).
// ForbiddenConfigKeys: ["model","approval_policy","sandbox_mode","sandbox_workspace_write"] — these
// are launcher-managed or security-sensitive; allowedCommands/deniedCommands go to RulesDir instead.
