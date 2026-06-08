package node

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	nodev1 "spawnery/gen/node/v1"
	"spawnery/internal/acp"
	"spawnery/internal/runtime"
	"spawnery/internal/spawnlet"
	"spawnery/internal/spawnlet/firewall"
)

// --- test doubles ----------------------------------------------------------

// fakeCPStream captures the NodeMessages the attacher sends (so a test can assert status phases) and
// serves EOF on Receive (the attacher's receive loop is not exercised here).
type fakeCPStream struct {
	mu   sync.Mutex
	sent []*nodev1.NodeMessage
}

func (f *fakeCPStream) Send(m *nodev1.NodeMessage) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sent = append(f.sent, m)
	return nil
}

func (f *fakeCPStream) Receive() (*nodev1.CPMessage, error) { return nil, io.EOF }

// phasesFor returns the SpawnPhase sequence reported for spawnID, in send order.
func (f *fakeCPStream) phasesFor(spawnID string) []nodev1.SpawnPhase {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []nodev1.SpawnPhase
	for _, m := range f.sent {
		if s := m.GetStatus(); s != nil && s.SpawnId == spawnID {
			out = append(out, s.Phase)
		}
	}
	return out
}

// lastRoster returns the most recent SessionRoster the attacher sent to the CP (nil if none).
func (f *fakeCPStream) lastRoster() *nodev1.SessionRoster {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := len(f.sent) - 1; i >= 0; i-- {
		if r := f.sent[i].GetRoster(); r != nil {
			return r
		}
	}
	return nil
}

func hasPhase(phases []nodev1.SpawnPhase, want nodev1.SpawnPhase) bool {
	for _, p := range phases {
		if p == want {
			return true
		}
	}
	return false
}

func lastPhase(phases []nodev1.SpawnPhase) nodev1.SpawnPhase {
	if len(phases) == 0 {
		return nodev1.SpawnPhase_SPAWN_PHASE_UNSPECIFIED
	}
	return phases[len(phases)-1]
}

// noopApplier is a firewall.Applier that records nothing — egress isn't enforced in these tests
// (NodeClass unset + EgressEnforce false), so Create never calls it, but Manager needs a non-nil one.
type noopApplier struct{}

func (noopApplier) Apply(context.Context, []firewall.Rule) error  { return nil }
func (noopApplier) Remove(context.Context, []firewall.Rule) error { return nil }

// scriptedPodBackend is a PodBackend whose Attach wires the agent's stdio to a scripted goose. The
// script runs in a goroutine; when it returns, the agent's stdout is closed (modelling the agent
// process exiting), which the pump observes as EOF.
type scriptedPodBackend struct {
	script  func(io.Reader, io.Writer)
	mu      sync.Mutex
	stopped bool
}

func (f *scriptedPodBackend) Ping(context.Context) error      { return nil }
func (f *scriptedPodBackend) Preflight(context.Context) error { return nil }
func (f *scriptedPodBackend) StartPod(context.Context, runtime.PodSpec) (*runtime.PodHandle, error) {
	return &runtime.PodHandle{PodIP: "10.0.0.5", NetnsPath: "/proc/7/ns/net", SidecarID: "sc", SandboxID: "sb"}, nil
}
func (f *scriptedPodBackend) StartAgent(_ context.Context, h *runtime.PodHandle, _ runtime.AgentSpec) error {
	h.AgentID = "ag"
	return nil
}
func (f *scriptedPodBackend) Stop(context.Context, *runtime.PodHandle) error {
	f.mu.Lock()
	f.stopped = true
	f.mu.Unlock()
	return nil
}
func (f *scriptedPodBackend) Attach(context.Context, *runtime.PodHandle) (*runtime.AttachedStream, error) {
	inR, inW := io.Pipe()   // pump writes -> inW; goose reads inR
	outR, outW := io.Pipe() // goose writes outW; pump reads outR
	go func() { f.script(inR, outW); _ = outW.Close() }()
	return &runtime.AttachedStream{Stdin: inW, Stdout: outR, Close: func() error { _ = inW.Close(); _ = outW.Close(); return nil }}, nil
}
func (f *scriptedPodBackend) ListManaged(context.Context) ([]runtime.ManagedPod, error) {
	return nil, nil
}
func (f *scriptedPodBackend) wasStopped() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.stopped
}

