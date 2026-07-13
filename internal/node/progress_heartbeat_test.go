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
	"spawnery/internal/runtime/fakepod"
)

// resumeProgressFor returns the ResumeProgress messages sent for spawnID, in send order.
func resumeProgressFor(f *fakeCPStream, spawnID string) []*nodev1.ResumeProgress {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []*nodev1.ResumeProgress
	for _, m := range f.sent {
		if rp := m.GetResumeProgress(); rp != nil && rp.SpawnId == spawnID {
			out = append(out, rp)
		}
	}
	return out
}

// TestApplyReportWait_EmitsHeartbeatResumeProgress verifies the bead's core fix: while startSpawn
// blocks in AwaitApplyReport waiting for a bundle-carrying apply-report that never arrives, it
// emits a steady stream of ResumeProgress (phase "installing_skills") instead of staying silent
// for the whole wait — the gap the CP's 30s resume stall window (sp-mwco.4.7) needs closed.
func TestApplyReportWait_EmitsHeartbeatResumeProgress(t *testing.T) {
	be := fakeBackend(t, fakepod.WithAttachScript(scriptGoose))
	fs := &fakeCPStream{}
	a := newAttacher(newGooseManager(t, be), fs)
	a.applyReportTimeout = 300 * time.Millisecond
	a.progressHeartbeat = 20 * time.Millisecond

	manifest := `{"artifacts":[{"kind":"skill","name":"s1","targets":["claude"],"bundle":"b1","skill":{"dir":"payloads/s1"}}]}`
	st := startSpawnWithManifest(t, "sp1", []byte(manifest))
	a.startSpawn(context.Background(), st)

	events := resumeProgressFor(fs, "sp1")
	var installing []*nodev1.ResumeProgress
	for _, e := range events {
		if e.Phase == "installing_skills" {
			installing = append(installing, e)
		}
	}
	// 300ms budget / 20ms tick ~= 15 possible ticks + 1 immediate; assert a deliberately slack
	// lower bound — the point is "many, not zero".
	if len(installing) < 3 {
		t.Fatalf("installing_skills ResumeProgress count = %d, want >= 3", len(installing))
	}
	for _, e := range installing {
		if e.Generation != st.Generation {
			t.Errorf("ResumeProgress.Generation = %d, want %d", e.Generation, st.Generation)
		}
		if e.Detail == "" {
			t.Error("ResumeProgress.Detail is empty")
		}
	}

	// Regression: the fatal-bundle contract from sp-mwco.2.7 must be untouched.
	if got := lastPhase(fs.phasesFor("sp1")); got != nodev1.SpawnPhase_ERROR {
		t.Fatalf("terminal phase = %v, want ERROR", got)
	}
	if lastSkillInstallReport(fs, "sp1") == nil {
		t.Fatal("no SkillInstallReport sent")
	}
}

// TestApplyReportWait_NoProgressAfterReport is a goroutine-leak / ordering guard: once
// AwaitApplyReport returns (the report arrived), no further "installing_skills" ResumeProgress may
// be sent after the SkillInstallReport — proving heartbeatProgress's stop() joins the ticker
// goroutine rather than merely signalling it, so message ordering stays deterministic.
func TestApplyReportWait_NoProgressAfterReport(t *testing.T) {
	be := fakeBackend(t, fakepod.WithAttachScript(scriptGoose))
	fs := &fakeCPStream{}
	mgr := newGooseManager(t, be)
	a := newAttacher(mgr, fs)
	a.applyReportTimeout = 2 * time.Second
	a.progressHeartbeat = 5 * time.Millisecond

	manifest := `{"artifacts":[{"kind":"skill","name":"s1","targets":["claude"]}]}`
	st := startSpawnWithManifest(t, "sp1", []byte(manifest))

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

	fs.mu.Lock()
	lastInstallingIdx, reportIdx := -1, -1
	for i, m := range fs.sent {
		if rp := m.GetResumeProgress(); rp != nil && rp.SpawnId == "sp1" && rp.Phase == "installing_skills" {
			lastInstallingIdx = i
		}
		if r := m.GetSkillInstallReport(); r != nil && r.SpawnId == "sp1" {
			reportIdx = i
		}
	}
	fs.mu.Unlock()

	if reportIdx == -1 {
		t.Fatal("no SkillInstallReport sent")
	}
	if lastInstallingIdx > reportIdx {
		t.Errorf("installing_skills ResumeProgress at idx %d sent after SkillInstallReport at idx %d — heartbeat goroutine not joined before proceeding", lastInstallingIdx, reportIdx)
	}
}

