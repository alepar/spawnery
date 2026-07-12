// Package agentinstall is a leaf package (zero spawnery-internal imports beyond
// its own stdlib-only spec sub-package). It implements the standalone
// agentinstall CLI adapter seam.
package agentinstall

import "spawnery/internal/agentinstall/spec"

// Canonical artifact model — single source of truth lives in the stdlib-only
// spec package (shared with the control plane). Re-exported here as aliases so
// existing agentinstall code and tests keep using the bare names.
type (
	Kind              = spec.Kind
	SkillPayload      = spec.SkillPayload
	MCPTransportStdio = spec.MCPTransportStdio
	MCPTransportHTTP  = spec.MCPTransportHTTP
	MCPPayload        = spec.MCPPayload
	ConfigPayload     = spec.ConfigPayload
	PluginPayload     = spec.PluginPayload
	Artifact          = spec.Artifact
	Manifest          = spec.Manifest
	Status            = spec.Status
	Capability        = spec.Capability
	Report            = spec.Report
	Result            = spec.Result
)

const (
	KindSkill  = spec.KindSkill
	KindMCP    = spec.KindMCP
	KindConfig = spec.KindConfig
	KindPlugin = spec.KindPlugin

	// CurrentSchemaVersion mirrors spec.CurrentSchemaVersion for in-package use.
	CurrentSchemaVersion = spec.CurrentSchemaVersion

	StatusApplied Status = spec.StatusApplied
	StatusSkipped Status = spec.StatusSkipped
	StatusFailed  Status = spec.StatusFailed

	// CapabilityApplied means all translated keys were written with full fidelity.
	CapabilityApplied Capability = spec.CapabilityApplied
	// CapabilityUnsupported means the key(s) cannot be expressed for this agent.
	CapabilityUnsupported Capability = spec.CapabilityUnsupported
	// CapabilityBestEffort means at least one key was approximated (e.g. a
	// model-tier-gated mode mapped to the closest available alternative).
	CapabilityBestEffort Capability = spec.CapabilityBestEffort
)

// LoadManifest reads and parses manifest.json from the staging directory,
// rejecting a manifest newer than this build understands. It delegates to spec.
func LoadManifest(dir string) (Manifest, error) {
	return spec.LoadManifest(dir)
}
