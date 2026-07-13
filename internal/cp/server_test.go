package cp

import (
	"context"
	"crypto/tls"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	connect "connectrpc.com/connect"
	"golang.org/x/net/http2"
	"golang.org/x/net/http2/h2c"
	"google.golang.org/protobuf/proto"
	authv1 "spawnery/gen/auth/v1"
	cpv1 "spawnery/gen/cp/v1"
	"spawnery/gen/cp/v1/cpv1connect"
	nodev1 "spawnery/gen/node/v1"
	"spawnery/internal/cp/auth"
	"spawnery/internal/cp/registry"
	"spawnery/internal/cp/router"
	"spawnery/internal/cp/scheduler"
	"spawnery/internal/cp/store"
	"spawnery/internal/cp/telemetry"
)

type capSender struct {
	mu   sync.Mutex
	sent []*nodev1.CPMessage
}

func (c *capSender) Send(m *nodev1.CPMessage) error {
	c.mu.Lock()
	c.sent = append(c.sent, m)
	c.mu.Unlock()
	return nil
}

func (c *capSender) firstStart() *nodev1.StartSpawn {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, m := range c.sent {
		if st := m.GetStart(); st != nil {
			return st
		}
	}
	return nil
}

func (c *capSender) lastCPMessage() *nodev1.CPMessage {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.sent) == 0 {
		return nil
	}
	return c.sent[len(c.sent)-1]
}

// starts returns every StartSpawn this sender has been asked to deliver, in send order.
func (c *capSender) starts() []*nodev1.StartSpawn {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []*nodev1.StartSpawn
	for _, m := range c.sent {
		if st := m.GetStart(); st != nil {
			out = append(out, st)
		}
	}
	return out
}

func (c *capSender) sessionOpens() []*nodev1.SessionOpen {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []*nodev1.SessionOpen
	for _, m := range c.sent {
		if open := m.GetOpen(); open != nil {
			out = append(out, open)
		}
	}
	return out
}

func TestSessionRequiresAndPreservesNodeAuthorization(t *testing.T) {
	tests := []struct {
		name string
		auth *authv1.AuthEnvelope
	}{
		{name: "missing envelope"},
		{name: "missing node token", auth: &authv1.AuthEnvelope{Intent: &authv1.SignedIntent{Domain: "opaque"}}},
		{name: "missing intent", auth: &authv1.AuthEnvelope{AccessToken: "as-issued-node-token"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s, _, rt := newTestServer(t)
			seedSpawn(t, context.Background(), s, "alice")
			sender := &capSender{}
			rt.Bind("sp-ws-alice", "n1", sender)
			_, handler := cpv1connect.NewSpawnServiceHandler(s)
			wrapped := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				r = r.WithContext(auth.WithOwner(r.Context(), "alice"))
				handler.ServeHTTP(w, r)
			})
			ts := httptest.NewServer(h2c.NewHandler(wrapped, &http2.Server{}))
			defer ts.Close()
			client := cpv1connect.NewSpawnServiceClient(sessionH2CClient(), ts.URL, connect.WithGRPC())
			streamCtx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
			defer cancel()
			stream := client.Session(streamCtx)
			if err := stream.Send(&cpv1.Frame{SpawnId: "sp-ws-alice", SessionAuth: tt.auth}); err != nil {
				t.Fatal(err)
			}
			_ = stream.CloseRequest()
			_, err := stream.Receive()
			if connect.CodeOf(err) != connect.CodeInvalidArgument {
				t.Fatalf("Session error = %v (code %v), want InvalidArgument", err, connect.CodeOf(err))
			}
			if len(sender.sessionOpens()) != 0 {
				t.Fatal("SessionOpen emitted for incomplete authorization")
			}
		})
	}

	s, _, rt := newTestServer(t)
	seedSpawn(t, context.Background(), s, "alice")
	sender := &capSender{}
	rt.Bind("sp-ws-alice", "n1", sender)
	_, handler := cpv1connect.NewSpawnServiceHandler(s)
	wrapped := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r = r.WithContext(auth.WithOwner(r.Context(), "alice"))
		handler.ServeHTTP(w, r)
	})
	ts := httptest.NewServer(h2c.NewHandler(wrapped, &http2.Server{}))
	defer ts.Close()
	client := cpv1connect.NewSpawnServiceClient(sessionH2CClient(), ts.URL, connect.WithGRPC())
	si := &authv1.SignedIntent{Domain: "opaque", Body: []byte("exact-body")}
	si.ProtoReflect().SetUnknown([]byte{0xa0, 0x06, 0x2a})
	want, _ := proto.MarshalOptions{Deterministic: true}.Marshal(si)
	streamCtx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	stream := client.Session(streamCtx)
	if err := stream.Send(&cpv1.Frame{SpawnId: "sp-ws-alice", SessionAuth: &authv1.AuthEnvelope{AccessToken: "exact-node-token", Intent: si}}); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if opens := sender.sessionOpens(); len(opens) > 0 {
			got, _ := proto.MarshalOptions{Deterministic: true}.Marshal(opens[0].GetAuth().GetIntent())
			if opens[0].GetAuth().GetAccessToken() != "exact-node-token" || string(got) != string(want) {
				t.Fatalf("SessionOpen authorization changed: token=%q intent=%x", opens[0].GetAuth().GetAccessToken(), got)
			}
			_ = stream.CloseRequest()
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("SessionOpen not emitted")
}

