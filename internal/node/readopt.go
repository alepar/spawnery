package node

// readopt.go — the NODE side of the spawnlet-restart re-adoption handshake (SE3 design §4.2/§4.3, bead
// sp-2tx8.3.4). It is the consumer of everything else in SE3: 3.1 (ListManaged reports the container ids +
// pod IP), 3.2 (SIGTERM leaves the pods running), 3.3 (the wire + the CP's per-spawn verdict).
//
// At startup the node's in-memory store is EMPTY but its pods are still running. It reports what it found
// — {spawn id, generation}, all the labels can tell it — and the CP, which owns the ledger, answers per
// spawn: ADOPT (here is the launch spec), REAP (unknown spawn / superseded generation) or DEFER.
//
// The asymmetry is the whole design and the reason this file replaced ReapOrphans:
//
//	ADOPT  -> rebuild the Spawn (Manager.Adopt), reap the lingering session servers, re-dial ACP, ACTIVE.
//	REAP   -> capture-before-reap (Manager.ReapPod). The ONLY destructive verdict.
//	anything else — DEFER, no decision, an unknown verdict, a generation that does not match the pod, a CP
//	we cannot reach, an answer that never comes -> LEAVE THE POD RUNNING and retry on the next connection.
//
// A CP blip must never become data loss. Today's ReapOrphans silently made it one.

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	nodev1 "spawnery/gen/node/v1"
	"spawnery/internal/agentcaps"
	"spawnery/internal/runtime"
	"spawnery/internal/spawnlet"
)

// defaultReadoptTimeout bounds how long the node waits for the CP's answer before giving up on THIS
// attempt. Generous: the CP answers synchronously off a store read. On expiry the node leaves every pod
// running, opens the serve gate, and retries the handshake on its next CP connection.
const defaultReadoptTimeout = 60 * time.Second

// readoptState is PROCESS-lived (like secretDeliveryReplay): the handshake belongs to the node process,
// not to a CP connection, so a stream that dies mid-handshake is retried on the next one.
//
// gate is closed once the node may serve StartSpawn — i.e. once an attempt has terminated one way or the
// other (answered, or timed out). Until then the CP must not be allowed to StartPod a spawn id whose pod we
// are still holding: the CRI lane would reap the stale same-named sandbox out from under the adoption, and
// Docker would collide on the container name.
type readoptState struct {
	mu        sync.Mutex
	done      bool          // an attempt completed with a CP answer; do not run it again
	gateOnce  sync.Once     // guards close(gate) against a double-close
	gate      chan struct{} // closed when the node may serve StartSpawn
	waitingID string        // the request id we are waiting on ("" = none)
	answer    chan *nodev1.ManagedPodsDecisions
	timeout   time.Duration
}

func newReadoptState() *readoptState {
	return &readoptState{gate: make(chan struct{}), answer: make(chan *nodev1.ManagedPodsDecisions, 1), timeout: defaultReadoptTimeout}
}

func (r *readoptState) completed() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.done
}

func (r *readoptState) openGate() { r.gateOnce.Do(func() { close(r.gate) }) }

// deliver routes a CP answer to the waiter. An answer for a request we are not waiting on is dropped (the
// proto says so): it is a late reply from a previous, abandoned connection.
func (r *readoptState) deliver(d *nodev1.ManagedPodsDecisions) {
	r.mu.Lock()
	waiting := r.waitingID
	r.mu.Unlock()
	if waiting == "" || d.GetRequestId() != waiting {
		slog.Warn("readopt: dropping decisions for an unexpected request", "got", d.GetRequestId(), "waiting", waiting)
		return
	}
	select {
	case r.answer <- d:
	default: // already answered; keep the first
	}
}

