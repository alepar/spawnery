package fakepod

import (
	"context"
	"fmt"
	"path"
	"strconv"
	"sync"
	"time"
)

// writerResumeTimeout bounds Unpause's wait for the resumed writer's first write.
const writerResumeTimeout = 2 * time.Second

type writerCfg struct {
	interval   time.Duration
	rootfsPath string
	mountPath  string
}

// WriterOption configures an AgentWriter.
type WriterOption func(*writerCfg)

// WriterInterval sets the writer's tick interval (default 200µs).
func WriterInterval(d time.Duration) WriterOption { return func(c *writerCfg) { c.interval = d } }

// WriterPaths overrides the two paths the writer writes to. rootfsPath must NOT be under a mount;
// mountPath must be under one (or be empty, for a rootfs-only writer).
func WriterPaths(rootfsPath, mountPath string) WriterOption {
	return func(c *writerCfg) { c.rootfsPath, c.mountPath = rootfsPath, mountPath }
}

// AgentWriter models the agent's own processes — builds, LSP servers, editor autosave — that keep
// writing until the container is paused. It writes a monotonically increasing sequence number to ONE
// rootfs path and ONE mount path *inside a single b.mu critical section*, so a consistent snapshot
// pair always carries the same number and a torn one does not. It writes only while the agent is
// RUNNING: a paused agent is frozen.
type AgentWriter struct {
	b       *Backend
	spawnID string
	cfg     writerCfg

	stop     chan struct{}
	done     chan struct{}
	stopOnce sync.Once

	mu    sync.Mutex
	ticks uint64
	ch    chan struct{} // closed on every landed write, then replaced
}

// StartAgentWriter attaches a background writer to spawnID's agent. Defaults: /work/.agent-seq in the
// rootfs and <first mount>/.agent-seq in the mount view. Stop it (or call Backend.Close) to join it.
func (b *Backend) StartAgentWriter(spawnID string, opts ...WriterOption) (*AgentWriter, error) {
	b.mu.Lock()
	p, ok := b.pods[spawnID]
	if !ok {
		b.mu.Unlock()
		return nil, fmt.Errorf("fakepod: StartAgentWriter: no pod %q", spawnID)
	}
	if p.agent == nil {
		b.mu.Unlock()
		return nil, fmt.Errorf("fakepod: StartAgentWriter: pod %q has no agent", spawnID)
	}
	if p.writer != nil {
		b.mu.Unlock()
		return nil, fmt.Errorf("fakepod: StartAgentWriter: pod %q already has a writer", spawnID)
	}
	cfg := writerCfg{interval: 200 * time.Microsecond, rootfsPath: "/work/.agent-seq"}
	if len(p.mounts) > 0 {
		cfg.mountPath = path.Join(path.Clean(p.mounts[0].ContainerPath), ".agent-seq")
	}
	for _, o := range opts {
		o(&cfg)
	}
	if isMountPath(p.mounts, cfg.rootfsPath) {
		b.mu.Unlock()
		return nil, fmt.Errorf("fakepod: StartAgentWriter: rootfs path %q is under a mount", cfg.rootfsPath)
	}
	if cfg.mountPath != "" && !isMountPath(p.mounts, cfg.mountPath) {
		b.mu.Unlock()
		return nil, fmt.Errorf("fakepod: StartAgentWriter: mount path %q is not under a mount", cfg.mountPath)
	}
	w := &AgentWriter{
		b:       b,
		spawnID: spawnID,
		cfg:     cfg,
		stop:    make(chan struct{}),
		done:    make(chan struct{}),
		ch:      make(chan struct{}),
	}
	p.writer = w
	b.mu.Unlock()
	go w.run()
	return w, nil
}

func (w *AgentWriter) run() {
	defer close(w.done)
	t := time.NewTicker(w.cfg.interval)
	defer t.Stop()
	for {
		select {
		case <-w.stop:
			return
		case <-t.C:
			w.tick()
		}
	}
}

// tick lands one write iff the agent is RUNNING. Both paths get the same sequence number, written
// under b.mu, so no snapshot can observe them half-updated.
// Lock order: b.mu -> w.mu. Never the reverse (design note 8).
func (w *AgentWriter) tick() {
	w.b.mu.Lock()
	p, ok := w.b.pods[w.spawnID]
	if !ok || p.agent == nil || p.agent.state != StateRunning {
		w.b.mu.Unlock()
		return
	}
	w.mu.Lock()
	w.ticks++
	seq := []byte(strconv.FormatUint(w.ticks, 10))
	w.mu.Unlock()
	p.write(w.cfg.rootfsPath, seq)
	if w.cfg.mountPath != "" {
		p.write(w.cfg.mountPath, seq)
	}
	w.b.mu.Unlock()

	w.mu.Lock()
	close(w.ch)
	w.ch = make(chan struct{})
	w.mu.Unlock()
}

// Ticks is the number of writes landed so far.
func (w *AgentWriter) Ticks() uint64 {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.ticks
}

// awaitTick blocks until at least one further write lands, the writer stops, ctx is done, or timeout
// elapses. It must be called WITHOUT b.mu held.
func (w *AgentWriter) awaitTick(ctx context.Context, timeout time.Duration) error {
	w.mu.Lock()
	start, ch := w.ticks, w.ch
	w.mu.Unlock()
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	for {
		select {
		case <-ch:
			w.mu.Lock()
			n, next := w.ticks, w.ch
			w.mu.Unlock()
			if n > start {
				return nil
			}
			ch = next
		case <-w.done:
			return nil // the writer is gone; no further writes will land — not an error
		case <-timer.C:
			return fmt.Errorf("fakepod: agent writer for %s did not resume within %s", w.spawnID, timeout)
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// Stop halts the writer and joins its goroutine. Idempotent.
func (w *AgentWriter) Stop() {
	w.stopOnce.Do(func() { close(w.stop) })
	<-w.done
}
