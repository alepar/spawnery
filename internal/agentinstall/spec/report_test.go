package spec_test

import (
	"encoding/json"
	"strings"
	"testing"

	"spawnery/internal/agentinstall/spec"
)

func TestApplyReportRoundTrip(t *testing.T) {
	env := spec.ApplyReport{
		Schema:   spec.CurrentApplyReportSchema,
		Agent:    "claude",
		Runnable: "claude-tui",
		Outcome:  spec.OutcomeBundleFailed,
		Bundles: []spec.BundleRollup{
			{Bundle: "bundle-1", Targeted: 7, Applied: 6, Failed: 1, Skipped: 0},
		},
		Reports: []spec.Report{
			{Agent: "claude", Kind: spec.KindSkill, Name: "s1", Status: spec.StatusApplied, Bundle: "bundle-1"},
			{Agent: "claude", Kind: spec.KindSkill, Name: "s2", Status: spec.StatusFailed, Reason: "boom", Bundle: "bundle-1"},
		},
	}
	data, err := json.Marshal(env)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(data), `"schema":1`) {
		t.Errorf("expected schema in JSON, got %s", data)
	}
	var got spec.ApplyReport
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.Outcome != spec.OutcomeBundleFailed {
		t.Errorf("Outcome: got %q want %q", got.Outcome, spec.OutcomeBundleFailed)
	}
	if len(got.Bundles) != 1 || got.Bundles[0].Bundle != "bundle-1" || got.Bundles[0].Targeted != 7 || got.Bundles[0].Applied != 6 {
		t.Fatalf("bundles not round-tripped: %+v", got.Bundles)
	}
	if len(got.Reports) != 2 || got.Reports[1].Bundle != "bundle-1" || got.Reports[1].Status != spec.StatusFailed {
		t.Fatalf("reports not round-tripped: %+v", got.Reports)
	}
}

func TestParseApplyReportRejectsFutureSchema(t *testing.T) {
	data := []byte(`{"schema":999,"agent":"claude","outcome":"ok","reports":[]}`)
	if _, err := spec.ParseApplyReport(data); err == nil {
		t.Fatal("expected error for schema newer than CurrentApplyReportSchema")
	} else if !strings.Contains(err.Error(), "schema") {
		t.Errorf("error should mention schema, got: %v", err)
	}
}

func TestParseApplyReportAcceptsCurrentSchema(t *testing.T) {
	data := []byte(`{"schema":1,"agent":"claude","outcome":"ok","reports":[]}`)
	env, err := spec.ParseApplyReport(data)
	if err != nil {
		t.Fatalf("ParseApplyReport: %v", err)
	}
	if env.Outcome != spec.OutcomeOK {
		t.Errorf("Outcome: got %q want ok", env.Outcome)
	}
}

func TestParseApplyReportRejectsMalformedJSON(t *testing.T) {
	if _, err := spec.ParseApplyReport([]byte("not json")); err == nil {
		t.Fatal("expected error for malformed JSON")
	}
}

func TestArtifactBundleRoundTrip(t *testing.T) {
	a := spec.Artifact{Kind: spec.KindSkill, Name: "s1", Targets: []string{"claude"}, Bundle: "bundle-1",
		Skill: &spec.SkillPayload{Dir: "payloads/s1"}}
	data, err := json.Marshal(a)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"bundle":"bundle-1"`) {
		t.Fatalf("bundle not serialized: %s", data)
	}
	var a2 spec.Artifact
	if err := json.Unmarshal(data, &a2); err != nil {
		t.Fatal(err)
	}
	if a2.Bundle != "bundle-1" {
		t.Fatalf("bundle not round-tripped: %+v", a2)
	}
}
