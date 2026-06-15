// Package scheduler assigns a spawn to a node, issues StartSpawn over that
// node's stream, and blocks until the node reports ACTIVE (or ERROR/timeout).
package scheduler

import (
	"context"
	"errors"
	"sync"
	"time"

	"connectrpc.com/connect"

	authv1 "spawnery/gen/auth/v1"
	nodev1 "spawnery/gen/node/v1"
	"spawnery/internal/cp/registry"
	"spawnery/internal/cp/router"
)

// spawnResult carries both the phase and the machine-readable NACK detail so callers can
// surface a node's CORRESPONDENCE/STALE/etc. rejection as a typed Connect error [AC1].
type spawnResult struct {
	phase  nodev1.SpawnPhase
	detail string // Status.Detail from the node (e.g. "STALE: intent is 35s old"); empty on ACTIVE
}

type Scheduler struct {
	reg     *registry.Registry
	rt      *router.Router
	timeout time.Duration

	mu      sync.Mutex
	pending map[string]chan spawnResult // spawn_id -> ACTIVE/ERROR signal
}

type RootfsRestore struct {
	SourceGeneration uint64
	Artifacts        []*nodev1.RootfsArtifact
	LocalOnly        bool
}

func New(reg *registry.Registry, rt *router.Router, timeout time.Duration) *Scheduler {
	return &Scheduler{reg: reg, rt: rt, timeout: timeout, pending: map[string]chan spawnResult{}}
}

// OnStatus is called by the node receive loop when a SpawnStatus arrives.
// detail carries the Status.Detail field from the node (machine-readable NACK code + human
// explanation on ERROR; empty on ACTIVE). It is threaded through so callers can return a
// typed Connect error containing the NACK code [AC1].
func (s *Scheduler) OnStatus(spawnID string, phase nodev1.SpawnPhase, detail string) {
	s.mu.Lock()
	ch, ok := s.pending[spawnID]
	s.mu.Unlock()
	if ok && (phase == nodev1.SpawnPhase_ACTIVE || phase == nodev1.SpawnPhase_ERROR) {
		select {
		case ch <- spawnResult{phase: phase, detail: detail}:
		default:
		}
	}
}

// PickNodeID selects an eligible node ID for the given placement without sending any message.
// Used by lifecycle handlers to resolve target_node_id before registering a PendingIntent [AC1].
// A follow-up Provision call with placement.TargetNodeID pinned to this ID will re-select the
// same node (if still available). Returns ResourceExhausted if no eligible node exists.
func (s *Scheduler) PickNodeID(placement registry.Placement) (string, error) {
	n := s.reg.PickFor(placement)
	if n == nil {
		return "", connect.NewError(connect.CodeResourceExhausted, errors.New("no eligible node with capacity"))
	}
	return n.ID, nil
}

// Provision picks a node, sends StartSpawn for the (already-minted) spawn id, waits for ACTIVE,
// and binds the route. Returns the chosen node id. The caller owns id-minting + persistence.
// gen is the live container row's generation: the node labels + heartbeat-reports its pod with it,
// and the inventory reconciler matches that report against the row — an omitted gen (0) would make
// the orphan arm Stop the pod the CP itself just started (sp-gzvo).
// env is the A4 AuthEnvelope (token + SignedIntent) to thread into StartSpawn [AC1]; nil is
// allowed in dev/insecure mode where the node will verify-and-log-not-enforce.
// baseImageDigest is threaded to the node for cross-node resume (sp-ei4.1.10); empty on fresh create.
func (s *Scheduler) Provision(ctx context.Context, id, appRef, model, name, appID, runnable, mode string, gen uint64, placement registry.Placement, env *authv1.AuthEnvelope, baseImageDigest string, rootfs *RootfsRestore, artifacts []*nodev1.ArtifactSpec) (string, error) {
	n := s.reg.PickFor(placement)
	if n == nil {
		return "", connect.NewError(connect.CodeResourceExhausted, errors.New("no eligible node with capacity"))
	}
	ch := make(chan spawnResult, 1)
	s.mu.Lock()
	s.pending[id] = ch
	s.mu.Unlock()
	defer func() { s.mu.Lock(); delete(s.pending, id); s.mu.Unlock() }()

	start := &nodev1.StartSpawn{
		SpawnId: id, AppRef: appRef, Model: model, Name: name, AppId: appID,
		Image: placement.Image, RunnableId: runnable, Mode: mode, Generation: gen,
		Auth: env, AssertedOwner: placement.Owner, BaseImageDigest: baseImageDigest,
	}
	if rootfs != nil && len(rootfs.Artifacts) > 0 {
		start.RootfsSourceGeneration = rootfs.SourceGeneration
		start.RootfsArtifacts = rootfs.Artifacts
		start.RootfsArtifactsLocalOnly = rootfs.LocalOnly
	}
	if len(artifacts) > 0 {
		start.Artifacts = artifacts
	}
	if err := n.Sender.Send(&nodev1.CPMessage{Msg: &nodev1.CPMessage_Start{Start: start}}); err != nil {
		return "", connect.NewError(connect.CodeUnavailable, err)
	}
	select {
	case res := <-ch:
		if res.phase != nodev1.SpawnPhase_ACTIVE {
			// Surface the node's machine-readable NACK code (e.g. "STALE: ...") as a
			// FailedPrecondition error so lifecycle callers can classify retryable failures [AC1].
			detail := res.detail
			if detail == "" {
				detail = "spawn failed to start"
			}
			return "", connect.NewError(connect.CodeFailedPrecondition, errors.New(detail))
		}
		s.rt.Bind(id, n.ID, n.Sender)
		return n.ID, nil
	case <-time.After(s.timeout):
		return "", connect.NewError(connect.CodeDeadlineExceeded, errors.New("spawn start timed out"))
	case <-ctx.Done():
		return "", ctx.Err()
	}
}