// awaitReadopt blocks until the node may serve spawn-starting work, or ctx dies. Returns false if ctx died
// first (the connection is going away — the caller should abandon the message). A nil readopt (unit tests
// that build an attacher directly) is an open gate.
func (a *attacher) awaitReadopt(ctx context.Context) bool {
	if a.readopt == nil {
		return true
	}
	select {
	case <-a.readopt.gate:
		return true
	case <-ctx.Done():
		return false
	}
}

// reconcileManagedPods is the handshake. It runs on its OWN goroutine right after Register (never on the
// receive loop — the answer arrives on that loop). It is a no-op once an attempt has completed.
func (a *attacher) reconcileManagedPods(ctx context.Context) {
	r := a.readopt
	if r == nil || r.completed() {
		return
	}
	pods, err := a.mgr.UntrackedPods(ctx)
	if err != nil {
		// We cannot even see the runtime. Destroy nothing; the next connection retries.
		slog.Error("readopt: cannot list managed pods — leaving everything running", "err", err)
		r.openGate()
		return
	}

	reqID := fmt.Sprintf("readopt-%d", time.Now().UnixNano())
	obs := make([]*nodev1.ObservedPod, 0, len(pods))
	for _, p := range pods {
		obs = append(obs, &nodev1.ObservedPod{SpawnId: p.SpawnID, Generation: p.Generation})
	}
	slog.Info("readopt: reporting surviving pods to the CP", "pods", len(obs), "request", reqID)

	r.mu.Lock()
	r.waitingID = reqID
	r.mu.Unlock()

	if err := a.send(&nodev1.NodeMessage{Msg: &nodev1.NodeMessage_ManagedPods{ManagedPods: &nodev1.ManagedPodsReport{
		NodeId: a.cfg.NodeID, RequestId: reqID, Pods: obs,
	}}}); err != nil {
		// The stream is dead. The pods stay running and the next connection retries: the gate stays SHUT,
		// because without a CP there is nothing to serve anyway.
		slog.Error("readopt: report send failed — pods left running, will retry on reconnect", "err", err)
		return
	}

	var dec *nodev1.ManagedPodsDecisions
	select {
	case dec = <-r.answer:
	case <-ctx.Done():
		slog.Warn("readopt: connection ended before the CP answered — pods left running, will retry")
		return
	case <-time.After(r.timeout):
		// The CP is reachable but silent. Serve anyway (a wedged handshake must not wedge the node), and
		// retry the handshake on the next connection. Nothing is destroyed.
		slog.Error("readopt: CP did not answer — every pod left running, unsupervised", "timeout", r.timeout, "pods", len(obs))
		r.openGate()
		return
	}

	byDecision := make(map[string]*nodev1.ReadoptDecision, len(dec.GetDecisions()))
	for _, d := range dec.GetDecisions() {
		byDecision[d.GetSpawnId()] = d
	}
	for _, p := range pods {
		a.applyReadopt(ctx, p, byDecision[p.SpawnID])
	}

	r.mu.Lock()
	r.done = true
	r.waitingID = ""
	r.mu.Unlock()
	r.openGate()
	slog.Info("readopt: reconcile complete — node is serving", "pods", len(pods))
}

