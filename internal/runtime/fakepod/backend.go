package fakepod

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"

	"spawnery/internal/runtime"
)

func (b *Backend) Ping(_ context.Context) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.fault(OpPing, "")
}

func (b *Backend) Preflight(_ context.Context) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.fault(OpPreflight, "")
}

// lookup resolves a handle to a pod by container id (the handle's ids are the identity, as on the
// real backends), falling back to SpawnID. Caller holds b.mu.
func (b *Backend) lookup(h *runtime.PodHandle) (*pod, error) {
	if h == nil {
		return nil, fmt.Errorf("nil pod handle")
	}
	for _, id := range []string{h.AgentID, h.SidecarID, h.SandboxID} {
		if id == "" {
			continue
		}
		if p, ok := b.byContainer[id]; ok {
			return p, nil
		}
	}
	if h.SpawnID != "" {
		if p, ok := b.pods[h.SpawnID]; ok {
			return p, nil
		}
	}
	return nil, fmt.Errorf("no pod for handle (agent=%q sidecar=%q sandbox=%q spawn=%q)",
		h.AgentID, h.SidecarID, h.SandboxID, h.SpawnID)
}

// newContainer creates and starts a container through the legal edges. Caller holds b.mu.
func (b *Backend) newContainer(p *pod, id, role string) (*container, error) {
	c := &container{id: id, role: role, state: StateAbsent}
	if err := c.transition(StateCreated); err != nil {
		return nil, err
	}
	if err := c.transition(StateRunning); err != nil {
		return nil, err
	}
	b.byContainer[id] = p
	return c, nil
}

// StartPod creates the sandbox + sidecar. It deliberately leaves PodHandle.SpawnID EMPTY, like both
// real backends — the Manager fills it in.
func (b *Backend) StartPod(_ context.Context, spec runtime.PodSpec) (*runtime.PodHandle, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if err := b.fault(OpStartPod, spec.ID); err != nil {
		return nil, err
	}
	if p, ok := b.pods[spec.ID]; ok && p.live() {
		return nil, fmt.Errorf("fakepod: StartPod: pod %q is already running", spec.ID)
	}
	gen, _ := strconv.ParseUint(spec.Labels[runtime.LabelGeneration], 10, 64)
	p := &pod{
		spawnID:    spec.ID,
		generation: gen,
		nodeID:     spec.Labels[runtime.LabelNodeID],
		labels:     spec.Labels,
		podIP:      b.cfg.podIP,
		netns:      b.cfg.netnsPath,
		podSpec:    spec,
		rootfs:     map[string][]byte{},
		mountData:  map[string][]byte{},
	}
	b.ensureBaseImage(spec.SidecarImage)
	var err error
	if p.sandbox, err = b.newContainer(p, spec.ID+"-sandbox", "sandbox"); err != nil {
		return nil, err
	}
	if p.sidecar, err = b.newContainer(p, spec.ID+"-sidecar", "sidecar"); err != nil {
		return nil, err
	}
	b.pods[spec.ID] = p
	b.record(OpStartPod, spec.ID, nil)
	return &runtime.PodHandle{
		PodIP:     p.podIP,
		NetnsPath: p.netns,
		SidecarID: p.sidecar.id,
		SandboxID: p.sandbox.id,
	}, nil
}

// StartAgent starts the agent into an EXISTING, running sandbox — into a non-existent one it errors.
// That is the two-phase contract, and a no-op here would hide a real ordering bug.
//
// Launching from a delta image replays that image's captured content into the agent's rootfs: that
// is what "a resumed spawn sees its writes" means.
func (b *Backend) StartAgent(_ context.Context, h *runtime.PodHandle, spec runtime.AgentSpec) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	p, err := b.lookup(h)
	if err != nil {
		return fmt.Errorf("fakepod: StartAgent: %w", err)
	}
	if ferr := b.fault(OpStartAgent, p.spawnID); ferr != nil {
		return ferr
	}
	if err := p.sandbox.requireState("StartAgent", StateRunning); err != nil {
		return err
	}
	if p.agent != nil && p.agent.state != StateRemoved {
		return fmt.Errorf("fakepod: StartAgent: pod %q already has an agent (%s)", p.spawnID, p.agent.state)
	}
	img := b.ensureBaseImage(spec.Image)
	if img == nil || !img.Launchable {
		return fmt.Errorf("fakepod: StartAgent: image %q is not launchable", spec.Image)
	}
	c, err := b.newContainer(p, p.spawnID+"-agent", "agent")
	if err != nil {
		return err
	}
	p.agent = c
	p.agentSpec = spec
	p.mounts = spec.Mounts
	for k, v := range img.content { // replay the delta's rootfs (resume)
		p.rootfs[k] = append([]byte(nil), v...)
	}
	h.AgentID = c.id
	b.record(OpStartAgent, p.spawnID, nil)
	return nil
}

// Attach returns the agent's stdio. It requires a RUNNING agent: attaching to a paused, stopped or
// removed one is an error, not an empty stream.
func (b *Backend) Attach(_ context.Context, h *runtime.PodHandle) (*runtime.AttachedStream, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	p, err := b.lookup(h)
	if err != nil {
		return nil, fmt.Errorf("fakepod: Attach: %w", err)
	}
	if ferr := b.fault(OpAttach, p.spawnID); ferr != nil {
		return nil, ferr
	}
	if p.agent == nil {
		return nil, fmt.Errorf("fakepod: Attach: pod %q has no agent", p.spawnID)
	}
	if err := p.agent.requireState("Attach", StateRunning); err != nil {
		return nil, err
	}
	b.record(OpAttach, p.spawnID, nil)
	if b.cfg.script != nil {
		inR, inW := io.Pipe()
		outR, outW := io.Pipe()
		go func() { b.cfg.script(inR, outW); _ = outW.Close() }()
		return &runtime.AttachedStream{
			Stdin:  inW,
			Stdout: outR,
			Close:  func() error { _ = inW.Close(); _ = outW.Close(); return nil },
		}, nil
	}
	pr, pw := io.Pipe()
	return &runtime.AttachedStream{Stdin: pw, Stdout: pr, Close: pw.Close}, nil
}

