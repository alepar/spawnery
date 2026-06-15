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
)

const (
	KindSkill  = spec.KindSkill
	KindMCP    = spec.KindMCP
	KindConfig = spec.KindConfig
	KindPlugin = spec.KindPlugin

	// CurrentSchemaVersion mirrors spec.CurrentSchemaVersion for in-package use.
	CurrentSchemaVersion = spec.CurrentSchemaVersion
)

// LoadManifest reads and parses manifest.json from the staging directory,
// rejecting a manifest newer than this build understands. It delegates to spec.
func LoadManifest(dir string) (Manifest, error) {
	return spec.LoadManifest(dir)
}

// Status is the outcome status of a single report entry.
type Status string

const (
	StatusApplied Status = "applied"
	StatusSkipped Status = "skipped"
	StatusFailed  Status = "failed"
)

// Capability describes the fidelity of a config translation: whether the canonical
// keys were fully honoured, approximated (best-effort), or not expressible for the
// target agent.  Only config- and plugin-kind reports set this field; mcp/skill leave it empty.
type Capability string

const (
	// CapabilityApplied means all translated keys were written with full fidelity.
	CapabilityApplied Capability = "applied"
	// CapabilityUnsupported means the key(s) cannot be expressed for this agent.
	CapabilityUnsupported Capability = "unsupported"
	// CapabilityBestEffort means at least one key was approximated (e.g. a
	// model-tier-gated mode mapped to the closest available alternative).
	CapabilityBestEffort Capability = "best-effort"
)

// Report is the structured outcome for one (artifact × agent) combination.
type Report struct {
	Agent             string     `json:"agent"`
	Kind              Kind       `json:"kind"`
	Name              string     `json:"name"`
	Status            Status     `json:"status"`
	Reason            string     `json:"reason,omitempty"`
	RuntimeDepMissing string     `json:"runtimeDepMissing,omitempty"`
	Capability        Capability `json:"capability,omitempty"`
}

// Result is the JSON-serializable aggregate output of an Apply run.
type Result struct {
	Reports []Report `json:"reports"`
}
