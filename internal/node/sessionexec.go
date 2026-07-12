package node

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"strconv"
	"strings"

	"spawnery/internal/runtime"
	"spawnery/internal/spawnlet"
)

// sessionExec is the container-exec boundary for additional-session launch/reap (sp-npxq.3). It keeps
// the launch/reap logic in attach.go unit-testable without docker: the real impl shells out via the
// Manager; tests inject a fake that records calls and returns in-memory ACP streams.
type sessionExec interface {
	// LaunchMosh creates a detached transparent-tmux session named tmuxName running runnable, via the
	// in-image launcher (`launcher --runnable <id> --tmux-session <name>`, no --keepalive — it's exec'd,
	// not PID 1). Returns once the launcher has detached the session.
	LaunchMosh(ctx context.Context, spawnID, runnable, tmuxName string) error
	// MoshAttachArgv is the argv the per-session tmuxRelay execs to attach a PTY to tmuxName.
	MoshAttachArgv(spawnID, tmuxName string) ([]string, error)
	// LaunchACP starts runnable's ACP server bound to port, wrapped in a node-named detached tmux
	// session (tmuxName) so it can be reaped by name (`tmux new-session -d -s <name> -- launcher
	// --runnable <id> --acp-port <N>`). Returns once tmux has spawned the launcher.
	LaunchACP(ctx context.Context, spawnID, runnable, tmuxName string, port int) error
	// DialACP opens an ACP stream to the in-pod endpoint at podIP:port (for the Nth Pump).
	DialACP(ctx context.Context, spawnID string, port int) (*runtime.AttachedStream, error)
	// KillTmux reaps a tmux session by name (`tmux kill-session -t <name>`), best-effort.
	KillTmux(ctx context.Context, spawnID, tmuxName string) error
	// ReapExtraSessions kills every tmux session in the spawn's agent container EXCEPT session #0's
	// ("spawn") — i.e. exactly the additional-session servers a previous node process launched
	// (spawn-<n> / acp-<n>). Their node-side registry died with that process, so without this they squat
	// their tmux names and ACP pool ports while the rebuilt registry re-allocates from 1 (design §4.6).
	// Best-effort: errors are the caller's to log, never to fail an adoption over.
	ReapExtraSessions(ctx context.Context, spawnID string) error
}

// realSessionExec is the production sessionExec: every op resolves to a docker/crictl exec or TCP dial
// via the Manager (Task 2 helpers).
type realSessionExec struct{ mgr *spawnlet.Manager }

func (s *realSessionExec) LaunchMosh(ctx context.Context, spawnID, runnable, tmuxName string) error {
	return s.mgr.ExecRun(ctx, spawnID, []string{"launcher", "--runnable", runnable, "--tmux-session", tmuxName})
}

func (s *realSessionExec) MoshAttachArgv(spawnID, tmuxName string) ([]string, error) {
	return s.mgr.TmuxAttachArgvFor(spawnID, tmuxName)
}

func (s *realSessionExec) LaunchACP(ctx context.Context, spawnID, runnable, tmuxName string, port int) error {
	// Wrap the foreground acp launcher in a node-named detached tmux session so `tmux kill-session`
	// reaps it (the launcher's acp path is NOT tmux-wrapped by sp-npxq.2; a bare docker-exec'd server
	// can't be reliably killed by the node). See plan decision 1.
	return s.mgr.ExecRun(ctx, spawnID, []string{
		"tmux", "new-session", "-d", "-s", tmuxName, "--",
		"launcher", "--runnable", runnable, "--acp-port", strconv.Itoa(port),
	})
}

func (s *realSessionExec) DialACP(ctx context.Context, spawnID string, port int) (*runtime.AttachedStream, error) {
	return s.mgr.AttachACPPort(ctx, spawnID, port)
}

func (s *realSessionExec) KillTmux(ctx context.Context, spawnID, tmuxName string) error {
	return s.mgr.ExecRun(ctx, spawnID, []string{"tmux", "kill-session", "-t", tmuxName})
}

func (s *realSessionExec) ReapExtraSessions(ctx context.Context, spawnID string) error {
	var out bytes.Buffer
	// `tmux list-sessions` exits non-zero when there is no server at all — that is the common, healthy
	// case (no additional sessions), not an error.
	if code, err := s.mgr.ExecStream(ctx, spawnID, []string{"tmux", "list-sessions", "-F", "#{session_name}"}, &out, io.Discard); err != nil || code != 0 {
		return nil
	}
	var firstErr error
	for _, name := range strings.Fields(out.String()) {
		if name == "" || name == "spawn" { // session #0's tmux; not ours to reap
			continue
		}
		if err := s.KillTmux(ctx, spawnID, name); err != nil && firstErr == nil {
			firstErr = fmt.Errorf("kill tmux session %q: %w", name, err)
		}
	}
	return firstErr
}
