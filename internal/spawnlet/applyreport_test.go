package spawnlet

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"testing"
	"time"

	"spawnery/internal/agentinstall/spec"
)

func writeApplyReportForTest(t *testing.T, st ArtifactStager, spawnID string, env spec.ApplyReport) {
	t.Helper()
	dir := st.ReportDirFor(spawnID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(env)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "apply-report.json"), data, 0o644); err != nil {
		t.Fatal(err)
	}
}

// --- AwaitApplyReport ---
//
// alive=nil throughout this section is deliberate: it exercises "today's behaviour exactly" (a
// pure report-file poll with no liveness check), the back-compat contract a nil AliveFunc
// guarantees. The liveness-probe behaviour itself (ErrAgentGone, two-consecutive-false, probe
// errors not counting as death) is covered separately below.

func TestAwaitApplyReport_ArrivesLate(t *testing.T) {
	st := ArtifactStager{Root: t.TempDir()}
	go func() {
		time.Sleep(50 * time.Millisecond)
		writeApplyReportForTest(t, st, "sp1", spec.ApplyReport{Schema: 1, Agent: "claude", Outcome: spec.OutcomeOK})
	}()
	env, err := st.AwaitApplyReport(context.Background(), "sp1", 2*time.Second, nil)
	if err != nil {
		t.Fatalf("AwaitApplyReport: %v", err)
	}
	if env == nil || env.Outcome != spec.OutcomeOK {
		t.Fatalf("env: %+v", env)
	}
}

func TestAwaitApplyReport_TimeoutReturnsNilNil(t *testing.T) {
	st := ArtifactStager{Root: t.TempDir()}
	env, err := st.AwaitApplyReport(context.Background(), "sp1", 50*time.Millisecond, nil)
	if err != nil {
		t.Fatalf("expected nil error on plain timeout, got %v", err)
	}
	if env != nil {
		t.Fatalf("expected nil env on timeout, got %+v", env)
	}
}

func TestAwaitApplyReport_CtxCancelReturnsError(t *testing.T) {
	st := ArtifactStager{Root: t.TempDir()}
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(30 * time.Millisecond)
		cancel()
	}()
	env, err := st.AwaitApplyReport(ctx, "sp1", 2*time.Second, nil)
	if err == nil {
		t.Fatal("expected ctx cancellation error")
	}
	if env != nil {
		t.Fatalf("expected nil env on cancellation, got %+v", env)
	}
}

func TestAwaitApplyReport_MalformedJSONTreatedAsAbsent(t *testing.T) {
	st := ArtifactStager{Root: t.TempDir()}
	dir := st.ReportDirFor("sp1")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "apply-report.json"), []byte("not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	env, err := st.AwaitApplyReport(context.Background(), "sp1", 100*time.Millisecond, nil)
	if err != nil {
		t.Fatalf("expected nil error (timeout, not ctx error), got %v", err)
	}
	if env != nil {
		t.Fatalf("expected nil env for malformed JSON, got %+v", env)
	}
}

// --- AwaitApplyReport liveness probe (ErrAgentGone) ---

// TestAwaitApplyReport_AgentGone_ReturnsErrAgentGoneFast is T6a: an alive probe that returns
// false twice in a row must end the wait in ~agentAlivePollEvery report-poll ticks, NOT after the
// (much larger) timeout budget — the core fix for ITEM D.
func TestAwaitApplyReport_AgentGone_ReturnsErrAgentGoneFast(t *testing.T) {
	st := ArtifactStager{Root: t.TempDir()}
	alive := func(context.Context) (bool, error) { return false, nil }

	start := time.Now()
	env, err := st.AwaitApplyReport(context.Background(), "sp1", 60*time.Second, alive)
	elapsed := time.Since(start)

	if !errors.Is(err, ErrAgentGone) {
		t.Fatalf("expected ErrAgentGone, got err=%v env=%+v", err, env)
	}
	if env != nil {
		t.Fatalf("expected nil env, got %+v", env)
	}
	if elapsed > 3*time.Second {
		t.Fatalf("AwaitApplyReport took %v to detect agent-gone, want well under the 60s budget", elapsed)
	}
}

