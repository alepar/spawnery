package agentinstall

import "time"

// CapabilityStatus describes the support level for a given (kind, agent) pair.
type CapabilityStatus string

const (
	CapStatusSupported  CapabilityStatus = "supported"
	CapStatusNoOp       CapabilityStatus = "no-op"
	CapStatusBestEffort CapabilityStatus = "best-effort"
)

// Format is the file format used by an agent for a particular config file.
type Format string

const (
	FormatJSON  Format = "json"
	FormatJSONC Format = "jsonc"
	FormatTOML  Format = "toml"
	FormatYAML  Format = "yaml"
)

// AgentLayout describes the filesystem layout and format conventions for a single agent.
type AgentLayout struct {
	// Name is the normalized emitter name (claude|codex|opencode|hermes|goose).
	Name string
	// ConfigRoot is the agent's primary configuration directory (existence indicates "detected").
	ConfigRoot string
	// SkillPath is the directory where skill subdirectories are installed.
	SkillPath string
	// MCPPath is the file where MCP server entries are written.
	MCPPath string
	// MCPFormat is the file format for MCPPath.
	MCPFormat Format
	// ConfigPath is the file where config keys are written.
	ConfigPath string
	// ConfigFormat is the file format for ConfigPath.
	ConfigFormat Format
	// SchemaVersion is the agent schema version this emitter targets.
	SchemaVersion string
	// ForbiddenConfigKeys lists config keys that must never be written by the emitter.
	ForbiddenConfigKeys []string
	// RulesDir is the directory where codex execpolicy rules are written.
	// Empty for agents that do not use an execpolicy rules file.
	RulesDir string
	// InstructionsPath is the dedicated managed-instructions file ("" = no-op).
	// Written on every Apply when config.instructions is non-empty (replace-on-apply).
	InstructionsPath string
}

// Options holds runtime options passed to each emitter method.
type Options struct {
	// HomeDir is the effective home directory (substituted for ~ in paths).
	HomeDir string
	// SecretsDir is the directory containing secret value files.
	SecretsDir string
	// ArtifactsDir is the staging root; relative skill source dirs resolve against it.
	ArtifactsDir string
	// SecretWaitTimeout is the maximum duration to wait for async-delivered secret files
	// (in /run/spawnery/secrets/<envVarName>) before declaring them missing.
	// A zero value disables the wait (single-check fallthrough to the emitter, which
	// will return StatusFailed if the file is absent).
	SecretWaitTimeout time.Duration
	// ProfileID and ProfileVersion stamp managed.json provenance entries.
	// May be empty for standalone CLI use (entries are still written without them).
	ProfileID      string
	ProfileVersion string
	// ManagedIndexPath, when non-empty, is the path to the ~/.spawnery/managed.json
	// provenance index to upsert after a successful apply.
	// Empty disables index writing (existing tests and standalone install use).
	ManagedIndexPath string
}

// Emitter installs or applies a single canonical Artifact into an agent's native config.
// Each method returns a Report; it must never panic.
type Emitter interface {
	Layout() AgentLayout
	InstallSkill(a Artifact, opts Options) Report
	InstallMCP(a Artifact, opts Options) Report
	ApplyConfig(a Artifact, opts Options) Report
	InstallPlugin(a Artifact, opts Options) Report
	// Capabilities returns the support level for each artifact kind (and "instructions")
	// for this emitter. Deferred/unimplemented emitters return no-op for all kinds.
	Capabilities() map[Kind]CapabilityStatus
}

// baseEmitter provides placeholder implementations for all three Emitter methods,
// returning a "skipped" Report with a "not implemented in this slice (seam only)" reason.
// Per-agent files embed this and override the methods that have real implementations.
type baseEmitter struct {
	layout AgentLayout
}

func (b baseEmitter) Layout() AgentLayout {
	return b.layout
}

func (b baseEmitter) InstallSkill(a Artifact, _ Options) Report {
	return Report{
		Agent:  b.layout.Name,
		Kind:   KindSkill,
		Name:   a.Name,
		Status: StatusSkipped,
		Reason: "not implemented in this slice (seam only)",
	}
}

func (b baseEmitter) InstallMCP(a Artifact, _ Options) Report {
	return Report{
		Agent:  b.layout.Name,
		Kind:   KindMCP,
		Name:   a.Name,
		Status: StatusSkipped,
		Reason: "not implemented in this slice (seam only)",
	}
}

func (b baseEmitter) ApplyConfig(a Artifact, _ Options) Report {
	return Report{
		Agent:  b.layout.Name,
		Kind:   KindConfig,
		Name:   a.Name,
		Status: StatusSkipped,
		Reason: "not implemented in this slice (seam only)",
	}
}

func (b baseEmitter) InstallPlugin(a Artifact, _ Options) Report {
	return Report{
		Agent:  b.layout.Name,
		Kind:   KindPlugin,
		Name:   a.Name,
		Status: StatusSkipped,
		Reason: "not implemented in this slice (seam only)",
	}
}

// Capabilities returns a no-op matrix for all kinds.
// Deferred/unimplemented emitters (hermes, goose) inherit this and report honestly.
// Fully-implemented emitters (claude, codex, opencode) override with their real matrix.
func (b baseEmitter) Capabilities() map[Kind]CapabilityStatus {
	return map[Kind]CapabilityStatus{
		KindSkill:            CapStatusNoOp,
		KindMCP:              CapStatusNoOp,
		KindConfig:           CapStatusNoOp,
		KindPlugin:           CapStatusNoOp,
		Kind("instructions"): CapStatusNoOp,
	}
}
