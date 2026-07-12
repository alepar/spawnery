package node

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	nodev1 "spawnery/gen/node/v1"
	"spawnery/internal/agentinstall/spec"
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
	be := &scriptedPodBackend{script: scriptGoose}
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
	if !be.wasStopped() {
		t.Error("pod backend Stop was not called after the fatal install-skills gate")
	}
}

// TestStartSpawn_MissingReportNoBundle_WarnsAndReachesActive verifies that a manifest with no
// bundle_ref groups tolerates a missing apply-report: the spawn still reaches ACTIVE, and a
// SkillInstallReport with synthesized "unknown" entries is sent.
func TestStartSpawn_MissingReportNoBundle_WarnsAndReachesActive(t *testing.T) {
	be := &scriptedPodBackend{script: scriptGoose}
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
	if len(report.Entries) != 1 || report.Entries[0].Status != nodev1.SkillInstallStatus_SKILL_INSTALL_STATUS_UNKNOWN {
		t.Errorf("entries: %+v", report.Entries)
	}
}

// TestStartSpawn_ReportArrivesOK_ReachesActive verifies the happy path: the agent container
// writes a real apply-report.json (simulated here by writing directly to the staging dir the
// real Manager materialized), and the spawn reaches ACTIVE carrying the applied-status entry.
func TestStartSpawn_ReportArrivesOK_ReachesActive(t *testing.T) {
	be := &scriptedPodBackend{script: scriptGoose}
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
	be := &scriptedPodBackend{script: scriptGoose}
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
