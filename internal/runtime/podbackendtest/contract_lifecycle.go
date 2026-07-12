package podbackendtest

import (
	"testing"

	"spawnery/internal/runtime"
)

// caseStartTwoPhaseOrdering pins the two-phase contract: the pod (sandbox+sidecar) must exist before
// the agent can be started into it. That ordering is what lets the egress floor be applied after the
// pod IP exists and BEFORE the untrusted agent runs — a StartAgent that quietly succeeds without a pod
// is an agent running outside the floor.
//
// NOTE it uses a handle with NON-EXISTENT ids, not an empty one: Docker's StartAgent with NetnsOf=""
// would legitimately start an unattached container. "Non-existent pod" is the portable assertion.
func caseStartTwoPhaseOrdering(t *testing.T, e *Env) {
	ctx := t.Context()
	id := uniqueSpawnID(t)
	labels := Labels(id, e.NodeID, 1)

	ghost := &runtime.PodHandle{
		SpawnID:      id,
		BaseImageRef: e.BaseImage,
		SidecarID:    id + "-nonexistent",
		SandboxID:    id + "-nonexistent",
	}
	if err := e.Backend.StartAgent(ctx, ghost, e.AgentSpec(id, e.BaseImage, labels)); err == nil {
		t.Fatal("StartAgent into a pod that was never started: got nil error, want failure " +
			"(the two-phase contract is what keeps the agent from starting outside the egress floor)")
	}

	// The ordered path works and StartAgent records the agent id on the handle — the Manager persists
	// that id and hands it back to Stop/CaptureDelta, so an unset one means teardown has no target.
	h := startPod(ctx, t, e, id, e.BaseImage, 1)
	if h.AgentID == "" {
		t.Fatalf("after StartAgent, handle.AgentID is empty: %+v (teardown and capture address the agent by this id)", h)
	}
}

// caseAttachLiveness pins Attach as a liveness oracle: it succeeds against a running agent and fails
// once the pod is torn down. (It deliberately says NOTHING about a paused agent: on the real lanes
// Attach is a TCP dial to the in-pod adapter and a frozen process's listening socket still completes
// the handshake.)
func caseAttachLiveness(t *testing.T, e *Env) {
	ctx := t.Context()
	id := uniqueSpawnID(t)
	h := startPod(ctx, t, e, id, e.BaseImage, 1)

	s, err := e.Backend.Attach(ctx, h)
	if err != nil {
		t.Fatalf("Attach to a running agent: %v", err)
	}
	if s == nil || s.Stdin == nil || s.Stdout == nil {
		t.Fatalf("Attach returned an incomplete stream: %+v", s)
	}
	closeStream(s)

	if err := e.Backend.Stop(ctx, h); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	s2, err := e.Backend.Attach(ctx, h)
	if err == nil {
		closeStream(s2)
		t.Fatal("Attach to a torn-down pod: got nil error, want failure " +
			"(a silent success here is a client wired to a dead agent)")
	}
}

// casePauseUnpauseRoundTrip pins the suspend gate's quiescence primitive: pause freezes the agent,
// unpause brings it back, and the agent is serving again afterwards.
func casePauseUnpauseRoundTrip(t *testing.T, e *Env) {
	ctx := t.Context()
	id := uniqueSpawnID(t)
	h := startPod(ctx, t, e, id, e.BaseImage, 1)

	if err := e.Backend.Pause(ctx, h); err != nil {
		t.Fatalf("Pause: %v", err)
	}
	if err := e.Backend.Unpause(ctx, h); err != nil {
		t.Fatalf("Unpause after Pause: %v", err)
	}
	s, err := e.Backend.Attach(ctx, h)
	if err != nil {
		t.Fatalf("Attach after Pause/Unpause: %v (the agent did not come back)", err)
	}
	closeStream(s)
}

// caseUnpauseOfRunningIsError: unpausing an agent that is not paused must report it. The suspend
// teardown tolerates that error; it must not be a silent no-op, or "was it actually paused?" becomes
// unanswerable.
func caseUnpauseOfRunningIsError(t *testing.T, e *Env) {
	ctx := t.Context()
	id := uniqueSpawnID(t)
	h := startPod(ctx, t, e, id, e.BaseImage, 1)

	if err := e.Backend.Unpause(ctx, h); err == nil {
		t.Fatal("Unpause of a RUNNING agent: got nil error, want a \"not paused\" failure")
	}
}

// casePauseDoubleIsError: a second Pause of an already-paused agent must fail, not silently no-op.
// A no-op here lets a double-suspend look like it quiesced when it never did.
func casePauseDoubleIsError(t *testing.T, e *Env) {
	ctx := t.Context()
	id := uniqueSpawnID(t)
	h := startPod(ctx, t, e, id, e.BaseImage, 1)

	if err := e.Backend.Pause(ctx, h); err != nil {
		t.Fatalf("Pause: %v", err)
	}
	if err := e.Backend.Pause(ctx, h); err == nil {
		t.Fatal("second Pause of an already-paused agent: got nil error, want failure")
	}
	if err := e.Backend.Unpause(ctx, h); err != nil {
		t.Fatalf("Unpause: %v", err)
	}
}

