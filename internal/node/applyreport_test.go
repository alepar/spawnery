package node

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	nodev1 "spawnery/gen/node/v1"
	"spawnery/internal/agentinstall/spec"
	"spawnery/internal/runtime/fakepod"
	"spawnery/internal/spawnlet"
)

// startSpawnWithManifest builds a StartSpawn carrying a manifest.json artifact (as
// profiles_assembly.go would) plus any extra artifacts, and returns it.
func startSpawnWithManifest(t *testing.T, spawnID string, manifest []byte) *nodev1.StartSpawn {
	t.Helper()
	return &nodev1.StartSpawn{
		SpawnId: spawnID, AppRef: writeNodeApp(t), Model: "m",
		Artifacts: []*nodev1.ArtifactSpec{
			{
				Id:          "manifest",
				Inline:      manifest,
				ContentType: nodev1.ArtifactContentType_ARTIFACT_CONTENT_TYPE_BYTES,
				DestPath:    "manifest.json",
			},
		},
	}
}

// TestStartSpawn_MissingReportWithBundle_FailsSpawn verifies that when the manifest declares a
// bundle_ref group and the apply-report envelope never arrives before the (test-shortened)
// deadline, the spawn is failed: ERROR status carrying step_key=install-skills, the
// SkillInstallReport is sent BEFORE the ERROR status, the pod is stopped, and ACTIVE is never
// reached.
func TestStartSpawn_MissingReportWithBundle_FailsSpawn(t *testing.T) {
	be := fakeBackend(t, fakepod.WithAttachScript(scriptGoose))
	fs := &fakeCPStream{}
	a := newAttacher(newGooseManager(t, be), fs)
	a.applyReportTimeout = 30 * time.Millisecond

	manifest := `{"artifacts":[{"kind":"skill","name":"s1","targets":["claude"],"bundle":"b1","skill":{"dir":"payloads/s1"}}]}`
	st := startSpawnWithManifest(t, "sp1", []byte(manifest))
	a.startSpawn(context.Background(), st)

	if got := lastPhase(fs.phasesFor("sp1")); got != nodev1.SpawnPhase_ERROR {
		t.Fatalf("terminal phase = %v, want ERROR", got)
	}

	// The ERROR step status must carry step_key=install-skills.
	var errStep *nodev1.SpawnStatus
	for _, s := range fs.stepStatusesFor("sp1") {
		if s.Phase == nodev1.SpawnPhase_ERROR {
			errStep = s
		}
	}
	if errStep == nil {
		t.Fatal("no ERROR step status emitted")
	}
	if errStep.StepKey != spawnlet.MilestoneInstallSkills {
		t.Errorf("error step key = %q, want %q", errStep.StepKey, spawnlet.MilestoneInstallSkills)
	}

	// A SkillInstallReport must have been sent, and it must precede the ERROR status.
	report := lastSkillInstallReport(fs, "sp1")
	if report == nil {
		t.Fatal("no SkillInstallReport sent")
	}
	if report.Outcome != nodev1.SkillInstallOutcome_SKILL_INSTALL_OUTCOME_ERROR {
		t.Errorf("outcome: got %v, want ERROR (a bundle-install timeout must never leave the CP with UNSPECIFIED)", report.Outcome)
	}
	if len(report.Entries) != 1 || report.Entries[0].Bundle != "b1" || report.Entries[0].Status != nodev1.SkillInstallStatus_SKILL_INSTALL_STATUS_UNKNOWN || report.Entries[0].Reason == "" {
		t.Errorf("entries: %+v, want one UNKNOWN entry for bundle b1 with a non-empty reason", report.Entries)
	}
	reportIdx, errIdx := -1, -1
	fs.mu.Lock()
	for i, m := range fs.sent {
		if r := m.GetSkillInstallReport(); r != nil && r.SpawnId == "sp1" {
			reportIdx = i
		}
		if s := m.GetStatus(); s != nil && s.SpawnId == "sp1" && s.Phase == nodev1.SpawnPhase_ERROR {
			if errIdx == -1 {
				errIdx = i
			}
		}
	}
	fs.mu.Unlock()
	if reportIdx == -1 || errIdx == -1 || reportIdx > errIdx {
		t.Errorf("SkillInstallReport (idx %d) must be sent before the ERROR status (idx %d)", reportIdx, errIdx)
	}

	// The pod must have been stopped (no leaked container).
	if !podWasStopped(be) {
		t.Error("pod backend Stop was not called after the fatal install-skills gate")
	}
}

