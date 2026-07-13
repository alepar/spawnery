package spawnlet

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"spawnery/internal/agentinstall/spec"
)

// SecretWaitTimeout is the --secret-wait-timeout budget the node injects into the agent
// container as SECRET_WAIT_TIMEOUT (manager.go's AgentSpec.Env), and the value
// apply-artifacts.sh's own shell default falls back to when the env var is unset (hand-run
// container). It is the dominant term in ApplyReportBudget: apply-artifacts.sh blocks up to this
// long waiting for startup secrets to land BEFORE it ever reaches agentinstall's ~25ms of actual
// install work (measured 2026-07-13, real 14-skill obra/superpowers bundle: 18-49ms wall clock).
// Exported (rather than a private const mirrored in the shell default) so the two can't drift
// silently — see TestSecretWaitTimeoutMatchesShellDefault.
const SecretWaitTimeout = 30 * time.Second

// applyReportSlack is added to SecretWaitTimeout to form ApplyReportBudget: headroom for the
// launcher's own preamble (base-config generation, CA install, etc.) plus agentinstall's ~25ms
// of real work — not for a slow install, which measurement shows does not happen.
const applyReportSlack = 15 * time.Second

// applyReportPollInterval paces AwaitApplyReport's poll of the report file.
const applyReportPollInterval = 250 * time.Millisecond

// agentAlivePollEvery paces AwaitApplyReport's liveness probe relative to its report poll: the
// probe runs on every Nth report-poll tick (~1s at applyReportPollInterval=250ms), not on every
// tick, so a probe hiccup costs at most one missed liveness sample, not a busy-loop of exec calls.
const agentAlivePollEvery = 4

// ApplyReportBudget returns AwaitApplyReport's default wait budget: SecretWaitTimeout plus
// applyReportSlack, the SAME for a bundle and a no-bundle manifest (sp-mwco.2.12 collapses the
// old bundle/no-bundle timeout split — see EvaluateApplyReport for where bundle-vs-not now only
// changes verdict SEVERITY, not how long the wait runs). Derived, not invented: the report is
// guaranteed no later than SECRET_WAIT_TIMEOUT + a short launcher preamble + agentinstall's ~25ms
// of real work, so anything beyond that is not a slow install, it's a broken one — waiting longer
// only delays diagnosis. Combined with AwaitApplyReport's liveness probe (ErrAgentGone), a report
// that will never arrive because the agent container is gone is detected in ~1-2s regardless of
// this budget; the budget only bounds a still-alive container's install.
func ApplyReportBudget() time.Duration {
	return SecretWaitTimeout + applyReportSlack
}

// Artifacts exposes the Manager's ArtifactStager (report-dir path + AwaitApplyReport) to callers
// outside the package (internal/node's startSpawn skill-install gate).
func (m *Manager) Artifacts() ArtifactStager { return m.artifacts }

// AliveFunc reports whether the agent container a wait is polling for is still running. Used by
// AwaitApplyReport so the wait can end the moment the agent is confirmed gone instead of sitting
// out its full timeout (sp-mwco.2.12 ITEM D) — a broken install (agent crashed / launcher aborted
// on an unreportable failure) fails in ~1-2s instead of waiting for a budget that was never going
// to be satisfied. A nil AliveFunc disables the liveness check entirely (today's behaviour: pure
// timeout), used by tests that exercise AwaitApplyReport directly without a running container.
type AliveFunc func(ctx context.Context) (bool, error)

// ErrAgentGone is returned by AwaitApplyReport when the agent container is confirmed gone before
// a report arrived — distinct from a plain timeout (nil, nil) because it carries a REASON the
// caller can propagate, rather than a caller having to infer "probably broken" from a deadline.
var ErrAgentGone = errors.New("agent container exited before writing apply-report.json")

// ErrReportUnreadable is returned by AwaitApplyReport when apply-report.json EXISTS but cannot be
// used: unreadable (EACCES/EPERM — e.g. written 0600 by container-root, which a userns-remapping
// runtime maps to a host uid the node is not), malformed/schema-rejected JSON, or any other I/O
// error that is not "not there yet". It is TERMINAL — the wait ends immediately with the real
// reason rather than polling out the budget and reporting a missing report for a file that is
// sitting right there. sp-mwco.2.12's invariant, stated negatively: a timeout must only ever mean
// "nothing was written".
var ErrReportUnreadable = errors.New("apply-report.json exists but could not be read")

