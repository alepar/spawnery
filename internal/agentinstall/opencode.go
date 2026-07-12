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
				Name:                "opencode",
				ConfigRoot:          configRoot,
				SkillPath:           "", // canonical-only: opencode reads ~/.agents/skills with no glue (sp-mwco.2.2 spike)
				MCPPath:             configFile,
				MCPFormat:           FormatJSONC,
				ConfigPath:          configFile,
				ConfigFormat:        FormatJSONC,
				SchemaVersion:       "opencode-1.15",
				ForbiddenConfigKeys: []string{"model", "permission"},
				InstructionsPath:    filepath.Join(configRoot, "profile-instructions.md"),
			},
		},
	}
}

// InstallSkill installs a skill into the canonical ~/.agents/skills/<name>/ dir only —
// opencode needs no per-agent glue beyond the canonical phase (sp-mwco.2.2 spike,
// 2026-07-12: file-level and behavioral read confirmed with zero extra config).
func (e opencodeEmitter) InstallSkill(a Artifact, opts Options) Report {
	return installSkill(e.layout, a, opts)
}

// Capabilities returns the support matrix for opencode: skill=no-op, mcp+config+instructions=supported, plugin=best-effort.
func (e opencodeEmitter) Capabilities() map[Kind]CapabilityStatus {
	return map[Kind]CapabilityStatus{
		KindSkill:            CapStatusNoOp,
		KindMCP:              CapStatusSupported,
		KindConfig:           CapStatusSupported,
		KindPlugin:           CapStatusBestEffort,
		Kind("instructions"): CapStatusSupported,
	}
}

// InstallMCP is implemented in mcp.go (sp-cywj). ApplyConfig is implemented in config.go (sp-g5x8).
// ForbiddenConfigKeys: ["model","permission"] — approvalPosture is not mapped for opencode
// (hard-errors on invalid config); allowedCommands/deniedCommands go via permission.bash.
