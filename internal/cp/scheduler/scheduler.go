// Package scheduler assigns a spawn to a node, issues StartSpawn over that
// node's stream, and blocks until the node reports ACTIVE (or ERROR/timeout).
package scheduler

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"connectrpc.com/connect"

	authv1 "spawnery/gen/auth/v1"
	nodev1 "spawnery/gen/node/v1"
	"spawnery/internal/cp/cpmetrics"
	"spawnery/internal/cp/registry"
	"spawnery/internal/cp/router"
	"spawnery/internal/intent"
)

// spawnResult carries both the phase and the machine-readable NACK detail so callers can
// surface a node's CORRESPONDENCE/STALE/etc. rejection as a typed Connect error [AC1].
type spawnResult struct {
	phase  nodev1.SpawnPhase
	detail string // Status.Detail from the node (e.g. "STALE: intent is 35s old"); empty on ACTIVE
}

// DefaultStartStallWindow is the no-progress stall window for a spawn start: Provision fails only
// when the node goes QUIET for this long, not because the work is legitimately slow. Exported so
// prod (cmd/spawnery_cp/main.go) and the e2e stack (skill_ingest_e2e_test.go) share one number by
// construction and cannot silently diverge. The node emits ResumeProgress heartbeats immediately and
// every 10s on this path (sp-mwco.4.1/4.7) — a 3x margin against this window.
const DefaultStartStallWindow = 30 * time.Second

// defaultStartBackstop is the generous absolute cap on a spawn start: it fires only if a node
// heartbeats forever without ever reaching ACTIVE/ERROR. Mirrors lifecycle.go's defaultResumeTimeout.
const defaultStartBackstop = 10 * time.Minute

// pendingStart is the in-flight bookkeeping for a spawn Provision is currently waiting on: the
// ACTIVE/ERROR result channel, the generation Progress events must match, and a coalescing
// (buffered cap 1) progress channel that resets Provision's stall timer.
type pendingStart struct {
	res      chan spawnResult
	gen      uint64
	progress chan struct{} // buffered cap 1; non-blocking coalescing progress signal for the stall loop
}

type Scheduler struct {
	reg         *registry.Registry
	rt          *router.Router
	stallWindow time.Duration
	backstop    time.Duration

	mu       sync.Mutex
	pending  map[string]*pendingStart // spawn_id -> in-flight start bookkeeping
	inflight map[string]InFlightStart // spawn_id -> the node/gen Provision is currently starting it on
}

// InFlightStart is the placement Provision has picked for a spawn that has not yet reached ACTIVE.
// This is the ONLY ownership signal available during artifact staging (sp-mwco.4.3): the live
// container row's node_id is set by Spawns().SetActive only AFTER Provision returns ACTIVE, so
// LiveContainer(spawnID).NodeID == nodeID is false during the exact window a re-presign can be
// requested. Provision writes an entry right after picking a node and removes it (via the same
// defer that clears pending) when it returns, success or failure.
type InFlightStart struct {
	NodeID string
	Gen    uint64
}

// InFlightStart returns the placement Provision currently has in flight for spawnID, if any.
func (s *Scheduler) InFlightStart(spawnID string) (InFlightStart, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	fs, ok := s.inflight[spawnID]
	return fs, ok
}

type RootfsRestore struct {
	SourceGeneration uint64
	Artifacts        []*nodev1.RootfsArtifact
	LocalOnly        bool
}

// New builds a Scheduler. stallWindow is the no-progress stall window (see DefaultStartStallWindow);
// the absolute backstop defaults to defaultStartBackstop and is overridden directly by in-package
// tests.
func New(reg *registry.Registry, rt *router.Router, stallWindow time.Duration) *Scheduler {
	return &Scheduler{
		reg: reg, rt: rt, stallWindow: stallWindow, backstop: defaultStartBackstop,
		pending: map[string]*pendingStart{}, inflight: map[string]InFlightStart{},
	}
}

// OnStatus is called by the node receive loop when a SpawnStatus arrives.
// detail carries the Status.Detail field from the node (machine-readable NACK code + human
// explanation on ERROR; empty on ACTIVE). It is threaded through so callers can return a
// typed Connect error containing the NACK code [AC1].
func (s *Scheduler) OnStatus(spawnID string, phase nodev1.SpawnPhase, detail string) {
	s.mu.Lock()
	p, ok := s.pending[spawnID]
	s.mu.Unlock()
	if ok && (phase == nodev1.SpawnPhase_ACTIVE || phase == nodev1.SpawnPhase_ERROR) {
		select {
		case p.res <- spawnResult{phase: phase, detail: detail}:
		default:
		}
	}
}