// TestHeartbeatProgress_StopsAndIsIdempotent is a direct unit test of the heartbeatProgress helper:
// it emits an immediate event, ticks on progressHeartbeat, stops cleanly (idempotent double-stop),
// and a cancelled context stops the ticker goroutine without an explicit stop() call.
func TestHeartbeatProgress_StopsAndIsIdempotent(t *testing.T) {
	be := fakeBackend(t, fakepod.WithAttachScript(scriptGoose))
	fs := &fakeCPStream{}
	a := newAttacher(newGooseManager(t, be), fs)
	a.progressHeartbeat = 5 * time.Millisecond

	stop := a.heartbeatProgress(context.Background(), "sp-hb", 7, "attaching", "d")
	time.Sleep(60 * time.Millisecond)
	stop()
	stop() // idempotent: must not panic on a double stop

	events := resumeProgressFor(fs, "sp-hb")
	if len(events) < 2 {
		t.Fatalf("attaching ResumeProgress count = %d, want >= 2", len(events))
	}
	for _, e := range events {
		if e.Phase != "attaching" {
			t.Errorf("Phase = %q, want %q", e.Phase, "attaching")
		}
		if e.Generation != 7 {
			t.Errorf("Generation = %d, want 7", e.Generation)
		}
	}

	countAfterStop := len(resumeProgressFor(fs, "sp-hb"))
	time.Sleep(40 * time.Millisecond)
	if got := len(resumeProgressFor(fs, "sp-hb")); got != countAfterStop {
		t.Errorf("events kept arriving after stop(): %d -> %d", countAfterStop, got)
	}

	// Second sub-case: a cancelled context stops the goroutine without an explicit stop() call.
	ctx, cancel := context.WithCancel(context.Background())
	_ = a.heartbeatProgress(ctx, "sp-hb2", 1, "attaching", "d")
	time.Sleep(30 * time.Millisecond)
	cancel()
	time.Sleep(10 * time.Millisecond) // let the goroutine observe ctx.Done() and exit
	countAfterCancel := len(resumeProgressFor(fs, "sp-hb2"))
	time.Sleep(40 * time.Millisecond)
	if got := len(resumeProgressFor(fs, "sp-hb2")); got != countAfterCancel {
		t.Errorf("events kept arriving after ctx cancel: %d -> %d", countAfterCancel, got)
	}
}

// TestTmuxReadyWait_EmitsHeartbeatResumeProgress verifies the tmux-mode `hasSession` poll loop is
// heartbeated too (the stacked-silence path: a slow apply wait followed by tmux readiness could
// otherwise cross the CP's 30s stall window with zero progress even without any single wait
// exceeding it alone).
func TestTmuxReadyWait_EmitsHeartbeatResumeProgress(t *testing.T) {
	var calls int
	be := fakeBackend(t, fakepod.WithAttachScript(scriptGoose))
	fs := &fakeCPStream{}
	a := newAttacher(newGooseManager(t, be), fs)
	a.progressHeartbeat = 20 * time.Millisecond

	a.tmuxHasSessionFn = func(_ context.Context, _, _ string) (bool, error) {
		calls++
		return calls >= 5, nil // ~4 * 150ms = ~600ms of waiting
	}

	a.startSpawn(context.Background(), &nodev1.StartSpawn{
		SpawnId: "sp-tmux-hb",
		AppRef:  writeNodeApp(t),
		Model:   "m",
		Mode:    "tmux",
	})
	defer a.stopSpawn(context.Background(), "sp-tmux-hb")

	if got := lastPhase(fs.phasesFor("sp-tmux-hb")); got != nodev1.SpawnPhase_ACTIVE {
		t.Fatalf("terminal phase = %v, want ACTIVE", got)
	}

	events := resumeProgressFor(fs, "sp-tmux-hb")
	var attaching int
	for _, e := range events {
		if e.Phase == "attaching" {
			attaching++
		}
	}
	if attaching < 2 {
		t.Fatalf("attaching ResumeProgress count = %d, want >= 2", attaching)
	}
}
