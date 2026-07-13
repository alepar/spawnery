package agentinstall

import "path/filepath"

// piEmitter handles artifact installation for the pi agent.
//
// pi needs no per-agent glue beyond the canonical phase: pi 0.80.2 reads
// ~/.agents/skills natively with no glue and no trust prompt in headless -p
// mode (sp-mwco.2.2 spike, 2026-07-12). SkillPath/MCPPath/ConfigPath stay
// blank (vestigial paths would be a false trail — §4.3).
type piEmitter struct {
	baseEmitter
}

func newPiEmitter(homeDir string) piEmitter {
	return piEmitter{
		baseEmitter: baseEmitter{
			layout: AgentLayout{
				Name:       "pi",
				ConfigRoot: filepath.Join(homeDir, ".pi"),
				SkillPath:  "", // canonical-only: pi reads ~/.agents/skills with no glue (sp-mwco.2.2 spike)
			},
		},
	}
}

// InstallSkill installs a skill into the canonical ~/.agents/skills/<name>/ dir only —
// pi needs no per-agent glue beyond the canonical phase (sp-mwco.2.2 spike).
func (e piEmitter) InstallSkill(a Artifact, opts Options) Report {
	return installSkill(e.layout, a, opts)
}

// Capabilities returns the support matrix for pi: skill=supported, everything else no-op.
func (e piEmitter) Capabilities() map[Kind]CapabilityStatus {
	return map[Kind]CapabilityStatus{
		KindSkill:            CapStatusSupported,
		KindMCP:              CapStatusNoOp,
		KindConfig:           CapStatusNoOp,
		KindPlugin:           CapStatusNoOp,
		Kind("instructions"): CapStatusNoOp,
	}
}

// InstallMCP, ApplyConfig, InstallPlugin are base placeholders — no glue registered for pi.
