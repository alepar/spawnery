package cp

// readopt_test.go: the CP side of the spawnlet-restart re-adoption handshake (SE3 §4.2/§4.3). The
// load-bearing property under test is asymmetric: ADOPT and REAP are only ever reached from a positive,
// read-confirmed fact about the ledger — anything the CP cannot confirm (store error, transient status,
// live claim) must DEFER and leave the pod running. A CP blip must never become data loss.

import (
	"context"
	"errors"
	"testing"
	"time"

	nodev1 "spawnery/gen/node/v1"
	"spawnery/internal/cp/store"
)

// setActive puts spawn id's live container row on node nodeID at generation gen (status=active).
func setActive(t *testing.T, s *Server, id, nodeID string, gen int64) {
	t.Helper()
	ctx := context.Background()
	if err := s.st.WithTx(ctx, func(tx store.Store) error {
		return tx.Spawns().SetActive(ctx, id, nodeID, gen)
	}); err != nil {
		t.Fatalf("SetActive(%s, %s, %d): %v", id, nodeID, gen, err)
	}
}

// report drives the handler and returns the single ManagedPodsDecisions the CP sent back.
func report(t *testing.T, s *Server, nodeID string, sender *capSender, pods ...*nodev1.ObservedPod) *nodev1.ManagedPodsDecisions {
	t.Helper()
	s.handleManagedPodsReport(context.Background(), nodeID, sender, &nodev1.ManagedPodsReport{
		NodeId: nodeID, RequestId: "req-1", Pods: pods,
	})
	sender.mu.Lock()
	defer sender.mu.Unlock()
	var out *nodev1.ManagedPodsDecisions
	for _, m := range sender.sent {
		if d, ok := m.Msg.(*nodev1.CPMessage_ReadoptDecisions); ok {
			if out != nil {
				t.Fatalf("CP sent more than one decisions message: %+v", sender.sent)
			}
			out = d.ReadoptDecisions
		}
	}
	return out
}

func decisionFor(t *testing.T, d *nodev1.ManagedPodsDecisions, id string) *nodev1.ReadoptDecision {
	t.Helper()
	if d == nil {
		t.Fatal("CP sent no ManagedPodsDecisions — the node would wait, then leave every pod running")
	}
	for _, dec := range d.GetDecisions() {
		if dec.GetSpawnId() == id {
			return dec
		}
	}
	t.Fatalf("no decision for spawn %s in %+v", id, d.GetDecisions())
	return nil
}

// A pod whose (spawn, generation) matches a live container row is ADOPTED, and the adopt spec carries
// the full launch spec the node needs to rebuild its in-memory Spawn (it persisted none of it).
func TestReadoptAdoptsMatchingGeneration(t *testing.T) {
	s, _, _ := newTestServer(t)
	makeSpawn(t, s, "sp1", "alice")
	setActive(t, s, "sp1", "n1", 1)

	sender := &capSender{}
	d := report(t, s, "n1", sender, &nodev1.ObservedPod{SpawnId: "sp1", Generation: 1})

	if d.GetRequestId() != "req-1" {
		t.Fatalf("request_id = %q, want req-1 (the node routes the answer by it)", d.GetRequestId())
	}
	dec := decisionFor(t, d, "sp1")
	if dec.GetVerdict() != nodev1.ReadoptVerdict_READOPT_VERDICT_ADOPT {
		t.Fatalf("verdict = %v, want ADOPT (reason %q)", dec.GetVerdict(), dec.GetReason())
	}
	if dec.GetGeneration() != 1 {
		t.Errorf("decision generation = %d, want 1 (the node re-checks its own fence against it)", dec.GetGeneration())
	}
	spec := dec.GetAdopt().GetSpec()
	if spec == nil {
		t.Fatal("ADOPT with no launch spec — the node cannot rebuild the spawn")
	}
	if spec.GetSpawnId() != "sp1" || spec.GetGeneration() != 1 {
		t.Errorf("spec = (%s, gen %d), want (sp1, gen 1)", spec.GetSpawnId(), spec.GetGeneration())
	}
	if spec.GetAppRef() != "examples/secret-app" || spec.GetModel() != "m" {
		t.Errorf("spec app_ref/model = %q/%q, want examples/secret-app/m", spec.GetAppRef(), spec.GetModel())
	}
	if spec.GetAssertedOwner() != "alice" {
		t.Errorf("spec asserted_owner = %q, want alice (the node's owner-binding check reads it)", spec.GetAssertedOwner())
	}
	// Adoption is CP-initiated: there is no client intent, and re-delivering artifacts/rootfs/secrets
	// to a pod that never stopped is meaningless.
	if spec.GetAuth() != nil {
		t.Errorf("adopt spec carries an auth envelope; an adoption has no client signed intent")
	}
	if len(spec.GetArtifacts()) != 0 || len(spec.GetRootfsArtifacts()) != 0 || len(spec.GetSecrets()) != 0 {
		t.Errorf("adopt spec must carry no artifacts/rootfs/secrets: %+v", spec)
	}
	// Read-only: the decision must not move the spawn row or send anything else down the stream.
	sp, err := s.st.Spawns().Get(context.Background(), "sp1")
	if err != nil || sp.Status != store.Active {
		t.Fatalf("spawn row changed under the handshake: status=%v err=%v (the handler is read-only)", sp.Status, err)
	}
	if len(sender.stops()) != 0 {
		t.Fatalf("readopt sent StopSpawn: %v", sender.stops())
	}
}

