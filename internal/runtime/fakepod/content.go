package fakepod

import (
	"context"
	"fmt"
	"path"
	"strings"

	"spawnery/internal/runtime"
)

// isMountPath reports whether p is at, or under, one of the agent's mount container paths.
func isMountPath(mounts []runtime.Mount, p string) bool {
	c := path.Clean(p)
	for _, m := range mounts {
		mp := path.Clean(m.ContainerPath)
		if c == mp || strings.HasPrefix(c, mp+"/") {
			return true
		}
	}
	return false
}

// write routes a write into the mount view or the rootfs view, per the mount table.
// Caller holds b.mu.
func (p *pod) write(file string, data []byte) {
	f := path.Clean(file)
	cp := append([]byte(nil), data...)
	if isMountPath(p.mounts, f) {
		p.mountData[f] = cp
		return
	}
	p.rootfs[f] = cp
}

// removeUnder deletes every path at, or under, prefix from BOTH views. The mount view is NOT
// protected here on purpose (see design note 6). Caller holds b.mu.
func (p *pod) removeUnder(prefix string) {
	pre := path.Clean(prefix)
	for _, view := range []map[string][]byte{p.rootfs, p.mountData} {
		for k := range view {
			if k == pre || strings.HasPrefix(k, pre+"/") {
				delete(view, k)
			}
		}
	}
}

// copyView deep-copies a content view, so a captured artifact can never alias live content.
func copyView(m map[string][]byte) map[string][]byte {
	out := make(map[string][]byte, len(m))
	for k, v := range m {
		out[k] = append([]byte(nil), v...)
	}
	return out
}

// AgentWrite is the test hook for "the agent wrote a file". It requires a RUNNING agent — a paused
// agent is frozen and its processes cannot write.
func (b *Backend) AgentWrite(spawnID, file string, data []byte) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	p, ok := b.pods[spawnID]
	if !ok {
		return fmt.Errorf("fakepod: AgentWrite: no pod %q", spawnID)
	}
	if p.agent == nil {
		return fmt.Errorf("fakepod: AgentWrite: pod %q has no agent", spawnID)
	}
	if err := p.agent.requireState("AgentWrite", StateRunning); err != nil {
		return err
	}
	p.write(file, data)
	return nil
}

// RootfsView returns a copy of the agent's writable-layer content (what CaptureDelta snapshots).
func (b *Backend) RootfsView(spawnID string) map[string][]byte {
	b.mu.Lock()
	defer b.mu.Unlock()
	p, ok := b.pods[spawnID]
	if !ok {
		return map[string][]byte{}
	}
	return copyView(p.rootfs)
}

// MountView returns a copy of the content under the agent's mount paths (what the journaler
// snapshots). A consistent suspend produces a rootfs artifact and a mount snapshot from the SAME
// instant; comparing the two is how a torn snapshot is detected.
func (b *Backend) MountView(spawnID string) map[string][]byte {
	b.mu.Lock()
	defer b.mu.Unlock()
	p, ok := b.pods[spawnID]
	if !ok {
		return map[string][]byte{}
	}
	return copyView(p.mountData)
}

// Exec models the `docker exec` scrub seam. It FAILS on a paused container ("container is paused"),
// exactly as Docker does — which is why the pre-SE2 suspend teardown unpauses before scrubbing.
// It understands `rm -rf <paths...>`; any other argv is a recorded no-op (mkdir/chmod).
func (b *Backend) Exec(_ context.Context, containerID string, argv []string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	p, ok := b.byContainer[containerID]
	if !ok {
		return fmt.Errorf("fakepod: exec: no such container %q", containerID)
	}
	c := p.container(containerID)
	if c == nil {
		return fmt.Errorf("fakepod: exec: no such container %q", containerID)
	}
	if err := b.fault(OpExec, containerID); err != nil {
		return err
	}
	switch c.state {
	case StateRunning:
		// ok
	case StatePaused:
		return fmt.Errorf("fakepod: exec %q: container is paused", containerID)
	default:
		return fmt.Errorf("fakepod: exec %q: container is %s", containerID, c.state)
	}
	if len(argv) >= 2 && argv[0] == "rm" && argv[1] == "-rf" {
		for _, target := range argv[2:] {
			p.removeUnder(target)
		}
	}
	b.record(OpExec, containerID+":"+strings.Join(argv, " "), nil)
	return nil
}

// ScrubFn returns a function with Manager.scrubFn's signature, wired to Exec — so a spawnlet test
// can inject the fake's exec seam with `m.scrubFn = b.ScrubFn()`.
func (b *Backend) ScrubFn() func(ctx context.Context, agentID string, paths []string) error {
	return func(ctx context.Context, agentID string, paths []string) error {
		return b.Exec(ctx, agentID, append([]string{"rm", "-rf"}, paths...))
	}
}
