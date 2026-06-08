// Package cp is the control plane: it accepts node Attach streams and client
// cp.v1 calls, routing between them. Both the node and the CP are transparent
// byte relays — ACP smarts live in the client and agent only.
package cp

import (
	"context"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"connectrpc.com/connect"
	"github.com/google/uuid"

	cpv1 "spawnery/gen/cp/v1"
	"spawnery/gen/cp/v1/cpv1connect"
	nodev1 "spawnery/gen/node/v1"
	"spawnery/internal/agentcaps"
	"spawnery/internal/cp/auth"
	"spawnery/internal/cp/lock"
	"spawnery/internal/cp/registry"
	"spawnery/internal/cp/router"
	"spawnery/internal/cp/scheduler"
	"spawnery/internal/cp/store"
	"spawnery/internal/cp/telemetry"
)

type Server struct {
	cpv1connect.UnimplementedSpawnServiceHandler // new RPCs default to CodeUnimplemented until sp-pc4
	reg   *registry.Registry
	rt    *router.Router
	sched *scheduler.Scheduler
	st    store.Store
	tel   telemetry.Sink
	locks *lock.Keyed

	maxSpawnsPerOwner int
}

// Server must satisfy the (now larger) connect handler interface; the 5 new lifecycle RPCs are
// served by the embedded Unimplemented handler until sp-pc4 overrides them.
var _ cpv1connect.SpawnServiceHandler = (*Server)(nil)

func NewServer(reg *registry.Registry, rt *router.Router, sched *scheduler.Scheduler, st store.Store, tel telemetry.Sink) *Server {
	return &Server{reg: reg, rt: rt, sched: sched, st: st, tel: tel, locks: lock.New()}
}

// --- node side: NodeService/Attach ----------------------------------------

// nodeStream is the concurrency-safe CP->node sender (one writer per stream).
type nodeStream struct {
	mu     sync.Mutex
	stream *connect.BidiStream[nodev1.NodeMessage, nodev1.CPMessage]
}

func (n *nodeStream) Send(m *nodev1.CPMessage) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.stream.Send(m)
}

func (s *Server) Attach(ctx context.Context, stream *connect.BidiStream[nodev1.NodeMessage, nodev1.CPMessage]) error {
	sender := &nodeStream{stream: stream}
	return s.runNode(ctx, sender, stream.Receive)
}