// TestStartSpawn_MissingReportNoBundle_WarnsAndReachesActive verifies that a manifest with no
// bundle_ref groups tolerates a missing apply-report: the spawn still reaches ACTIVE, and a
// SkillInstallReport with synthesized "unknown" entries is sent.
func TestStartSpawn_MissingReportNoBundle_WarnsAndReachesActive(t *testing.T) {
	be := fakeBackend(t, fakepod.WithAttachScript(scriptGoose))
	fs := &fakeCPStream{}
	a := newAttacher(newGooseManager(t, be), fs)
	a.applyReportTimeout = 30 * time.Millisecond

	manifest := `{"artifacts":[{"kind":"skill","name":"s1","targets":["claude"]}]}` // no "bundle" field
	st := startSpawnWithManifest(t, "sp1", []byte(manifest))
	a.startSpawn(context.Background(), st)
	defer a.stopSpawn(context.Background(), "sp1")

	if got := lastPhase(fs.phasesFor("sp1")); got != nodev1.SpawnPhase_ACTIVE {
		t.Fatalf("terminal phase = %v, want ACTIVE", got)
	}

	report := lastSkillInstallReport(fs, "sp1")
	if report == nil {
		t.Fatal("no SkillInstallReport sent")
	}
	if report.Outcome != nodev1.SkillInstallOutcome_SKILL_INSTALL_OUTCOME_WARN {
		t.Errorf("outcome: got %v, want WARN (a tolerated no-bundle missing report must never leave the CP with UNSPECIFIED)", report.Outcome)
	}
	if len(report.Entries) != 1 || report.Entries[0].Status != nodev1.SkillInstallStatus_SKILL_INSTALL_STATUS_UNKNOWN {
		t.Errorf("entries: %+v", report.Entries)
	}
}

