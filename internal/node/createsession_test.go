package node

import (
	"context"
	"io"
	"strconv"
	"sync"
	"testing"
	"time"

	nodev1 "spawnery/gen/node/v1"
	"spawnery/internal/runtime"
)

// fakeSessionExec records launch/reap calls and serves in-memory ACP streams (scriptGoose), so the
// createSession/closeSession launch/reap logic is testable without docker.
type fakeSessionExec struct {
	mu           sync.Mutex
	moshLaunched []string // tmux names launched (mosh)
	acpLaunched  []struct {
		name string
		port int
	}
	killed        []string // tmux names killed
	dials         int
	acpClosed     int // count of AttachedStream.Close calls on acp dials (acp pump teardown / conn release)
	launchMoshErr error
	launchACPErr  error
	dialErr       error

	// dialGate, when non-nil, parks the FIRST DialACP call (closing dialReached when it arrives) until
	// dialGate is closed — a deterministic seam to interleave a CloseSession + a new CreateSession
	// against an in-flight acp launch that is mid-handshake. Later dials are not gated.
	dialGate    chan struct{}
	dialReached chan struct{}
	gateOnce    sync.Once

	// killGate, when non-nil, parks the FIRST KillTmux call (closing killReached when it arrives) until
	// killGate is closed — a deterministic seam to hold closeSession INSIDE its teardown window (after
	// the a.mu pump/relay teardown) so a parked launch can be released to race the close. Later kills are
	// not gated.
	killGate    chan struct{}
	killReached chan struct{}
	killOnce    sync.Once

	// moshGate, when non-nil, parks the FIRST LaunchMosh call after recording the launch. This lets fork
	// barrier tests hold a session in STARTING before a tmux relay exists.
	moshGate    chan struct{}
	moshReached chan struct{}
	moshOnce    sync.Once
}

func (f *fakeSessionExec) LaunchMosh(_ context.Context, _, _, tmuxName string) error {
	f.mu.Lock()
	f.moshLaunched = append(f.moshLaunched, tmuxName)
	f.mu.Unlock()
	if f.moshGate != nil {
		first := false
		f.moshOnce.Do(func() { first = true })
		if first {
			close(f.moshReached)
			<-f.moshGate
		}
	}
	return f.launchMoshErr
}
func (f *fakeSessionExec) MoshAttachArgv(_, tmuxName string) ([]string, error) {
	return []string{"true"}, nil // a never-run argv: no client attaches in unit tests
}
func (f *fakeSessionExec) LaunchACP(_ context.Context, _, _, tmuxName string, port int) error {
	f.mu.Lock()
	f.acpLaunched = append(f.acpLaunched, struct {
		name string
		port int
	}{tmuxName, port})
	f.mu.Unlock()
	return f.launchACPErr
}
func (f *fakeSessionExec) DialACP(_ context.Context, _ string, _ int) (*runtime.AttachedStream, error) {
	if f.dialGate != nil {
		first := false
		f.gateOnce.Do(func() { first = true })
		if first {
			close(f.dialReached)
			<-f.dialGate
		}
	}
	if f.dialErr != nil {
		return nil, f.dialErr
	}
	f.mu.Lock()
	f.dials++
	f.mu.Unlock()
	inR, inW := io.Pipe()   // pump writes -> inW; goose reads inR
	outR, outW := io.Pipe() // goose writes outW; pump reads outR
	go func() { scriptGoose(inR, outW); _ = outW.Close() }()
	return &runtime.AttachedStream{Stdin: inW, Stdout: outR, Close: func() error {
		f.mu.Lock()
		f.acpClosed++
		f.mu.Unlock()
		_ = inW.Close()
		_ = outW.Close()
		return nil
	}}, nil
}
func (f *fakeSessionExec) KillTmux(_ context.Context, _, tmuxName string) error {
	if f.killGate != nil {
		first := false
		f.killOnce.Do(func() { first = true })
		if first {
			close(f.killReached)
			<-f.killGate
		}
	}
	f.mu.Lock()
	f.killed = append(f.killed, tmuxName)
	f.mu.Unlock()
	return nil
}

// newSessionAttacher builds an attacher with a fake exec boundary and a registered, ACTIVE pinned
// session #0 for spawnID — the precondition createSession requires.
func newSessionAttacher(spawnID string, sx sessionExec, fs cpStream) *attacher {
	reg := newSessionRegistry(spawnID)
	reg.register(&sessionEntry{id: SessionZeroID, state: nodev1.SessionState_SESSION_STATE_ACTIVE, pinned: true, runnable: "goose-acp"})
	return &attacher{
		cfg: Config{MaxSpawns: 2}, sx: sx, stream: fs,
		pumps:      map[sessionKey]*Pump{},
		tmuxRelays: map[sessionKey]*tmuxRelay{},
		sessions:   map[string]*sessionRegistry{spawnID: reg},
		pending:    map[sessionKey][]pendingClient{},
	}
}

