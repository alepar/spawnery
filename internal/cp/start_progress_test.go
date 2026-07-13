package cp

import (
	"context"
	"testing"
	"time"

	connect "connectrpc.com/connect"

	cpv1 "spawnery/gen/cp/v1"
	nodev1 "spawnery/gen/node/v1"
	"spawnery/internal/cp/auth"
	"spawnery/internal/cp/registry"
	"spawnery/internal/cp/router"
	"spawnery/internal/cp/scheduler"
	"spawnery/internal/cp/store"
	"spawnery/internal/cp/telemetry"
)

// newTestServerWithStallWindow mirrors newTestServerSink but lets the caller pin the scheduler's
// start stall window, so tests can drive it well below the 30s default without waiting real minutes.
func newTestServerWithStallWindow(t *testing.T, stallWindow time.Duration) (*Server, *registry.Registry) {
	t.Helper()
	reg := registry.New()
	rt := router.New()
	sc := scheduler.New(reg, rt, stallWindow)
	st := store.NewTestStore(t)
	if err := Seed(context.Background(), st, map[string]string{"dev-token": "alice"},
		[]AppSeed{{ID: "secret-app", Ref: "examples/secret-app", Version: "1.0.0", Mounts: []string{"main"}}}); err != nil {
		t.Fatal(err)
	}
	s := NewServer(reg, rt, sc, st, telemetry.NopSink{})
	return s, reg
}

// TestFreshCreateResumeProgressResetsSchedulerStallWindow is the hermetic stand-in for the bead's
// acceptance criterion ("a healthy 14-member bundle install reaches ACTIVE without tripping the CP
// start deadline"): on the FRESH-CREATE path (CreateSpawn -> scheduler.Provision), the node's
// NodeMessage_ResumeProgress heartbeats (attach.go's startSpawn already emits these) must reach the
// scheduler's stall detector, not just lifecycle's resume-only waiter. Before sp-mwco.4.8 wired
// server.go's ResumeProgress case into s.sched.Progress, this path dropped the heartbeats and
// Provision died at the fixed stall window regardless of legitimate progress.
func TestFreshCreateResumeProgressResetsSchedulerStallWindow(t *testing.T) {
	stallWindow := 60 * time.Millisecond
	s, reg := newTestServerWithStallWindow(t, stallWindow)

	sender := &capSender{}
	in := make(chan *nodev1.NodeMessage, 8)
	recv := func() (*nodev1.NodeMessage, error) {
		m, ok := <-in
		if !ok {
			return nil, context.Canceled
		}
		return m, nil
	}
	runCtx, runCancel := context.WithCancel(context.Background())
	defer runCancel()
	go s.runNode(runCtx, sender, recv)

	in <- &nodev1.NodeMessage{Msg: &nodev1.NodeMessage_Register{Register: &nodev1.Register{NodeId: "n1", MaxSpawns: 1}}}
	deadline := time.Now().Add(2 * time.Second)
	for {
		if _, ok := reg.Get("n1"); ok {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("node never registered")
		}
		time.Sleep(time.Millisecond)
	}

	ctx := auth.WithOwner(context.Background(), "alice")
	resp, err := s.CreateSpawn(ctx, connect.NewRequest(&cpv1.CreateSpawnRequest{AppId: "secret-app", Model: "m"}))
	if err != nil {
		t.Fatalf("CreateSpawn: %v", err)
	}
	spawnID := resp.Msg.SpawnId

	var gen uint64
	deadline = time.Now().Add(2 * time.Second)
	for {
		if st := sender.firstStart(); st != nil {
			gen = st.GetGeneration()
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("no StartSpawn was sent")
		}
		time.Sleep(time.Millisecond)
	}

	// Total ResumeProgress span is several times the stall window — the miniature of the bead's
	// 60s-CP-deadline-vs-2m-node-budget contradiction. Without the fix this stalls at the first
	// stallWindow and the spawn errors well before this loop finishes.
	for i := 0; i < 8; i++ {
		time.Sleep(stallWindow / 3)
		in <- &nodev1.NodeMessage{Msg: &nodev1.NodeMessage_ResumeProgress{ResumeProgress: &nodev1.ResumeProgress{
			SpawnId: spawnID, Generation: gen, Phase: "installing_skills", Detail: "heartbeat",
		}}}
	}
	in <- &nodev1.NodeMessage{Msg: &nodev1.NodeMessage_Status{Status: &nodev1.SpawnStatus{
		SpawnId: spawnID, Phase: nodev1.SpawnPhase_ACTIVE,
	}}}

	waitActive(t, s, spawnID)
	close(in)
}
