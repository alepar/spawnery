package spec

import (
	"encoding/json"
	"fmt"
)

// CurrentApplyReportSchema is the highest apply-report schema this build understands. An envelope
// declaring a higher schema is rejected as a hard error (a newer producer than this consumer).
const CurrentApplyReportSchema = 1

// Status is the outcome status of a single report entry.
type Status string

const (
	StatusApplied Status = "applied"
	StatusSkipped Status = "skipped"
	StatusFailed  Status = "failed"
)

// Capability describes the fidelity of a config translation: whether the canonical keys were
// fully honoured, approximated (best-effort), or not expressible for the target agent. Only
// config- and plugin-kind reports set this field; mcp/skill leave it empty.
type Capability string

const (
	// CapabilityApplied means all translated keys were written with full fidelity.
	CapabilityApplied Capability = "applied"
	// CapabilityUnsupported means the key(s) cannot be expressed for this agent.
	CapabilityUnsupported Capability = "unsupported"
	// CapabilityBestEffort means at least one key was approximated (e.g. a model-tier-gated
	// mode mapped to the closest available alternative).
	CapabilityBestEffort Capability = "best-effort"
)

// Report is the structured outcome for one (artifact x agent) combination. Bundle echoes the
// owning Artifact.Bundle (empty for a non-bundle artifact) so a per-skill status carries its
// group for the all-or-nothing bundle verdict.
type Report struct {
	Agent             string     `json:"agent"`
	Kind              Kind       `json:"kind"`
	Name              string     `json:"name"`
	Status            Status     `json:"status"`
	Reason            string     `json:"reason,omitempty"`
	RuntimeDepMissing string     `json:"runtimeDepMissing,omitempty"`
	Capability        Capability `json:"capability,omitempty"`
	Bundle            string     `json:"bundle,omitempty"`
}

// Result is the JSON-serializable aggregate output of an Apply run.
type Result struct {
	Reports []Report `json:"reports"`
}

// Outcome is the overall verdict of an apply run, carried by ApplyReport.Outcome.
type Outcome string

const (
	// OutcomeOK means every targeted artifact applied cleanly.
	OutcomeOK Outcome = "ok"
	// OutcomeWarn means at least one non-bundle entry was skipped/failed, but no bundle group
	// was partially installed.
	OutcomeWarn Outcome = "warn"
	// OutcomeBundleFailed means at least one bundle_ref group had targeted != applied members
	// (the all-or-nothing bundle contract): the spawn must fail.
	OutcomeBundleFailed Outcome = "bundle_failed"
	// OutcomeError means the run could not dispatch at all (manifest load/IO failure before
	// any artifact was processed).
	OutcomeError Outcome = "error"
)

// BundleRollup is the per-bundle_ref-group tally used to compute the all-or-nothing verdict.
// Targeted counts manifest artifacts belonging to Bundle whose Targets include the agent this
// report was produced for; Applied/Failed/Skipped tally the matching report entries' Status.
// Applied < Targeted (whether from an explicit failed/skipped entry or from a targeting artifact
// that produced no entry at all) means the bundle is partially installed.
type BundleRollup struct {
	Bundle   string `json:"bundle"`
	Targeted int    `json:"targeted"`
	Applied  int    `json:"applied"`
	Failed   int    `json:"failed"`
	Skipped  int    `json:"skipped"`
}

// ApplyReport is the envelope written by `agentinstall apply --report <path>`: schema 1. It is
// the completion signal spawnlet's AwaitApplyReport polls for (its presence, not a separate
// sentinel file, marks the apply run as done) and the source of the per-skill status propagated
// to the CP.
type ApplyReport struct {
	Schema   int     `json:"schema"`
	Agent    string  `json:"agent"`
	Runnable string  `json:"runnable,omitempty"`
	Outcome  Outcome `json:"outcome"`
	// Error is set only for Outcome==OutcomeError (manifest load/IO failure before dispatch).
	Error   string         `json:"error,omitempty"`
	Bundles []BundleRollup `json:"bundles,omitempty"`
	Reports []Report       `json:"reports"`
}

// ParseApplyReport unmarshals an apply-report envelope, rejecting a schema newer than this
// build understands.
func ParseApplyReport(data []byte) (*ApplyReport, error) {
	var r ApplyReport
	if err := json.Unmarshal(data, &r); err != nil {
		return nil, fmt.Errorf("parse apply-report: %w", err)
	}
	if r.Schema > CurrentApplyReportSchema {
		return nil, fmt.Errorf("apply-report schema %d is newer than this build understands (max %d); update spawnlet",
			r.Schema, CurrentApplyReportSchema)
	}
	return &r, nil
}
