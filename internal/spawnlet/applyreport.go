package spawnlet

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"spawnery/internal/agentinstall/spec"
)

// applyReportTimeout bounds how long the node waits (after containers_ready) for the agent
// container's apply-artifacts.sh / `agentinstall apply --report` to write the apply-report
// envelope, when the manifest declares a bundle (so the wait can end in a FATAL verdict — see
// ApplyReportTimeoutFor). Sized for a cold-start N-member bundle install, well above
// --secret-wait-timeout's own 30s default (a bundle install runs AFTER any per-artifact secret
// wait, not concurrently with it).
const applyReportTimeout = 2 * time.Minute

// applyReportNoBundleTimeout bounds the wait when the manifest has NO bundle, so a missing
// report can only ever warn, never fail the spawn (see ApplyReportTimeoutFor). Kept short: an
// artifact-carrying spawn whose runnable's apply-artifacts.sh never invokes agentinstall at all
// (e.g. goose/hermes/shell today, pending sp-mwco.2.6's runnable->emitter reconciliation) must
// not stall spawn start by the full patient budget just to arrive at a warning anyway — 20s is
// comfortably above a real claude/codex/opencode install's typical duration while staying well
// under readyTimeout (30s), so it doesn't dominate total startup latency.
const applyReportNoBundleTimeout = 20 * time.Second

// applyReportPollInterval paces AwaitApplyReport's poll of the report file.
const applyReportPollInterval = 250 * time.Millisecond

// ApplyReportTimeoutFor returns the default AwaitApplyReport wait budget for manifest: the
// patient bundle budget (applyReportTimeout) when manifest declares a bundle_ref artifact —
// EvaluateApplyReport can end this wait in a FATAL verdict, so it must give a genuinely slow
// install time to finish — or the short applyReportNoBundleTimeout otherwise, since a
// no-bundle miss only ever warns.
func ApplyReportTimeoutFor(manifest spec.Manifest) time.Duration {
	if manifestHasBundle(manifest) {
		return applyReportTimeout
	}
	return applyReportNoBundleTimeout
}

// Artifacts exposes the Manager's ArtifactStager (report-dir path + AwaitApplyReport) to callers
// outside the package (internal/node's startSpawn skill-install gate).
func (m *Manager) Artifacts() ArtifactStager { return m.artifacts }

// AwaitApplyReport polls ReportDirFor(spawnID)/apply-report.json until it appears (returned
// parsed), ctx is cancelled, or timeout elapses. A malformed or schema-rejected file is treated
// as not-yet-arrived (the CLI's tmp+rename makes a torn read effectively impossible, but a
// defensive re-poll costs nothing and is safer than misclassifying "not written yet" as
// permanently absent). timeout<=0 applies applyReportTimeout.
//
// Returns (env, nil) on arrival; (nil, ctx.Err()) if the context is cancelled/deadline-exceeded
// while waiting; (nil, nil) on a plain timeout (the caller decides fatal-vs-warn from the
// manifest, since a missing report is only fatal when a bundle is in play).
func (a ArtifactStager) AwaitApplyReport(ctx context.Context, spawnID string, timeout time.Duration) (*spec.ApplyReport, error) {
	if timeout <= 0 {
		timeout = applyReportTimeout
	}
	deadlineCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	path := filepath.Join(a.ReportDirFor(spawnID), "apply-report.json")
	ticker := time.NewTicker(applyReportPollInterval)
	defer ticker.Stop()
	for {
		if env, ok := readApplyReport(path); ok {
			return env, nil
		}
		select {
		case <-deadlineCtx.Done():
			if err := ctx.Err(); err != nil {
				return nil, err // caller's own ctx was cancelled/expired, not just our timeout
			}
			return nil, nil // plain timeout
		case <-ticker.C:
		}
	}
}

// readApplyReport reads and parses path, returning ok=false on any error (missing file, torn/
// malformed JSON, or a schema newer than this build understands) — all treated identically as
// "not yet arrived" by AwaitApplyReport's poll loop.
func readApplyReport(path string) (*spec.ApplyReport, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, false
	}
	env, err := spec.ParseApplyReport(data)
	if err != nil {
		return nil, false
	}
	return env, true
}