// runNode is the receive loop, split out so it is unit-testable without gRPC.
func (s *Server) runNode(ctx context.Context, sender registry.NodeSender, recv func() (*nodev1.NodeMessage, error)) error {
	var nodeID string
	var nodeClass string
	var token uint64 // this connection's registry token (0 until accepted); guards teardown
	defer func() {
		// Only tear down routes if THIS connection is still the registered owner. A rejected
		// duplicate (token 0) or a displaced old stream must not drop the live node's routes.
		if nodeID == "" || !s.reg.RemoveIfCurrent(nodeID, token) {
			return
		}
		dropped := s.rt.DropNode(nodeID)
		if len(dropped) > 0 {
			_, _ = s.st.Spawns().MarkUnreachable(context.Background(), dropped)
		}
		for _, id := range dropped {
			_ = s.tel.Emit(telemetry.Event{Kind: "session_end", NodeID: nodeID, SpawnID: id, Timestamp: time.Now().UTC()})
		}
	}()
	for {
		msg, err := recv()
		if err != nil {
			return nil // stream closed
		}
		// If a newer connection has taken over this node id (we were displaced), stop serving.
		if token != 0 && !s.reg.IsCurrent(nodeID, token) {
			return nil
		}
		switch m := msg.Msg.(type) {
		case *nodev1.NodeMessage_Register:
			nodeID = m.Register.NodeId
			nodeClass = m.Register.NodeClass
			if nodeClass == "" {
				nodeClass = "cloud" // safe default: an unidentified node is assumed restricted
			}
			tok, accepted := s.reg.Register(&registry.Node{ID: nodeID, Sender: sender, Max: m.Register.MaxSpawns, Free: m.Register.MaxSpawns, Images: m.Register.AgentImages, Class: nodeClass, Owner: m.Register.NodeOwner})
			if !accepted {
				// A live node already holds this id: reject the duplicate rather than corrupt routing.
				log.Printf("rejecting registration for node id=%s: another node with that id is still alive", nodeID)
				return connect.NewError(connect.CodeAlreadyExists, fmt.Errorf("node id %q is already registered and alive", nodeID))
			}
			token = tok
			log.Printf("node connected: id=%s class=%s owner=%q max_spawns=%d images=%v", nodeID, nodeClass, m.Register.NodeOwner, m.Register.MaxSpawns, m.Register.AgentImages)
			s.reconcileInventory(ctx, nodeID, m.Register.Running) // a returning node reports what it still runs
			s.upsertAgentCatalog(ctx, m.Register.AgentImages, m.Register.Binaries)
		case *nodev1.NodeMessage_Heartbeat:
			s.reg.Heartbeat(nodeID, token, m.Heartbeat.ActiveSpawns, m.Heartbeat.FreeSlots)
			s.reconcileInventory(ctx, nodeID, m.Heartbeat.Running)
		case *nodev1.NodeMessage_Status:
			s.sched.OnStatus(m.Status.SpawnId, m.Status.Phase)
			if m.Status.Phase == nodev1.SpawnPhase_ACTIVE {
				var owner string
				if sp, err := s.st.Spawns().Get(ctx, m.Status.SpawnId); err == nil {
					owner = sp.OwnerID
				}
				_ = s.tel.Emit(telemetry.Event{Kind: "spawn_create", Owner: owner, NodeID: nodeID, NodeClass: nodeClass, SpawnID: m.Status.SpawnId, Tier: "reviewed", Storage: "managed", Timestamp: time.Now().UTC()})
			}
		case *nodev1.NodeMessage_Frame:
			s.rt.FromNode(m.Frame.SpawnId, m.Frame.SessionId, m.Frame.ClientId, m.Frame.Data) // opaque bytes; never inspected
		case *nodev1.NodeMessage_Roster:
			s.rt.UpdateRoster(m.Roster.SpawnId, nodeID, m.Roster.Sessions) // node-authoritative session set; CP mirrors
		case *nodev1.NodeMessage_SessionStatus:
			s.rt.ApplySessionStatus(m.SessionStatus.SpawnId, m.SessionStatus.SessionId, m.SessionStatus.State)
		}
	}
}

// reconcileInventory marks any ACTIVE spawn the CP believes runs on nodeID but the node is NOT
// reporting in its running inventory (e.g. it restarted and lost the pod, or the pod died) as
// unreachable, dropping its route. Starting spawns are skipped (may not be in the inventory yet).
// User-driven recovery (RecreateSpawn) takes it from there. Builds on the node's RunningSpawn report.
func (s *Server) reconcileInventory(ctx context.Context, nodeID string, running []*nodev1.RunningSpawn) {
	if nodeID == "" {
		return
	}
	reported := make(map[string]bool, len(running))
	for _, rs := range running {
		reported[rs.GetSpawnId()] = true
	}
	live, err := s.st.Spawns().LiveContainersByNode(ctx, nodeID)
	if err != nil {
		return
	}
	var lost []string
	for _, c := range live {
		if c.Phase == store.PhaseActive && !reported[c.SpawnID] {
			lost = append(lost, c.SpawnID)
		}
	}
	if len(lost) == 0 {
		return
	}
	for _, id := range lost {
		s.rt.Drop(id)
	}
	if n, err := s.st.Spawns().MarkUnreachable(ctx, lost); err == nil && n > 0 {
		log.Printf("node %s inventory: %d active spawn(s) not reported -> unreachable", nodeID, n)
	}
}

// upsertAgentCatalog records each advertised image and the binaries it ships so the durable catalog
// (ListAgentImages + runnable validation) reflects what connected nodes can run. Idempotent across
// reconnects: created_at is preserved and the binary set is replaced. Errors are logged, not fatal —
// a catalog write must never break node registration.
func (s *Server) upsertAgentCatalog(ctx context.Context, images, binaries []string) {
	now := time.Now().Unix()
	for _, img := range images {
		if img == "" {
			continue
		}
		if err := s.st.WithTx(ctx, func(tx store.Store) error {
			return tx.AgentImages().Upsert(ctx, store.AgentImage{Image: img, CreatedAt: now}, binaries)
		}); err != nil {
			log.Printf("register: upsert agent image %q: %v", img, err)
		}
	}
}