// applyReadopt executes ONE verdict. Everything that is not an explicit ADOPT or REAP leaves the pod
// running: the destructive path is reachable only from a positive CP decision, or from an ADOPT whose
// rebuild failed (and that one captures first).
func (a *attacher) applyReadopt(ctx context.Context, mp runtime.ManagedPod, d *nodev1.ReadoptDecision) {
	switch {
	case d == nil:
		slog.Warn("readopt: no CP decision for this pod — left running, unsupervised", "spawn", mp.SpawnID, "gen", mp.Generation)
		return
	case d.GetGeneration() != mp.Generation:
		// Belt and braces on the CP's fence (the proto asks the node to re-check its own).
		slog.Warn("readopt: decision generation does not match the pod — left running",
			"spawn", mp.SpawnID, "pod_gen", mp.Generation, "decision_gen", d.GetGeneration())
		return
	}
	switch d.GetVerdict() {
	case nodev1.ReadoptVerdict_READOPT_VERDICT_ADOPT:
		if err := a.adoptPod(ctx, mp, d.GetAdopt()); err != nil {
			slog.Error("readopt: rebuild failed — capture-before-reaping the pod", "spawn", mp.SpawnID, "err", err)
			a.reapPod(ctx, mp)
		}
	case nodev1.ReadoptVerdict_READOPT_VERDICT_REAP:
		slog.Info("readopt: CP says reap", "spawn", mp.SpawnID, "gen", mp.Generation, "reason", d.GetReason())
		a.reapPod(ctx, mp)
	default: // DEFER, UNSPECIFIED, or anything a newer CP invents
		slog.Warn("readopt: no adopt/reap verdict — pod left running, unsupervised",
			"spawn", mp.SpawnID, "verdict", d.GetVerdict().String(), "reason", d.GetReason())
	}
}

func (a *attacher) reapPod(ctx context.Context, mp runtime.ManagedPod) {
	if err := a.mgr.ReapPod(ctx, mp); err != nil {
		slog.Error("readopt: reap failed", "spawn", mp.SpawnID, "err", err)
	}
}