// A pod whose spawn has an owner-sealed journaled mount must be adopted with
// journal_key_delivery_pending=true, and the CP must mark the spawn delivery-pending (GetSpawn surfaces
// it; it is what arms the owner's re-delivery over the migrate key-travel path).
func TestReadoptAdoptSetsJournalKeyDeliveryPending(t *testing.T) {
	s, reg, rt := newTestServer(t)
	activeSpawnWithRoute(t, s, reg, rt, "sp1", "alice", &capSender{})
	sealKey(t, s, "sp1", "main")

	d := report(t, s, "n1", &capSender{}, &nodev1.ObservedPod{SpawnId: "sp1", Generation: 1})
	dec := decisionFor(t, d, "sp1")
	if dec.GetVerdict() != nodev1.ReadoptVerdict_READOPT_VERDICT_ADOPT {
		t.Fatalf("verdict = %v, want ADOPT", dec.GetVerdict())
	}
	if !dec.GetAdopt().GetJournalKeyDeliveryPending() {
		t.Error("adopt spec journal_key_delivery_pending = false, want true (spawn has an owner-sealed mount)")
	}
	if !s.deliveryPending.isPending("sp1") {
		t.Error("CP did not mark the spawn delivery-pending; the web UI's re-delivery prompt would never fire")
	}
}

// A pod whose spawn has NO owner-sealed mount (ephemeral/node-local only) must not be flagged for
// delivery: node-local custody survives the restart under node.key, and there is nothing to re-deliver.
func TestReadoptAdoptLeavesJournalKeyDeliveryPendingFalse(t *testing.T) {
	s, reg, rt := newTestServer(t)
	activeSpawnWithRoute(t, s, reg, rt, "sp1", "alice", &capSender{})

	d := report(t, s, "n1", &capSender{}, &nodev1.ObservedPod{SpawnId: "sp1", Generation: 1})
	dec := decisionFor(t, d, "sp1")
	if dec.GetVerdict() != nodev1.ReadoptVerdict_READOPT_VERDICT_ADOPT {
		t.Fatalf("verdict = %v, want ADOPT", dec.GetVerdict())
	}
	if dec.GetAdopt().GetJournalKeyDeliveryPending() {
		t.Error("adopt spec journal_key_delivery_pending = true, want false (no owner-sealed mount)")
	}
	if s.deliveryPending.isPending("sp1") {
		t.Error("CP marked the spawn delivery-pending with no owner-sealed mount")
	}
}

// A pod whose generation is behind the live row is a superseded episode (the CP re-created the spawn):
// REAP, exactly as adoptOrStop's orphan arm would.
func TestReadoptReapsStaleGeneration(t *testing.T) {
	s, _, _ := newTestServer(t)
	makeSpawn(t, s, "sp1", "alice")
	setActive(t, s, "sp1", "n1", 1)
	// Advance to generation 2 (mirrors RecreateSpawn: end gen 1, claim a fresh gen 2 container), so the
	// live row is gen 2 while the node still reports the gen-1 pod it never stopped.
	ctx := context.Background()
	var gen2 int64
	if err := s.st.WithTx(ctx, func(tx store.Store) error {
		g, e := tx.Spawns().ClaimStarting(ctx, "sp1", []store.Status{store.Active})
		gen2 = g
		return e
	}); err != nil {
		t.Fatalf("ClaimStarting: %v", err)
	}
	setActive(t, s, "sp1", "n1", gen2)

	d := report(t, s, "n1", &capSender{}, &nodev1.ObservedPod{SpawnId: "sp1", Generation: 1})
	dec := decisionFor(t, d, "sp1")
	if dec.GetVerdict() != nodev1.ReadoptVerdict_READOPT_VERDICT_REAP {
		t.Fatalf("stale generation: verdict = %v, want REAP", dec.GetVerdict())
	}
	if dec.GetAdopt() != nil {
		t.Error("REAP must carry no adopt spec — there is no half-adopted state")
	}
	if dec.GetReason() == "" {
		t.Error("REAP must carry a reason; the node logs it before destroying a pod")
	}
}

