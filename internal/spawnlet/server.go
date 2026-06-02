package spawnlet

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"

	"connectrpc.com/connect"
	spawnv1 "spawnery/gen/spawn/v1"
	"spawnery/gen/spawn/v1/spawnv1connect"
	"spawnery/internal/runtime"
)

type Server struct {
	spawnv1connect.UnimplementedSpawnServiceHandler
	m      *Manager
	attach func(ctx context.Context, sp *Spawn) (*runtime.AttachedStream, error)
}

func NewServer(m *Manager) *Server {
	return &Server{
		m: m,
		attach: func(ctx context.Context, sp *Spawn) (*runtime.AttachedStream, error) {
			return runtime.AttachACP(ctx, sp.NetnsPath)
		},
	}
}

func newID() string {
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func (s *Server) CreateSpawn(ctx context.Context, req *connect.Request[spawnv1.CreateSpawnRequest]) (*connect.Response[spawnv1.CreateSpawnResponse], error) {
	id := newID()
	if _, err := s.m.Create(ctx, id, req.Msg.AppPath, req.Msg.Model); err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	return connect.NewResponse(&spawnv1.CreateSpawnResponse{SpawnId: id}), nil
}

func (s *Server) StopSpawn(ctx context.Context, req *connect.Request[spawnv1.StopSpawnRequest]) (*connect.Response[spawnv1.StopSpawnResponse], error) {
	if err := s.m.Stop(ctx, req.Msg.SpawnId); err != nil {
		return nil, connect.NewError(connect.CodeNotFound, err)
	}
	return connect.NewResponse(&spawnv1.StopSpawnResponse{}), nil
}

func (s *Server) Session(ctx context.Context, stream *connect.BidiStream[spawnv1.Frame, spawnv1.Frame]) error {
	// First frame binds the spawn.
	first, err := stream.Receive()
	if err != nil {
		return err
	}
	sp, ok := s.m.Store().Get(first.SpawnId)
	if !ok {
		return connect.NewError(connect.CodeNotFound, fmt.Errorf("spawn not found: %s", first.SpawnId))
	}
	att, err := s.attach(ctx, sp)
	if err != nil {
		return connect.NewError(connect.CodeInternal, err)
	}
	defer att.Close()

	ep := StreamEndpoint{
		Recv: func() ([]byte, error) {
			f, err := stream.Receive()
			if err != nil {
				return nil, err
			}
			return f.Data, nil
		},
		Send: func(b []byte) error {
			return stream.Send(&spawnv1.Frame{SpawnId: sp.ID, Data: b})
		},
	}
	// feed the first frame's payload through, then relay.
	if len(first.Data) > 0 {
		_, _ = att.Stdin.Write(first.Data)
	}
	Relay(ctx, ep, AgentIO{Stdin: att.Stdin, Stdout: att.Stdout})
	return nil
}
