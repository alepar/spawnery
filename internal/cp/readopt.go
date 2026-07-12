package cp

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	nodev1 "spawnery/gen/node/v1"
	"spawnery/internal/cp/registry"
	"spawnery/internal/cp/store"
)

// Re-adoption handshake, CP side (SE3 design §4.2/§4.3, bead sp-2tx8.3.3).
//
// A restarted spawnlet leaves its pods running (gracefulDetachAll) but loses every byte of per-spawn
// in-memory state. At startup it reports the pods its runtime still has — {spawn id, generation} from the
// labels, all it can know — and the CP, which owns the spawn ledger, answers per spawn: ADOPT (with the
// launch spec to rebuild from), REAP, or DEFER.
//
// The asymmetry is the whole design. ADOPT and REAP are reached only from a positive, read-confirmed fact
// about the ledger. Everything else — a store error, a spawn a driver currently owns — is DEFER: leave the
// pod running and decide next time. A pod the CP says nothing about is left running too. A CP blip must
// never become data loss (§4.3); today's ReapOrphans silently would.
//
// This handler is READ-ONLY with respect to spawn rows and the router: it decides, it does not adopt.
// reconcileInventory/adoptOrStop remains the single writer of node-binding/route/status and does the real
// adopt on the node's next Heartbeat — which, once the node has rebuilt, carries the spawns. The only
// state touched here is the informational deliveryPending flag (see adoptSpawnFor).

// handleManagedPodsReport answers one ManagedPodsReport with one ManagedPodsDecisions on the same stream.
// nodeID is the stream's AUTHORITATIVE node id (the mTLS identity in enforced mode) — not the report's
// self-asserted one. A report before Register, or one whose self-asserted node_id disagrees, is ignored:
// we will not hand adopt specs (or reap verdicts) to a node we cannot name.
func (s *Server) handleManagedPodsReport(ctx context.Context, nodeID string, sender registry.NodeSender, rep *nodev1.ManagedPodsReport) {
	if nodeID == "" {
		slog.Warn("readopt: managed-pods report before Register — ignored", "pods", len(rep.GetPods()))
		return
	}
	if rn := rep.GetNodeId(); rn != "" && rn != nodeID {
		slog.Warn("readopt: report node_id != stream identity — ignored", "node", nodeID, "asserted", rn)
		return
	}
	decisions := make([]*nodev1.ReadoptDecision, 0, len(rep.GetPods()))
	for _, p := range rep.GetPods() {
		d := s.readoptDecision(ctx, nodeID, p)
		slog.Info("readopt: decision", "node", nodeID, "spawn", d.GetSpawnId(), "gen", d.GetGeneration(),
			"verdict", d.GetVerdict().String(), "reason", d.GetReason())
		decisions = append(decisions, d)
	}
	// Always answer, even for an empty report: the node blocks on this reply before it serves.
	if err := sender.Send(&nodev1.CPMessage{Msg: &nodev1.CPMessage_ReadoptDecisions{
		ReadoptDecisions: &nodev1.ManagedPodsDecisions{RequestId: rep.GetRequestId(), Decisions: decisions},
	}}); err != nil {
		slog.Error("readopt: send decisions failed (node will leave its pods running and retry)",
			"node", nodeID, "err", err)
	}
}

