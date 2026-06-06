package spawnlet

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"

	"connectrpc.com/connect"
	spawnv1 "spawnery/gen/spawn/v1"
	"spawnery/gen/spawn/v1/spawnv1connect"
)

type Server struct {
	spawnv1connect.UnimplementedSpawnServiceHandler
	m *Manager
}

func NewServer(m *Manager) *Server { return &Server{m: m} }

// HandleTerminal starts a mosh-backed terminal session for a spawn and returns the connect info
// {host, port, key} as JSON. spawnctl attach/exec/shell POST here; the mosh UDP data plane then
// goes straight to this node. An optional JSON body {"cmd":[...]} selects the in-container command:
// empty => the opencode TUI; e.g. ["/bin/bash"] => a raw shell (un-audited; owner-only).
//
//	POST /terminal?spawn=<id>            -> opencode TUI
//	POST /terminal?spawn=<id>  {"cmd":["/bin/bash"]} -> raw shell
func (s *Server) HandleTerminal(w http.ResponseWriter, r *http.Request) {
	spawnID := r.URL.Query().Get("spawn")
	if spawnID == "" {
		http.Error(w, "missing ?spawn=<id>", http.StatusBadRequest)
		return
	}
	var body struct {
		Cmd []string `json:"cmd"`
	}
	if r.Body != nil {
		_ = json.NewDecoder(r.Body).Decode(&body) // empty/absent body is fine (cmd nil)
	}
	ts, err := s.m.StartTerminal(r.Context(), spawnID, body.Cmd)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(ts)
}

func newID() string {
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func (s *Server) CreateSpawn(ctx context.Context, req *connect.Request[spawnv1.CreateSpawnRequest]) (*connect.Response[spawnv1.CreateSpawnResponse], error) {
	id := newID()
	if _, err := s.m.Create(ctx, id, req.Msg.AppPath, req.Msg.Model, "", 0); err != nil { // standalone: no CP name/generation
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
	att, err := s.m.Attach(ctx, sp)
	if err != nil {
		return connect.NewError(connect.CodeInternal, err)
	}
	defer func() { _ = att.Close() }()

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
