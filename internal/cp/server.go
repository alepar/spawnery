// Package cp is the control plane: it accepts node Attach streams and client
// cp.v1 calls, routing between them. Both the node and the CP are transparent
// byte relays — ACP smarts live in the client and agent only.
package cp

import (
	"context"
	"fmt"
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
			s.reg.Add(&registry.Node{ID: nodeID, Sender: sender, Max: m.Register.MaxSpawns, Free: m.Register.MaxSpawns, Images: m.Register.AgentImages})
		case *nodev1.NodeMessage_Heartbeat:
			s.reg.Heartbeat(nodeID, m.Heartbeat.ActiveSpawns, m.Heartbeat.FreeSlots)
		case *nodev1.NodeMessage_Status:
			s.sched.OnStatus(m.Status.SpawnId, m.Status.Phase)
			if m.Status.Phase == nodev1.SpawnPhase_ACTIVE {
				var owner string
				if sp, err := s.st.Spawns().Get(ctx, m.Status.SpawnId); err == nil {
					owner = sp.OwnerID
				}
				_ = s.tel.Emit(telemetry.Event{Kind: "spawn_create", Owner: owner, NodeID: nodeID, SpawnID: m.Status.SpawnId, Tier: "reviewed", Storage: "managed", Timestamp: time.Now().UTC()})
			}
		case *nodev1.NodeMessage_Frame:
			s.rt.FromNode(m.Frame.SpawnId, m.Frame.Data) // opaque bytes; never inspected/logged
		}
	}
}

// --- client side: cp.v1 SpawnService --------------------------------------

func (s *Server) CreateSpawn(ctx context.Context, req *connect.Request[cpv1.CreateSpawnRequest]) (*connect.Response[cpv1.CreateSpawnResponse], error) {
	owner, ok := auth.OwnerFromContext(ctx)
	if !ok {
		return nil, connect.NewError(connect.CodeUnauthenticated, fmt.Errorf("no owner"))
	}
	appID := req.Msg.AppId
	ver, err := s.st.Apps().LatestReviewed(ctx, appID)
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("unknown app: %s", appID))
	}
	decls, err := s.st.Apps().DeclaredMounts(ctx, appID, ver.Version)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	mounts := make([]store.Mount, len(decls))
	for i, d := range decls {
		mounts[i] = store.Mount{Name: d.Name, BackendURI: "scratch"}
	}
	id, err := uuid.NewV7()
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	spawnID := id.String()

	unlock := s.locks.Lock(spawnID)
	defer unlock()

	now := time.Now().Unix()
	sp := store.Spawn{
		ID: spawnID, OwnerID: owner, AppID: appID, AppVersion: ver.Version, AppRef: ver.Ref,
		Model: req.Msg.Model, Status: store.Starting, CreatedAt: now, LastUsedAt: now,
	}
	if err := s.st.WithTx(ctx, func(tx store.Store) error { return tx.Spawns().Create(ctx, sp, mounts) }); err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	nodeID, err := s.sched.Provision(ctx, spawnID, ver.Ref, req.Msg.Model)
	if err != nil {
		_ = s.st.Spawns().SetError(ctx, spawnID)
		return nil, err
	}
	if err := s.st.Spawns().SetActive(ctx, spawnID, nodeID, 1); err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	return connect.NewResponse(&cpv1.CreateSpawnResponse{SpawnId: spawnID}), nil
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

	cs := &clientStream{stream: stream, spawnID: spawnID}
	done, err := s.rt.AttachClient(spawnID, cs)
	if err != nil {
		return connect.NewError(connect.CodeInternal, err)
	}
	_ = s.tel.Emit(telemetry.Event{Kind: "session_start", Owner: sp.OwnerID, SpawnID: spawnID, Timestamp: time.Now().UTC()})
	defer func() {
		s.rt.DetachClient(spawnID)
		_ = s.tel.Emit(telemetry.Event{Kind: "session_end", Owner: sp.OwnerID, SpawnID: spawnID, Timestamp: time.Now().UTC()})
	}()

	if len(first.Data) > 0 {
		_ = s.rt.FromClient(spawnID, first.Data)
	}
	recvErr := make(chan error, 1)
	go func() {
		for {
			f, err := stream.Receive()
			if err != nil {
				recvErr <- err
				return
			}
			if ferr := s.rt.FromClient(spawnID, f.Data); ferr != nil {
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