// adoptPod rebuilds one spawn end to end: the Manager's in-memory Spawn, the lingering session servers
// reaped, the ACP re-dialled, session #0 re-registered, ACTIVE reported. ANY failure returns an error and
// the caller reaps — there is no half-adopted state, so this function undoes its own partial work.
//
// The ACP re-dial is safe because the agent is the ACP SERVER and the node merely dials it (both lanes
// attach over TCP to podIP:7000): the agent survived the node's death and saw nothing but a client
// disconnect. The session it had is gone; a fresh one is opened here.
func (a *attacher) adoptPod(ctx context.Context, mp runtime.ManagedPod, ad *nodev1.AdoptSpawn) error {
	st := ad.GetSpec()
	if st == nil {
		return fmt.Errorf("ADOPT verdict without a launch spec")
	}
	sp, err := a.mgr.Adopt(ctx, mp, spawnlet.AdoptSpec{
		AppRef:                    st.GetAppRef(),
		Model:                     st.GetModel(),
		Name:                      st.GetName(),
		AppID:                     st.GetAppId(),
		Image:                     st.GetImage(),
		RunnableID:                st.GetRunnableId(),
		Mode:                      st.GetMode(),
		Generation:                st.GetGeneration(),
		Mounts:                    mountBindingsFromProto(st.GetMounts()),
		BaseImageDigest:           st.GetBaseImageDigest(),
		JournalKeyDeliveryPending: ad.GetJournalKeyDeliveryPending(),
	})
	if err != nil {
		return err
	}
	// Re-register the spawn's GitHub link(s) with the proactive refresher (sp-2tx8.3.5 D5): the
	// refresher's state is in-memory and died with the previous process. Best-effort by construction.
	a.noteGitHubLinksFromMounts(sp.ID, mp.Generation, st.GetMounts())
	// Re-push the MITM CA + a live GitHub token into the still-running sidecar (sp-2tx8.9 §3.1, "on
	// re-adopt"). Idempotent: the sidecar never stopped, so it still HAS its secrets — this covers a token
	// that ROTATED while this node was down. Asynchronous and NON-FATAL, unlike the create-time push: the
	// pod is already serving, and a spawn whose token cannot be re-delivered is still healthy for
	// everything that is not git. The push loop reports STALE if it gives up. Must run AFTER the Note
	// above (the refresher's link state died with the previous process; without it there is no token to
	// mint) and AFTER Adopt (which put the Spawn — and its ControlURL/ControlToken — in the store).
	if a.ghControl != nil {
		a.ghControl.PushAsync(ctx, sp.ID)
	}
	// From here on, a failure must remove the spawn from the store again (mgr.Stop would TEAR THE POD DOWN
	// through teardown — running the mount finalizers — which is not what a rebuild failure means: the
	// caller's ReapPod captures first and does not finalize, and also removes the egress floor Adopt
	// applied, so "undo() then ReapPod" is the complete undo).
	undo := func() {
		for _, w := range a.mgr.DetachSpawn(sp.ID) {
			w.Stop()
		}
	}

	// Lingering additional-session servers from the previous process squat their tmux names and ACP pool
	// ports; the rebuilt registry re-allocates from 1 (design §4.6). Best-effort: not worth failing over.
	if err := a.sx.ReapExtraSessions(ctx, sp.ID); err != nil {
		slog.Warn("readopt: reaping lingering session servers", "spawn", sp.ID, "err", err)
	}

	if sp.Mode == string(agentcaps.ModeTmux) {
		ok, herr := a.mgr.TmuxHasSession(ctx, sp.AgentID, "spawn")
		if herr != nil || !ok {
			undo()
			return fmt.Errorf("tmux session 'spawn' is gone (err=%v)", herr)
		}
		relay := newTmuxRelay(a.mgr.TmuxAttachArgv(sp.AgentID, "spawn"), func(clientID string, data []byte) error {
			return a.send(&nodev1.NodeMessage{Msg: &nodev1.NodeMessage_Frame{Frame: &nodev1.Frame{
				SpawnId: sp.ID, SessionId: SessionZeroID, ClientId: clientID, Data: data,
			}}})
		}).withHasSession(func(ctx context.Context) (bool, error) {
			return a.mgr.TmuxHasSession(ctx, sp.AgentID, "spawn")
		})
		a.mu.Lock()
		a.tmuxRelays[zeroKey(sp.ID)] = relay
		a.registerSessionZeroLocked(sp, st.GetRunnableId(), "spawn")
		a.mu.Unlock()
		a.emitRoster(sp.ID)
		a.statusActive(sp.ID, sp.BaseImageDigest)
		return nil
	}

	att, err := a.mgr.Attach(ctx, sp)
	if err != nil {
		undo()
		return fmt.Errorf("re-dial ACP: %w", err)
	}
	p := newPump(att.Stdin, att.Stdout)
	p.closeFn = att.Close
	p.exitFn = a.agentDeathReclaim(ctx, sp.ID, p) // identical semantics to startSpawn's
	a.mu.Lock()
	a.pumps[zeroKey(sp.ID)] = p
	a.mu.Unlock()
	if err := p.start(ctx, readyTimeout); err != nil {
		p.stop()
		a.mu.Lock()
		delete(a.pumps, zeroKey(sp.ID))
		a.mu.Unlock()
		undo()
		return fmt.Errorf("agent not ready after re-dial: %w", err)
	}
	a.mu.Lock()
	a.registerSessionZeroLocked(sp, st.GetRunnableId(), "7000")
	a.mu.Unlock()
	a.emitRoster(sp.ID)
	a.statusActive(sp.ID, sp.BaseImageDigest)
	slog.Info("readopt: adopted spawn", "spawn", sp.ID, "gen", sp.Generation, "mode", sp.Mode)
	return nil
}

// registerSessionZeroLocked re-creates the spawn's session-#0 registry entry and takes its capacity slot.
// runnable and endpoint mirror startSpawn's own registration (Spawn carries no runnable field — see
// AdoptSpec's doc comment — so the caller threads it through from the CP's re-delivered launch spec).
// Caller holds a.mu.
func (a *attacher) registerSessionZeroLocked(sp *spawnlet.Spawn, runnable, endpoint string) {
	reg := newSessionRegistry(sp.ID)
	reg.register(&sessionEntry{
		id: SessionZeroID, transport: transportForMode(sp.Mode), runnable: runnable,
		state: nodev1.SessionState_SESSION_STATE_ACTIVE, endpoint: endpoint, pinned: true,
	})
	a.sessions[sp.ID] = reg
	a.active++
}
