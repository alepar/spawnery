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

	cpv1 "spawnery/gen/cp/v1"
	nodev1 "spawnery/gen/node/v1"
	"spawnery/internal/cp/apps"
	"spawnery/internal/cp/auth"
	"spawnery/internal/cp/registry"
	"spawnery/internal/cp/router"
	"spawnery/internal/cp/scheduler"
	"spawnery/internal/cp/telemetry"
)

type Server struct {
	reg   *registry.Registry
	rt    *router.Router
	sched *scheduler.Scheduler
	apps  *apps.Resolver
	tel   telemetry.Sink
}

func NewServer(reg *registry.Registry, rt *router.Router, sched *scheduler.Scheduler, ar *apps.Resolver, tel telemetry.Sink) *Server {
	return &Server{reg: reg, rt: rt, sched: sched, apps: ar, tel: tel}
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
			for _, id := range s.rt.DropNode(nodeID) {
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
				owner, _ := s.rt.Owner(m.Status.SpawnId)
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
	ref, ok := s.apps.Resolve(req.Msg.AppId)
	if !ok {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("unknown app: %s", req.Msg.AppId))
	}
	id, _, err := s.sched.Create(ctx, owner, req.Msg.AppId, ref, req.Msg.Model)
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&cpv1.CreateSpawnResponse{SpawnId: id}), nil
}

func (s *Server) StopSpawn(ctx context.Context, req *connect.Request[cpv1.StopSpawnRequest]) (*connect.Response[cpv1.StopSpawnResponse], error) {
	owner, _ := auth.OwnerFromContext(ctx)
	if err := s.stop(owner, req.Msg.SpawnId); err != nil {
		return nil, err
	}
	return connect.NewResponse(&cpv1.StopSpawnResponse{}), nil
}

// stop validates ownership, tells the node to destroy the pod, drops the route,
// and emits session_end.
func (s *Server) stop(owner, spawnID string) error {
	rtOwner, ok := s.rt.Owner(spawnID)
	if !ok {
		return connect.NewError(connect.CodeNotFound, fmt.Errorf("unknown spawn"))
	}
	if owner != rtOwner {
		return connect.NewError(connect.CodePermissionDenied, fmt.Errorf("not your spawn"))
	}
	s.rt.StopOnNode(spawnID)
	s.rt.Drop(spawnID)
	_ = s.tel.Emit(telemetry.Event{Kind: "session_end", Owner: rtOwner, SpawnID: spawnID, Timestamp: time.Now().UTC()})
	return nil
}

func (s *Server) Session(ctx context.Context, stream *connect.BidiStream[cpv1.Frame, cpv1.Frame]) error {
	first, err := stream.Receive()
	if err != nil {
		return err
	}
	spawnID := first.SpawnId
	owner, _ := auth.OwnerFromContext(ctx)
	rtOwner, ok := s.rt.Owner(spawnID)
	if !ok {
		return connect.NewError(connect.CodeNotFound, fmt.Errorf("unknown spawn"))
	}
	if owner != rtOwner {
		return connect.NewError(connect.CodePermissionDenied, fmt.Errorf("not your spawn"))
	}

	cs := &clientStream{stream: stream, spawnID: spawnID}
	done, err := s.rt.AttachClient(spawnID, cs)
	if err != nil {
		return connect.NewError(connect.CodeInternal, err)
	}
	_ = s.tel.Emit(telemetry.Event{Kind: "session_start", Owner: rtOwner, SpawnID: spawnID, Timestamp: time.Now().UTC()})
	defer func() {
		s.rt.DetachClient(spawnID)
		_ = s.tel.Emit(telemetry.Event{Kind: "session_end", Owner: rtOwner, SpawnID: spawnID, Timestamp: time.Now().UTC()})
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
