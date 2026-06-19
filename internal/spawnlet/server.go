package spawnlet

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"

	"connectrpc.com/connect"
	"spawnery/internal/execstream"

	spawnv1 "spawnery/gen/spawn/v1"
	"spawnery/gen/spawn/v1/spawnv1connect"
)

type Server struct {
	spawnv1connect.UnimplementedSpawnServiceHandler
	m *Manager
}

func NewServer(m *Manager) *Server { return &Server{m: m} }

// HandleTerminal starts a mosh-backed terminal session for a spawn and returns the connect info
// {host, port, key} as JSON. spawnctl attach/shell POST here; the mosh UDP data plane then
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

// HandleExec runs a command non-interactively in a spawn's agent container and streams its stdout,
// stderr, and exit code back as an execstream frame protocol over the (chunked) HTTP response. It is
// the node side of `spawnctl exec` (sp-8v39) — a scriptable, exit-code-propagating sibling of the
// mosh-backed /terminal. Like /terminal it is node-direct, owner-only, and un-audited (raw exec bypasses
// the sidecar). Pre-stream failures (missing spawn/cmd) are clean non-200s; a failure after streaming
// has begun is sent as an execstream Error frame.
//
//	POST /exec?spawn=<id>  {"cmd":["go","test","./..."]}
func (s *Server) HandleExec(w http.ResponseWriter, r *http.Request) {
	spawnID := r.URL.Query().Get("spawn")
	if spawnID == "" {
		http.Error(w, "missing ?spawn=<id>", http.StatusBadRequest)
		return
	}
	var body struct {
		Cmd []string `json:"cmd"`
	}
	if r.Body != nil {
		_ = json.NewDecoder(r.Body).Decode(&body)
	}
	if len(body.Cmd) == 0 {
		http.Error(w, "missing cmd: POST {\"cmd\":[...]}", http.StatusBadRequest)
		return
	}
	// Resolve the spawn before committing to a 200 streaming response, so an unknown spawn is a clean
	// non-200 the client treats as fatal (rather than a mid-stream error frame).
	if _, ok := s.m.Store().Get(spawnID); !ok {
		http.Error(w, "spawn not found: "+spawnID, http.StatusNotFound)
		return
	}

	w.Header().Set("Content-Type", "application/octet-stream")
	w.WriteHeader(http.StatusOK)
	flush := func() {}
	if f, ok := w.(http.Flusher); ok {
		flush = f.Flush
	}
	mux := execstream.NewMuxer(w, flush)
	code, err := s.m.ExecStream(r.Context(), spawnID, body.Cmd,
		mux.Writer(execstream.Stdout), mux.Writer(execstream.Stderr))
	if err != nil {
		_ = mux.WriteError(err.Error())
		return
	}
	_ = mux.WriteExit(code)
}

func newID() string {
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func (s *Server) CreateSpawn(ctx context.Context, req *connect.Request[spawnv1.CreateSpawnRequest]) (*connect.Response[spawnv1.CreateSpawnResponse], error) {
	id := newID()
	if _, err := s.m.Create(ctx, id, req.Msg.AppPath, req.Msg.Model, "", "", 0); err != nil { // standalone: no CP name/generation
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