// A pod whose spawn has NO live container row (suspended, deleted, errored) is an orphan: REAP.
func TestReadoptReapsSpawnWithNoLiveContainer(t *testing.T) {
	s, _, _ := newTestServer(t)
	makeSpawn(t, s, "sp1", "alice") // status=starting, live row gen 1
	ctx := context.Background()
	if err := s.st.WithTx(ctx, func(tx store.Store) error {
		return tx.Spawns().EndContainer(ctx, "sp1", 1, store.PhaseStopped)
	}); err != nil {
		t.Fatalf("EndContainer: %v", err)
	}

	d := report(t, s, "n1", &capSender{}, &nodev1.ObservedPod{SpawnId: "sp1", Generation: 1})
	if got := decisionFor(t, d, "sp1").GetVerdict(); got != nodev1.ReadoptVerdict_READOPT_VERDICT_REAP {
		t.Fatalf("no live container row: verdict = %v, want REAP", got)
	}
}

// An unknown spawn id (never existed / hard-deleted) is an orphan: REAP.
func TestReadoptReapsUnknownSpawn(t *testing.T) {
	s, _, _ := newTestServer(t)
	d := report(t, s, "n1", &capSender{}, &nodev1.ObservedPod{SpawnId: "ghost", Generation: 1})
	if got := decisionFor(t, d, "ghost").GetVerdict(); got != nodev1.ReadoptVerdict_READOPT_VERDICT_REAP {
		t.Fatalf("unknown spawn: verdict = %v, want REAP", got)
	}
}

// THE one that matters: a store error must NEVER be laundered into a destructive verdict. The CP cannot
// see its own ledger, so it must not tell the node to destroy anything (spec §4.3).
func TestReadoptDefersOnStoreError(t *testing.T) {
	s, _, _ := newTestServer(t)
	makeSpawn(t, s, "sp1", "alice")
	setActive(t, s, "sp1", "n1", 1)
	s.st = liveErrStore{Store: s.st, err: errors.New("boom: db unavailable")}

	d := report(t, s, "n1", &capSender{}, &nodev1.ObservedPod{SpawnId: "sp1", Generation: 1})
	dec := decisionFor(t, d, "sp1")
	if dec.GetVerdict() != nodev1.ReadoptVerdict_READOPT_VERDICT_DEFER {
		t.Fatalf("store error: verdict = %v, want DEFER — a CP blip must never destroy a pod", dec.GetVerdict())
	}
	if dec.GetAdopt() != nil {
		t.Error("DEFER must carry no adopt spec")
	}
}

// A spawn a driver currently owns (Suspending/Resuming) is nobody else's business mid-flight: DEFER.
// Mirrors reconcileInventory's transient-status skip.
func TestReadoptDefersOnTransientStatus(t *testing.T) {
	s, _, _ := newTestServer(t)
	makeSpawn(t, s, "sp1", "alice")
	setActive(t, s, "sp1", "n1", 1)
	ctx := context.Background()
	if err := s.st.WithTx(ctx, func(tx store.Store) error {
		return tx.Spawns().SetSuspending(ctx, "sp1", 1)
	}); err != nil {
		t.Fatalf("SetSuspending: %v", err)
	}

	d := report(t, s, "n1", &capSender{}, &nodev1.ObservedPod{SpawnId: "sp1", Generation: 1})
	if got := decisionFor(t, d, "sp1").GetVerdict(); got != nodev1.ReadoptVerdict_READOPT_VERDICT_DEFER {
		t.Fatalf("suspending spawn: verdict = %v, want DEFER (a driver owns it)", got)
	}
}