// lookupRunnable finds the first runnable matching id across an image's binaries.
func lookupRunnable(bins []string, id string) (agentcaps.Runnable, bool) {
	for _, b := range bins {
		if r, ok := agentcaps.Lookup(b, id); ok {
			return r, true
		}
	}
	return agentcaps.Runnable{}, false
}

// --- client side: cp.v1 SpawnService --------------------------------------

// SetMaxSpawnsPerOwner sets the per-owner concurrent-spawn cap (0 = unlimited).
func (s *Server) SetMaxSpawnsPerOwner(n int) { s.maxSpawnsPerOwner = n }

// checkSpawnQuota returns ResourceExhausted if the owner is at/over the per-owner spawn cap.
func (s *Server) checkSpawnQuota(ctx context.Context, owner string) error {
	if s.maxSpawnsPerOwner <= 0 {
		return nil
	}
	existing, err := s.st.Spawns().ListByOwner(ctx, owner)
	if err != nil {
		return connect.NewError(connect.CodeInternal, err)
	}
	if len(existing) >= s.maxSpawnsPerOwner {
		return connect.NewError(connect.CodeResourceExhausted, fmt.Errorf("spawn limit reached (%d)", s.maxSpawnsPerOwner))
	}
	return nil
}

func (s *Server) CreateSpawn(ctx context.Context, req *connect.Request[cpv1.CreateSpawnRequest]) (*connect.Response[cpv1.CreateSpawnResponse], error) {
	owner, ok := auth.OwnerFromContext(ctx)
	if !ok {
		return nil, connect.NewError(connect.CodeUnauthenticated, fmt.Errorf("no owner"))
	}
	if err := s.checkSpawnQuota(ctx, owner); err != nil {
		return nil, err
	}
	appID := req.Msg.AppId
	var err error
	var ver store.AppVersion
	if v := req.Msg.Version; v != "" {
		ver, err = s.st.Apps().GetVersion(ctx, appID, v)
		if err != nil {
			return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("unknown app version: %s@%s", appID, v))
		}
	} else {
		ver, err = s.st.Apps().LatestReviewed(ctx, appID)
		if err != nil {
			return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("unknown app: %s", appID))
		}
	}
	// Resolve the optional agent selection: validate the runnable is offered by the chosen
	// image's binaries, resolve the run mode, and reject modes we can't launch yet.
	var selImage, selRunnable, selMode string
	if req.Msg.Image == "" && req.Msg.RunnableId != "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("image is required when runnable_id is set"))
	}
	if req.Msg.Image != "" {
		selImage = req.Msg.Image
		if req.Msg.RunnableId == "" {
			return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("runnable_id is required when image is set"))
		}
		bins, berr := s.st.AgentImages().Binaries(ctx, selImage)
		if berr != nil {
			return nil, connect.NewError(connect.CodeInternal, berr)
		}
		if len(bins) == 0 {
			return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("unknown or empty agent image: %s", selImage))
		}
		run, found := lookupRunnable(bins, req.Msg.RunnableId)
		if !found {
			return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("runnable %q is not offered by image %s", req.Msg.RunnableId, selImage))
		}
		selRunnable = run.ID
		selMode = string(run.Mode)
	}
	decls, err := s.st.Apps().DeclaredMounts(ctx, appID, ver.Version)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	mounts := make([]store.Mount, len(decls))
	for i, d := range decls {
		mounts[i] = store.Mount{Name: d.Name, BackendURI: "scratch"}
	}
	placement, err := s.placementFor(ctx, owner, appID, ver)
	if err != nil {
		return nil, err
	}
	id, err := uuid.NewV7()
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	spawnID := id.String()

	unlock := s.locks.Lock(spawnID)
	defer unlock()

	// Name is best-effort dedup: the lock above is keyed by spawn id (not owner), so concurrent
	// creates for one owner may still produce duplicate names — harmless, since the spawn id is the key.
	name := strings.TrimSpace(req.Msg.Name)
	if name == "" {
		base := appID
		if app, aerr := s.st.Apps().Get(ctx, appID); aerr == nil && app.DisplayName != "" {
			base = app.DisplayName
		}
		existing, lerr := s.st.Spawns().ListByOwner(ctx, owner)
		if lerr != nil {
			return nil, connect.NewError(connect.CodeInternal, lerr)
		}
		taken := make(map[string]bool, len(existing))
		for _, e := range existing {
			taken[e.Name] = true
		}
		name = nextSpawnName(base, taken)
	}

	now := time.Now().Unix()
	sp := store.Spawn{
		ID: spawnID, OwnerID: owner, Name: name, AppID: appID, AppVersion: ver.Version, AppRef: ver.Ref, Pinned: req.Msg.Pin,
		Model: req.Msg.Model, Image: selImage, RunnableID: selRunnable, Mode: selMode,
		Status: store.Starting, CreatedAt: now, LastUsedAt: now,
	}
	if err := s.st.WithTx(ctx, func(tx store.Store) error { return tx.Spawns().Create(ctx, sp, mounts) }); err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	// Provision asynchronously: return the spawn in 'starting' immediately; the background goroutine
	// drives it to active/error on the node's signal, so the UI can show a 'starting' period. The
	// request ctx is done once we return, so the goroutine uses a detached ctx.
	go s.provisionSpawn(context.WithoutCancel(ctx), spawnID, ver.Ref, req.Msg.Model, placement)
	return connect.NewResponse(&cpv1.CreateSpawnResponse{SpawnId: spawnID}), nil
}