func TestSessionRejectsMissingOrForeignCallerBeforeNodeOpen(t *testing.T) {
	tests := []struct {
		name     string
		owner    string
		wantCode connect.Code
	}{
		{name: "missing caller", wantCode: connect.CodeUnauthenticated},
		{name: "foreign owner", owner: "mallory", wantCode: connect.CodePermissionDenied},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s, _, rt := newTestServer(t)
			seedSpawn(t, context.Background(), s, "alice")
			sender := &capSender{}
			rt.Bind("sp-ws-alice", "n1", sender)
			_, handler := cpv1connect.NewSpawnServiceHandler(s)
			wrapped := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if tt.owner != "" {
					r = r.WithContext(auth.WithOwner(r.Context(), tt.owner))
				}
				handler.ServeHTTP(w, r)
			})
			ts := httptest.NewServer(h2c.NewHandler(wrapped, &http2.Server{}))
			defer ts.Close()
			client := cpv1connect.NewSpawnServiceClient(sessionH2CClient(), ts.URL, connect.WithGRPC())
			ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
			defer cancel()
			stream := client.Session(ctx)
			if err := stream.Send(&cpv1.Frame{
				SpawnId: "sp-ws-alice", SessionId: "exec-rejected",
				ExecRequest: &authv1.ExecRequest{Argv: []string{"true"}},
				SessionAuth: &authv1.AuthEnvelope{AccessToken: "node-token", Intent: &authv1.SignedIntent{}},
			}); err != nil {
				t.Fatal(err)
			}
			_ = stream.CloseRequest()
			_, err := stream.Receive()
			if connect.CodeOf(err) != tt.wantCode {
				t.Fatalf("Session error = %v (code %v), want %v", err, connect.CodeOf(err), tt.wantCode)
			}
			if opens := sender.sessionOpens(); len(opens) != 0 {
				t.Fatalf("node SessionOpen emitted for rejected caller: %+v", opens)
			}
		})
	}
}