// TestAwaitApplyReport_ReportWinsOverAgentGone is T6b: if the report lands in the same tick the
// probe would report the agent dead, the report wins — a launcher that wrote an error report and
// then aborted must surface its real per-skill reason, not a generic "agent gone".
func TestAwaitApplyReport_ReportWinsOverAgentGone(t *testing.T) {
	st := ArtifactStager{Root: t.TempDir()}
	writeApplyReportForTest(t, st, "sp1", spec.ApplyReport{Schema: 1, Outcome: spec.OutcomeError, Error: "no emitter"})
	alive := func(context.Context) (bool, error) { return false, nil }

	env, err := st.AwaitApplyReport(context.Background(), "sp1", 60*time.Second, alive)
	if err != nil {
		t.Fatalf("expected nil error (report present), got %v", err)
	}
	if env == nil || env.Outcome != spec.OutcomeError {
		t.Fatalf("expected the real error report to win, got %+v", env)
	}
}

// TestAwaitApplyReport_ProbeErrorIsNotDeath is T6c: an alive probe that errors (exec could not
// even be launched) must not count toward the two-consecutive-false threshold — the wait
// continues to a plain timeout instead of misreporting a live spawn as gone.
func TestAwaitApplyReport_ProbeErrorIsNotDeath(t *testing.T) {
	st := ArtifactStager{Root: t.TempDir()}
	alive := func(context.Context) (bool, error) {
		return false, errors.New("exec: docker: executable file not found in $PATH")
	}

	env, err := st.AwaitApplyReport(context.Background(), "sp1", 1200*time.Millisecond, alive)
	if err != nil {
		t.Fatalf("expected nil error (plain timeout, probe errors don't count as death), got %v", err)
	}
	if env != nil {
		t.Fatalf("expected nil env, got %+v", env)
	}
}

// --- SecretWaitTimeout / ApplyReportBudget (ITEM A) ---

// TestApplyReportBudget_DerivedFromSecretWaitTimeoutPlusSlack is T4a: the budget is derived
// (SecretWaitTimeout + applyReportSlack), and — the split this task collapses — the SAME for a
// bundle and a no-bundle manifest.
func TestApplyReportBudget_DerivedFromSecretWaitTimeoutPlusSlack(t *testing.T) {
	want := SecretWaitTimeout + applyReportSlack
	if got := ApplyReportBudget(); got != want {
		t.Errorf("ApplyReportBudget() = %v, want %v", got, want)
	}
}

// TestSecretWaitTimeoutMatchesShellDefault is T4b: a drift guard between the Go constant and
// apply-artifacts.sh's own shell default for SECRET_WAIT_TIMEOUT, so the two can never silently
// diverge (manager.go injects SecretWaitTimeout into the agent env; the shell default is only the
// hand-run-container fallback, but it documents the same intended value).
func TestSecretWaitTimeoutMatchesShellDefault(t *testing.T) {
	data, err := os.ReadFile("../../deploy/agent/apply-artifacts.sh")
	if err != nil {
		t.Fatalf("read apply-artifacts.sh: %v", err)
	}
	re := regexp.MustCompile(`SECRET_WAIT_TIMEOUT:-([0-9a-z]+)`)
	m := re.FindSubmatch(data)
	if m == nil {
		t.Fatal("apply-artifacts.sh: SECRET_WAIT_TIMEOUT default not found")
	}
	shellDefault, err := time.ParseDuration(string(m[1]))
	if err != nil {
		t.Fatalf("parse shell default %q: %v", m[1], err)
	}
	if shellDefault != SecretWaitTimeout {
		t.Errorf("apply-artifacts.sh SECRET_WAIT_TIMEOUT default = %v, want %v (spawnlet.SecretWaitTimeout)", shellDefault, SecretWaitTimeout)
	}
}

// --- EvaluateApplyReport ---

func TestEvaluateApplyReport_AllApplied(t *testing.T) {
	m := spec.Manifest{Artifacts: []spec.Artifact{{Kind: spec.KindSkill, Name: "s1", Targets: []string{"claude"}}}}
	env := &spec.ApplyReport{Schema: 1, Outcome: spec.OutcomeOK, Reports: []spec.Report{
		{Agent: "claude", Kind: spec.KindSkill, Name: "s1", Status: spec.StatusApplied},
	}}
	err, entries := EvaluateApplyReport(m, env)
	if err != nil {
		t.Fatalf("expected nil error, got %v", err)
	}
	if len(entries) != 1 || entries[0].Status != "applied" {
		t.Fatalf("entries: %+v", entries)
	}
}

