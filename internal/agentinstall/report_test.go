package agentinstall_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"spawnery/internal/agentinstall"
	"spawnery/internal/agentinstall/spec"
)

func TestBuildApplyReport_AllApplied_OK(t *testing.T) {
	m := agentinstall.Manifest{Artifacts: []agentinstall.Artifact{
		{Kind: agentinstall.KindSkill, Name: "s1", Targets: []string{"claude"}},
	}}
	result := agentinstall.Result{Reports: []agentinstall.Report{
		{Agent: "claude", Kind: agentinstall.KindSkill, Name: "s1", Status: agentinstall.StatusApplied},
	}}
	env, code := agentinstall.BuildApplyReport(m, result, "claude")
	if env.Outcome != spec.OutcomeOK {
		t.Errorf("Outcome: got %q want ok", env.Outcome)
	}
	if code != 0 {
		t.Errorf("exit code: got %d want 0", code)
	}
}

func TestBuildApplyReport_NonBundleFailure_Warn(t *testing.T) {
	m := agentinstall.Manifest{Artifacts: []agentinstall.Artifact{
		{Kind: agentinstall.KindSkill, Name: "s1", Targets: []string{"claude"}},
	}}
	result := agentinstall.Result{Reports: []agentinstall.Report{
		{Agent: "claude", Kind: agentinstall.KindSkill, Name: "s1", Status: agentinstall.StatusFailed, Reason: "boom"},
	}}
	env, code := agentinstall.BuildApplyReport(m, result, "claude")
	if env.Outcome != spec.OutcomeWarn {
		t.Errorf("Outcome: got %q want warn", env.Outcome)
	}
	if code != 2 {
		t.Errorf("exit code: got %d want 2", code)
	}
}

func TestBuildApplyReport_BundleMemberFailed_BundleFailed(t *testing.T) {
	m := agentinstall.Manifest{Artifacts: []agentinstall.Artifact{
		{Kind: agentinstall.KindSkill, Name: "s1", Targets: []string{"claude"}, Bundle: "b1"},
		{Kind: agentinstall.KindSkill, Name: "s2", Targets: []string{"claude"}, Bundle: "b1"},
	}}
	result := agentinstall.Result{Reports: []agentinstall.Report{
		{Agent: "claude", Kind: agentinstall.KindSkill, Name: "s1", Status: agentinstall.StatusApplied},
		{Agent: "claude", Kind: agentinstall.KindSkill, Name: "s2", Status: agentinstall.StatusFailed, Reason: "boom"},
	}}
	env, code := agentinstall.BuildApplyReport(m, result, "claude")
	if env.Outcome != spec.OutcomeBundleFailed {
		t.Fatalf("Outcome: got %q want bundle_failed", env.Outcome)
	}
	if code != 3 {
		t.Errorf("exit code: got %d want 3", code)
	}
	if len(env.Bundles) != 1 || env.Bundles[0].Targeted != 2 || env.Bundles[0].Applied != 1 || env.Bundles[0].Failed != 1 {
		t.Fatalf("bundle rollup: %+v", env.Bundles)
	}
	// Report entries must carry their bundle id.
	for _, r := range env.Reports {
		if r.Bundle != "b1" {
			t.Errorf("report %+v missing bundle tag", r)
		}
	}
}

// TestBuildApplyReport_SilentSkipTrap covers the ApplyFiltered silent-skip trap (ground truth
// #5): a bundle member that targets the agent but produced NO report entry at all (as opposed to
// an explicit skipped/failed entry) must still trip bundle_failed.
func TestBuildApplyReport_SilentSkipTrap(t *testing.T) {
	m := agentinstall.Manifest{Artifacts: []agentinstall.Artifact{
		{Kind: agentinstall.KindSkill, Name: "s1", Targets: []string{"claude"}, Bundle: "b1"},
		{Kind: agentinstall.KindSkill, Name: "s2", Targets: []string{"claude"}, Bundle: "b1"}, // no matching report entry below
	}}
	result := agentinstall.Result{Reports: []agentinstall.Report{
		{Agent: "claude", Kind: agentinstall.KindSkill, Name: "s1", Status: agentinstall.StatusApplied},
	}}
	env, _ := agentinstall.BuildApplyReport(m, result, "claude")
	if env.Outcome != spec.OutcomeBundleFailed {
		t.Fatalf("Outcome: got %q want bundle_failed", env.Outcome)
	}
	if len(env.Bundles) != 1 || env.Bundles[0].Targeted != 2 || env.Bundles[0].Applied != 1 {
		t.Fatalf("bundle rollup: %+v", env.Bundles)
	}
}