func TestSessionRelaysExplicitExecBind(t *testing.T) {
	s, _, rt := newTestServer(t)
	seedSpawn(t, context.Background(), s, "alice")
	sender := &capSender{}
	rt.Bind("sp-ws-alice", "n1", sender)
	_, handler := cpv1connect.NewSpawnServiceHandler(s)
	wrapper := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r = r.WithContext(auth.WithOwner(r.Context(), "alice"))
		handler.ServeHTTP(w, r)
	})
	ts := httptest.NewServer(h2c.NewHandler(wrapper, &http2.Server{}))
	defer ts.Close()
	client := cpv1connect.NewSpawnServiceClient(sessionH2CClient(), ts.URL, connect.WithGRPC())
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	stream := client.Session(ctx)
	req := &authv1.ExecRequest{Argv: []string{"sh", "-lc", "exit 7"}}
	if err := stream.Send(&cpv1.Frame{
		SpawnId: "sp-ws-alice", SessionId: "exec-123", ExecRequest: req,
		SessionAuth: &authv1.AuthEnvelope{AccessToken: "node-token", Intent: &authv1.SignedIntent{}},
	}); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if opens := sender.sessionOpens(); len(opens) > 0 {
			open := opens[0]
			if open.GetSessionId() != "exec-123" || !proto.Equal(open.GetExecRequest(), req) {
				t.Fatalf("exec bind changed in CP relay: %+v", open)
			}
			if err := stream.Send(&cpv1.Frame{SessionId: "exec-substitution", Data: []byte("ignored")}); err != nil {
				t.Fatal(err)
			}
			_ = stream.CloseRequest()
			_, err := stream.Receive()
			if connect.CodeOf(err) != connect.CodeInvalidArgument {
				t.Fatalf("post-bind selector error = %v (code %v), want InvalidArgument", err, connect.CodeOf(err))
			}
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("exec SessionOpen not emitted")
}

func TestSessionRejectsExecWithoutExplicitSessionID(t *testing.T) {
	s, _, rt := newTestServer(t)
	seedSpawn(t, context.Background(), s, "alice")
	sender := &capSender{}
	rt.Bind("sp-ws-alice", "n1", sender)
	_, handler := cpv1connect.NewSpawnServiceHandler(s)
	wrapper := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r = r.WithContext(auth.WithOwner(r.Context(), "alice"))
		handler.ServeHTTP(w, r)
	})
	ts := httptest.NewServer(h2c.NewHandler(wrapper, &http2.Server{}))
	defer ts.Close()
	client := cpv1connect.NewSpawnServiceClient(sessionH2CClient(), ts.URL, connect.WithGRPC())
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	stream := client.Session(ctx)
	if err := stream.Send(&cpv1.Frame{
		SpawnId: "sp-ws-alice", ExecRequest: &authv1.ExecRequest{Argv: []string{"true"}},
		SessionAuth: &authv1.AuthEnvelope{AccessToken: "node-token", Intent: &authv1.SignedIntent{}},
	}); err != nil {
		t.Fatal(err)
	}
	_ = stream.CloseRequest()
	_, err := stream.Receive()
	if connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("Session error = %v (code %v), want InvalidArgument", err, connect.CodeOf(err))
	}
	if len(sender.sessionOpens()) != 0 {
		t.Fatal("SessionOpen emitted without explicit exec session id")
	}
}

func sessionH2CClient() *http.Client {
	return &http.Client{Transport: &http2.Transport{
		AllowHTTP: true,
		DialTLSContext: func(ctx context.Context, network, addr string, _ *tls.Config) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, network, addr)
		},
	}}
}

// stops returns every StopSpawn this sender has been asked to deliver (reconcile orphan arm).
func (c *capSender) stops() []*nodev1.StopSpawn {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []*nodev1.StopSpawn
	for _, m := range c.sent {
		if st := m.GetStop(); st != nil {
			out = append(out, st)
		}
	}
	return out
}

func newTestServer(t *testing.T) (*Server, *registry.Registry, *router.Router) {
	return newTestServerSink(t, telemetry.NopSink{})
}

func newTestServerSink(t *testing.T, sink telemetry.Sink) (*Server, *registry.Registry, *router.Router) {
	reg := registry.New()
	rt := router.New()
	sc := scheduler.New(reg, rt, time.Second)
	st := store.NewTestStore(t)
	if err := Seed(context.Background(), st, map[string]string{"dev-token": "alice"},
		[]AppSeed{{ID: "secret-app", Ref: "examples/secret-app", Version: "1.0.0", Mounts: []string{"main"}}}); err != nil {
		t.Fatal(err)
	}
	s := NewServer(reg, rt, sc, st, sink)
	return s, reg, rt
}

