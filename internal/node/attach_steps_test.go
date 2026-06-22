package node

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	nodev1 "spawnery/gen/node/v1"
	"spawnery/internal/runtime"
	"spawnery/internal/spawnlet"
	"spawnery/internal/storage/journal"
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

// --- Fix 2: mint-credentials failure attribution ---

// TestStepFailureAttributesMintCredentials asserts that when mintGitHubMountsAtProvision
// fails (no mint channel configured), the terminal ERROR SpawnStatus carries
// StepKey=="mint-credentials" and StepTotal>0. Mirrors the create-pod and prepare-mounts
// failure tests above.
func TestStepFailureAttributesMintCredentials(t *testing.T) {
	// A mount with a non-empty GithubMintRef.SecretId causes MintCredentials=true,
	// which adds mint-credentials to the applicable subset and emits emitStep before
	// mintGitHubMountsAtProvision is called.
	mounts := []*nodev1.MountBinding{{
		Name:          "main",
		BackendUri:    "github:octo/demo",
		GithubMintRef: &nodev1.GitHubMintRef{SecretId: "gh:octo"},
	}}

	be := &scriptedPodBackend{script: scriptGoose}
	fs := &fakeCPStream{}
	a := newAttacher(newGooseManager(t, be), fs)
	// nil mint client: MintInitial returns "github mint client unavailable" — causes the
	// mintGitHubMountsAtProvision call in startSpawn to fail and hit emitErr.
	a.githubRefresh = newGitHubRefresher(nil)

	a.startSpawn(context.Background(), &nodev1.StartSpawn{
		SpawnId: "sp1", AppRef: writeNodeApp(t), Model: "m", Mounts: mounts,
	})

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
		t.Fatal("no ERROR step status emitted (StepTotal=0 on mint-credentials error)")
	}
	if errStep.StepKey != spawnlet.MilestoneMintCredentials {
		t.Errorf("error step key = %q, want %q", errStep.StepKey, spawnlet.MilestoneMintCredentials)
	}
	if errStep.StepTotal == 0 {
		t.Error("error step StepTotal=0, want >0")
	}
}

// --- Fix 3: non-fatal restore-snapshot stays ACTIVE ---

// restoreFailJournal wraps fakeNodeJournal and overrides RestoreGeneration to always fail,
// exercising the non-fatal journal-restore fallback path (manager.go ~1075-1089).
type restoreFailJournal struct {
	fakeNodeJournal
}

func (f *restoreFailJournal) RestoreGeneration(_ context.Context, _ string, _ uint64, _ string, _ journal.ManifestID, _ string) error {
	return fmt.Errorf("restore boom: simulated disk failure")
}

// TestNonFatalRestoreSnapshotDoesNotErrorStaysActive verifies the non-fatal restore path:
// when journal.RestoreGeneration fails during provision, the node (a) still emits a
// restore-snapshot STARTING step, (b) does NOT emit an ERROR SpawnStatus attributed to
// restore-snapshot, and (c) the spawn still reaches ACTIVE (falls back to the seeded dir).
func TestNonFatalRestoreSnapshotDoesNotErrorStaysActive(t *testing.T) {
	be := &scriptedPodBackend{script: scriptGoose}

	// stateDir holds the journal-state JSON that makes HasJournalPins("sp1") return true.
	// journalRecord format (internal/spawnlet/journalstate.go): {"generation":N,"manifests":{...}}.
	stateDir := t.TempDir()
	stateFile := filepath.Join(stateDir, "sp1.json")
	if err := os.WriteFile(stateFile, []byte(`{"generation":1,"manifests":{"main":"manifest-abc"}}`), 0o600); err != nil {
		t.Fatalf("write journal state: %v", err)
	}

	mgr := spawnlet.NewManagerWithBackend(be, noopApplier{}, spawnlet.ManagerConfig{
		NodeID: "node-test", AgentImage: "a", SidecarImage: "s", DataRoot: t.TempDir(),
	})
	// restoreFailJournal makes RestoreGeneration fail; the manager logs and falls back to seed.
	mgr.SetJournal(&restoreFailJournal{}, stateDir)

	fs := &fakeCPStream{}
	a := newAttacher(mgr, fs)
	// writeNodeJournalApp has a node-local durability mount named "main", so the journal seam
	// engages and the restore path runs (HasJournalPins is true, manifest pin found for "main").
	a.startSpawn(context.Background(), &nodev1.StartSpawn{
		SpawnId: "sp1", AppRef: writeNodeJournalApp(t), Model: "m", Generation: 1,
	})
	defer a.stopSpawn(context.Background(), "sp1")

	// (c) spawn must reach ACTIVE despite the restore failure (non-fatal fallback).
	if got := lastPhase(fs.phasesFor("sp1")); got != nodev1.SpawnPhase_ACTIVE {
		t.Fatalf("terminal phase = %v, want ACTIVE (restore failure is non-fatal)", got)
	}

	// (a) restore-snapshot STARTING step must have been emitted.
	var gotRestoreStarting bool
	for _, s := range fs.stepStatusesFor("sp1") {
		if s.Phase == nodev1.SpawnPhase_STARTING && s.StepKey == spawnlet.MilestoneRestoreSnapshot {
			gotRestoreStarting = true
			break
		}
	}
	if !gotRestoreStarting {
		t.Fatal("restore-snapshot STARTING step not emitted")
	}

	// (b) no ERROR status attributed to restore-snapshot — it is non-fatal.
	for _, s := range fs.stepStatusesFor("sp1") {
		if s.Phase == nodev1.SpawnPhase_ERROR && s.StepKey == spawnlet.MilestoneRestoreSnapshot {
			t.Fatalf("got ERROR with StepKey=%q: restore failure must be non-fatal, not ERROR", s.StepKey)
		}
	}
}
