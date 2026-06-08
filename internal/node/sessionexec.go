package node

import (
	"context"
	"strconv"

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
