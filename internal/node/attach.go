// Package node implements the spawnlet's CP-attached mode: it dials the CP,
// registers, heartbeats, and services CPMessages by reusing the existing
// spawnlet Manager + transparent Relay. It never accepts inbound connections.
package node

import (
	"context"
	"log"
	"sync"
	"time"

	"connectrpc.com/connect"

	nodev1 "spawnery/gen/node/v1"
	"spawnery/gen/node/v1/nodev1connect"
	"spawnery/internal/runtime"
	"spawnery/internal/spawnlet"
)

type Config struct {
	NodeID     string
	CPURL      string // e.g. http://127.0.0.1:8080
	MaxSpawns  uint32
	AgentImage string
	NodeClass  string
	NodeOwner  string
}

// session tracks a live relay so SessionClose can cancel it.
type session struct{ cancel context.CancelFunc }

type attacher struct {
	cfg   Config
	mgr   *spawnlet.Manager
	httpc connect.HTTPClient

	mu       sync.Mutex
	sessions map[string]*session    // spawn_id -> relay cancel
	inboxes  map[string]chan []byte // spawn_id -> client->agent byte channel
	active   uint32

	sendMu sync.Mutex
	stream *connect.BidiStreamForClient[nodev1.NodeMessage, nodev1.CPMessage]
}

func Run(ctx context.Context, mgr *spawnlet.Manager, httpc connect.HTTPClient, cfg Config) error {
	a := &attacher{
		cfg: cfg, mgr: mgr, httpc: httpc,
		sessions: map[string]*session{},
		inboxes:  map[string]chan []byte{},
	}
	client := nodev1connect.NewNodeServiceClient(httpc, cfg.CPURL, connect.WithGRPC())
	a.stream = client.Attach(ctx)

	if err := a.send(&nodev1.NodeMessage{Msg: &nodev1.NodeMessage_Register{Register: &nodev1.Register{
		NodeId: cfg.NodeID, MaxSpawns: cfg.MaxSpawns, AgentImages: []string{cfg.AgentImage}, NodeClass: cfg.NodeClass, NodeOwner: cfg.NodeOwner,
	}}}); err != nil {
		return err
	}
	go a.heartbeatLoop(ctx)

	for {
		msg, err := a.stream.Receive()
		if err != nil {
			return err
		}
		a.handle(ctx, msg)
	}
}

func (a *attacher) send(m *nodev1.NodeMessage) error {
	a.sendMu.Lock()
	defer a.sendMu.Unlock()
	return a.stream.Send(m)
}

func (a *attacher) heartbeatLoop(ctx context.Context) {
	t := time.NewTicker(5 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			a.mu.Lock()
			active := a.active
			a.mu.Unlock()
			free := a.cfg.MaxSpawns - active
			_ = a.send(&nodev1.NodeMessage{Msg: &nodev1.NodeMessage_Heartbeat{Heartbeat: &nodev1.Heartbeat{
				ActiveSpawns: active, FreeSlots: free,
			}}})
		}
	}
}

func (a *attacher) status(spawnID string, ph nodev1.SpawnPhase, detail string) {
	_ = a.send(&nodev1.NodeMessage{Msg: &nodev1.NodeMessage_Status{Status: &nodev1.SpawnStatus{SpawnId: spawnID, Phase: ph, Detail: detail}}})
}

func (a *attacher) handle(ctx context.Context, msg *nodev1.CPMessage) {
	switch m := msg.Msg.(type) {
	case *nodev1.CPMessage_Start:
		go a.startSpawn(ctx, m.Start)
	case *nodev1.CPMessage_Stop:
		a.stopSpawn(ctx, m.Stop.SpawnId)
	case *nodev1.CPMessage_Open:
		a.openSession(ctx, m.Open.SpawnId)
	case *nodev1.CPMessage_Close:
		a.closeSession(m.Close.SpawnId)
	case *nodev1.CPMessage_Frame:
		a.feed(m.Frame.SpawnId, m.Frame.Data)
	default:
		// TODO(sp-gd9): handle *nodev1.CPMessage_Suspend (persist mounts + tear down, then emit
		// NodeMessage_SuspendComplete with per-mount markers). Inert until the suspend path lands.
	}
}

func (a *attacher) startSpawn(ctx context.Context, st *nodev1.StartSpawn) {
	a.status(st.SpawnId, nodev1.SpawnPhase_STARTING, "")
	if _, err := a.mgr.Create(ctx, st.SpawnId, st.AppRef, st.Model); err != nil {
		log.Printf("startSpawn %s: %v", st.SpawnId, err)
		a.status(st.SpawnId, nodev1.SpawnPhase_ERROR, err.Error())
		return
	}
	a.mu.Lock()
	a.active++
	a.mu.Unlock()
	a.status(st.SpawnId, nodev1.SpawnPhase_ACTIVE, "")
}

func (a *attacher) stopSpawn(ctx context.Context, spawnID string) {
	a.closeSession(spawnID)
	_ = a.mgr.Stop(ctx, spawnID)
	a.mu.Lock()
	if a.active > 0 {
		a.active--
	}
	a.mu.Unlock()
	a.status(spawnID, nodev1.SpawnPhase_STOPPED, "")
}

// openSession attaches the existing transparent Relay to the pod stdio, with a
// per-spawn inbound channel fed by CPMessage.Frame and outbound bytes wrapped
// as NodeMessage.Frame back to the CP.
func (a *attacher) openSession(ctx context.Context, spawnID string) {
	sp, ok := a.mgr.Store().Get(spawnID)
	if !ok {
		return
	}
	att, err := runtime.AttachACP(ctx, sp.NetnsPath)
	if err != nil {
		return
	}
	rctx, cancel := context.WithCancel(ctx)
	inbox := make(chan []byte, 64)
	a.mu.Lock()
	a.sessions[spawnID] = &session{cancel: cancel}
	a.inboxes[spawnID] = inbox
	a.mu.Unlock()

	ep := spawnlet.StreamEndpoint{
		Recv: func() ([]byte, error) {
			select {
			case b := <-inbox:
				return b, nil
			case <-rctx.Done():
				return nil, rctx.Err()
			}
		},
		Send: func(b []byte) error {
			return a.send(&nodev1.NodeMessage{Msg: &nodev1.NodeMessage_Frame{Frame: &nodev1.Frame{SpawnId: spawnID, Data: append([]byte(nil), b...)}}})
		},
	}
	go func() {
		defer att.Close()
		spawnlet.Relay(rctx, ep, spawnlet.AgentIO{Stdin: att.Stdin, Stdout: att.Stdout})
	}()
}

func (a *attacher) feed(spawnID string, data []byte) {
	a.mu.Lock()
	inbox, ok := a.inboxes[spawnID]
	a.mu.Unlock()
	if ok {
		select {
		case inbox <- append([]byte(nil), data...):
		default:
		}
	}
}

func (a *attacher) closeSession(spawnID string) {
	a.mu.Lock()
	if s, ok := a.sessions[spawnID]; ok {
		s.cancel()
		delete(a.sessions, spawnID)
	}
	delete(a.inboxes, spawnID)
	a.mu.Unlock()
}
