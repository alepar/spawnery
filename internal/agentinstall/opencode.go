package agentinstall

import "path/filepath"

// opencodeEmitter handles artifact installation for the opencode agent.
type opencodeEmitter struct {
	baseEmitter
}

func newOpencodeEmitter(xdgConfigHome string) opencodeEmitter {
	configRoot := filepath.Join(xdgConfigHome, "opencode")
	configFile := filepath.Join(configRoot, "opencode.json")
	return opencodeEmitter{
		baseEmitter: baseEmitter{
			layout: AgentLayout{
				Name:          "opencode",
				ConfigRoot:    configRoot,
				SkillPath:     "", // no-op: skills layout unconfirmed (S6)
				MCPPath:       configFile,
				MCPFormat:     FormatJSONC,
				ConfigPath:    configFile,
				ConfigFormat:  FormatJSONC,
				SchemaVersion: "opencode-1.15",
			},
		},
	}
}

// InstallSkill is a permanent no-op — opencode skills layout unconfirmed (S6).
func (e opencodeEmitter) InstallSkill(a Artifact, _ Options) Report {
	return Report{
		Agent:  e.layout.Name,
		Kind:   KindSkill,
		Name:   a.Name,
		Status: StatusSkipped,
		Reason: "opencode skills layout unconfirmed (S6)",
	}
}

// InstallMCP and ApplyConfig are base placeholders (sp-cywj/g5x8 fill).
