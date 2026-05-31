package cp

import (
	"context"
	"testing"
	"time"

	nodev1 "spawnery/gen/node/v1"
	"spawnery/internal/cp/apps"
	"spawnery/internal/cp/registry"
	"spawnery/internal/cp/router"
	"spawnery/internal/cp/scheduler"
	"spawnery/internal/cp/telemetry"
)

type capSender struct{ sent []*nodev1.CPMessage }

func (c *capSender) Send(m *nodev1.CPMessage) error { c.sent = append(c.sent, m); return nil }

func newTestServer() (*Server, *registry.Registry, *router.Router) {
	reg := registry.New()
	rt := router.New()
	sc := scheduler.New(reg, rt, time.Second)
	s := NewServer(reg, rt, sc, apps.New(map[string]string{"secret-app": "examples/secret-app"}), telemetry.NopSink{})
	return s, reg, rt
}

func TestRunNodeRegistersAndRoutesFrames(t *testing.T) {
	s, reg, rt := newTestServer()
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
	rt.Bind("sp1", "n1", "alice", sender)
	if _, err := rt.AttachClient("sp1", cl); err != nil {
		t.Fatal(err)
	}
	in <- &nodev1.NodeMessage{Msg: &nodev1.NodeMessage_Frame{Frame: &nodev1.Frame{SpawnId: "sp1", Data: []byte("hi")}}}

	deadline = time.Now().Add(time.Second)
	for len(cl.got) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("node frame never reached client")
		}
		time.Sleep(time.Millisecond)
	}
	if string(cl.got[0]) != "hi" {
		t.Fatalf("got %q", cl.got[0])
	}
	close(in)
}

type capClient struct{ got [][]byte }

func (c *capClient) Send(b []byte) error { c.got = append(c.got, append([]byte(nil), b...)); return nil }
