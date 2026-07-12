package agentinstall

import "path/filepath"

// hermesEmitter handles artifact installation for the hermes agent.
// MCP and config are deferred to sp-mofj. Skill install is implemented: canonical-only,
// same as opencode/goose.
type hermesEmitter struct {
	baseEmitter
}

func newHermesEmitter(homeDir string) hermesEmitter {
	configRoot := filepath.Join(homeDir, ".hermes")
	return hermesEmitter{
		baseEmitter: baseEmitter{
			layout: AgentLayout{
				Name:       "hermes",
				ConfigRoot: configRoot,
				// SkillPath is blank: ~/.agents/skills is the canonical dir, implicit for
				// every agent (CanonicalSkillsDir), not a hermes-specific native copy target.
				// hermes additionally needs a `skills.external_dirs` upsert in config.yaml to
				// read it — sp-mwco.2.5's glue, spike-confirmed required and sufficient.
				SkillPath:     "",
				MCPPath:       filepath.Join(configRoot, "config.yaml"),
				MCPFormat:     FormatYAML,
				ConfigPath:    filepath.Join(configRoot, "config.yaml"),
				ConfigFormat:  FormatYAML,
				SchemaVersion: "hermes-1.0",
			},
		},
	}
}

const hermesReason = "deferred to sp-mofj"

// InstallSkill installs a skill into the canonical ~/.agents/skills/<name>/ dir only.
// hermes additionally needs the skills.external_dirs config.yaml upsert to read it —
// sp-mwco.2.5.
func (e hermesEmitter) InstallSkill(a Artifact, opts Options) Report {
	return installSkill(e.layout, a, opts)
}

// InstallMCP is deferred to sp-mofj.
func (e hermesEmitter) InstallMCP(a Artifact, _ Options) Report {
	return Report{
		Agent:  e.layout.Name,
		Kind:   KindMCP,
		Name:   a.Name,
		Status: StatusSkipped,
		Reason: hermesReason,
	}
}

// ApplyConfig is deferred to sp-mofj.
func (e hermesEmitter) ApplyConfig(a Artifact, _ Options) Report {
	return Report{
		Agent:  e.layout.Name,
		Kind:   KindConfig,
		Name:   a.Name,
		Status: StatusSkipped,
		Reason: hermesReason,
	}
}