// TestStartSpawn_ReportArrivesOK_ReachesActive verifies the happy path: the agent container
// writes a real apply-report.json (simulated here by writing directly to the staging dir the
// real Manager materialized), and the spawn reaches ACTIVE carrying the applied-status entry.
func TestStartSpawn_ReportArrivesOK_ReachesActive(t *testing.T) {
	be := fakeBackend(t, fakepod.WithAttachScript(scriptGoose))
	fs := &fakeCPStream{}
	mgr := newGooseManager(t, be)
	a := newAttacher(mgr, fs)
	a.applyReportTimeout = 2 * time.Second

	manifest := `{"artifacts":[{"kind":"skill","name":"s1","targets":["claude"]}]}`
	st := startSpawnWithManifest(t, "sp1", []byte(manifest))

	// Poll for the staging dir to appear (Materialize runs inside CreateWithSelection, which
	// startSpawn calls synchronously below) and write a real envelope, mirroring what
	// apply-artifacts.sh's `agentinstall apply --report` would do inside the agent container.
	go func() {
		reportPath := filepath.Join(mgr.Artifacts().ReportDirFor("sp1"), "apply-report.json")
		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) {
			if _, err := os.Stat(filepath.Dir(reportPath)); err == nil {
				env := spec.ApplyReport{Schema: 1, Agent: "claude", Outcome: spec.OutcomeOK, Reports: []spec.Report{
					{Agent: "claude", Kind: spec.KindSkill, Name: "s1", Status: spec.StatusApplied},
				}}
				data, _ := json.Marshal(env)
				_ = os.WriteFile(reportPath, data, 0o644)
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
	}()

	a.startSpawn(context.Background(), st)
	defer a.stopSpawn(context.Background(), "sp1")

	if got := lastPhase(fs.phasesFor("sp1")); got != nodev1.SpawnPhase_ACTIVE {
		t.Fatalf("terminal phase = %v, want ACTIVE", got)
	}
	report := lastSkillInstallReport(fs, "sp1")
	if report == nil {
		t.Fatal("no SkillInstallReport sent")
	}
	if report.Outcome != nodev1.SkillInstallOutcome_SKILL_INSTALL_OUTCOME_OK {
		t.Errorf("outcome: got %v, want OK", report.Outcome)
	}
	if len(report.Entries) != 1 || report.Entries[0].Status != nodev1.SkillInstallStatus_SKILL_INSTALL_STATUS_APPLIED {
		t.Errorf("entries: %+v", report.Entries)
	}
}

// TestStartSpawn_NoArtifacts_SkipsGateEntirely verifies a spawn with no artifacts at all does
// not emit the install-skills milestone or a SkillInstallReport (regression guard: the gate must
// stay off for the overwhelming majority of spawns that carry no skills).
func TestStartSpawn_NoArtifacts_SkipsGateEntirely(t *testing.T) {
	be := fakeBackend(t, fakepod.WithAttachScript(scriptGoose))
	fs := &fakeCPStream{}
	a := newAttacher(newGooseManager(t, be), fs)
	a.startSpawn(context.Background(), &nodev1.StartSpawn{SpawnId: "sp1", AppRef: writeNodeApp(t), Model: "m"})
	defer a.stopSpawn(context.Background(), "sp1")

	if got := lastPhase(fs.phasesFor("sp1")); got != nodev1.SpawnPhase_ACTIVE {
		t.Fatalf("terminal phase = %v, want ACTIVE", got)
	}
	for _, s := range fs.stepStatusesFor("sp1") {
		if s.StepKey == spawnlet.MilestoneInstallSkills {
			t.Errorf("install-skills milestone emitted for an artifact-less spawn: %+v", s)
		}
	}
	if lastSkillInstallReport(fs, "sp1") != nil {
		t.Error("SkillInstallReport sent for an artifact-less spawn")
	}
}

// alwaysDead simulates an agent container that is gone from the very first liveness probe tick.
func alwaysDead(context.Context, string) (bool, error) { return false, nil }

// TestStartSpawn_AgentGone_FailsFastWithReason is T8a: when the agent container is confirmed gone
// (agentAliveFn reports false) before a report ever arrives, the spawn must fail FAST — well under
// the (generously large) applyReportTimeout — with a SkillInstallReport carrying outcome=ERROR and
// an ERROR SpawnStatus whose Detail names the real reason, not a generic deadline.
func TestStartSpawn_AgentGone_FailsFastWithReason(t *testing.T) {
	be := fakeBackend(t, fakepod.WithAttachScript(scriptGoose))
	fs := &fakeCPStream{}
	a := newAttacher(newGooseManager(t, be), fs)
	a.applyReportTimeout = 60 * time.Second // large budget: proves the fast-fail is NOT the timeout
	a.agentAliveFn = alwaysDead

	manifest := `{"artifacts":[{"kind":"skill","name":"s1","targets":["claude"],"bundle":"b1","skill":{"dir":"payloads/s1"}}]}`
	st := startSpawnWithManifest(t, "sp1", []byte(manifest))

	start := time.Now()
	a.startSpawn(context.Background(), st)
	elapsed := time.Since(start)

	if elapsed > 5*time.Second {
		t.Fatalf("startSpawn took %v to fail on agent-gone, want well under the 60s budget", elapsed)
	}
	if got := lastPhase(fs.phasesFor("sp1")); got != nodev1.SpawnPhase_ERROR {
		t.Fatalf("terminal phase = %v, want ERROR", got)
	}

	report := lastSkillInstallReport(fs, "sp1")
	if report == nil {
		t.Fatal("no SkillInstallReport sent")
	}
	if report.Outcome != nodev1.SkillInstallOutcome_SKILL_INSTALL_OUTCOME_ERROR {
		t.Errorf("outcome: got %v, want ERROR (must never leave the CP with UNSPECIFIED on a terminal failure)", report.Outcome)
	}

	var errStatus *nodev1.SpawnStatus
	for _, s := range fs.stepStatusesFor("sp1") {
		if s.Phase == nodev1.SpawnPhase_ERROR {
			errStatus = s
		}
	}
	if errStatus == nil {
		t.Fatal("no ERROR status emitted")
	}
	if !strings.Contains(errStatus.Detail, "agent container exited") {
		t.Errorf("ERROR status detail = %q, want it to name the agent-gone reason (spawnlet.ErrAgentGone)", errStatus.Detail)
	}

	if !podWasStopped(be) {
		t.Error("pod backend Stop was not called after the agent-gone fast-fail")
	}
}

// TestStartSpawn_AgentGone_FatalEvenWithNoBundle is T8b: agent-gone is fatal even for a manifest
// with no bundle_ref groups — unlike a plain missing-report timeout (which only warns for a
// no-bundle manifest), a confirmed-dead agent can never go ACTIVE regardless of bundle policy.
func TestStartSpawn_AgentGone_FatalEvenWithNoBundle(t *testing.T) {
	be := fakeBackend(t, fakepod.WithAttachScript(scriptGoose))
	fs := &fakeCPStream{}
	a := newAttacher(newGooseManager(t, be), fs)
	a.applyReportTimeout = 60 * time.Second
	a.agentAliveFn = alwaysDead

	manifest := `{"artifacts":[{"kind":"skill","name":"s1","targets":["claude"]}]}` // no "bundle" field
	st := startSpawnWithManifest(t, "sp1", []byte(manifest))
	a.startSpawn(context.Background(), st)

	if got := lastPhase(fs.phasesFor("sp1")); got != nodev1.SpawnPhase_ERROR {
		t.Fatalf("terminal phase = %v, want ERROR (agent-gone is fatal regardless of bundle policy)", got)
	}
	report := lastSkillInstallReport(fs, "sp1")
	if report == nil {
		t.Fatal("no SkillInstallReport sent")
	}
	if len(report.Entries) != 1 || report.Entries[0].Status != nodev1.SkillInstallStatus_SKILL_INSTALL_STATUS_UNKNOWN {
		t.Errorf("entries: %+v", report.Entries)
	}
}

// TestStartSpawn_CtxCancelledDuringWait_ReportsErrorNotUnspecified is sp-mwco.2.13: when the
// caller's ctx is cancelled WHILE the wait is in progress (neither a plain timeout nor an
// agent-gone/report-unreadable terminal condition), the generic `awaitErr != nil` branch must
// still ship an explicit outcome=ERROR envelope with synthetic per-skill entries — never
// UNSPECIFIED with zero entries.
func TestStartSpawn_CtxCancelledDuringWait_ReportsErrorNotUnspecified(t *testing.T) {
	be := fakeBackend(t, fakepod.WithAttachScript(scriptGoose))
	fs := &fakeCPStream{}
	a := newAttacher(newGooseManager(t, be), fs)
	a.applyReportTimeout = 60 * time.Second // large budget: proves the exit is the cancellation, not the budget

	ctx, cancel := context.WithCancel(context.Background())
	// AwaitApplyReport probes liveness on every 4th 250ms report-poll tick (~1s in), so cancelling
	// from inside the probe fires deterministically INSIDE the wait; returning alive=true means
	// ErrAgentGone can never race it.
	a.agentAliveFn = func(context.Context, string) (bool, error) {
		cancel()
		return true, nil
	}

	manifest := `{"artifacts":[{"kind":"skill","name":"s1","targets":["claude"]}]}` // non-bundle: proves this isn't just the bundle policy
	st := startSpawnWithManifest(t, "sp1", []byte(manifest))
	a.startSpawn(ctx, st)

	if got := lastPhase(fs.phasesFor("sp1")); got != nodev1.SpawnPhase_ERROR {
		t.Fatalf("terminal phase = %v, want ERROR", got)
	}
	if !podWasStopped(be) {
		t.Error("pod backend Stop was not called after ctx cancellation during the install-skills wait")
	}

	report := lastSkillInstallReport(fs, "sp1")
	if report == nil {
		t.Fatal("no SkillInstallReport sent")
	}
	if report.Outcome != nodev1.SkillInstallOutcome_SKILL_INSTALL_OUTCOME_ERROR {
		t.Errorf("outcome: got %v, want ERROR (must never leave the CP with UNSPECIFIED)", report.Outcome)
	}
	if len(report.Entries) != 1 || report.Entries[0].Status != nodev1.SkillInstallStatus_SKILL_INSTALL_STATUS_UNKNOWN || report.Entries[0].Reason == "" {
		t.Errorf("entries: %+v, want one UNKNOWN entry with a non-empty reason", report.Entries)
	}
}

// TestSkillInstallOutcomeProto_NeverUnspecified is sp-mwco.2.13 hole (D): a recognized
// spec.Outcome string maps to its matching wire enum, and any unrecognized/empty outcome string
// maps to WARN (never UNSPECIFIED) — the envelope verdict must always be explicit even for a
// foreign or missing outcome value.
func TestSkillInstallOutcomeProto_NeverUnspecified(t *testing.T) {
	cases := []struct {
		name    string
		outcome spec.Outcome
		want    nodev1.SkillInstallOutcome
	}{
		{"ok", spec.OutcomeOK, nodev1.SkillInstallOutcome_SKILL_INSTALL_OUTCOME_OK},
		{"warn", spec.OutcomeWarn, nodev1.SkillInstallOutcome_SKILL_INSTALL_OUTCOME_WARN},
		{"bundle_failed", spec.OutcomeBundleFailed, nodev1.SkillInstallOutcome_SKILL_INSTALL_OUTCOME_BUNDLE_FAILED},
		{"error", spec.OutcomeError, nodev1.SkillInstallOutcome_SKILL_INSTALL_OUTCOME_ERROR},
		{"empty", spec.Outcome(""), nodev1.SkillInstallOutcome_SKILL_INSTALL_OUTCOME_WARN},
		{"foreign", spec.Outcome("some_future_verdict"), nodev1.SkillInstallOutcome_SKILL_INSTALL_OUTCOME_WARN},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := &spec.ApplyReport{Schema: 1, Outcome: tc.outcome}
			if got := skillInstallOutcomeProto(env); got != tc.want {
				t.Errorf("skillInstallOutcomeProto(outcome=%q) = %v, want %v", tc.outcome, got, tc.want)
			}
		})
	}

	// The nil envelope is the ABSENCE of a signal, and absence is never a state the CP may see. This
	// case is what makes the invariant structural rather than conventional: if the mapper returned
	// UNSPECIFIED here, the guarantee would rest on every present and future send site remembering to
	// normalize first, and the one that forgets would reopen the hole silently.
	if got := skillInstallOutcomeProto(nil); got != nodev1.SkillInstallOutcome_SKILL_INSTALL_OUTCOME_WARN {
		t.Errorf("skillInstallOutcomeProto(nil) = %v, want WARN (we know install was attempted; we do NOT know it succeeded)", got)
	}
}

// lastSkillInstallReport returns the most recent SkillInstallReport sent for spawnID, or nil.
func lastSkillInstallReport(f *fakeCPStream, spawnID string) *nodev1.SkillInstallReport {
	f.mu.Lock()
	defer f.mu.Unlock()
	var last *nodev1.SkillInstallReport
	for _, m := range f.sent {
		if r := m.GetSkillInstallReport(); r != nil && r.SpawnId == spawnID {
			last = r
		}
	}
	return last
}
