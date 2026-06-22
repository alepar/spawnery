package node

import (
	"context"
	"fmt"
	"testing"

	nodev1 "spawnery/gen/node/v1"
	"spawnery/internal/runtime"
	"spawnery/internal/spawnlet"
)

// --- helpers ---

// stepStatusesFor filters the CP stream to SpawnStatus messages with StepTotal>0 for spawnID.
func (f *fakeCPStream) stepStatusesFor(spawnID string) []*nodev1.SpawnStatus {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []*nodev1.SpawnStatus
	for _, m := range f.sent {
		if s := m.GetStatus(); s != nil && s.SpawnId == spawnID && s.StepTotal > 0 {
			out = append(out, s)
		}
	}
	return out
}

// startPodFailBackend is a scriptedPodBackend variant whose StartPod always returns an error.
type startPodFailBackend struct{ scriptedPodBackend }

func (f *startPodFailBackend) StartPod(context.Context, runtime.PodSpec) (*runtime.PodHandle, error) {
	return nil, fmt.Errorf("startpod boom")
}

// --- tests ---

// TestStepEmissionHappyPath asserts that startSpawn emits provisioning milestones in order
// (monotonically increasing StepIndex, constant StepTotal=N, correct StepKey/Label), followed
// by an ACTIVE terminal status — for a basic ACP spawn with no GitHub/resume/egress.
func TestStepEmissionHappyPath(t *testing.T) {
	be := &scriptedPodBackend{script: scriptGoose}
	fs := &fakeCPStream{}
	a := newAttacher(newGooseManager(t, be), fs)
	a.startSpawn(context.Background(), &nodev1.StartSpawn{SpawnId: "sp1", AppRef: writeNodeApp(t), Model: "m"})
	defer a.stopSpawn(context.Background(), "sp1")

	steps := fs.stepStatusesFor("sp1")
	if len(steps) == 0 {
		t.Fatal("no milestone step statuses emitted")
	}

	// Compute expected subset (basic ACP, no github/resume/egress).
	flags := spawnlet.ProvisionFlags{AwaitReady: true}
	want := spawnlet.ApplicableMilestones(flags)
	wantN := uint32(len(want))

	if uint32(len(steps)) != wantN {
		keys := make([]string, len(steps))
		for i, s := range steps {
			keys[i] = s.StepKey
		}
		t.Fatalf("got %d step statuses (%v), want %d", len(steps), keys, wantN)
	}

	// Verify ordered, monotonic, constant N, correct keys+labels, all STARTING.
	for i, s := range steps {
		if s.StepTotal != wantN {
			t.Errorf("step[%d] StepTotal=%d, want %d", i, s.StepTotal, wantN)
		}
		wantIdx := uint32(i + 1)
		if s.StepIndex != wantIdx {
			t.Errorf("step[%d] StepIndex=%d, want %d", i, s.StepIndex, wantIdx)
		}
		if s.StepKey != want[i].Key {
			t.Errorf("step[%d] StepKey=%q, want %q", i, s.StepKey, want[i].Key)
		}
		if s.StepLabel != want[i].Label {
			t.Errorf("step[%d] StepLabel=%q, want %q", i, s.StepLabel, want[i].Label)
		}
		if s.Phase != nodev1.SpawnPhase_STARTING {
			t.Errorf("step[%d] Phase=%v, want STARTING", i, s.Phase)
		}
	}

	// Terminal phase must be ACTIVE.
	if got := lastPhase(fs.phasesFor("sp1")); got != nodev1.SpawnPhase_ACTIVE {
		t.Errorf("terminal phase = %v, want ACTIVE", got)
	}
}

