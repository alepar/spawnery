package runtime

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"os"
	"runtime"
	"time"

	"golang.org/x/sys/unix"
)

// acpSocket is the abstract Unix socket the in-container acpadapter listens on.
const acpSocket = "@spawnlet-acp"

// AttachACP enters the pod network namespace at netnsPath (e.g.
// /proc/<sidecar-pid>/ns/net) and dials the agent's abstract ACP socket,
// returning a bidirectional stream whose Stdin/Stdout are the same connection.
// Requires CAP_SYS_ADMIN (setns). Used for both the Docker and CRI backends.
func AttachACP(ctx context.Context, netnsPath string) (*AttachedStream, error) {
	conn, err := dialInNetns(ctx, netnsPath, acpSocket)
	if err != nil {
		return nil, err
	}
	return &AttachedStream{
		Stdin:  conn,
		Stdout: conn,
		Close:  conn.Close,
	}, nil
}

// dialInNetns retries the dial for a few seconds: right after the agent
// container starts, the adapter may not have bound the socket yet.
func dialInNetns(ctx context.Context, netnsPath, sock string) (net.Conn, error) {
	deadline := time.Now().Add(5 * time.Second)
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		conn, err := dialOnce(netnsPath, sock)
		if err == nil {
			return conn, nil
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("dial ACP socket in %s: %w", netnsPath, err)
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}
}

// dialOnce locks the OS thread, switches its network namespace to netnsPath,
// dials the socket, then restores the thread's namespace. The returned conn's
// fd is valid regardless of namespace once connected.
func dialOnce(netnsPath, sock string) (net.Conn, error) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	orig, err := os.Open("/proc/thread-self/ns/net")
	if err != nil {
		return nil, fmt.Errorf("open current netns: %w", err)
	}
	defer orig.Close()

	target, err := os.Open(netnsPath)
	if err != nil {
		return nil, fmt.Errorf("open target netns %s: %w", netnsPath, err)
	}
	defer target.Close()

	if err := unix.Setns(int(target.Fd()), unix.CLONE_NEWNET); err != nil {
		return nil, fmt.Errorf("setns %s: %w", netnsPath, err)
	}
	// Restore this thread's original namespace no matter what. A failure here is
	// pathological (it would leave this OS thread in the pod netns); log it so a
	// leaked mis-namespaced thread is diagnosable.
	defer func() {
		if err := unix.Setns(int(orig.Fd()), unix.CLONE_NEWNET); err != nil {
			slog.Error("AttachACP: failed to restore netns on thread", "err", err)
		}
	}()

	return net.Dial("unix", sock)
}
