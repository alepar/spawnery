package agentinstall

import "time"

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
}

// Emitter installs or applies a single canonical Artifact into an agent's native config.
// Each method returns a Report; it must never panic.
type Emitter interface {
	Layout() AgentLayout
	InstallSkill(a Artifact, opts Options) Report
	InstallMCP(a Artifact, opts Options) Report
	ApplyConfig(a Artifact, opts Options) Report
	InstallPlugin(a Artifact, opts Options) Report
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
