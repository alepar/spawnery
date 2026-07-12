package fakepod

import (
	"io"
	"sync"

	"spawnery/internal/runtime"
)

// Op names a backend operation, for the ops log and fault injection.
type Op string

const (
	OpPing               Op = "ping"
	OpPreflight          Op = "preflight"
	OpStartPod           Op = "startpod"
	OpStartAgent         Op = "startagent"
	OpStop               Op = "stop"
	OpAttach             Op = "attach"
	OpListManaged        Op = "listmanaged"
	OpContainerEnv       Op = "containerenv"
	OpResolveImageDigest Op = "resolvedigest"
	OpEnsureImage        Op = "ensureimage"
	OpCaptureDelta       Op = "capture"
	OpCaptureDeltaAs     Op = "capture-as"
	OpReleaseDelta       Op = "release"
	OpExportDelta        Op = "export"
	OpImportDelta        Op = "import"
	OpPause              Op = "pause"
	OpUnpause            Op = "unpause"
	OpRestoreForked      Op = "restore"
	OpExec               Op = "exec"
)

// pod is one spawn's pod: three containers, a mount table, and the agent's content view.
type pod struct {
	spawnID    string
	generation uint64
	nodeID     string
	labels     map[string]string

	podIP string
	netns string

	sandbox *container
	sidecar *container
	agent   *container

	podSpec   runtime.PodSpec
	agentSpec runtime.AgentSpec
	mounts    []runtime.Mount // the agent's mount table: partitions content into rootfs vs mount

	rootfs    map[string][]byte // the agent's writable layer (what CaptureDelta snapshots)
	mountData map[string][]byte // content under a mount path (what the journaler snapshots)

	writer *AgentWriter
}

// live reports whether any of the pod's containers still exists.
func (p *pod) live() bool {
	for _, c := range []*container{p.sandbox, p.sidecar, p.agent} {
		if c != nil && c.state != StateRemoved {
			return true
		}
	}
	return false
}

func (p *pod) container(id string) *container {
	for _, c := range []*container{p.sandbox, p.sidecar, p.agent} {
		if c != nil && c.id == id {
			return c
		}
	}
	return nil
}

type config struct {
	podIP            string
	netnsPath        string
	script           func(io.Reader, io.Writer)
	ensureImageRef   string
	resolveDigest    string
	zeroLayerCapture bool
	deltaSizeBytes   int64
	listManaged      []runtime.ManagedPod
	listManagedSet   bool
	faults           map[Op]error
	baseImages       map[string]int // ref -> layer count
}

// Option configures a Backend at construction. Each has a setter equivalent for arming mid-test.
type Option func(*config)

// WithPodIP overrides the pod IP returned by StartPod (default "10.0.0.5").
func WithPodIP(ip string) Option { return func(c *config) { c.podIP = ip } }

// WithNetnsPath overrides the netns path returned by StartPod (default "/proc/7/ns/net").
func WithNetnsPath(p string) Option { return func(c *config) { c.netnsPath = p } }

// WithAttachScript makes Attach hand the agent's stdio to script (stdin reader, stdout writer), as
// the old scriptedPodBackend did. Without it, Attach returns a loopback pipe.
func WithAttachScript(script func(io.Reader, io.Writer)) Option {
	return func(c *config) { c.script = script }
}

// WithEnsureImageRef forces EnsureImage to return ref regardless of the image chain.
func WithEnsureImageRef(ref string) Option { return func(c *config) { c.ensureImageRef = ref } }

// WithResolveDigest forces ResolveImageDigest to return d.
func WithResolveDigest(d string) Option { return func(c *config) { c.resolveDigest = d } }

// WithZeroLayerCapture makes every capture commit a layer count EQUAL to the base's, so the
// moby#47065 zero-layer guard fires.
func WithZeroLayerCapture() Option { return func(c *config) { c.zeroLayerCapture = true } }

