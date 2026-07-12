package podbackendtest

import (
	"testing"
)

// contractCase is one named behaviour every PodBackend must have.
type contractCase struct {
	// name is stable: it is what a lane names in Env.Exceptions and what shows up as the subtest.
	name string
	// missingHook returns the name of a required Env hook the lane did not supply ("" = all present).
	// A case with a missing hook FAILS unless the lane registered a named exception — a lane cannot
	// go green by simply not supplying a hook.
	missingHook func(e *Env) string
	run         func(t *testing.T, e *Env)
}

// noHooks is the missingHook for a case that drives the backend through PodBackend alone.
func noHooks(*Env) string { return "" }

// contractCases is THE contract. Adding a case here means every lane must satisfy it or name an
// exception for it.
func contractCases() []contractCase {
	return []contractCase{
		// --- lifecycle (contract_lifecycle.go) ---
		{name: "start_two_phase_ordering", missingHook: noHooks, run: caseStartTwoPhaseOrdering},
		{name: "attach_liveness", missingHook: noHooks, run: caseAttachLiveness},
		{name: "pause_unpause_round_trip", missingHook: noHooks, run: casePauseUnpauseRoundTrip},
		{name: "unpause_of_running_is_error", missingHook: noHooks, run: caseUnpauseOfRunningIsError},
		{name: "pause_double_is_error", missingHook: noHooks, run: casePauseDoubleIsError},
		{name: "restore_forked_source_unpauses", missingHook: noHooks, run: caseRestoreForkedSourceUnpauses},
		{name: "exec_on_paused_fails", missingHook: needExec, run: caseExecOnPausedFails},
		{name: "list_managed_round_trips_labels", missingHook: noHooks, run: caseListManagedRoundTripsLabels},
		{name: "stop_is_idempotent", missingHook: noHooks, run: caseStopIsIdempotent},

		// --- image / capture (contract_image.go) ---
		{name: "capture_as_preserves_source", missingHook: needWrite, run: caseCaptureAsPreservesSource},
		{name: "capture_artifact_inherits_content", missingHook: needWriteAndReadArtifact, run: caseCaptureArtifactInheritsContent},
		{name: "ensure_image_launchable_after_capture", missingHook: needWrite, run: caseEnsureImageLaunchableAfterCapture},
		{name: "capture_layer_count_guard", missingHook: needArmZeroLayer, run: caseCaptureLayerCountGuard},
		{name: "capture_delta_on_paused_agent", missingHook: needWriteAndReadArtifact, run: caseCaptureDeltaOnPausedAgent},
	}
}

func needExec(e *Env) string {
	if e.Exec == nil {
		return "Exec"
	}
	return ""
}

func needWrite(e *Env) string {
	if e.Write == nil {
		return "Write"
	}
	return ""
}

func needWriteAndReadArtifact(e *Env) string {
	if e.Write == nil {
		return "Write"
	}
	if e.ReadArtifact == nil {
		return "ReadArtifact"
	}
	return ""
}

func needArmZeroLayer(e *Env) string {
	if e.ArmZeroLayerCapture == nil {
		return "ArmZeroLayerCapture"
	}
	return ""
}

// RunContract runs the PodBackend contract against the backend f hands back. f is called once per
// case (fresh state per case) — it must be safe to call repeatedly and must register its own cleanup.
//
// Callers: the fakepod arm (hermetic), the Docker backend (tag `e2e`), the CRI backend (tag
// `cri_delta_e2e`). If a case fails for your lane, the answer is to fix the lane or to register a
// NAMED exception in Env.Exceptions — never to weaken the assertion for everyone.
func RunContract(t *testing.T, f Factory) {
	t.Helper()
	cases := contractCases()

	names := make([]string, 0, len(cases))
	for _, c := range cases {
		names = append(names, c.name)
	}

	// Validate the lane's Env once, up front, against a throwaway instance.
	probe := f(t)
	if probe == nil {
		t.Fatal("podbackendtest: Factory returned a nil Env")
	}
	if err := validateEnv(probe); err != nil {
		t.Fatal(err)
	}
	if err := validateExceptions(names, probe.Exceptions); err != nil {
		t.Fatal(err)
	}
	exceptions := probe.Exceptions

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if reason, excepted := exceptions[c.name]; excepted {
				// NOT t.Skip: this is a recorded, reviewed divergence, not a missing dependency. It is
				// logged and the subtest passes — the review gate is that it had to be written down.
				t.Logf("CONTRACT EXCEPTION [%s]: %s", c.name, reason)
				return
			}
			e := f(t)
			if e == nil {
				t.Fatal("podbackendtest: Factory returned a nil Env")
			}
			if hook := c.missingHook(e); hook != "" {
				t.Fatalf("contract case %q needs Env.%s, which this lane did not supply. "+
					"Supply the hook, or register a named exception: Env.Exceptions[%q] = \"<why this lane cannot>\"",
					c.name, hook, c.name)
			}
			c.run(t, e)
		})
	}
}
