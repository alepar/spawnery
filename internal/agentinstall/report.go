package agentinstall

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"spawnery/internal/agentinstall/spec"
)

// NewErrorApplyReport builds an ApplyReport for a run that never reached dispatch (manifest
// load/IO failure). agent is the CLI's --agent flag value (may be "").
func NewErrorApplyReport(agent, errMsg string) *spec.ApplyReport {
	return &spec.ApplyReport{
		Schema:  spec.CurrentApplyReportSchema,
		Agent:   agent,
		Outcome: spec.OutcomeError,
		Error:   errMsg,
	}
}

// BuildApplyReport correlates a completed Apply/ApplyFiltered Result against manifest to produce
// the apply-report envelope + the CLI exit code implied by its verdict. agent is the CLI's
// --agent flag value; bundle rollups (and therefore the all-or-nothing bundle verdict) are only
// computed when agent is non-empty — the only real caller (apply-artifacts.sh) always sets it.
//
// Verdict:
//   - any bundle group with Applied != Targeted -> bundle_failed
//   - else any report entry with status failed/skipped -> warn
//   - else -> ok
func BuildApplyReport(manifest Manifest, result Result, agent string) (*spec.ApplyReport, int) {
	env := &spec.ApplyReport{
		Schema:  spec.CurrentApplyReportSchema,
		Agent:   agent,
		Outcome: spec.OutcomeOK,
		Reports: make([]spec.Report, len(result.Reports)),
	}

	// bundleOf indexes manifest artifacts by (kind, name) -> bundle, for tagging report entries.
	bundleOf := make(map[artifactKey]string, len(manifest.Artifacts))
	for _, art := range manifest.Artifacts {
		if art.Bundle != "" {
			bundleOf[artifactKey{art.Kind, art.Name}] = art.Bundle
		}
	}

	copy(env.Reports, result.Reports)
	for i := range env.Reports {
		if b, ok := bundleOf[artifactKey{env.Reports[i].Kind, env.Reports[i].Name}]; ok {
			env.Reports[i].Bundle = b
		}
	}

	warn := false
	for _, r := range env.Reports {
		if r.Status == spec.StatusFailed || r.Status == spec.StatusSkipped {
			warn = true
		}
	}

	if agent != "" {
		env.Bundles = bundleRollups(manifest, env.Reports, agent)
	}
	bundleFailed := false
	for _, b := range env.Bundles {
		if b.Applied != b.Targeted {
			bundleFailed = true
		}
	}

	switch {
	case bundleFailed:
		env.Outcome = spec.OutcomeBundleFailed
	case warn:
		env.Outcome = spec.OutcomeWarn
	default:
		env.Outcome = spec.OutcomeOK
	}

	return env, ExitCodeFor(env.Outcome)
}

// artifactKey identifies a manifest artifact by (kind, name) for bundle/report correlation.
// Names are assumed unique within a kind (mirrors the assumption managed.json provenance and
// apply-artifacts.sh's report parsing already make).
type artifactKey struct {
	kind Kind
	name string
}

// bundleRollups computes the per-bundle_ref targeted/applied/failed/skipped tally for agent: for
// every manifest artifact belonging to a bundle that targets agent, look up its matching report
// entry (by kind+name+agent). A targeting artifact with NO matching entry counts toward Targeted
// but neither Applied/Failed/Skipped — the ApplyFiltered silent-skip trap — so it still trips the
// Applied != Targeted bundle_failed verdict.
func bundleRollups(manifest Manifest, reports []Report, agent string) []spec.BundleRollup {
	order := make([]string, 0)
	byBundle := make(map[string]*spec.BundleRollup)
	get := func(bundle string) *spec.BundleRollup {
		if r, ok := byBundle[bundle]; ok {
			return r
		}
		r := &spec.BundleRollup{Bundle: bundle}
		byBundle[bundle] = r
		order = append(order, bundle)
		return r
	}

	entryFor := make(map[artifactKey]Report, len(reports))
	for _, r := range reports {
		if r.Agent == agent {
			entryFor[artifactKey{r.Kind, r.Name}] = r
		}
	}

	for _, art := range manifest.Artifacts {
		if art.Bundle == "" {
			continue
		}
		// Every bundle_ref group gets a rollup entry, even one with zero members targeting
		// agent (Targeted stays 0, which never trips bundle_failed) — visible on the CP as
		// "this bundle has nothing to say about this agent" rather than silently absent.
		roll := get(art.Bundle)
		if !artifactTargetsAgent(art.Targets, agent) {
			continue
		}
		roll.Targeted++
		entry, ok := entryFor[artifactKey{art.Kind, art.Name}]
		if !ok {
			continue // targeted but no entry at all: not applied, not explicitly failed/skipped
		}
		switch entry.Status {
		case spec.StatusApplied:
			roll.Applied++
		case spec.StatusFailed:
			roll.Failed++
		case spec.StatusSkipped:
			roll.Skipped++
		}
	}

	out := make([]spec.BundleRollup, 0, len(order))
	for _, b := range order {
		out = append(out, *byBundle[b])
	}
	return out
}

// ExitCodeFor maps an apply-report verdict to the `agentinstall apply --report` process exit
// code: 0=ok, 2=warn, 3=bundle_failed, 1=error.
func ExitCodeFor(outcome spec.Outcome) int {
	switch outcome {
	case spec.OutcomeOK:
		return 0
	case spec.OutcomeWarn:
		return 2
	case spec.OutcomeBundleFailed:
		return 3
	default: // OutcomeError, or an unrecognized future value
		return 1
	}
}

// ApplyReportMode is the mode apply-report.json is written with: world-readable (0644), NOT
// os.CreateTemp's default 0600. The report is a non-sensitive status document (per-skill install
// outcomes; no secrets), and it is written by container-root INSIDE the agent container while it
// is read back by the node process on the HOST, which is a different uid whenever the container
// runtime remaps user namespaces (the dev docker daemon runs userns-remap base 100000, so
// container-root lands as host uid 100000 and the node polls as its own uid). A 0600 file across
// that boundary is unreadable to the poller — the failure sp-rwkk was filed for. The file must
// therefore follow the same readable-across-userns rule as the directory it lives in, whose
// EPERM->0777 fallback (spawnlet's prepareReportDir) exists for exactly this reason.
const ApplyReportMode = 0o644

// WriteApplyReportAtomic writes env as JSON to path via a same-dir tmp file + rename, so a
// concurrent reader (spawnlet's AwaitApplyReport poll) never observes a partial write — the
// file's presence at path IS the completion signal. The file lands at ApplyReportMode: chmod is
// explicit (not umask-masked) and applied to the tmp file BEFORE the rename, so the report is
// never briefly visible at its final path with the wrong mode.
func WriteApplyReportAtomic(path string, env *spec.ApplyReport) error {
	data, err := json.Marshal(env)
	if err != nil {
		return fmt.Errorf("marshal apply-report: %w", err)
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("apply-report: mkdir %s: %w", dir, err)
	}
	tmp, err := os.CreateTemp(dir, ".apply-report-*.tmp")
	if err != nil {
		return fmt.Errorf("apply-report: create temp file: %w", err)
	}
	tmpPath := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpPath)
		return fmt.Errorf("apply-report: write temp file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("apply-report: close temp file: %w", err)
	}
	if err := os.Chmod(tmpPath, ApplyReportMode); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("apply-report: chmod temp file: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("apply-report: rename into place: %w", err)
	}
	return nil
}