// caseRestoreForkedSourceUnpauses: on both lanes a source-preserving capture only ever PAUSES the
// source, so restoring it is an unpause. Pinned separately from Unpause because fork calls this one.
func caseRestoreForkedSourceUnpauses(t *testing.T, e *Env) {
	ctx := t.Context()
	id := uniqueSpawnID(t)
	h := startPod(ctx, t, e, id, e.BaseImage, 1)

	if err := e.Backend.Pause(ctx, h); err != nil {
		t.Fatalf("Pause: %v", err)
	}
	if err := e.Backend.RestoreForkedSource(ctx, h); err != nil {
		t.Fatalf("RestoreForkedSource on a paused source: %v", err)
	}
	s, err := e.Backend.Attach(ctx, h)
	if err != nil {
		t.Fatalf("Attach after RestoreForkedSource: %v (the forked source did not come back)", err)
	}
	closeStream(s)

	// On an agent that was never paused it reports "not paused" — the early-failure path tolerates
	// that error, but it must BE an error (see the PodBackend doc on RestoreForkedSource).
	if err := e.Backend.RestoreForkedSource(ctx, h); err == nil {
		t.Fatal("RestoreForkedSource on a RUNNING source: got nil error, want a \"not paused\" failure")
	}
}

// caseExecOnPausedFails pins the constraint the whole suspend ORDERING hangs on: you cannot exec into
// a paused container. That is why the pre-SE2 teardown unpauses before it scrubs — and unpausing
// reopens the write window, which is SE2's torn snapshot. If exec-on-paused ever silently succeeded,
// the ordering bug would look like a free lunch.
func caseExecOnPausedFails(t *testing.T, e *Env) {
	ctx := t.Context()
	id := uniqueSpawnID(t)
	h := startPod(ctx, t, e, id, e.BaseImage, 1)

	// Sanity: exec works on a RUNNING agent, so a failure below is about the pause, not about exec.
	if err := e.Exec(ctx, h, []string{"true"}); err != nil {
		t.Fatalf("exec on a running agent: %v", err)
	}
	if err := e.Backend.Pause(ctx, h); err != nil {
		t.Fatalf("Pause: %v", err)
	}
	if err := e.Exec(ctx, h, []string{"true"}); err == nil {
		t.Fatal("exec on a PAUSED agent: got nil error, want failure (a frozen container cannot run a process)")
	}
	if err := e.Backend.Unpause(ctx, h); err != nil {
		t.Fatalf("Unpause: %v", err)
	}
}

// caseListManagedRoundTripsLabels: the Manager reaps orphans, fences stale generations, AND re-adopts
// surviving pods from what ListManaged reports. A generation that does not round-trip is a fence that
// cannot fire; a missing container id or pod IP is a pod the node cannot re-adopt after a restart (it
// cannot name the containers, re-dial ACP, or re-scope the egress floor) — see the SE3 design §4.5.
//
// Every lane must report SidecarID, AgentID and PodIP for a fully-started pod. SandboxID stays
// lane-specific (Docker has no sandbox) and is deliberately not asserted here.
// It deliberately does NOT assert that a Stopped pod disappears: Docker's Stop stops but does not
// remove, so its labelled containers linger.
func caseListManagedRoundTripsLabels(t *testing.T, e *Env) {
	ctx := t.Context()
	id := uniqueSpawnID(t)
	const gen = uint64(7)
	h := startPod(ctx, t, e, id, e.BaseImage, gen)

	pods, err := e.Backend.ListManaged(ctx)
	if err != nil {
		t.Fatalf("ListManaged: %v", err)
	}
	p, ok := findManaged(pods, id)
	if !ok {
		t.Fatalf("ListManaged does not report the running pod %s: %+v "+
			"(a pod the node cannot see is a pod it cannot reap)", id, pods)
	}
	if p.Generation != gen {
		t.Errorf("ListManaged generation = %d, want %d (the generation fence reads this)", p.Generation, gen)
	}
	if p.NodeID != e.NodeID {
		t.Errorf("ListManaged node id = %q, want %q", p.NodeID, e.NodeID)
	}
	if p.SidecarID != h.SidecarID {
		t.Errorf("ListManaged sidecar id = %q, want %q (the id StartPod handed back) "+
			"— re-adoption rebuilds the PodHandle from this", p.SidecarID, h.SidecarID)
	}
	if p.AgentID != h.AgentID {
		t.Errorf("ListManaged agent id = %q, want %q (the id StartAgent handed back) "+
			"— without it a re-adopted spawn cannot be captured, paused or torn down", p.AgentID, h.AgentID)
	}
	if p.PodIP == "" || p.PodIP != h.PodIP {
		t.Errorf("ListManaged pod IP = %q, want %q (the IP StartPod handed back) "+
			"— the egress floor is scoped to it and the node re-dials ACP on it", p.PodIP, h.PodIP)
	}
}

// caseStopIsIdempotent: Stop is the ONE method that must never error. It is called on every failure
// path, twice, and on handles whose containers may already be gone.
func caseStopIsIdempotent(t *testing.T, e *Env) {
	ctx := t.Context()
	id := uniqueSpawnID(t)
	h := startPod(ctx, t, e, id, e.BaseImage, 1)

	if err := e.Backend.Stop(ctx, h); err != nil {
		t.Fatalf("first Stop: %v", err)
	}
	if err := e.Backend.Stop(ctx, h); err != nil {
		t.Fatalf("second Stop of the same pod: %v (Stop is retried on every failure path; it must be idempotent)", err)
	}

	ghost := &runtime.PodHandle{
		SpawnID:   uniqueSpawnID(t),
		SidecarID: "nonexistent-sidecar",
		AgentID:   "nonexistent-agent",
		SandboxID: "nonexistent-sandbox",
	}
	if err := e.Backend.Stop(ctx, ghost); err != nil {
		t.Fatalf("Stop of a pod that does not exist: %v (orphan reaping calls Stop on handles it reconstructed; a hard error there wedges teardown)", err)
	}
}