// provisionSpawn runs the async provision for a spawn that CreateSpawn left in 'starting'. It takes
// the per-spawn lock (serializing a Stop/Suspend during starting AFTER it) and bails if the spawn was
// already stopped in the lock gap; then Provision -> SetActive, or SetError on failure, with the same
// teardown compensation as the old inline path on a post-provision SetActive failure.
func (s *Server) provisionSpawn(ctx context.Context, spawnID, appRef, model string, placement registry.Placement) {
	unlock := s.locks.Lock(spawnID)
	defer unlock()
	sp, err := s.st.Spawns().Get(ctx, spawnID)
	if err != nil || sp.Status != store.Starting {
		return // stopped/deleted in the lock gap, or already advanced
	}
	placement.Image = sp.Image
	nodeID, err := s.sched.Provision(ctx, spawnID, appRef, model, sp.Name, sp.AppID, sp.RunnableID, sp.Mode, placement)
	if err != nil {
		log.Printf("provisionSpawn %s: provision failed: %v", spawnID, err)
		if serr := s.st.Spawns().SetError(ctx, spawnID); serr != nil {
			log.Printf("provisionSpawn %s: SetError after provision failure also failed: %v", spawnID, serr)
		}
		return
	}
	if err := s.st.Spawns().SetActive(ctx, spawnID, nodeID, 1); err != nil {
		s.rt.StopOnNode(spawnID)
		s.rt.Drop(spawnID)
		if serr := s.st.Spawns().SetError(ctx, spawnID); serr != nil {
			log.Printf("provisionSpawn %s: SetError after SetActive failure also failed: %v", spawnID, serr)
		}
	}
}

// placementFor computes node placement for a spawn of the given app version. Reviewed/scanned
// versions run anywhere; unverified/unknown versions are author-self-host only (PermissionDenied for
// a non-creator caller). Shared by CreateSpawn and ResumeSpawn.
func (s *Server) placementFor(ctx context.Context, owner, appID string, ver store.AppVersion) (registry.Placement, error) {
	if ver.Tier == store.TierReviewed || ver.Tier == store.TierScanned {
		return registry.Placement{}, nil
	}
	creator, err := s.st.Apps().Creator(ctx, appID)
	if err != nil {
		return registry.Placement{}, connect.NewError(connect.CodeInternal, err)
	}
	if creator != owner {
		return registry.Placement{}, connect.NewError(connect.CodePermissionDenied, fmt.Errorf("only the author can run an unverified version of %s", appID))
	}
	return registry.Placement{Class: "self-hosted", Owner: owner}, nil
}