// CreateSession{mosh,shell} launches a uniquely-named tmux session via the launcher, registers a
// per-session relay, and flips the session to ACTIVE in the roster.
func TestCreateSessionMoshLaunchesTmuxAndRelay(t *testing.T) {
	sx := &fakeSessionExec{}
	fs := &fakeCPStream{}
	a := newSessionAttacher("s1", sx, fs)

	a.handle(context.Background(), &nodev1.CPMessage{Msg: &nodev1.CPMessage_CreateSession{CreateSession: &nodev1.CreateSession{
		SpawnId: "s1", Transport: nodev1.SessionTransport_SESSION_TRANSPORT_MOSH, Runnable: "shell",
	}}})

	waitFor(t, "mosh session ACTIVE", func() bool {
		r := fs.lastRoster()
		if r == nil {
			return false
		}
		for _, s := range r.Sessions {
			if s.SessionId == "1" && s.State == nodev1.SessionState_SESSION_STATE_ACTIVE {
				return true
			}
		}
		return false
	})

	sx.mu.Lock()
	defer sx.mu.Unlock()
	if len(sx.moshLaunched) != 1 || sx.moshLaunched[0] != "spawn-1" {
		t.Fatalf("launcher tmux name = %v, want [spawn-1]", sx.moshLaunched)
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.tmuxRelays[sessionKey{"s1", "1"}] == nil {
		t.Fatal("mosh session must register a tmuxRelay keyed by (spawn,session)")
	}
	// additional sessions never consume a capacity slot (decision 7).
	if a.active != 0 {
		t.Fatalf("active = %d, want 0 (additional sessions are not capacity)", a.active)
	}
	// the entry endpoint is the tmux session name.
	e, _ := a.sessions["s1"].get("1")
	if e.endpoint != "spawn-1" {
		t.Fatalf("mosh endpoint = %q, want tmux name spawn-1", e.endpoint)
	}
}

// CreateSession{acp} allocates the lowest pool port, launches the tmux-wrapped acp launcher, dials a
// new Pump that completes the ACP handshake, and flips the session ACTIVE with endpoint = port.
func TestCreateSessionACPLaunchesPumpOnPoolPort(t *testing.T) {
	sx := &fakeSessionExec{}
	fs := &fakeCPStream{}
	a := newSessionAttacher("s1", sx, fs)

	a.handle(context.Background(), &nodev1.CPMessage{Msg: &nodev1.CPMessage_CreateSession{CreateSession: &nodev1.CreateSession{
		SpawnId: "s1", Transport: nodev1.SessionTransport_SESSION_TRANSPORT_ACP, Runnable: "goose-acp",
	}}})

	waitFor(t, "acp session ACTIVE", func() bool {
		r := fs.lastRoster()
		if r == nil {
			return false
		}
		for _, s := range r.Sessions {
			if s.SessionId == "1" && s.State == nodev1.SessionState_SESSION_STATE_ACTIVE {
				return true
			}
		}
		return false
	})

	sx.mu.Lock()
	if len(sx.acpLaunched) != 1 || sx.acpLaunched[0].port != acpPoolLo || sx.acpLaunched[0].name != "acp-1" {
		sx.mu.Unlock()
		t.Fatalf("acp launch = %+v, want one at port %d name acp-1", sx.acpLaunched, acpPoolLo)
	}
	sx.mu.Unlock()

	a.mu.Lock()
	defer a.mu.Unlock()
	if a.pumps[sessionKey{"s1", "1"}] == nil {
		t.Fatal("acp session must register a Pump keyed by (spawn,session)")
	}
	e, _ := a.sessions["s1"].get("1")
	if e.endpoint != strconv.Itoa(acpPoolLo) {
		t.Fatalf("acp endpoint = %q, want port %d", e.endpoint, acpPoolLo)
	}
}

// When the acp pool is exhausted, CreateSession{acp} is rejected: no entry registered, an ERROR
// SessionStatus is emitted, and no launch happens.
func TestCreateSessionACPRejectedWhenPoolExhausted(t *testing.T) {
	sx := &fakeSessionExec{}
	fs := &fakeCPStream{}
	a := newSessionAttacher("s1", sx, fs)
	// drain the pool.
	reg := a.sessions["s1"]
	for {
		if _, ok := reg.allocPort("drain"); !ok {
			break
		}
	}

	a.handle(context.Background(), &nodev1.CPMessage{Msg: &nodev1.CPMessage_CreateSession{CreateSession: &nodev1.CreateSession{
		SpawnId: "s1", Transport: nodev1.SessionTransport_SESSION_TRANSPORT_ACP, Runnable: "goose-acp",
	}}})

	if st := lastSessionStatus(fs); st == nil || st.State != nodev1.SessionState_SESSION_STATE_ERROR {
		t.Fatalf("want an ERROR SessionStatus on exhaustion, got %+v", st)
	}
	if len(reg.snapshot()) != 1 { // only session #0
		t.Fatalf("exhausted CreateSession must not register a session, roster=%d", len(reg.snapshot()))
	}
	sx.mu.Lock()
	defer sx.mu.Unlock()
	if len(sx.acpLaunched) != 0 {
		t.Fatal("no acp launch should happen when the pool is exhausted")
	}
}

func TestCreateSessionMoshRejectedDuringForkBarrier(t *testing.T) {
	sx := &fakeSessionExec{}
	fs := &fakeCPStream{}
	a := newSessionAttacher("s1", sx, fs)
	a.forkBarriers = map[string]forkIngressBarrier{
		"s1": {sourceGeneration: 1, transferSetID: "ts-1"},
	}

	a.handle(context.Background(), &nodev1.CPMessage{Msg: &nodev1.CPMessage_CreateSession{CreateSession: &nodev1.CreateSession{
		SpawnId: "s1", Transport: nodev1.SessionTransport_SESSION_TRANSPORT_MOSH, Runnable: "shell",
	}}})
	time.Sleep(20 * time.Millisecond)

	sx.mu.Lock()
	moshLaunches := len(sx.moshLaunched)
	sx.mu.Unlock()
	if moshLaunches != 0 {
		t.Fatalf("mosh CreateSession during fork barrier must not launch tmux, launches=%d", moshLaunches)
	}
	if got := len(a.sessions["s1"].snapshot()); got != 1 {
		t.Fatalf("rejected mosh CreateSession must not register a session, roster=%d", got)
	}
	if st := lastSessionStatus(fs); st == nil || st.State != nodev1.SessionState_SESSION_STATE_ERROR {
		t.Fatalf("want an ERROR SessionStatus on fork barrier rejection, got %+v", st)
	}
}

func TestCreateSessionACPRejectedDuringForkBarrierBeforeLaunchOrPump(t *testing.T) {
	sx := &fakeSessionExec{}
	fs := &fakeCPStream{}
	a := newSessionAttacher("s1", sx, fs)
	a.forkBarriers = map[string]forkIngressBarrier{
		"s1": {sourceGeneration: 1, transferSetID: "ts-1"},
	}

	a.handle(context.Background(), &nodev1.CPMessage{Msg: &nodev1.CPMessage_CreateSession{CreateSession: &nodev1.CreateSession{
		SpawnId: "s1", Transport: nodev1.SessionTransport_SESSION_TRANSPORT_ACP, Runnable: "goose-acp",
	}}})
	time.Sleep(20 * time.Millisecond)

	sx.mu.Lock()
	acpLaunches, dials := len(sx.acpLaunched), sx.dials
	sx.mu.Unlock()
	if acpLaunches != 0 || dials != 0 {
		t.Fatalf("acp CreateSession during fork barrier must not launch or dial, launches=%d dials=%d", acpLaunches, dials)
	}
	a.mu.Lock()
	pump := a.pumps[sessionKey{"s1", "1"}]
	a.mu.Unlock()
	if pump != nil {
		t.Fatal("rejected acp CreateSession must not register a Pump")
	}
	if got := len(a.sessions["s1"].snapshot()); got != 1 {
		t.Fatalf("rejected acp CreateSession must not register a session, roster=%d", got)
	}
	if st := lastSessionStatus(fs); st == nil || st.State != nodev1.SessionState_SESSION_STATE_ERROR {
		t.Fatalf("want an ERROR SessionStatus on fork barrier rejection, got %+v", st)
	}
}

// opencode-tui is freely creatable as an additional mosh session WITHOUT a served-opencode sibling
// (sp-npxq: every image runnable is selectable; the launcher attaches to a served backend if present,
// else runs standalone). The old "requires served opencode" gate is gone.
func TestCreateSessionOpencodeTuiNeedsNoServedSibling(t *testing.T) {
	sx := &fakeSessionExec{}
	fs := &fakeCPStream{}
	a := newSessionAttacher("s1", sx, fs) // session #0 runnable = goose-acp (no served opencode)

	a.handle(context.Background(), &nodev1.CPMessage{Msg: &nodev1.CPMessage_CreateSession{CreateSession: &nodev1.CreateSession{
		SpawnId: "s1", Transport: nodev1.SessionTransport_SESSION_TRANSPORT_MOSH, Runnable: "opencode-tui",
	}}})
	waitFor(t, "opencode-tui launched", func() bool {
		sx.mu.Lock()
		defer sx.mu.Unlock()
		return len(sx.moshLaunched) == 1
	})
	if st := lastSessionStatus(fs); st != nil && st.State == nodev1.SessionState_SESSION_STATE_ERROR {
		t.Fatalf("opencode-tui must not be rejected without a served sibling, got ERROR: %+v", st)
	}
}

// Closing a mosh session kills its tmux session, tears down its relay, and removes it from the roster;
// session #0 is untouched.
func TestCloseSessionMoshReapsOnlyThatSession(t *testing.T) {
	sx := &fakeSessionExec{}
	fs := &fakeCPStream{}
	a := newSessionAttacher("s1", sx, fs)
	a.handle(context.Background(), &nodev1.CPMessage{Msg: &nodev1.CPMessage_CreateSession{CreateSession: &nodev1.CreateSession{
		SpawnId: "s1", Transport: nodev1.SessionTransport_SESSION_TRANSPORT_MOSH, Runnable: "shell",
	}}})
	waitFor(t, "mosh active", func() bool {
		a.mu.Lock()
		defer a.mu.Unlock()
		_, ok := a.tmuxRelays[sessionKey{"s1", "1"}]
		return ok
	})

	a.handle(context.Background(), &nodev1.CPMessage{Msg: &nodev1.CPMessage_CloseSession{CloseSession: &nodev1.CloseSession{
		SpawnId: "s1", SessionId: "1",
	}}})

	if got := fs.lastRoster(); got == nil || len(got.Sessions) != 1 || got.Sessions[0].SessionId != SessionZeroID {
		t.Fatalf("after close want roster of just session #0, got %+v", got)
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if _, ok := a.tmuxRelays[sessionKey{"s1", "1"}]; ok {
		t.Fatal("mosh relay not torn down on close")
	}
	sx.mu.Lock()
	defer sx.mu.Unlock()
	if len(sx.killed) != 1 || sx.killed[0] != "spawn-1" {
		t.Fatalf("close must kill tmux session spawn-1, killed=%v", sx.killed)
	}
}

// Closing an acp session stops its Pump, kills its tmux wrapper, and FREES its port (so the next acp
// session reuses the lowest port).
func TestCloseSessionACPFreesPort(t *testing.T) {
	sx := &fakeSessionExec{}
	fs := &fakeCPStream{}
	a := newSessionAttacher("s1", sx, fs)
	a.handle(context.Background(), &nodev1.CPMessage{Msg: &nodev1.CPMessage_CreateSession{CreateSession: &nodev1.CreateSession{
		SpawnId: "s1", Transport: nodev1.SessionTransport_SESSION_TRANSPORT_ACP, Runnable: "goose-acp",
	}}})
	waitFor(t, "acp active", func() bool {
		a.mu.Lock()
		defer a.mu.Unlock()
		_, ok := a.pumps[sessionKey{"s1", "1"}]
		return ok
	})

	a.handle(context.Background(), &nodev1.CPMessage{Msg: &nodev1.CPMessage_CloseSession{CloseSession: &nodev1.CloseSession{
		SpawnId: "s1", SessionId: "1",
	}}})
	waitFor(t, "acp pump gone", func() bool {
		a.mu.Lock()
		defer a.mu.Unlock()
		_, ok := a.pumps[sessionKey{"s1", "1"}]
		return !ok
	})
	sx.mu.Lock()
	if len(sx.killed) != 1 || sx.killed[0] != "acp-1" {
		sx.mu.Unlock()
		t.Fatalf("close must kill tmux wrapper acp-1, killed=%v", sx.killed)
	}
	sx.mu.Unlock()

	// the freed port is reused by the next acp session (lowest-free).
	a.handle(context.Background(), &nodev1.CPMessage{Msg: &nodev1.CPMessage_CreateSession{CreateSession: &nodev1.CreateSession{
		SpawnId: "s1", Transport: nodev1.SessionTransport_SESSION_TRANSPORT_ACP, Runnable: "goose-acp",
	}}})
	waitFor(t, "second acp active", func() bool {
		a.mu.Lock()
		defer a.mu.Unlock()
		_, ok := a.pumps[sessionKey{"s1", "2"}]
		return ok
	})
	sx.mu.Lock()
	defer sx.mu.Unlock()
	last := sx.acpLaunched[len(sx.acpLaunched)-1]
	if last.port != acpPoolLo {
		t.Fatalf("freed port not reused: second acp launched on port %d, want %d", last.port, acpPoolLo)
	}
}

// lastSessionStatus returns the most recent SessionStatus the attacher sent (nil if none).
func lastSessionStatus(f *fakeCPStream) *nodev1.SessionStatus {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := len(f.sent) - 1; i >= 0; i-- {
		if s := f.sent[i].GetSessionStatus(); s != nil {
			return s
		}
	}
	return nil
}
