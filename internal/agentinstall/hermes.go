package agentinstall

import "path/filepath"

// hermesEmitter handles artifact installation for the hermes agent.
// All three methods are deferred to sp-mofj.
type hermesEmitter struct {
	baseEmitter
}

func newHermesEmitter(homeDir string) hermesEmitter {
	configRoot := filepath.Join(homeDir, ".hermes")
	return hermesEmitter{
		baseEmitter: baseEmitter{
			layout: AgentLayout{
				Name:          "hermes",
				ConfigRoot:    configRoot,
				SkillPath:     filepath.Join(homeDir, ".agents", "skills"),
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

// InstallSkill is deferred to sp-mofj.
func (e hermesEmitter) InstallSkill(a Artifact, _ Options) Report {
	return Report{
		Agent:  e.layout.Name,
		Kind:   KindSkill,
		Name:   a.Name,
		Status: StatusSkipped,
		Reason: hermesReason,
	}
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