// Stop tears the pod down: agent, then sidecar, then sandbox, each driven to removed. Stop is the ONE
// idempotent method — an unknown or already-removed pod is not an error (both real lanes agree, and
// teardown is retried on failure paths).
func (b *Backend) Stop(_ context.Context, h *runtime.PodHandle) error {
	b.mu.Lock()
	p, err := b.lookup(h)
	if err != nil {
		b.mu.Unlock()
		return nil
	}
	if ferr := b.fault(OpStop, p.spawnID); ferr != nil {
		b.mu.Unlock()
		return ferr
	}
	w := p.writer
	p.writer = nil
	b.mu.Unlock()
	if w != nil {
		w.Stop() // join the writer goroutine BEFORE teardown — never under b.mu (see design note 8)
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, c := range []*container{p.agent, p.sidecar, p.sandbox} {
		if c == nil {
			continue
		}
		switch c.state {
		case StateRunning, StatePaused:
			_ = c.transition(StateStopped)
			_ = c.transition(StateRemoved)
		case StateCreated, StateStopped:
			_ = c.transition(StateRemoved)
		case StateAbsent, StateRemoved:
			// nothing to do
		}
	}
	b.lastStop = h
	b.record(OpStop, p.spawnID, nil)
	return nil
}

// ListManaged derives the managed inventory from the live pods' labels (or returns the scripted
// override). A torn-down pod is not managed.
func (b *Backend) ListManaged(_ context.Context) ([]runtime.ManagedPod, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if err := b.fault(OpListManaged, ""); err != nil {
		return nil, err
	}
	if b.cfg.listManagedSet {
		return append([]runtime.ManagedPod(nil), b.cfg.listManaged...), nil
	}
	out := make([]runtime.ManagedPod, 0, len(b.pods))
	for _, p := range b.pods {
		if !p.live() {
			continue
		}
		mp := runtime.ManagedPod{SpawnID: p.spawnID, Generation: p.generation, NodeID: p.nodeID, PodIP: p.podIP}
		if p.sandbox != nil && p.sandbox.state != StateRemoved {
			mp.SandboxID = p.sandbox.id
		}
		if p.sidecar != nil && p.sidecar.state != StateRemoved {
			mp.SidecarID = p.sidecar.id
		}
		if p.agent != nil && p.agent.state != StateRemoved {
			mp.AgentID = p.agent.id
		}
		out = append(out, mp)
	}
	return out, nil
}

// ErrNotPaused is returned by Unpause / RestoreForkedSource when the agent is not paused. Both real
// backends return a tolerable "not paused" error there (see the PodBackend doc) and the suspend
// teardown ignores it — so callers can errors.Is against it rather than string-matching.
var ErrNotPaused = errors.New("fakepod: agent is not paused")

// Pause quiesces the agent. A double Pause is an ERROR (illegal edge), not a no-op.
func (b *Backend) Pause(_ context.Context, h *runtime.PodHandle) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	p, err := b.lookup(h)
	if err != nil {
		return fmt.Errorf("fakepod: Pause: %w", err)
	}
	b.pauseCount++ // count the ATTEMPT, as the old fake did
	if ferr := b.fault(OpPause, p.spawnID); ferr != nil {
		return ferr
	}
	if p.agent == nil {
		return fmt.Errorf("fakepod: Pause: pod %q has no agent", p.spawnID)
	}
	if err := p.agent.transition(StatePaused); err != nil {
		return err
	}
	b.record(OpPause, p.spawnID, nil)
	return nil
}

func (b *Backend) Unpause(ctx context.Context, h *runtime.PodHandle) error {
	return b.unpause(ctx, h, OpUnpause)
}

// RestoreForkedSource returns the source agent to running after a source-preserving CaptureDeltaAs —
// which, on both real lanes, is exactly an Unpause.
func (b *Backend) RestoreForkedSource(ctx context.Context, h *runtime.PodHandle) error {
	return b.unpause(ctx, h, OpRestoreForked)
}

func (b *Backend) unpause(ctx context.Context, h *runtime.PodHandle, op Op) error {
	b.mu.Lock()
	p, err := b.lookup(h)
	if err != nil {
		b.mu.Unlock()
		return fmt.Errorf("fakepod: %s: %w", op, err)
	}
	b.unpauseCount++
	if ferr := b.fault(op, p.spawnID); ferr != nil {
		b.mu.Unlock()
		return ferr
	}
	if p.agent == nil {
		b.mu.Unlock()
		return fmt.Errorf("fakepod: %s: pod %q has no agent", op, p.spawnID)
	}
	if p.agent.state != StatePaused {
		b.mu.Unlock()
		return fmt.Errorf("%w (agent %q is %s)", ErrNotPaused, p.agent.id, p.agent.state)
	}
	if err := p.agent.transition(StateRunning); err != nil {
		b.mu.Unlock()
		return err
	}
	w := p.writer
	b.record(op, p.spawnID, nil)
	b.mu.Unlock()

	// Determinism hook (design note 7): a resumed agent's processes immediately do work. Block until
	// the attached writer lands one write, so "the quiescence window was reopened" is a deterministic
	// failure rather than a timing race. b.mu is NOT held — the writer takes it on every tick.
	if w != nil {
		return w.awaitTick(ctx, writerResumeTimeout)
	}
	return nil
}