// Progress routes a no-progress-stall-timer reset for an in-flight spawn start, matched by spawn_id
// AND generation. Called by the node's ResumeProgress heartbeats (sp-mwco.4.1/4.7 — the node already
// emits these on the StartSpawn path). Stale-generation or unmatched signals are dropped (no reset).
// Returns true if the signal matched a live, current-generation Provision call.
func (s *Scheduler) Progress(spawnID string, gen uint64) bool {
	s.mu.Lock()
	p, ok := s.pending[spawnID]
	s.mu.Unlock()
	if !ok || p.gen != gen {
		return false
	}
	select {
	case p.progress <- struct{}{}:
	default:
	}
	return true
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
// env is the A4 AuthEnvelope (token + SignedIntent) to thread into StartSpawn [AC1]. Low-level
// hermetic fixtures that disable the public two-phase flow may pass nil.
// mounts are the persisted per-mount backend bindings to thread into StartSpawn; nil/empty means
// the spawn has no bound mounts.
// baseImageDigest is threaded to the node for cross-node resume (sp-ei4.1.10); empty on fresh create.
func (s *Scheduler) Provision(ctx context.Context, id, appRef, model, name, appID, runnable, mode string, gen uint64, placement registry.Placement, env *authv1.AuthEnvelope, mounts []*nodev1.MountBinding, baseImageDigest string, rootfs *RootfsRestore, artifacts []*nodev1.ArtifactSpec, secrets []*nodev1.SealedSecret, operations ...intent.Op) (string, error) {
	n := s.reg.PickFor(placement)
	if n == nil {
		cpmetrics.PlacementNoCapacity()
		return "", connect.NewError(connect.CodeResourceExhausted, errors.New("no eligible node with capacity"))
	}
	p := &pendingStart{res: make(chan spawnResult, 1), gen: gen, progress: make(chan struct{}, 1)}
	s.mu.Lock()
	s.pending[id] = p
	s.inflight[id] = InFlightStart{NodeID: n.ID, Gen: gen}
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.pending, id)
		delete(s.inflight, id)
		s.mu.Unlock()
	}()

	start := &nodev1.StartSpawn{
		SpawnId: id, AppRef: appRef, Model: model, Name: name, AppId: appID,
		Image: placement.Image, RunnableId: runnable, Mode: mode, Generation: gen,
		Auth: env, AssertedOwner: placement.Owner, BaseImageDigest: baseImageDigest,
	}
	if len(operations) == 1 {
		start.IntentOp = string(operations[0])
	}
	if len(mounts) > 0 {
		start.Mounts = mounts
	}
	if rootfs != nil && len(rootfs.Artifacts) > 0 {
		start.RootfsSourceGeneration = rootfs.SourceGeneration
		start.RootfsArtifacts = rootfs.Artifacts
		start.RootfsArtifactsLocalOnly = rootfs.LocalOnly
	}
	if len(artifacts) > 0 {
		start.Artifacts = artifacts
	}
	if len(secrets) > 0 {
		start.Secrets = secrets
	}
	if err := n.Sender.Send(&nodev1.CPMessage{Msg: &nodev1.CPMessage_Start{Start: start}}); err != nil {
		return "", connect.NewError(connect.CodeUnavailable, err)
	}

	// Per-heartbeat stall window: resets on each Progress event from the node. Generous absolute
	// backstop: fires only if the stall detector fails to catch a node that heartbeats forever
	// without ever reaching ACTIVE/ERROR. Mirrors lifecycle.go's resume stall/backstop pair.
	stall := time.NewTimer(s.stallWindow)
	defer stall.Stop()
	backstop := time.After(s.backstop)

	for {
		select {
		case res := <-p.res:
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
			cpmetrics.PlacementSuccess() // count only after Bind: the placement is committed and ACTIVE
			return n.ID, nil

		case <-p.progress:
			// Progress from the node: reset the stall timer.
			if !stall.Stop() {
				select {
				case <-stall.C:
				default:
				}
			}
			stall.Reset(s.stallWindow)

		case <-stall.C:
			return "", connect.NewError(connect.CodeDeadlineExceeded,
				fmt.Errorf("spawn start stalled (no progress for %s)", s.stallWindow))

		case <-backstop:
			return "", connect.NewError(connect.CodeDeadlineExceeded,
				fmt.Errorf("spawn start exceeded absolute budget of %s", s.backstop))

		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
}