// readoptDecision decides ONE reported pod. It never returns a nil decision: an undecidable pod gets an
// explicit DEFER rather than a silent omission, so the node's log says why its pod is still unsupervised.
//
// The REAP predicates mirror adoptOrStop's orphan arm exactly (§4.3 reuses that fencing):
//   - no live container row  -> the spawn is suspended/deleted/errored; the pod is a leftover.
//   - generation mismatch    -> the episode was superseded (recreate/fork/migrate) or the pod predates
//     the CP's row; either way it is not the live episode.
//
// The DEFER predicates are everything we cannot confirm:
//   - any store error        -> we cannot see the ledger. Destroying a pod on a DB blip is unforgivable.
//   - Suspending/Resuming/Forking, or a live claim -> a driver owns this spawn right now and will finish
//     the transition; adopting or reaping under it would race the driver (same skip reconcileInventory
//     applies).
func (s *Server) readoptDecision(ctx context.Context, nodeID string, p *nodev1.ObservedPod) *nodev1.ReadoptDecision {
	id, gen := p.GetSpawnId(), p.GetGeneration()
	dec := func(v nodev1.ReadoptVerdict, reason string) *nodev1.ReadoptDecision {
		return &nodev1.ReadoptDecision{SpawnId: id, Generation: gen, Verdict: v, Reason: reason}
	}
	if id == "" {
		return dec(nodev1.ReadoptVerdict_READOPT_VERDICT_DEFER, "empty spawn id")
	}

	c, ok, err := s.st.Spawns().LiveContainer(ctx, id)
	if err != nil {
		return dec(nodev1.ReadoptVerdict_READOPT_VERDICT_DEFER, fmt.Sprintf("live container lookup failed: %v", err))
	}
	if !ok {
		return dec(nodev1.ReadoptVerdict_READOPT_VERDICT_REAP, "no live container row (spawn suspended, deleted or errored)")
	}
	if c.Generation != int64(gen) {
		return dec(nodev1.ReadoptVerdict_READOPT_VERDICT_REAP,
			fmt.Sprintf("generation %d superseded by %d", gen, c.Generation))
	}

	sp, err := s.st.Spawns().Get(ctx, id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return dec(nodev1.ReadoptVerdict_READOPT_VERDICT_REAP, "spawn row is gone (deleted)")
		}
		return dec(nodev1.ReadoptVerdict_READOPT_VERDICT_DEFER, fmt.Sprintf("spawn lookup failed: %v", err))
	}
	switch sp.Status {
	case store.Suspending, store.Resuming, store.Forking:
		return dec(nodev1.ReadoptVerdict_READOPT_VERDICT_DEFER, "spawn is mid-transition ("+string(sp.Status)+")")
	}
	if sp.ClaimHolder != nil && sp.ClaimDeadline != nil && *sp.ClaimDeadline > time.Now().UnixNano() {
		return dec(nodev1.ReadoptVerdict_READOPT_VERDICT_DEFER, "spawn is claimed by an in-flight driver")
	}

	adopt, err := s.adoptSpawnFor(ctx, sp, gen)
	if err != nil {
		// We know the pod is legitimate; we just cannot assemble its launch spec right now. Leaving it
		// running is strictly better than destroying a live spawn over a failed mount read.
		return dec(nodev1.ReadoptVerdict_READOPT_VERDICT_DEFER, fmt.Sprintf("cannot build adopt spec: %v", err))
	}
	d := dec(nodev1.ReadoptVerdict_READOPT_VERDICT_ADOPT, "live episode on this node")
	d.Adopt = adopt
	// The row's node binding is (re)written by adoptOrStop on the node's next heartbeat, not here — this
	// handler is read-only w.r.t. spawn rows and the router (design decision 5). nodeID is used only for
	// logging above.
	return d
}

// adoptSpawnFor assembles the launch spec the node rebuilds its in-memory Spawn from — the same StartSpawn
// a fresh start/resume carries, minus the four fields an adoption has no use for (auth: no client intent;
// artifacts + rootfs_artifacts: the pod never stopped; secrets: see below).
//
// journal_key_delivery_pending: an OWNER-SEALED journaled mount's Kopia repo password lived only in the
// node's memory and died with the process. The CP cannot re-deliver it — it custodies only owner-sealed
// ciphertext it has no key to open — so it re-arms the SAME key-travel path a migration uses: mark the
// spawn delivery-pending (GetSpawn surfaces it, the web UI prompts), the owner client fetches the
// ciphertext, reseals it to this node's sub-key, and DeliverSecrets lands it as a SecretDelivery. The pod
// runs throughout; only new journal snapshots wait. NODE-LOCAL-custody mounts need nothing — their
// password is sealed on this node's own disk under node.key and survived the restart.
func (s *Server) adoptSpawnFor(ctx context.Context, sp store.Spawn, gen uint64) (*nodev1.AdoptSpawn, error) {
	mounts, err := s.st.Spawns().GetMounts(ctx, sp.ID)
	if err != nil {
		return nil, fmt.Errorf("get mounts: %w", err)
	}
	classes, err := s.classifyMounts(ctx, sp.ID)
	if err != nil {
		return nil, fmt.Errorf("classify mounts: %w", err)
	}
	pending := false
	for _, class := range classes {
		if class == mountClassOwnerSealed {
			pending = true
			break
		}
	}
	if pending {
		s.deliveryPending.mark(sp.ID)
	}
	return &nodev1.AdoptSpawn{
		Spec: &nodev1.StartSpawn{
			SpawnId:         sp.ID,
			AppRef:          sp.AppRef,
			Model:           sp.Model,
			Name:            sp.Name,
			AppId:           sp.AppID,
			Image:           sp.Image,
			RunnableId:      sp.RunnableID,
			Mode:            sp.Mode,
			Generation:      gen,
			Mounts:          storeToNodeMounts(mounts),
			AssertedOwner:   sp.OwnerID,
			BaseImageDigest: sp.BaseImageDigest,
		},
		JournalKeyDeliveryPending: pending,
	}, nil
}
