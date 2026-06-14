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
				Name:          "codex",
				ConfigRoot:    codexHome,
				SkillPath:     filepath.Join(codexHome, "skills"),
				MCPPath:       filepath.Join(codexHome, "config.toml"),
				MCPFormat:     FormatTOML,
				ConfigPath:    filepath.Join(codexHome, "config.toml"),
				ConfigFormat:  FormatTOML,
				SchemaVersion: "codex-0.139",
			},
		},
	}
}

// InstallSkill installs a skill directory tree into <codexHome>/skills/<name>/.
func (e codexEmitter) InstallSkill(a Artifact, opts Options) Report {
	return installSkillTree(e.layout, a, opts)
}

// InstallMCP and ApplyConfig are base placeholders (sp-cywj/g5x8 fill).