// WithBaseImage pre-registers ref as a launchable base image with the given layer count.
func WithBaseImage(ref string, layers int) Option {
	return func(c *config) {
		if c.baseImages == nil {
			c.baseImages = map[string]int{}
		}
		c.baseImages[ref] = layers
	}
}

// WithFailOn makes op return err.
func WithFailOn(op Op, err error) Option {
	return func(c *config) {
		if c.faults == nil {
			c.faults = map[Op]error{}
		}
		c.faults[op] = err
	}
}

// WithListManaged forces ListManaged to return pods verbatim (orphan-reap tests) instead of deriving
// them from the fake's own live pods.
func WithListManaged(pods []runtime.ManagedPod) Option {
	return func(c *config) { c.listManaged, c.listManagedSet = pods, true }
}

// WithDeltaSizeBytes forces DeltaSize to report n bytes for any spawn.
func WithDeltaSizeBytes(n int64) Option { return func(c *config) { c.deltaSizeBytes = n } }

// Backend is an in-memory runtime.PodBackend. Safe for concurrent use: every method serialises on mu.
type Backend struct {
	mu  sync.Mutex
	cfg config

	pods        map[string]*pod
	byContainer map[string]*pod
	images      map[string]*image

	ops            []string
	capturedRefs   []string
	releasedSpawns []string
	importBaseRefs []string
	lastStop       *runtime.PodHandle
	pauseCount     int
	unpauseCount   int
}

// New builds a Backend. Register it with t.Cleanup(b.Close) to join any background agent writers.
func New(opts ...Option) *Backend {
	cfg := config{podIP: "10.0.0.5", netnsPath: "/proc/7/ns/net", faults: map[Op]error{}}
	for _, o := range opts {
		o(&cfg)
	}
	if cfg.faults == nil {
		cfg.faults = map[Op]error{}
	}
	b := &Backend{
		cfg:         cfg,
		pods:        map[string]*pod{},
		byContainer: map[string]*pod{},
		images:      map[string]*image{},
	}
	for ref, layers := range cfg.baseImages {
		b.images[ref] = &image{
			ImageInfo: ImageInfo{Ref: ref, Layers: layers, Launchable: true},
			content:   map[string][]byte{},
		}
	}
	return b
}

var _ runtime.PodBackend = (*Backend)(nil)

// Close stops every background agent writer and joins its goroutine. Idempotent.
// b.mu must NOT be held while stopping a writer — the writer's tick takes b.mu.
func (b *Backend) Close() {
	b.mu.Lock()
	var ws []*AgentWriter
	for _, p := range b.pods {
		if p.writer != nil {
			ws = append(ws, p.writer)
			p.writer = nil
		}
	}
	b.mu.Unlock()
	for _, w := range ws {
		w.Stop()
	}
}

// --- fault injection & recording ---------------------------------------------------------------

// FailOn arms op to return err (until ClearFailOn).
func (b *Backend) FailOn(op Op, err error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.cfg.faults[op] = err
}

// ClearFailOn disarms op.
func (b *Backend) ClearFailOn(op Op) {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.cfg.faults, op)
}

// fault returns the armed error for op (recording the failed call), or nil. Caller holds b.mu.
func (b *Backend) fault(op Op, arg string) error {
	if err, ok := b.cfg.faults[op]; ok && err != nil {
		b.record(op, arg, err)
		return err
	}
	return nil
}

// record appends "<op>[:<arg>]" to the ops log, with a trailing "!" when the call failed.
// Caller holds b.mu.
func (b *Backend) record(op Op, arg string, err error) {
	e := string(op)
	if arg != "" {
		e += ":" + arg
	}
	if err != nil {
		e += "!"
	}
	b.ops = append(b.ops, e)
}

// --- accessors (test hooks) --------------------------------------------------------------------

// Ops returns the ordered log of backend calls, e.g. ["startpod:sp1","capture:sp1","stop:sp1"].
func (b *Backend) Ops() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]string(nil), b.ops...)
}

