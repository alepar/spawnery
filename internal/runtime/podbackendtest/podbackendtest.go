// Package podbackendtest is the cross-lane behavioural contract for runtime.PodBackend.
//
// RunContract(t, f) runs one table of named cases against whatever backend the Factory hands back.
// Three callers run it: fakepod (hermetic, every `go test ./...`), the Docker backend (build tag
// `e2e`) and the CRI backend (build tag `cri_delta_e2e`). Anything the fake asserts, the real lanes
// must also assert — otherwise the fake is fiction and a green hermetic suite means nothing.
//
// A lane that genuinely cannot support a case registers a NAMED exception in Env.Exceptions
// (case name -> reason). It may not simply omit the hook and stay green: a case whose required hook
// is missing FAILS unless an exception is registered, and an exception naming an unknown case (a
// typo) FAILS too. Dropping an assertion must always be a visible, reviewed act.
package podbackendtest

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"testing"

	"spawnery/internal/runtime"
)

// Factory produces a live backend + the hooks the contract needs. RunContract calls it ONCE PER CASE,
// so each case gets clean state; the Factory must register its own teardown with t.Cleanup.
type Factory func(t *testing.T) *Env

// Env is one lane's live PodBackend plus what the contract needs to drive it. The required fields
// are required of every lane; the hooks are optional, but a case that needs a missing hook fails
// unless the lane registered a named exception for it.
type Env struct {
	// --- required of every lane ---

	// Backend is the live backend under contract.
	Backend runtime.PodBackend
	// NodeID is the node id stamped into the pod labels (round-tripped through ListManaged).
	NodeID string
	// BaseImage is a launchable agent image ref for this lane. It is also what the contract pins as
	// PodHandle.BaseImageRef, exactly as Manager.Create does.
	BaseImage string
	// RootfsFile is a path inside the agent that (a) the agent can write, (b) is NOT under any mount
	// in AgentSpec.Mounts, and (c) therefore lands in the writable layer a delta capture commits.
	RootfsFile string
	// PodSpec / AgentSpec build the lane's specs. Each lane owns its own images, argv form (Docker's
	// Cmd overrides CMD, CRI's overrides ENTRYPOINT — see the PodBackend doc) and runtime handler.
	PodSpec   func(spawnID string, labels map[string]string) runtime.PodSpec
	AgentSpec func(spawnID, imageRef string, labels map[string]string) runtime.AgentSpec

	// --- optional hooks (case-gated; a missing one needs a named exception) ---

	// Write writes data to file inside h's RUNNING agent. It must FAIL if the agent is not running —
	// "the source still takes writes" is how the contract proves a fork did not destroy its source.
	Write func(ctx context.Context, h *runtime.PodHandle, file string, data []byte) error
	// ReadArtifact reads file out of the captured delta image ref (however the lane can: read the
	// image content, or launch a throwaway container from it and cat the file).
	ReadArtifact func(ctx context.Context, ref, file string) ([]byte, error)
	// Exec runs argv inside h's agent container. It must FAIL on a paused container, as `docker exec`
	// does — that is why the suspend teardown unpauses before it scrubs.
	Exec func(ctx context.Context, h *runtime.PodHandle, argv []string) error
	// ArmZeroLayerCapture forces the lane's next capture to commit a layer count <= the base's, so the
	// moby#47065 guard must fire. It returns a disarm func.
	ArmZeroLayerCapture func() (disarm func())

	// Exceptions maps a contract case name to the reason this lane cannot support it. Both the name
	// and the reason are validated: a typo'd name, or an empty reason, fails the run.
	Exceptions map[string]string
}

