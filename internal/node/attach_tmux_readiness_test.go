package node

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	nodev1 "spawnery/gen/node/v1"
	"spawnery/internal/agentcaps"
	"spawnery/internal/spawnlet"
)

// TestStartSpawnTmuxReadinessGateGoesActiveOnlyAfterHasSessionTrue verifies Part 1 of sp-m859.4:
// a tmux-mode startSpawn must NOT emit ACTIVE until has-session returns true, and must emit the
// await-ready milestone before polling.
func TestStartSpawnTmuxReadinessGateGoesActiveOnlyAfterHasSessionTrue(t *testing.T) {
	var calls atomic.Int32
	be := &scriptedPodBackend{script: scriptGoose}
	fs := &fakeCPStream{}
	a := newAttacher(newGooseManager(t, be), fs)

	// has-session returns false twice then true; we verify ACTIVE only arrives after the third call.
	a.tmuxHasSessionFn = func(_ context.Context, _, _ string) (bool, error) {
		n := calls.Add(1)
		return n >= 3, nil // false, false, true
	}

	a.startSpawn(context.Background(), &nodev1.StartSpawn{
		SpawnId: "sp-tmux-1",
		AppRef:  writeNodeApp(t),
		Model:   "m",
		Mode:    string(agentcaps.ModeTmux),
	})
	defer a.stopSpawn(context.Background(), "sp-tmux-1")

	// Must end ACTIVE.
	if got := lastPhase(fs.phasesFor("sp-tmux-1")); got != nodev1.SpawnPhase_ACTIVE {
		t.Fatalf("terminal phase = %v, want ACTIVE", got)
	}

	// has-session must have been called at least 3 times (false, false, true).
	if got := calls.Load(); got < 3 {
		t.Fatalf("hasSession called %d times, want >= 3", got)
	}

	// The await-ready milestone must have been emitted as a STARTING step.
	steps := fs.stepStatusesFor("sp-tmux-1")
	var gotAwaitReady bool
	for _, s := range steps {
		if s.Phase == nodev1.SpawnPhase_STARTING && s.StepKey == spawnlet.MilestoneAwaitReady {
			gotAwaitReady = true
			break
		}
	}
	if !gotAwaitReady {
		t.Fatal("await-ready STARTING step not emitted for tmux-mode spawn")
	}

	// The tmux relay must be registered and the session registry must exist.
	a.mu.Lock()
	hasRelay := a.tmuxRelays[zeroKey("sp-tmux-1")] != nil
	hasReg := a.sessions["sp-tmux-1"] != nil
	a.mu.Unlock()
	if !hasRelay {
		t.Fatal("tmux relay not registered after ACTIVE")
	}
	if !hasReg {
		t.Fatal("session registry not registered after ACTIVE")
	}
}

// TestStartSpawnTmuxReadinessGateContextCancelErrors verifies that when the context is cancelled
// before has-session returns true, startSpawn emits ERROR (not ACTIVE) and does not register a relay.
func TestStartSpawnTmuxReadinessGateContextCancelErrors(t *testing.T) {
	be := &scriptedPodBackend{script: scriptGoose}
	fs := &fakeCPStream{}
	a := newAttacher(newGooseManager(t, be), fs)

	// has-session always returns false.
	a.tmuxHasSessionFn = func(_ context.Context, _, _ string) (bool, error) {
		return false, nil
	}

	// Use a context that cancels quickly to trigger the timeout path.
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	a.startSpawn(ctx, &nodev1.StartSpawn{
		SpawnId: "sp-tmux-timeout",
		AppRef:  writeNodeApp(t),
		Model:   "m",
		Mode:    string(agentcaps.ModeTmux),
	})

	// Must end ERROR, not ACTIVE.
	if got := lastPhase(fs.phasesFor("sp-tmux-timeout")); got != nodev1.SpawnPhase_ERROR {
		t.Fatalf("terminal phase = %v, want ERROR on readiness timeout", got)
	}

	// The ERROR must carry StepKey=await-ready and StepTotal>0.
	var errStep *nodev1.SpawnStatus
	for _, s := range fs.stepStatusesFor("sp-tmux-timeout") {
		if s.Phase == nodev1.SpawnPhase_ERROR {
			errStep = s
			break
		}
	}
	if errStep == nil {
		t.Fatal("no ERROR step status (StepTotal>0) emitted on tmux readiness timeout")
	}
	if errStep.StepKey != spawnlet.MilestoneAwaitReady {
		t.Errorf("error step key = %q, want %q", errStep.StepKey, spawnlet.MilestoneAwaitReady)
	}
	if errStep.StepTotal == 0 {
		t.Error("error step StepTotal=0, want >0")
	}

	// No relay must have been registered.
	a.mu.Lock()
	hasRelay := a.tmuxRelays[zeroKey("sp-tmux-timeout")] != nil
	act := a.active
	a.mu.Unlock()
	if hasRelay {
		t.Fatal("tmux relay must not be registered after readiness timeout")
	}
	if act != 0 {
		t.Fatalf("active = %d, want 0 after readiness timeout", act)
	}
}

// TestStartSpawnTmuxAwaitReadyInMilestoneSet verifies that AwaitReady=true for tmux-mode spawns
// means ApplicableMilestones includes await-ready, which in turn means the step catalog for a
// tmux spawn now includes await-ready (regression guard against sp-m859.4 Part 1 being reverted).
func TestStartSpawnTmuxAwaitReadyInMilestoneSet(t *testing.T) {
	flags := spawnlet.ProvisionFlags{AwaitReady: true} // as set by tmux-mode startSpawn
	steps := spawnlet.ApplicableMilestones(flags)
	var found bool
	for _, s := range steps {
		if s.Key == spawnlet.MilestoneAwaitReady {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("await-ready milestone not in ApplicableMilestones({AwaitReady:true}) — catalog regression")
	}
}