// CapturedRefs returns every delta ref produced by CaptureDelta/CaptureDeltaAs, in call order.
func (b *Backend) CapturedRefs() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]string(nil), b.capturedRefs...)
}

// ReleasedSpawns returns the spawn ids passed to ReleaseDelta, in call order.
func (b *Backend) ReleasedSpawns() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]string(nil), b.releasedSpawns...)
}

// ImportBaseRefs returns the baseRef seen by each ImportDelta, in call order.
func (b *Backend) ImportBaseRefs() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]string(nil), b.importBaseRefs...)
}

// LastStopHandle returns the handle passed to the most recent Stop (nil if never called).
func (b *Backend) LastStopHandle() *runtime.PodHandle {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.lastStop
}

// PauseCount / UnpauseCount count ATTEMPTS, including failed ones (as the old fake did).
func (b *Backend) PauseCount() int   { b.mu.Lock(); defer b.mu.Unlock(); return b.pauseCount }
func (b *Backend) UnpauseCount() int { b.mu.Lock(); defer b.mu.Unlock(); return b.unpauseCount }

// State returns the state of a pod's container by role ("sandbox"|"sidecar"|"agent");
// StateAbsent when the pod or container does not exist.
func (b *Backend) State(spawnID, role string) State {
	b.mu.Lock()
	defer b.mu.Unlock()
	p, ok := b.pods[spawnID]
	if !ok {
		return StateAbsent
	}
	var c *container
	switch role {
	case "sandbox":
		c = p.sandbox
	case "sidecar":
		c = p.sidecar
	case "agent":
		c = p.agent
	}
	if c == nil {
		return StateAbsent
	}
	return c.state
}

// Labels returns a copy of the pod-level labels the Manager applied (managed/spawn-id/generation/
// node-id). Keeping this accessor is also what keeps pod.labels from tripping the `unused` linter.
func (b *Backend) Labels(spawnID string) map[string]string {
	b.mu.Lock()
	defer b.mu.Unlock()
	p, ok := b.pods[spawnID]
	if !ok {
		return nil
	}
	out := make(map[string]string, len(p.labels))
	for k, v := range p.labels {
		out[k] = v
	}
	return out
}

// PodSpec returns the PodSpec the Manager passed for spawnID.
func (b *Backend) PodSpec(spawnID string) runtime.PodSpec {
	b.mu.Lock()
	defer b.mu.Unlock()
	if p, ok := b.pods[spawnID]; ok {
		return p.podSpec
	}
	return runtime.PodSpec{}
}

// AgentSpec returns the AgentSpec the Manager passed for spawnID.
func (b *Backend) AgentSpec(spawnID string) runtime.AgentSpec {
	b.mu.Lock()
	defer b.mu.Unlock()
	if p, ok := b.pods[spawnID]; ok {
		return p.agentSpec
	}
	return runtime.AgentSpec{}
}

// SetListManaged forces ListManaged's return value (orphan-reap tests).
func (b *Backend) SetListManaged(pods []runtime.ManagedPod) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.cfg.listManaged, b.cfg.listManagedSet = pods, true
}

// SetEnsureImageRef forces EnsureImage's return value.
func (b *Backend) SetEnsureImageRef(ref string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.cfg.ensureImageRef = ref
}

// SetZeroLayerCapture arms/disarms the zero-layer commit (moby#47065 guard).
func (b *Backend) SetZeroLayerCapture(on bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.cfg.zeroLayerCapture = on
}

// SetDeltaSizeBytes forces DeltaSize's return value.
func (b *Backend) SetDeltaSizeBytes(n int64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.cfg.deltaSizeBytes = n
}

// WithoutDeltaSize returns a PodBackend view of b that does NOT expose DeltaSize — the "unknown
// size" backend Manager.DeltaSize's type assertion must fail against. Embedding the INTERFACE (not
// *Backend) is what hides the method: only PodBackend's own methods are promoted.
func WithoutDeltaSize(b *Backend) runtime.PodBackend { return noDeltaSize{b} }

type noDeltaSize struct{ runtime.PodBackend }
