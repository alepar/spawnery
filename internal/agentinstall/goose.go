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
				SkillPath:     "", // canonical-only: goose reads ~/.agents/skills with no glue (sp-mwco.2.2 spike)
				MCPPath:       configFile,
				MCPFormat:     FormatYAML,
				ConfigPath:    configFile,
				ConfigFormat:  FormatYAML,
				SchemaVersion: "goose-1.0",
			},
		},
	}
}

// InstallSkill installs a skill into the canonical ~/.agents/skills/<name>/ dir only —
// goose needs no per-agent glue beyond the canonical phase (sp-mwco.2.2 spike,
// 2026-07-12: no headless-enable step exists in v1.41.0; the planned Summon/Skills-
// extension work was dropped).
func (e gooseEmitter) InstallSkill(a Artifact, opts Options) Report {
	return installSkill(e.layout, a, opts)
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

// Capabilities returns the support matrix for goose: skill=supported (native,
// unconditional read — sp-mwco.2.2 spike), everything else no-op.
func (e gooseEmitter) Capabilities() map[Kind]CapabilityStatus {
	return map[Kind]CapabilityStatus{
		KindSkill:            CapStatusSupported,
		KindMCP:              CapStatusNoOp,
		KindConfig:           CapStatusNoOp,
		KindPlugin:           CapStatusNoOp,
		Kind("instructions"): CapStatusNoOp,
	}
}

// InstallMCP is a base placeholder (sp-cywj fills).