func (s *Server) StopSpawn(ctx context.Context, req *connect.Request[cpv1.StopSpawnRequest]) (*connect.Response[cpv1.StopSpawnResponse], error) {
	owner, ok := auth.OwnerFromContext(ctx)
	if !ok {
		return nil, connect.NewError(connect.CodeUnauthenticated, fmt.Errorf("no owner"))
	}
	if err := s.stop(ctx, owner, req.Msg.SpawnId); err != nil {
		return nil, err
	}
	return connect.NewResponse(&cpv1.StopSpawnResponse{}), nil
}

// stop validates ownership via the store, tells the node to destroy the pod, drops the route, and
// soft-deletes the spawn (today's StopSpawn is a destroy; suspend is Part 3).
func (s *Server) stop(ctx context.Context, owner, spawnID string) error {
	unlock := s.locks.Lock(spawnID)
	defer unlock()
	sp, err := s.st.Spawns().Get(ctx, spawnID)
	if err != nil {
		return connect.NewError(connect.CodeNotFound, fmt.Errorf("unknown spawn"))
	}
	if sp.OwnerID != owner {
		return connect.NewError(connect.CodePermissionDenied, fmt.Errorf("not your spawn"))
	}
	s.rt.StopOnNode(spawnID)
	s.rt.Drop(spawnID)
	if err := s.st.Spawns().MarkDeleted(ctx, spawnID, time.Now().Unix()); err != nil {
		return connect.NewError(connect.CodeInternal, err)
	}
	_ = s.tel.Emit(telemetry.Event{Kind: "session_end", Owner: owner, SpawnID: spawnID, Timestamp: time.Now().UTC()})
	return nil
}

// --- client-facing session RPCs (answered from CP's mirrored roster) ------

// node and cp SessionTransport enums share ordinals by construction (see the protos), so the cast is total.
func toNodeTransport(t cpv1.SessionTransport) nodev1.SessionTransport {
	return nodev1.SessionTransport(t)
}
func toCPTransport(t nodev1.SessionTransport) cpv1.SessionTransport { return cpv1.SessionTransport(t) }

func sessionStateString(st nodev1.SessionState) string {
	switch st {
	case nodev1.SessionState_SESSION_STATE_STARTING:
		return "starting"
	case nodev1.SessionState_SESSION_STATE_ACTIVE:
		return "active"
	case nodev1.SessionState_SESSION_STATE_CLOSING:
		return "closing"
	case nodev1.SessionState_SESSION_STATE_CLOSED:
		return "closed"
	case nodev1.SessionState_SESSION_STATE_ERROR:
		return "error"
	default:
		return "unspecified"
	}
}

// ownSpawn loads a spawn and verifies the caller owns it (shared by the session RPCs).
func (s *Server) ownSpawn(ctx context.Context, spawnID string) error {
	owner, ok := auth.OwnerFromContext(ctx)
	if !ok {
		return connect.NewError(connect.CodeUnauthenticated, fmt.Errorf("no owner"))
	}
	sp, err := s.st.Spawns().Get(ctx, spawnID)
	if err != nil {
		return connect.NewError(connect.CodeNotFound, fmt.Errorf("unknown spawn"))
	}
	if sp.OwnerID != owner {
		return connect.NewError(connect.CodePermissionDenied, fmt.Errorf("not your spawn"))
	}
	return nil
}

// ListSessions returns the CP's mirrored roster for a spawn (no node round-trip).
func (s *Server) ListSessions(ctx context.Context, req *connect.Request[cpv1.ListSessionsRequest]) (*connect.Response[cpv1.ListSessionsResponse], error) {
	if err := s.ownSpawn(ctx, req.Msg.SpawnId); err != nil {
		return nil, err
	}
	mirror := s.rt.ListSessions(req.Msg.SpawnId)
	out := make([]*cpv1.SessionDescriptor, 0, len(mirror))
	for _, si := range mirror {
		out = append(out, &cpv1.SessionDescriptor{
			SessionId: si.SessionId, Transport: toCPTransport(si.Transport), Runnable: si.Runnable,
			Status: sessionStateString(si.State), Pinned: si.Pinned,
		})
	}
	return connect.NewResponse(&cpv1.ListSessionsResponse{Sessions: out}), nil
}

