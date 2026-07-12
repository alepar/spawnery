package spawnlet

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
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

func TestAwaitApplyReport_ArrivesLate(t *testing.T) {
	st := ArtifactStager{Root: t.TempDir()}
	go func() {
		time.Sleep(50 * time.Millisecond)
		writeApplyReportForTest(t, st, "sp1", spec.ApplyReport{Schema: 1, Agent: "claude", Outcome: spec.OutcomeOK})
	}()
	env, err := st.AwaitApplyReport(context.Background(), "sp1", 2*time.Second)
	if err != nil {
		t.Fatalf("AwaitApplyReport: %v", err)
	}
	if env == nil || env.Outcome != spec.OutcomeOK {
		t.Fatalf("env: %+v", env)
	}
}

func TestAwaitApplyReport_TimeoutReturnsNilNil(t *testing.T) {
	st := ArtifactStager{Root: t.TempDir()}
	env, err := st.AwaitApplyReport(context.Background(), "sp1", 50*time.Millisecond)
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
	env, err := st.AwaitApplyReport(ctx, "sp1", 2*time.Second)
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
	env, err := st.AwaitApplyReport(context.Background(), "sp1", 100*time.Millisecond)
	if err != nil {
		t.Fatalf("expected nil error (timeout, not ctx error), got %v", err)
	}
	if env != nil {
		t.Fatalf("expected nil env for malformed JSON, got %+v", env)
	}
}

// --- ApplyReportTimeoutFor ---

// TestApplyReportTimeoutFor_NoBundleIsShort verifies a manifest with no bundle_ref artifacts gets
// the short (non-fatal-miss) budget, not the full bundle-patient one — an artifact-carrying spawn
// whose runnable's apply-artifacts.sh never invokes agentinstall (e.g. goose today) must not
// stall spawn start by the full 2m just to end up warning.
func TestApplyReportTimeoutFor_NoBundleIsShort(t *testing.T) {
	m := spec.Manifest{Artifacts: []spec.Artifact{
		{Kind: spec.KindSkill, Name: "s1", Targets: []string{"claude"}},
	}}
	got := ApplyReportTimeoutFor(m)
	if got != applyReportNoBundleTimeout {
		t.Errorf("ApplyReportTimeoutFor(no bundle) = %v, want %v", got, applyReportNoBundleTimeout)
	}
	if got >= applyReportTimeout {
		t.Errorf("no-bundle timeout (%v) should be well under the bundle timeout (%v)", got, applyReportTimeout)
	}
}

// TestApplyReportTimeoutFor_BundleIsPatient verifies a manifest with a bundle_ref artifact gets
// the full patient budget, since a missing report there ends in a FATAL verdict and a genuinely
// slow install must be given time to finish.
func TestApplyReportTimeoutFor_BundleIsPatient(t *testing.T) {
	m := spec.Manifest{Artifacts: []spec.Artifact{
		{Kind: spec.KindSkill, Name: "s1", Targets: []string{"claude"}, Bundle: "b1"},
	}}
	if got := ApplyReportTimeoutFor(m); got != applyReportTimeout {
		t.Errorf("ApplyReportTimeoutFor(bundle) = %v, want %v", got, applyReportTimeout)
	}
}

// TestApplyReportTimeoutFor_EmptyManifestIsShort verifies an empty manifest (no artifacts at
// all — shouldn't normally reach the gate, but defensive) also gets the short budget.
func TestApplyReportTimeoutFor_EmptyManifestIsShort(t *testing.T) {
	if got := ApplyReportTimeoutFor(spec.Manifest{}); got != applyReportNoBundleTimeout {
		t.Errorf("ApplyReportTimeoutFor(empty) = %v, want %v", got, applyReportNoBundleTimeout)
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