// InstallEntry is the spawnlet-side per-skill install status, threaded into the node's
// SkillInstallReport NodeMessage (internal/node/attach.go) and, from there, to the CP.
type InstallEntry struct {
	Agent  string
	Kind   string
	Name   string
	Status string
	Reason string
	Bundle string
}

// EvaluateApplyReport applies the sp-mwco.2.7 all-or-nothing bundle policy to a (possibly absent)
// apply-report envelope against manifest, returning a non-nil fatal error when the spawn must be
// failed (a partially-installed bundle, or — only when the manifest declares a bundle — a report
// that never arrived / carries a load-time error) plus the per-skill entries to propagate to the
// CP regardless of the verdict.
//
// env==nil (the report never arrived within the deadline):
//   - manifest has >=1 bundle artifact: fatal (a missing report on a bundle install is exactly
//     the "looks healthy but isn't" failure mode this task exists to close).
//   - no bundle: not fatal; entries are synthesized with StatusUnknown so the spawn's per-skill
//     surface still shows SOMETHING rather than silently nothing.
//
// env!=nil: bundle_failed is always fatal (regardless of Bundle presence in the manifest — the
// CLI already computed that verdict); outcome=error is fatal only when the manifest has a bundle
// (an isolated non-bundle load failure warns instead, consistent with "partial bundle installs
// fail the spawn; individual non-bundle failures warn").
func EvaluateApplyReport(manifest spec.Manifest, env *spec.ApplyReport) (error, []InstallEntry) {
	hasBundle := manifestHasBundle(manifest)

	if env == nil {
		if hasBundle {
			return fmt.Errorf("skill install report missing: apply-report.json was not written before the deadline (bundle install cannot be verified)"), nil
		}
		return nil, syntheticUnknownEntries(manifest)
	}

	entries := make([]InstallEntry, 0, len(env.Reports))
	for _, r := range env.Reports {
		entries = append(entries, InstallEntry{
			Agent: r.Agent, Kind: string(r.Kind), Name: r.Name,
			Status: string(r.Status), Reason: r.Reason, Bundle: r.Bundle,
		})
	}

	switch env.Outcome {
	case spec.OutcomeBundleFailed:
		return bundleFailedError(env), entries
	case spec.OutcomeError:
		if hasBundle {
			return fmt.Errorf("skill install report error: %s", env.Error), entries
		}
		return nil, entries
	default: // ok, warn
		return nil, entries
	}
}

// manifestHasBundle reports whether any manifest artifact belongs to a bundle_ref group.
func manifestHasBundle(manifest spec.Manifest) bool {
	for _, art := range manifest.Artifacts {
		if art.Bundle != "" {
			return true
		}
	}
	return false
}

// syntheticUnknownEntries builds one InstallEntry per manifest artifact with StatusUnknown, used
// when the apply-report never arrived and the manifest has no bundle (so it's a warning, not a
// fatal, but the per-skill surface should still say "unknown" rather than nothing at all).
func syntheticUnknownEntries(manifest spec.Manifest) []InstallEntry {
	if len(manifest.Artifacts) == 0 {
		return nil
	}
	entries := make([]InstallEntry, 0, len(manifest.Artifacts))
	for _, art := range manifest.Artifacts {
		entries = append(entries, InstallEntry{
			Kind: string(art.Kind), Name: art.Name, Status: StatusUnknown, Bundle: art.Bundle,
			Reason: "apply-report.json missing at deadline",
		})
	}
	return entries
}

// StatusUnknown marks an InstallEntry synthesized because no apply-report arrived to confirm the
// real outcome — distinct from the spec.Status values (applied/skipped/failed), which always
// come from an actual report.
const StatusUnknown = "unknown"

// bundleFailedError formats the fatal error for an OutcomeBundleFailed envelope, naming every
// partially-installed bundle group and its targeted/applied tally.
func bundleFailedError(env *spec.ApplyReport) error {
	var msgs []string
	for _, b := range env.Bundles {
		if b.Applied != b.Targeted {
			msgs = append(msgs, fmt.Sprintf("skill bundle %s: %d/%d members installed", b.Bundle, b.Applied, b.Targeted))
		}
	}
	if len(msgs) == 0 {
		return fmt.Errorf("skill bundle install failed (outcome=%s)", env.Outcome)
	}
	return errors.New(strings.Join(msgs, "; "))
}