// AwaitApplyReport polls ReportDirFor(spawnID)/apply-report.json until it appears (returned
// parsed), the agent is confirmed gone (see alive), the report turns out to be present-but-
// unusable (ErrReportUnreadable), ctx is cancelled, or timeout elapses. timeout<=0 applies
// ApplyReportBudget.
//
// ONLY os.IsNotExist keeps the poll running: a file that is not there yet is the sole legitimate
// reason to wait. Everything else about an existing file — a permission error, a malformed or
// schema-rejected body, any other errno — is a permanent condition that waiting cannot fix, and
// is returned immediately wrapped in ErrReportUnreadable (with the file's uid/mode when the OS
// let us stat it). The CLI's tmp+rename write makes a torn read of a half-written report
// impossible, so "malformed" can only mean genuinely broken, not in-flight.
//
// alive, if non-nil, is polled roughly every agentAlivePollEvery report-poll ticks (~1s at
// applyReportPollInterval=250ms). The report always wins ties: on each tick the report is read
// FIRST and returned immediately if present, so a launcher that wrote an error report and then
// exited still surfaces its real per-skill reason rather than racing ErrAgentGone. A single
// alive==false is not enough to conclude the agent is gone (one docker/crictl hiccup must not
// fail a healthy spawn) — TWO CONSECUTIVE alive==false results are required. An alive error (the
// exec itself could not be launched — daemon unreachable, binary missing) is treated as "unknown,
// not death" and does not count toward the two-in-a-row threshold; a healthy spawn is not failed
// over an inability to ask the question. On ErrAgentGone, one final report read is attempted
// first (the report may have landed in the same instant the agent exited) and returned if present.
//
// Returns (env, nil) on arrival; (nil, ErrReportUnreadable-wrapped) if the report is present but
// unusable; (nil, ErrAgentGone) if alive confirms the agent is gone with no report; (nil,
// ctx.Err()) if the context is cancelled/deadline-exceeded while waiting; (nil, nil) on a plain
// timeout — which now means, and only means, that nothing was ever written (the caller decides
// fatal-vs-warn from the manifest, since an absent report is only fatal when a bundle is in play).
func (a ArtifactStager) AwaitApplyReport(ctx context.Context, spawnID string, timeout time.Duration, alive AliveFunc) (*spec.ApplyReport, error) {
	if timeout <= 0 {
		timeout = ApplyReportBudget()
	}
	deadlineCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	path := filepath.Join(a.ReportDirFor(spawnID), "apply-report.json")
	ticker := time.NewTicker(applyReportPollInterval)
	defer ticker.Stop()
	consecutiveDead := 0
	tick := 0
	for {
		env, err := readApplyReport(path)
		if err != nil {
			return nil, err // present but unusable — terminal, do not wait out the budget
		}
		if env != nil {
			return env, nil
		}
		select {
		case <-deadlineCtx.Done():
			if err := ctx.Err(); err != nil {
				return nil, err // caller's own ctx was cancelled/expired, not just our timeout
			}
			return nil, nil // plain timeout
		case <-ticker.C:
			tick++
			if alive == nil || tick%agentAlivePollEvery != 0 {
				continue
			}
			ok, err := alive(ctx)
			if err != nil {
				continue // couldn't even ask — treat as unknown, not death
			}
			if ok {
				consecutiveDead = 0
				continue
			}
			consecutiveDead++
			if consecutiveDead < 2 {
				continue
			}
			// Final read: the report may have landed in the instant the agent exited. A
			// present-but-unusable report still beats a generic "agent gone" — it names the
			// actual problem.
			env, err := readApplyReport(path)
			if err != nil {
				return nil, err
			}
			if env != nil {
				return env, nil
			}
			return nil, ErrAgentGone
		}
	}
}

// readApplyReport reads and parses path, returning:
//
//	(env, nil)  — a valid report
//	(nil, nil)  — the file does not exist yet: the ONLY "keep polling" case
//	(nil, err)  — the file exists but is unusable (ErrReportUnreadable-wrapped): TERMINAL
//
// The terminal cases are what stop a permission error (EACCES on a 0600 report written by
// container-root under a userns-remapping runtime) or a malformed body from masquerading as "not
// written yet" and burning the caller's whole wait budget — see ErrReportUnreadable.
func readApplyReport(path string) (*spec.ApplyReport, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil // genuinely not written yet — poll again
		}
		// Two %w verbs: callers match ErrReportUnreadable for the class, and the wrapped errno is
		// still reachable (errors.Is(err, os.ErrPermission)) for anyone who wants the specific
		// cause without string-matching.
		return nil, fmt.Errorf("%w (%s): %w", ErrReportUnreadable, describeReportFile(path), err)
	}
	env, err := spec.ParseApplyReport(data)
	if err != nil {
		return nil, fmt.Errorf("%w (%s): parse: %v", ErrReportUnreadable, describeReportFile(path), err)
	}
	return env, nil
}

// describeReportFile renders the ownership/mode facts a reader needs to diagnose an unreadable
// report — the shape of the sp-rwkk failure is "written 0600 by a uid you are not" — plus the
// uid the poller itself is running as, since the whole question is whether those two match.
// Best-effort: a stat that itself fails degrades to just the path.
func describeReportFile(path string) string {
	fi, err := os.Stat(path)
	if err != nil {
		return path
	}
	desc := fmt.Sprintf("%s mode %#o", path, fi.Mode().Perm())
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		desc += fmt.Sprintf(" uid %d gid %d", st.Uid, st.Gid)
	}
	return desc + fmt.Sprintf("; reader uid %d — userns-remap?", os.Getuid())
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
		return nil, SyntheticUnknownEntries(manifest, "apply-report.json missing at deadline")
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

// SyntheticUnknownEntries builds one InstallEntry per manifest artifact with StatusUnknown and
// Reason=reason, used whenever there is no real apply-report to report per-skill status from:
// EvaluateApplyReport's plain-timeout/no-bundle case (reason names the missing deadline), and
// internal/node's agent-gone fast-fail path (reason names the agent's exit). Exported so both
// call sites share one entry shape rather than the node hand-rolling its own.
func SyntheticUnknownEntries(manifest spec.Manifest, reason string) []InstallEntry {
	if len(manifest.Artifacts) == 0 {
		return nil
	}
	entries := make([]InstallEntry, 0, len(manifest.Artifacts))
	for _, art := range manifest.Artifacts {
		entries = append(entries, InstallEntry{
			Kind: string(art.Kind), Name: art.Name, Status: StatusUnknown, Bundle: art.Bundle,
			Reason: reason,
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