func TestBuildApplyReport_ArtifactNotTargetingAgent_OK(t *testing.T) {
	m := agentinstall.Manifest{Artifacts: []agentinstall.Artifact{
		{Kind: agentinstall.KindSkill, Name: "s1", Targets: []string{"codex"}, Bundle: "b1"},
	}}
	// ApplyFiltered would produce zero entries for this artifact at all (not targeting claude).
	result := agentinstall.Result{}
	env, code := agentinstall.BuildApplyReport(m, result, "claude")
	if env.Outcome != spec.OutcomeOK {
		t.Fatalf("Outcome: got %q want ok", env.Outcome)
	}
	if code != 0 {
		t.Errorf("exit code: got %d want 0", code)
	}
	if len(env.Bundles) != 1 || env.Bundles[0].Targeted != 0 || env.Bundles[0].Applied != 0 {
		t.Fatalf("bundle rollup: %+v", env.Bundles)
	}
}

func TestBuildApplyReport_ManagedIndexFailure_Warn(t *testing.T) {
	m := agentinstall.Manifest{}
	result := agentinstall.Result{Reports: []agentinstall.Report{
		{Kind: agentinstall.Kind("managed-index"), Status: agentinstall.StatusFailed, Reason: "disk full"},
	}}
	env, code := agentinstall.BuildApplyReport(m, result, "claude")
	if env.Outcome != spec.OutcomeWarn {
		t.Fatalf("Outcome: got %q want warn", env.Outcome)
	}
	if code != 2 {
		t.Errorf("exit code: got %d want 2", code)
	}
}

func TestExitCodeFor(t *testing.T) {
	cases := []struct {
		outcome spec.Outcome
		want    int
	}{
		{spec.OutcomeOK, 0},
		{spec.OutcomeWarn, 2},
		{spec.OutcomeBundleFailed, 3},
		{spec.OutcomeError, 1},
	}
	for _, c := range cases {
		if got := agentinstall.ExitCodeFor(c.outcome); got != c.want {
			t.Errorf("ExitCodeFor(%q) = %d, want %d", c.outcome, got, c.want)
		}
	}
}

func TestWriteApplyReportAtomic(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "report", "apply-report.json")
	env := agentinstall.NewErrorApplyReport("claude", "boom")
	if err := agentinstall.WriteApplyReportAtomic(path, env); err != nil {
		t.Fatalf("WriteApplyReportAtomic: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read report: %v", err)
	}
	var got spec.ApplyReport
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("parse report: %v", err)
	}
	if got.Outcome != spec.OutcomeError || got.Error != "boom" {
		t.Errorf("report: %+v", got)
	}
	// No leftover tmp files in the report dir.
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "apply-report.json" {
		t.Errorf("report dir should contain exactly apply-report.json, got %v", entries)
	}
}

// TestWriteApplyReportAtomic_WorldReadable is the sp-rwkk write-side regression: the report is
// written by container-root but read back by the node on the HOST, a different uid under
// userns-remap. os.CreateTemp's default 0600 made it unreadable there, so the node timed out
// waiting for a file that existed. It must land world-readable.
func TestWriteApplyReportAtomic_WorldReadable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "report", "apply-report.json")
	if err := agentinstall.WriteApplyReportAtomic(path, agentinstall.NewErrorApplyReport("claude", "boom")); err != nil {
		t.Fatalf("WriteApplyReportAtomic: %v", err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := fi.Mode().Perm(); got != agentinstall.ApplyReportMode {
		t.Fatalf("apply-report.json mode = %#o, want %#o (world-readable across the userns boundary)", got, agentinstall.ApplyReportMode)
	}
}
