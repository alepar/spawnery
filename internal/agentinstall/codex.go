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

// InstallSkill installs a skill into the canonical ~/.agents/skills/<name>/ dir and
// additionally copies it into <codexHome>/skills/<name>/ — the pre-existing legacy
// compat copy. codex's read side (whether it reads either directory at all) is
// unverified pending sp-9e6q; keeping the legacy copy avoids retracting a write this
// slice never claimed to have verified either way. Revisit once sp-9e6q unblocks
// probing codex's actual skill read behavior.
func (e codexEmitter) InstallSkill(a Artifact, opts Options) Report {
	return installSkill(e.layout, a, opts)
}

// Capabilities returns the support matrix for codex: skill=best-effort — codexEmitter
// really does write both the canonical tree and the $CODEX_HOME/skills compat copy, but
// whether codex reads either directory at all is unverified: codex cannot complete a
// single turn via the current OpenRouter routing once it bootstraps its own built-in
// skills, which it always does on first run (sp-9e6q, P0, sp-mwco.2.2 spike). supported
// would overclaim a read side nobody has observed; no-op would understate that files are
// genuinely written. best-effort is the honest middle: written, not proven read. Revisit
// once sp-9e6q unblocks probing codex's actual skill read behavior. Every other kind is
// fully supported.
func (e codexEmitter) Capabilities() map[Kind]CapabilityStatus {
	return map[Kind]CapabilityStatus{
		KindSkill:            CapStatusBestEffort,
		KindMCP:              CapStatusSupported,
		KindConfig:           CapStatusSupported,
		KindPlugin:           CapStatusSupported,
		Kind("instructions"): CapStatusSupported,
	}
}

// InstallMCP is implemented in mcp.go (sp-cywj). ApplyConfig is implemented in config.go (sp-g5x8).
// ForbiddenConfigKeys: ["model","approval_policy","sandbox_mode","sandbox_workspace_write"] — these
// are launcher-managed or security-sensitive; allowedCommands/deniedCommands go to RulesDir instead.