func TestRunNodeRegistersAndRoutesFrames(t *testing.T) {
	s, reg, rt := newTestServer(t)
	in := make(chan *nodev1.NodeMessage, 8)
	recv := func() (*nodev1.NodeMessage, error) {
		m, ok := <-in
		if !ok {
			return nil, context.Canceled
		}
		return m, nil
	}
	sender := &capSender{}
	go s.runNode(context.Background(), sender, recv)

	in <- &nodev1.NodeMessage{Msg: &nodev1.NodeMessage_Register{Register: &nodev1.Register{NodeId: "n1", MaxSpawns: 1}}}
	cl := &capClient{}
	deadline := time.Now().Add(time.Second)
	for {
		if _, ok := reg.Get("n1"); ok {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("node never registered")
		}
		time.Sleep(time.Millisecond)
	}
	rt.Bind("sp1", "n1", sender)
	if _, _, err := rt.AttachClient("sp1", "0", "c1", "", nil, cl, 0); err != nil {
		t.Fatal(err)
	}
	in <- &nodev1.NodeMessage{Msg: &nodev1.NodeMessage_Frame{Frame: &nodev1.Frame{SpawnId: "sp1", ClientId: "c1", Data: []byte("hi")}}}

	deadline = time.Now().Add(time.Second)
	for cl.count() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("node frame never reached client")
		}
		time.Sleep(time.Millisecond)
	}
	if string(cl.first()) != "hi" {
		t.Fatalf("got %q", cl.first())
	}
	close(in)
}

type capClient struct {
	mu  sync.Mutex
	got [][]byte
}

func (c *capClient) Send(b []byte) error {
	c.mu.Lock()
	c.got = append(c.got, append([]byte(nil), b...))
	c.mu.Unlock()
	return nil
}

func (c *capClient) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.got)
}

func (c *capClient) first() []byte {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.got) == 0 {
		return nil
	}
	return c.got[0]
}

func TestCreateSpawnPersistsNodeID(t *testing.T) {
	s, reg, _ := newTestServer(t)

	sender := &capSender{}
	reg.Add(&registry.Node{ID: "n1", Sender: sender, Max: 1, Free: 1})
	go func() {
		for {
			if st := sender.firstStart(); st != nil {
				s.sched.OnStatus(st.GetSpawnId(), nodev1.SpawnPhase_ACTIVE, "")
				return
			}
			time.Sleep(time.Millisecond)
		}
	}()

	ctx := auth.WithOwner(context.Background(), "alice")
	resp, err := s.CreateSpawn(ctx, connect.NewRequest(&cpv1.CreateSpawnRequest{AppId: "secret-app", Model: "m"}))
	if err != nil {
		t.Fatal(err)
	}
	id := resp.Msg.SpawnId
	waitActive(t, s, id) // async CreateSpawn — wait for background provision to complete

	live, err := s.st.Spawns().LiveContainersByNode(ctx, "n1")
	if err != nil || len(live) != 1 || live[0].SpawnID != id {
		t.Fatalf("LiveContainersByNode(n1)=%+v err=%v want [%s]", live, err, id)
	}
	got, _ := s.st.Spawns().Get(ctx, id)
	if got.Status != store.Active {
		t.Fatalf("status=%v want active", got.Status)
	}
}

// A node-reported SessionRoster is mirrored into the CP router so ListSessions can serve it.
// (recvFromChan adapter lives in node_class_test.go.) runNode is kept alive in a goroutine: letting it
// return would run its deferred DropNode and tear down the route under test.
func TestRunNodeMirrorsRoster(t *testing.T) {
	s, _, rt := newTestServer(t)
	sender := &capSender{}
	rt.Bind("s1", "node-1", sender)
	in := make(chan *nodev1.NodeMessage, 2)
	go s.runNode(context.Background(), sender, recvFromChan(in))
	in <- &nodev1.NodeMessage{Msg: &nodev1.NodeMessage_Register{Register: &nodev1.Register{NodeId: "node-1"}}}
	in <- &nodev1.NodeMessage{Msg: &nodev1.NodeMessage_Roster{Roster: &nodev1.SessionRoster{
		SpawnId: "s1", Sessions: []*nodev1.SessionInfo{{SessionId: "0", State: nodev1.SessionState_SESSION_STATE_ACTIVE, Pinned: true}},
	}}}
	deadline := time.Now().Add(time.Second)
	for {
		if got := rt.ListSessions("s1"); len(got) == 1 && got[0].Pinned {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("roster never mirrored: %+v", rt.ListSessions("s1"))
		}
		time.Sleep(time.Millisecond)
	}
	close(in)
}