// validateEnv checks the fields every lane must supply.
func validateEnv(e *Env) error {
	var missing []string
	if e.Backend == nil {
		missing = append(missing, "Backend")
	}
	if e.NodeID == "" {
		missing = append(missing, "NodeID")
	}
	if e.BaseImage == "" {
		missing = append(missing, "BaseImage")
	}
	if e.RootfsFile == "" {
		missing = append(missing, "RootfsFile")
	}
	if e.PodSpec == nil {
		missing = append(missing, "PodSpec")
	}
	if e.AgentSpec == nil {
		missing = append(missing, "AgentSpec")
	}
	if len(missing) > 0 {
		return fmt.Errorf("podbackendtest: Env is missing required field(s): %s", strings.Join(missing, ", "))
	}
	return nil
}

// validateExceptions rejects an exception for a case name that does not exist (a typo would silently
// disable nothing while looking like coverage) and an exception with no reason (a dropped assertion).
func validateExceptions(cases []string, ex map[string]string) error {
	if len(ex) == 0 {
		return nil
	}
	known := make(map[string]bool, len(cases))
	for _, c := range cases {
		known[c] = true
	}
	var bad []string
	for name, reason := range ex {
		switch {
		case !known[name]:
			bad = append(bad, fmt.Sprintf("%q: no such contract case", name))
		case strings.TrimSpace(reason) == "":
			bad = append(bad, fmt.Sprintf("%q: empty reason (an exception without a reason is a dropped assertion)", name))
		}
	}
	if len(bad) == 0 {
		return nil
	}
	sort.Strings(bad)
	return fmt.Errorf("podbackendtest: invalid Env.Exceptions: %s (known cases: %s)",
		strings.Join(bad, "; "), strings.Join(cases, ", "))
}

// Labels builds the standard spawnery pod labels the Manager applies.
func Labels(spawnID, nodeID string, generation uint64) map[string]string {
	return map[string]string{
		runtime.LabelManaged:    "true",
		runtime.LabelSpawnID:    spawnID,
		runtime.LabelGeneration: strconv.FormatUint(generation, 10),
		runtime.LabelNodeID:     nodeID,
	}
}

// uniqueSpawnID returns a fresh spawn id that is also a valid Docker tag component (DeltaTag embeds
// it in "spawnery/delta:<id>").
func uniqueSpawnID(t *testing.T) string {
	t.Helper()
	var b [6]byte
	if _, err := rand.Read(b[:]); err != nil {
		t.Fatalf("rand: %v", err)
	}
	return "ct" + hex.EncodeToString(b[:])
}

// startPod brings a pod up through the two-phase path and launches the agent from imageRef, with the
// handle filled in exactly as Manager.Create does (SpawnID + the pinned BaseImageRef). Teardown is
// registered with t.Cleanup.
func startPod(ctx context.Context, t *testing.T, e *Env, spawnID, imageRef string, generation uint64) *runtime.PodHandle {
	t.Helper()
	labels := Labels(spawnID, e.NodeID, generation)
	h, err := e.Backend.StartPod(ctx, e.PodSpec(spawnID, labels))
	if err != nil {
		t.Fatalf("StartPod(%s): %v", spawnID, err)
	}
	// The backends deliberately leave these empty; the Manager fills them in before any capture.
	h.SpawnID = spawnID
	h.BaseImageRef = e.BaseImage
	t.Cleanup(func() { _ = e.Backend.Stop(context.WithoutCancel(ctx), h) })
	if err := e.Backend.StartAgent(ctx, h, e.AgentSpec(spawnID, imageRef, labels)); err != nil {
		t.Fatalf("StartAgent(%s, image=%s): %v", spawnID, imageRef, err)
	}
	return h
}

// closeStream closes an AttachedStream if it carries a Close func.
func closeStream(s *runtime.AttachedStream) {
	if s != nil && s.Close != nil {
		_ = s.Close()
	}
}

// findManaged returns the ManagedPod for spawnID, or false.
func findManaged(pods []runtime.ManagedPod, spawnID string) (runtime.ManagedPod, bool) {
	for _, p := range pods {
		if p.SpawnID == spawnID {
			return p, true
		}
	}
	return runtime.ManagedPod{}, false
}