// CreateSession asks the hosting node to launch an additional session (node allocates the id).
func (s *Server) CreateSession(ctx context.Context, req *connect.Request[cpv1.CreateSessionRequest]) (*connect.Response[cpv1.CreateSessionResponse], error) {
	if err := s.ownSpawn(ctx, req.Msg.SpawnId); err != nil {
		return nil, err
	}
	if req.Msg.Transport == cpv1.SessionTransport_SESSION_TRANSPORT_UNSPECIFIED {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("transport is required"))
	}
	if req.Msg.Runnable == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("runnable is required"))
	}
	if err := s.rt.CreateSession(req.Msg.SpawnId, toNodeTransport(req.Msg.Transport), req.Msg.Runnable); err != nil {
		return nil, connect.NewError(connect.CodeUnavailable, err)
	}
	return connect.NewResponse(&cpv1.CreateSessionResponse{}), nil
}

// CloseSession asks the hosting node to reap one session. Session "0" is pinned (reject; stop the spawn).
func (s *Server) CloseSession(ctx context.Context, req *connect.Request[cpv1.CloseSessionRequest]) (*connect.Response[cpv1.CloseSessionResponse], error) {
	if err := s.ownSpawn(ctx, req.Msg.SpawnId); err != nil {
		return nil, err
	}
	// Default empty -> "0" so a missing session_id is rejected as pinned rather than silently succeeding.
	sessionID := req.Msg.SessionId
	if sessionID == "" {
		sessionID = "0" // node.SessionZeroID, inlined (wire-stable) to avoid an import cycle
	}
	if sessionID == "0" {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("session #0 is pinned; stop the spawn instead"))
	}
	if err := s.rt.CloseSession(req.Msg.SpawnId, sessionID); err != nil {
		return nil, connect.NewError(connect.CodeUnavailable, err)
	}
	return connect.NewResponse(&cpv1.CloseSessionResponse{}), nil
}

func (s *Server) Session(ctx context.Context, stream *connect.BidiStream[cpv1.Frame, cpv1.Frame]) error {
	first, err := stream.Receive()
	if err != nil {
		return err
	}
	spawnID := first.SpawnId
	owner, _ := auth.OwnerFromContext(ctx)
	sp, err := s.st.Spawns().Get(ctx, spawnID)
	if err != nil {
		return connect.NewError(connect.CodeNotFound, fmt.Errorf("unknown spawn"))
	}
	if owner != sp.OwnerID {
		return connect.NewError(connect.CodePermissionDenied, fmt.Errorf("not your spawn"))
	}

	clientID := uuid.Must(uuid.NewV7()).String()
	cs := &clientStream{stream: stream, spawnID: spawnID}
	// cursor 0: the cp.v1 Session-RPC transport has no resume cursor (only the WS bind does).
	// session "0": this transport has no per-session selector yet (web uses the WS bind for that).
	done, err := s.rt.AttachClient(spawnID, "0", clientID, cs, 0)
	if err != nil {
		return connect.NewError(connect.CodeInternal, err)
	}
	_ = s.tel.Emit(telemetry.Event{Kind: "session_start", Owner: sp.OwnerID, SpawnID: spawnID, Timestamp: time.Now().UTC()})
	defer func() {
		s.rt.DetachClient(spawnID, "0", clientID)
		_ = s.tel.Emit(telemetry.Event{Kind: "session_end", Owner: sp.OwnerID, SpawnID: spawnID, Timestamp: time.Now().UTC()})
	}()

	if len(first.Data) > 0 {
		_ = s.rt.FromClient(spawnID, "0", clientID, first.Data)
	}
	recvErr := make(chan error, 1)
	go func() {
		for {
			f, err := stream.Receive()
			if err != nil {
				recvErr <- err
				return
			}
			if ferr := s.rt.FromClient(spawnID, "0", clientID, f.Data); ferr != nil {
				recvErr <- ferr
				return
			}
		}
	}()
	select {
	case <-done:
		return nil
	case <-recvErr:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// clientStream is the CP->client sender for the router.
type clientStream struct {
	stream  *connect.BidiStream[cpv1.Frame, cpv1.Frame]
	spawnID string
}

func (c *clientStream) Send(b []byte) error {
	return c.stream.Send(&cpv1.Frame{SpawnId: c.spawnID, Data: b})
}