// The client-facing ListSessions RPC reads the CP's mirrored roster (owner-checked) and maps node
// session state to the client status string.
func TestListSessionsRPC(t *testing.T) {
	s, _, rt := newTestServer(t)
	ctx := auth.WithOwner(context.Background(), "alice")
	now := time.Now().Unix()
	sp := store.Spawn{
		ID: "s1", OwnerID: "alice", Name: "n", AppID: "secret-app", AppVersion: "1.0.0",
		AppRef: "examples/secret-app", Model: "m", CreatedAt: now, LastUsedAt: now,
	}
	if err := s.st.WithTx(ctx, func(tx store.Store) error { return tx.Spawns().Create(ctx, sp, nil) }); err != nil {
		t.Fatal(err)
	}
	rt.Bind("s1", "node-1", &capSender{})
	rt.UpdateRoster("s1", "node-1", []*nodev1.SessionInfo{
		{SessionId: "0", Transport: nodev1.SessionTransport_SESSION_TRANSPORT_ACP, Runnable: "goose-acp", State: nodev1.SessionState_SESSION_STATE_ACTIVE, Pinned: true},
	})
	resp, err := s.ListSessions(ctx, connect.NewRequest(&cpv1.ListSessionsRequest{SpawnId: "s1"}))
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Msg.Sessions) != 1 || resp.Msg.Sessions[0].Status != "active" || !resp.Msg.Sessions[0].Pinned {
		t.Fatalf("ListSessions RPC wrong: %+v", resp.Msg.Sessions)
	}
	// A foreign caller is denied.
	other := auth.WithOwner(context.Background(), "mallory")
	if _, err := s.ListSessions(other, connect.NewRequest(&cpv1.ListSessionsRequest{SpawnId: "s1"})); err == nil {
		t.Fatalf("ListSessions must reject a non-owner")
	}
}

// CreateSession rejects an unspecified transport or empty runnable, and CloseSession rejects an empty
// (defaulted-to-#0) or explicit #0 session id, all without round-tripping to the node.
func TestSessionRPCInputValidation(t *testing.T) {
	s, _, rt := newTestServer(t)
	ctx := auth.WithOwner(context.Background(), "alice")
	now := time.Now().Unix()
	sp := store.Spawn{
		ID: "s1", OwnerID: "alice", Name: "n", AppID: "secret-app", AppVersion: "1.0.0",
		AppRef: "examples/secret-app", Model: "m", CreatedAt: now, LastUsedAt: now,
	}
	if err := s.st.WithTx(ctx, func(tx store.Store) error { return tx.Spawns().Create(ctx, sp, nil) }); err != nil {
		t.Fatal(err)
	}
	rt.Bind("s1", "node-1", &capSender{})

	mustInvalid := func(name string, err error) {
		if connect.CodeOf(err) != connect.CodeInvalidArgument {
			t.Fatalf("%s: want InvalidArgument, got %v", name, err)
		}
	}
	_, err := s.CreateSession(ctx, connect.NewRequest(&cpv1.CreateSessionRequest{
		SpawnId: "s1", Transport: cpv1.SessionTransport_SESSION_TRANSPORT_UNSPECIFIED, Runnable: "shell"}))
	mustInvalid("CreateSession unspecified transport", err)
	_, err = s.CreateSession(ctx, connect.NewRequest(&cpv1.CreateSessionRequest{
		SpawnId: "s1", Transport: cpv1.SessionTransport_SESSION_TRANSPORT_MOSH, Runnable: ""}))
	mustInvalid("CreateSession empty runnable", err)
	_, err = s.CloseSession(ctx, connect.NewRequest(&cpv1.CloseSessionRequest{SpawnId: "s1", SessionId: ""}))
	mustInvalid("CloseSession empty (defaults to #0)", err)
	_, err = s.CloseSession(ctx, connect.NewRequest(&cpv1.CloseSessionRequest{SpawnId: "s1", SessionId: "0"}))
	mustInvalid("CloseSession #0", err)
}