// A spawn with an active (unexpired) claim is mid-flight under some other driver even though its
// status is still Active (e.g. SetSpawnModel, or a Recreate that has not yet flipped status): DEFER.
// Distinct from the transient-status check above — this is the "live claim" arm of design decision 2.
func TestReadoptDefersOnLiveClaim(t *testing.T) {
	s, _, _ := newTestServer(t)
	makeSpawn(t, s, "sp1", "alice")
	setActive(t, s, "sp1", "n1", 1)
	ctx := context.Background()
	sp, err := s.st.Spawns().Get(ctx, "sp1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	now := time.Now()
	if _, err := s.st.Spawns().Acquire(ctx, "sp1", "some-driver", "lease-1",
		now.UnixNano(), now.Add(time.Minute).UnixNano(), sp.StatusSeq); err != nil {
		t.Fatalf("Acquire: %v", err)
	}

	d := report(t, s, "n1", &capSender{}, &nodev1.ObservedPod{SpawnId: "sp1", Generation: 1})
	if got := decisionFor(t, d, "sp1").GetVerdict(); got != nodev1.ReadoptVerdict_READOPT_VERDICT_DEFER {
		t.Fatalf("live-claimed spawn: verdict = %v, want DEFER (another driver owns it)", got)
	}
}

// A report that arrives before Register (no node identity) is ignored: no decisions, no reaping.
func TestReadoptIgnoresReportBeforeRegister(t *testing.T) {
	s, _, _ := newTestServer(t)
	makeSpawn(t, s, "sp1", "alice")
	setActive(t, s, "sp1", "n1", 1)

	sender := &capSender{}
	s.handleManagedPodsReport(context.Background(), "", sender, &nodev1.ManagedPodsReport{
		NodeId: "n1", RequestId: "req-1", Pods: []*nodev1.ObservedPod{{SpawnId: "sp1", Generation: 1}},
	})
	sender.mu.Lock()
	n := len(sender.sent)
	sender.mu.Unlock()
	if n != 0 {
		t.Fatalf("unregistered report answered with %d message(s); want silence", n)
	}
}

// A report whose self-asserted node_id disagrees with the stream's (verified) identity is ignored —
// same posture as Register, where the mTLS identity is authoritative.
func TestReadoptIgnoresNodeIDMismatch(t *testing.T) {
	s, _, _ := newTestServer(t)
	makeSpawn(t, s, "sp1", "alice")
	setActive(t, s, "sp1", "n1", 1)

	sender := &capSender{}
	s.handleManagedPodsReport(context.Background(), "n1", sender, &nodev1.ManagedPodsReport{
		NodeId: "n2", RequestId: "req-1", Pods: []*nodev1.ObservedPod{{SpawnId: "sp1", Generation: 1}},
	})
	sender.mu.Lock()
	n := len(sender.sent)
	sender.mu.Unlock()
	if n != 0 {
		t.Fatalf("mismatched-node_id report answered with %d message(s); want silence", n)
	}
}

// A node with zero managed pods still gets an answer — otherwise it blocks until its timeout on every
// clean start.
func TestReadoptAnswersEmptyReport(t *testing.T) {
	s, _, _ := newTestServer(t)
	sender := &capSender{}
	d := report(t, s, "n1", sender)
	if d == nil || d.GetRequestId() != "req-1" || len(d.GetDecisions()) != 0 {
		t.Fatalf("empty report: got %+v, want an empty-decisions answer echoing req-1", d)
	}
}

// The handshake must be reachable over the real node stream: Register, then a ManagedPodsReport, and the
// CP answers on the same stream. Without this case in runNode's switch the message is silently dropped and
// the node leaves every pod unsupervised.
func TestRunNodeAnswersManagedPodsReport(t *testing.T) {
	s, _, _ := newTestServer(t)
	makeSpawn(t, s, "sp1", "alice")
	setActive(t, s, "sp1", "n1", 1)

	in := make(chan *nodev1.NodeMessage, 4)
	sender := &capSender{}
	go s.runNode(context.Background(), sender, recvFromChan(in))

	in <- &nodev1.NodeMessage{Msg: &nodev1.NodeMessage_Register{Register: &nodev1.Register{NodeId: "n1", MaxSpawns: 1}}}
	in <- &nodev1.NodeMessage{Msg: &nodev1.NodeMessage_ManagedPods{ManagedPods: &nodev1.ManagedPodsReport{
		NodeId: "n1", RequestId: "req-9", Pods: []*nodev1.ObservedPod{{SpawnId: "sp1", Generation: 1}},
	}}}

	deadline := time.Now().Add(2 * time.Second)
	for {
		sender.mu.Lock()
		var got *nodev1.ManagedPodsDecisions
		for _, m := range sender.sent {
			if d, ok := m.Msg.(*nodev1.CPMessage_ReadoptDecisions); ok {
				got = d.ReadoptDecisions
			}
		}
		sender.mu.Unlock()
		if got != nil {
			if got.GetRequestId() != "req-9" || len(got.GetDecisions()) != 1 ||
				got.GetDecisions()[0].GetVerdict() != nodev1.ReadoptVerdict_READOPT_VERDICT_ADOPT {
				t.Fatalf("runNode answer = %+v, want ADOPT for sp1 echoing req-9", got)
			}
			close(in)
			return
		}
		if time.Now().After(deadline) {
			close(in)
			t.Fatal("runNode never answered the ManagedPodsReport — the case is missing from its switch")
		}
		time.Sleep(time.Millisecond)
	}
}
