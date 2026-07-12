package spawnlet

// adopt.go — spawnlet-restart re-adoption (SE3, design §4.2/§4.3). A restarted spawnlet finds its pods
// STILL RUNNING (gracefulDetachAll, sp-2tx8.3.2) and an EMPTY in-memory store. This file is the node-local
// half of putting that back together: find the pods we are not tracking (UntrackedPods), rebuild a Spawn
// for the ones the CP confirms (Adopt), and destroy — with a capture first — the ones it disowns (ReapPod).
//
// The node NEVER decides adopt-vs-reap here: that is the CP's call, matched on generation
// (internal/node/readopt.go drives the handshake). This file only executes the verdict.

import (
	"context"
	"log"

	"spawnery/internal/runtime"
	"spawnery/internal/spawnlet/firewall"
)

// UntrackedPods returns every spawnery-managed pod this node's runtime still has that this Manager is NOT
// tracking — the survivors of a previous node process (the in-mem store is empty after a restart), each
// carrying the container ids and the pod IP a rebuild needs (sp-2tx8.3.1).
//
// Scoped by the spawnery.node-id label: a pod created by a DIFFERENT node id is not ours — two spawnlets
// sharing one Docker daemon (dev stack + an e2e run, or multi-node-on-one-host) must neither adopt nor reap
// each other's pods. It is logged and left alone (design §4.3). Unlabeled pods (pre-label versions) are
// treated as ours.
//
// It has NO side effects: nothing is captured, stopped or adopted. The caller reports this set to the CP and
// applies the CP's per-spawn verdict. That asymmetry is the point — the old ReapOrphans destroyed on the
// node's own authority, so a CP blip became data loss.
func (m *Manager) UntrackedPods(ctx context.Context) ([]runtime.ManagedPod, error) {
	managed, err := m.pod.ListManaged(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]runtime.ManagedPod, 0, len(managed))
	for _, mp := range managed {
		if mp.NodeID != "" && mp.NodeID != m.cfg.NodeID {
			log.Printf("reconcile: leaving pod spawn=%s alone (node-id %q is not ours)", mp.SpawnID, mp.NodeID)
			continue
		}
		if _, live := m.store.Get(mp.SpawnID); live {
			continue // already ours (an earlier reconcile pass adopted it, or it never left)
		}
		out = append(out, mp)
	}
	return out, nil
}

// ReapPod destroys ONE untracked pod: capture-before-reap, then stop. This is the CP's REAP verdict (an
// unknown/dormant spawn or a superseded generation) and the fallback for an ADOPT whose rebuild failed —
// today's ReapOrphans behaviour, now applied per pod instead of to everything in sight.
//
// Crash-survival (spec §4): with DeltaCapture enabled, CaptureDelta runs on the agent container BEFORE
// pod.Stop, so the spawn's work survives for a future resume. Best-effort and non-fatal — a capture failure
// just means the next resume starts from the last known-good delta (or the base image if none existed).
//
// moby#47065 note: the layer-count guard in CaptureDelta wants the BaseImageRef of the launch image. A
// reaped pod has no Spawn record, so BaseImageRef is empty and the guard degrades to rejecting only truly
// zero-layer commits — unchanged from ReapOrphans.
//
// It also removes the pod's egress-floor rules (which the previous process never removed): the pod IP is
// about to be released back to the bridge, and a stale DROP would later bite whatever recycles it.
func (m *Manager) ReapPod(ctx context.Context, mp runtime.ManagedPod) error {
	log.Printf("reaping pod spawn=%s gen=%d (CP verdict: reap, or rebuild failed)", mp.SpawnID, mp.Generation)

	if m.cfg.DeltaCapture && mp.AgentID != "" {
		h := &runtime.PodHandle{SpawnID: mp.SpawnID, AgentID: mp.AgentID}
		if ref, cerr := m.pod.CaptureDelta(ctx, h); cerr != nil {
			log.Printf("capture-before-reap spawn=%s: %v (non-fatal; delta may be stale)", mp.SpawnID, cerr)
		} else {
			log.Printf("capture-before-reap spawn=%s ref=%s", mp.SpawnID, ref)
		}
	}

	err := m.pod.Stop(ctx, &runtime.PodHandle{SidecarID: mp.SidecarID, AgentID: mp.AgentID, SandboxID: mp.SandboxID})

	if m.egressEnforced() && mp.PodIP != "" {
		if ferr := m.fw.Remove(ctx, firewall.Rules(mp.PodIP, m.cfg.EgressAllowCIDRs)); ferr != nil {
			log.Printf("reap: remove egress floor for %s (ip %s): %v", mp.SpawnID, mp.PodIP, ferr)
		}
	}
	return err
}
