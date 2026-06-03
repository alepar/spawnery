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
	defer func() {
		if nodeID != "" {
			s.reg.Remove(nodeID)
			dropped := s.rt.DropNode(nodeID)
			if len(dropped) > 0 {
				_, _ = s.st.Spawns().MarkUnreachable(context.Background(), dropped)
			}
			for _, id := range dropped {
				_ = s.tel.Emit(telemetry.Event{Kind: "session_end", NodeID: nodeID, SpawnID: id, Timestamp: time.Now().UTC()})
			}
		}
	}()
	for {
		msg, err := recv()
		if err != nil {
			return nil // stream closed
		}
		switch m := msg.Msg.(type) {
		case *nodev1.NodeMessage_Register:
			nodeID = m.Register.NodeId
			nodeClass = m.Register.NodeClass
			if nodeClass == "" {
				nodeClass = "cloud" // safe default: an unidentified node is assumed restricted
			}
			s.reg.Add(&registry.Node{ID: nodeID, Sender: sender, Max: m.Register.MaxSpawns, Free: m.Register.MaxSpawns, Images: m.Register.AgentImages, Class: nodeClass, Owner: m.Register.NodeOwner})
			log.Printf("node connected: id=%s class=%s owner=%q max_spawns=%d images=%v", nodeID, nodeClass, m.Register.NodeOwner, m.Register.MaxSpawns, m.Register.AgentImages)
		case *nodev1.NodeMessage_Heartbeat:
			s.reg.Heartbeat(nodeID, m.Heartbeat.ActiveSpawns, m.Heartbeat.FreeSlots)
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
			s.rt.FromNode(m.Frame.SpawnId, m.Frame.ClientId, m.Frame.Data) // opaque bytes; never inspected
		}
	}
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
		Model: req.Msg.Model, Status: store.Starting, CreatedAt: now, LastUsedAt: now,
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
	if sp, err := s.st.Spawns().Get(ctx, spawnID); err != nil || sp.Status != store.Starting {
		return // stopped/deleted in the lock gap, or already advanced
	}
	nodeID, err := s.sched.Provision(ctx, spawnID, appRef, model, placement)
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
	owner, _ := auth.OwnerFromContext(ctx)
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
	done, err := s.rt.AttachClient(spawnID, clientID, cs, 0)
	if err != nil {
		return connect.NewError(connect.CodeInternal, err)
	}
	_ = s.tel.Emit(telemetry.Event{Kind: "session_start", Owner: sp.OwnerID, SpawnID: spawnID, Timestamp: time.Now().UTC()})
	defer func() {
		s.rt.DetachClient(spawnID, clientID)
		_ = s.tel.Emit(telemetry.Event{Kind: "session_end", Owner: sp.OwnerID, SpawnID: spawnID, Timestamp: time.Now().UTC()})
	}()

	if len(first.Data) > 0 {
		_ = s.rt.FromClient(spawnID, clientID, first.Data)
	}
	recvErr := make(chan error, 1)
	go func() {
		for {
			f, err := stream.Receive()
			if err != nil {
				recvErr <- err
				return
			}
			if ferr := s.rt.FromClient(spawnID, clientID, f.Data); ferr != nil {
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
