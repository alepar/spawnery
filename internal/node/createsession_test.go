package node

import (
	"context"
	"io"
	"sync"
	"testing"

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
	launchMoshErr error
	launchACPErr  error
	dialErr       error
}

func (f *fakeSessionExec) LaunchMosh(_ context.Context, _, _, tmuxName string) error {
	f.mu.Lock()
	f.moshLaunched = append(f.moshLaunched, tmuxName)
	f.mu.Unlock()
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
	if f.dialErr != nil {
		return nil, f.dialErr
	}
	f.mu.Lock()
	f.dials++
	f.mu.Unlock()
	inR, inW := io.Pipe()   // pump writes -> inW; goose reads inR
	outR, outW := io.Pipe() // goose writes outW; pump reads outR
	go func() { scriptGoose(inR, outW); _ = outW.Close() }()
	return &runtime.AttachedStream{Stdin: inW, Stdout: outR, Close: func() error { _ = inW.Close(); _ = outW.Close(); return nil }}, nil
}
func (f *fakeSessionExec) KillTmux(_ context.Context, _, tmuxName string) error {
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