// attachFailPodBackend creates the pod fine but fails Attach — exercising startSpawn's attach-error path.
type attachFailPodBackend struct{ scriptedPodBackend }

func (f *attachFailPodBackend) Attach(context.Context, *runtime.PodHandle) (*runtime.AttachedStream, error) {
	return nil, fmt.Errorf("attach boom")
}

// scriptGooseDieOnPrompt answers initialize + session/new, then exits after the first prompt's
// end_turn — modelling an agent that dies after going active.
func scriptGooseDieOnPrompt(in io.Reader, out io.Writer) {
	rd := acp.NewReader(in)
	for {
		m, err := rd.ReadMessage()
		if err != nil {
			return
		}
		switch m.Method {
		case "initialize":
			acp.WriteMessage(out, acp.Message{ID: m.ID, Result: []byte(`{"protocolVersion":1}`)})
		case "session/new":
			acp.WriteMessage(out, acp.Message{ID: m.ID, Result: []byte(`{"sessionId":"s1"}`)})
		case "session/prompt":
			acp.WriteMessage(out, acp.Message{ID: m.ID, Result: []byte(`{"stopReason":"end_turn"}`)})
			return // die after one turn
		}
	}
}

func writeNodeApp(t *testing.T) string {
	t.Helper()
	app := t.TempDir()
	if err := os.WriteFile(filepath.Join(app, "spawneryapp.yml"), []byte(`
id: spawnery/secret
storage:
  mounts:
    - name: main
      path: data
      seed: seed
`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(app, "seed"), 0o755); err != nil {
		t.Fatal(err)
	}
	return app
}

func newGooseManager(t *testing.T, be runtime.PodBackend) *spawnlet.Manager {
	t.Helper()
	return spawnlet.NewManagerWithBackend(be, noopApplier{}, spawnlet.ManagerConfig{
		AgentImage: "a", SidecarImage: "s", DataRoot: t.TempDir(),
	})
}

func newAttacher(mgr *spawnlet.Manager, fs cpStream) *attacher {
	return &attacher{cfg: Config{MaxSpawns: 2}, mgr: mgr, stream: fs, pumps: map[sessionKey]*Pump{}, tmuxRelays: map[sessionKey]*tmuxRelay{}, sessions: map[string]*sessionRegistry{}}
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		if cond() {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// --- tests -----------------------------------------------------------------

// startSpawn must report STARTING then ERROR when the spawn cannot be created (here: a bogus app ref
// that fails manifest parsing), and must NOT register a pump or consume a capacity slot.
func TestStartSpawnCreateFailureReportsErrorNoLeak(t *testing.T) {
	mgr := spawnlet.NewManager(runtime.NewFake(), spawnlet.ManagerConfig{AgentImage: "a", SidecarImage: "s", DataRoot: t.TempDir()})
	fs := &fakeCPStream{}
	a := newAttacher(mgr, fs)

	a.startSpawn(context.Background(), &nodev1.StartSpawn{SpawnId: "sp1", AppRef: "/no/such/app", Model: "m"})

	phases := fs.phasesFor("sp1")
	if len(phases) < 2 || phases[0] != nodev1.SpawnPhase_STARTING || lastPhase(phases) != nodev1.SpawnPhase_ERROR {
		t.Fatalf("phases = %v, want STARTING...ERROR", phases)
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(a.pumps) != 0 {
		t.Fatalf("pump map should be empty after a create failure, got %d", len(a.pumps))
	}
	if a.active != 0 {
		t.Fatalf("active count should be 0 after a create failure, got %d", a.active)
	}
}

// The happy path: Create + Attach + pump handshake succeed, so startSpawn reports ACTIVE, registers
// the pump, and consumes one capacity slot.
func TestStartSpawnSuccessReportsActive(t *testing.T) {
	be := &scriptedPodBackend{script: scriptGoose}
	a := newAttacher(newGooseManager(t, be), &fakeCPStream{})
	a.startSpawn(context.Background(), &nodev1.StartSpawn{SpawnId: "sp1", AppRef: writeNodeApp(t), Model: "m"})
	defer a.stopSpawn(context.Background(), "sp1")

	if got := lastPhase(a.stream.(*fakeCPStream).phasesFor("sp1")); got != nodev1.SpawnPhase_ACTIVE {
		t.Fatalf("final phase = %v, want ACTIVE", got)
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.pumps[zeroKey("sp1")] == nil {
		t.Fatal("pump not registered after success")
	}
	if a.active != 1 {
		t.Fatalf("active = %d, want 1", a.active)
	}
}

// After a spawn goes active, the agent dying must self-clean: report ERROR, drop the pump, release
// the capacity slot, and reclaim the container (mgr.Stop). Guards the sp-bjd capacity-leak fix.
func TestStartSpawnAgentDeathSelfCleans(t *testing.T) {
	be := &scriptedPodBackend{script: scriptGooseDieOnPrompt}
	fs := &fakeCPStream{}
	a := newAttacher(newGooseManager(t, be), fs)
	a.startSpawn(context.Background(), &nodev1.StartSpawn{SpawnId: "sp1", AppRef: writeNodeApp(t), Model: "m"})

	if got := lastPhase(fs.phasesFor("sp1")); got != nodev1.SpawnPhase_ACTIVE {
		t.Fatalf("final phase before death = %v, want ACTIVE", got)
	}
	// A prompt makes the scripted goose answer one turn then exit -> exitFn must ERROR + reclaim.
	a.fromClient("sp1", SessionZeroID, "ghost", encodeFrame(Frame{Kind: "prompt", Text: "go"}))

	waitFor(t, "exitFn reclaim", func() bool {
		a.mu.Lock()
		defer a.mu.Unlock()
		return len(a.pumps) == 0 && a.active == 0
	})
	if !hasPhase(fs.phasesFor("sp1"), nodev1.SpawnPhase_ERROR) {
		t.Fatalf("phases = %v, want an ERROR after agent death", fs.phasesFor("sp1"))
	}
	if !be.wasStopped() {
		t.Fatal("exitFn must mgr.Stop the dead spawn to reclaim the container")
	}
}

// A failure in mgr.Attach (after Create succeeded) must report ERROR, leave no pump/capacity, and
// tear down the just-created pod (mgr.Stop).
func TestStartSpawnAttachFailureReportsErrorAndStops(t *testing.T) {
	be := &attachFailPodBackend{}
	fs := &fakeCPStream{}
	a := newAttacher(newGooseManager(t, be), fs)
	a.startSpawn(context.Background(), &nodev1.StartSpawn{SpawnId: "sp1", AppRef: writeNodeApp(t), Model: "m"})

	if got := lastPhase(fs.phasesFor("sp1")); got != nodev1.SpawnPhase_ERROR {
		t.Fatalf("final phase = %v, want ERROR on attach failure", got)
	}
	a.mu.Lock()
	n, act := len(a.pumps), a.active
	a.mu.Unlock()
	if n != 0 || act != 0 {
		t.Fatalf("pumps=%d active=%d, want 0/0 after attach failure", n, act)
	}
	if !be.wasStopped() {
		t.Fatal("attach failure must mgr.Stop the partially-created spawn")
	}
}

// handle() routes Open/Frame/Close to the spawn's pump: Open attaches a client, a prompt Frame is
// forwarded to the agent (logged frames appear), and Close detaches the client.
func TestHandleRoutesOpenFrameClose(t *testing.T) {
	be := &scriptedPodBackend{script: scriptGoose}
	a := newAttacher(newGooseManager(t, be), &fakeCPStream{})
	ctx := context.Background()
	a.startSpawn(ctx, &nodev1.StartSpawn{SpawnId: "sp1", AppRef: writeNodeApp(t), Model: "m"})
	defer a.stopSpawn(ctx, "sp1")
	p := a.pumps[zeroKey("sp1")]

	a.handle(ctx, &nodev1.CPMessage{Msg: &nodev1.CPMessage_Open{Open: &nodev1.SessionOpen{SpawnId: "sp1", ClientId: "c1", Cursor: 0}}})
	waitFor(t, "client attach", func() bool {
		p.mu.Lock()
		defer p.mu.Unlock()
		return len(p.clients) == 1
	})

	a.handle(ctx, &nodev1.CPMessage{Msg: &nodev1.CPMessage_Frame{Frame: &nodev1.Frame{SpawnId: "sp1", ClientId: "c1", Data: encodeFrame(Frame{Kind: "prompt", Text: "hi"})}}})
	waitFor(t, "prompt logged", func() bool {
		p.mu.Lock()
		defer p.mu.Unlock()
		return len(p.log) > 0 // the prompt produced user/agent/turn frames in the pump log
	})

	a.handle(ctx, &nodev1.CPMessage{Msg: &nodev1.CPMessage_Close{Close: &nodev1.SessionClose{SpawnId: "sp1", ClientId: "c1"}}})
	waitFor(t, "client detach", func() bool {
		p.mu.Lock()
		defer p.mu.Unlock()
		return len(p.clients) == 0
	})
}

// A started spawn auto-registers a pinned session #0 in its registry and emits a SessionRoster to the
// CP carrying that one session (sp-npxq.1).
func TestStartSpawnRegistersSessionZeroAndEmitsRoster(t *testing.T) {
	be := &scriptedPodBackend{script: scriptGoose}
	fs := &fakeCPStream{}
	a := newAttacher(newGooseManager(t, be), fs)
	ctx := context.Background()
	a.startSpawn(ctx, &nodev1.StartSpawn{SpawnId: "s1", AppRef: writeNodeApp(t), Model: "m", RunnableId: "goose-acp"})
	defer a.stopSpawn(ctx, "s1")

	reg := a.sessions["s1"]
	if reg == nil {
		t.Fatalf("no session registry for s1")
	}
	z, ok := reg.get(SessionZeroID)
	if !ok || !z.pinned || z.transport != nodev1.SessionTransport_SESSION_TRANSPORT_ACP {
		t.Fatalf("session #0 not registered/pinned/acp: %+v ok=%v", z, ok)
	}
	got := fs.lastRoster()
	if got == nil || len(got.Sessions) != 1 || got.Sessions[0].SessionId != SessionZeroID {
		t.Fatalf("roster not emitted with session #0: %+v", got)
	}
}

// CreateSession registers a new session and launches it (via the fake exec boundary, so it reaches
// ACTIVE); CloseSession reaps a non-pinned session but is a no-op for the pinned session #0 (sp-npxq.1
// roster behavior, with the sp-npxq.3 real launch/reap wired through a fakeSessionExec).
func TestCreateAndCloseSessionUpdateRoster(t *testing.T) {
	a := &attacher{
		cfg:        Config{NodeID: "n1", MaxSpawns: 2},
		sx:         &fakeSessionExec{},
		pumps:      map[sessionKey]*Pump{},
		tmuxRelays: map[sessionKey]*tmuxRelay{},
		sessions:   map[string]*sessionRegistry{"s1": newSessionRegistry("s1")},
	}
	a.sessions["s1"].register(&sessionEntry{id: SessionZeroID, state: nodev1.SessionState_SESSION_STATE_ACTIVE, pinned: true})
	fs := &fakeCPStream{}
	a.stream = fs
	ctx := context.Background()

	a.handle(ctx, &nodev1.CPMessage{Msg: &nodev1.CPMessage_CreateSession{CreateSession: &nodev1.CreateSession{
		SpawnId: "s1", Transport: nodev1.SessionTransport_SESSION_TRANSPORT_MOSH, Runnable: "shell",
	}}})

	// the new (non-zero) session launches to ACTIVE via the fake (sp-npxq.3); roster has 2 sessions.
	var newID string
	waitFor(t, "new session ACTIVE", func() bool {
		r := fs.lastRoster()
		if r == nil || len(r.Sessions) != 2 {
			return false
		}
		for _, s := range r.Sessions {
			if s.SessionId != SessionZeroID {
				if s.State == nodev1.SessionState_SESSION_STATE_ACTIVE && s.Runnable == "shell" {
					newID = s.SessionId
					return true
				}
			}
		}
		return false
	})

	// closing the pinned session #0 is rejected: roster unchanged.
	a.handle(ctx, &nodev1.CPMessage{Msg: &nodev1.CPMessage_CloseSession{CloseSession: &nodev1.CloseSession{SpawnId: "s1", SessionId: SessionZeroID}}})
	if len(fs.lastRoster().Sessions) != 2 {
		t.Fatalf("closing pinned #0 must be a no-op")
	}

	// closing the new session reaps it: roster back to 1.
	a.handle(ctx, &nodev1.CPMessage{Msg: &nodev1.CPMessage_CloseSession{CloseSession: &nodev1.CloseSession{SpawnId: "s1", SessionId: newID}}})
	if len(fs.lastRoster().Sessions) != 1 {
		t.Fatalf("after CloseSession want roster of 1, got %+v", fs.lastRoster())
	}
}