// TestStepFailureAttributesCreatePod asserts that a StartPod error attributes to create-pod
// (StepTotal>0, StepKey=create-pod, Phase=ERROR). The create-pod milestone is emitted inside
// CreateWithSelection before StartPod, so current=create-pod when the error is returned.
func TestStepFailureAttributesCreatePod(t *testing.T) {
	be := &startPodFailBackend{}
	fs := &fakeCPStream{}
	a := newAttacher(newGooseManager(t, be), fs)
	a.startSpawn(context.Background(), &nodev1.StartSpawn{SpawnId: "sp1", AppRef: writeNodeApp(t), Model: "m"})

	// Must end with ERROR.
	if got := lastPhase(fs.phasesFor("sp1")); got != nodev1.SpawnPhase_ERROR {
		t.Fatalf("terminal phase = %v, want ERROR", got)
	}

	// Find the ERROR step status (StepTotal>0).
	var errStep *nodev1.SpawnStatus
	for _, s := range fs.stepStatusesFor("sp1") {
		if s.Phase == nodev1.SpawnPhase_ERROR {
			errStep = s
			break
		}
	}
	if errStep == nil {
		t.Fatal("no ERROR step status emitted (StepTotal=0 on error)")
	}
	if errStep.StepKey != spawnlet.MilestoneCreatePod {
		t.Errorf("error step key = %q, want %q", errStep.StepKey, spawnlet.MilestoneCreatePod)
	}
	if errStep.StepTotal == 0 {
		t.Error("error step StepTotal=0, want >0")
	}
}

// TestStepFailureAttributesPrepareMounts asserts that a failure during manifest parsing (which
// occurs inside CreateWithSelection after prepare-mounts is emitted) attributes to prepare-mounts.
func TestStepFailureAttributesPrepareMounts(t *testing.T) {
	mgr := newGooseManager(t, &scriptedPodBackend{script: scriptGoose})
	fs := &fakeCPStream{}
	a := newAttacher(mgr, fs)
	// A bogus AppRef causes manifest.Parse to fail inside CreateWithSelection, AFTER the
	// prepare-mounts milestone has been emitted.
	a.startSpawn(context.Background(), &nodev1.StartSpawn{SpawnId: "sp1", AppRef: "/no/such/app", Model: "m"})

	if got := lastPhase(fs.phasesFor("sp1")); got != nodev1.SpawnPhase_ERROR {
		t.Fatalf("terminal phase = %v, want ERROR", got)
	}

	var errStep *nodev1.SpawnStatus
	for _, s := range fs.stepStatusesFor("sp1") {
		if s.Phase == nodev1.SpawnPhase_ERROR {
			errStep = s
			break
		}
	}
	if errStep == nil {
		t.Fatal("no ERROR step status emitted (StepTotal=0 on error)")
	}
	if errStep.StepKey != spawnlet.MilestonePrepareMounts {
		t.Errorf("error step key = %q, want %q", errStep.StepKey, spawnlet.MilestonePrepareMounts)
	}
	if errStep.StepTotal == 0 {
		t.Error("error step StepTotal=0, want >0")
	}
}

// TestStepSubsetMatchesCatalog asserts that the emitted STARTING step keys for a basic ACP
// spawn match exactly the ApplicableMilestones output — no extra steps, no missing steps.
func TestStepSubsetMatchesCatalog(t *testing.T) {
	be := &scriptedPodBackend{script: scriptGoose}
	fs := &fakeCPStream{}
	a := newAttacher(newGooseManager(t, be), fs)
	a.startSpawn(context.Background(), &nodev1.StartSpawn{SpawnId: "sp1", AppRef: writeNodeApp(t), Model: "m"})
	defer a.stopSpawn(context.Background(), "sp1")

	// Basic ACP spawn: AwaitReady=true, everything else false.
	flags := spawnlet.ProvisionFlags{AwaitReady: true}
	expected := spawnlet.ApplicableMilestones(flags)

	got := fs.stepStatusesFor("sp1")
	// Filter to STARTING only (ignore the potential ERROR which is absent on success).
	var startingSteps []*nodev1.SpawnStatus
	for _, s := range got {
		if s.Phase == nodev1.SpawnPhase_STARTING {
			startingSteps = append(startingSteps, s)
		}
	}

	if len(startingSteps) != len(expected) {
		t.Fatalf("emitted %d STARTING step statuses, want %d (catalog=%v)", len(startingSteps), len(expected), catalogKeys(expected))
	}
	for i, s := range startingSteps {
		if s.StepKey != expected[i].Key {
			t.Errorf("step[%d] key=%q, want %q", i, s.StepKey, expected[i].Key)
		}
	}
}

func catalogKeys(ms []spawnlet.Milestone) []string {
	out := make([]string, len(ms))
	for i, m := range ms {
		out[i] = m.Key
	}
	return out
}
