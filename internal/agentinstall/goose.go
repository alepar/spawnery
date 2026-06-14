package agentinstall

import "path/filepath"

// gooseEmitter handles artifact installation for the goose agent.
type gooseEmitter struct {
	baseEmitter
}

func newGooseEmitter(xdgConfigHome string) gooseEmitter {
	configRoot := filepath.Join(xdgConfigHome, "goose")
	configFile := filepath.Join(configRoot, "config.yaml")
	return gooseEmitter{
		baseEmitter: baseEmitter{
			layout: AgentLayout{
				Name:          "goose",
				ConfigRoot:    configRoot,
				SkillPath:     "", // no-op: skill installation deferred
				MCPPath:       configFile,
				MCPFormat:     FormatYAML,
				ConfigPath:    configFile,
				ConfigFormat:  FormatYAML,
				SchemaVersion: "goose-1.0",
			},
		},
	}
}

// InstallSkill is a permanent no-op for goose (deferred).
func (e gooseEmitter) InstallSkill(a Artifact, _ Options) Report {
	return Report{
		Agent:  e.layout.Name,
		Kind:   KindSkill,
		Name:   a.Name,
		Status: StatusSkipped,
		Reason: "deferred",
	}
}

// ApplyConfig is permanently deferred for goose — goose config format/scope not yet validated.
func (e gooseEmitter) ApplyConfig(a Artifact, _ Options) Report {
	return Report{
		Agent:  e.layout.Name,
		Kind:   KindConfig,
		Name:   a.Name,
		Status: StatusSkipped,
		Reason: "goose config deferred",
	}
}

// InstallMCP is a base placeholder (sp-cywj fills).