func TestEvaluateApplyReport_BundleFailed(t *testing.T) {
	m := spec.Manifest{Artifacts: []spec.Artifact{
		{Kind: spec.KindSkill, Name: "s1", Targets: []string{"claude"}, Bundle: "b1"},
	}}
	env := &spec.ApplyReport{Schema: 1, Outcome: spec.OutcomeBundleFailed,
		Bundles: []spec.BundleRollup{{Bundle: "b1", Targeted: 7, Applied: 6, Failed: 1}},
		Reports: []spec.Report{{Agent: "claude", Kind: spec.KindSkill, Name: "s1", Status: spec.StatusApplied, Bundle: "b1"}},
	}
	err, entries := EvaluateApplyReport(m, env)
	if err == nil {
		t.Fatal("expected fatal error for bundle_failed")
	}
	if len(entries) != 1 {
		t.Fatalf("entries should still be populated: %+v", entries)
	}
}

func TestEvaluateApplyReport_MissingReportWithBundle_Fatal(t *testing.T) {
	m := spec.Manifest{Artifacts: []spec.Artifact{
		{Kind: spec.KindSkill, Name: "s1", Targets: []string{"claude"}, Bundle: "b1"},
	}}
	err, entries := EvaluateApplyReport(m, nil)
	if err == nil {
		t.Fatal("expected fatal error for missing report with a bundle in the manifest")
	}
	if entries != nil {
		t.Fatalf("expected no entries for a fatal missing-report case, got %+v", entries)
	}
}

func TestEvaluateApplyReport_MissingReportNoBundle_WarnsWithUnknownEntries(t *testing.T) {
	m := spec.Manifest{Artifacts: []spec.Artifact{
		{Kind: spec.KindSkill, Name: "s1", Targets: []string{"claude"}},
		{Kind: spec.KindMCP, Name: "m1", Targets: []string{"claude"}},
	}}
	err, entries := EvaluateApplyReport(m, nil)
	if err != nil {
		t.Fatalf("expected nil error (no bundle), got %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("expected 2 synthesized entries, got %+v", entries)
	}
	for _, e := range entries {
		if e.Status != StatusUnknown {
			t.Errorf("entry %+v: status should be %q", e, StatusUnknown)
		}
	}
}

func TestEvaluateApplyReport_ErrorOutcomeNoBundle_Warns(t *testing.T) {
	m := spec.Manifest{}
	env := &spec.ApplyReport{Schema: 1, Outcome: spec.OutcomeError, Error: "manifest load failed"}
	err, _ := EvaluateApplyReport(m, env)
	if err != nil {
		t.Fatalf("expected nil error (no bundle in manifest), got %v", err)
	}
}

func TestEvaluateApplyReport_ErrorOutcomeWithBundle_Fatal(t *testing.T) {
	m := spec.Manifest{Artifacts: []spec.Artifact{
		{Kind: spec.KindSkill, Name: "s1", Targets: []string{"claude"}, Bundle: "b1"},
	}}
	env := &spec.ApplyReport{Schema: 1, Outcome: spec.OutcomeError, Error: "manifest load failed"}
	err, _ := EvaluateApplyReport(m, env)
	if err == nil {
		t.Fatal("expected fatal error (bundle present, report never dispatched)")
	}
}

func TestEvaluateApplyReport_Warn_NotFatal(t *testing.T) {
	m := spec.Manifest{Artifacts: []spec.Artifact{{Kind: spec.KindSkill, Name: "s1", Targets: []string{"claude"}}}}
	env := &spec.ApplyReport{Schema: 1, Outcome: spec.OutcomeWarn, Reports: []spec.Report{
		{Agent: "claude", Kind: spec.KindSkill, Name: "s1", Status: spec.StatusFailed, Reason: "boom"},
	}}
	err, entries := EvaluateApplyReport(m, env)
	if err != nil {
		t.Fatalf("expected nil error for warn, got %v", err)
	}
	if len(entries) != 1 || entries[0].Status != "failed" {
		t.Fatalf("entries: %+v", entries)
	}
}
