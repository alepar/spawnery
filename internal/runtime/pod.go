package runtime

import "context"

// Container/sandbox label keys identifying spawnery-managed pods so a restarted node (or the CP) can
// reconcile a running pod against the authoritative ledger and reap orphans/stale generations.
const (
	LabelManaged    = "spawnery.managed"    // "true" on every spawnery-created container/sandbox
	LabelSpawnID    = "spawnery.spawn-id"   // the spawn id
	LabelGeneration = "spawnery.generation" // the spawn's generation (decimal uint64), for fencing
	LabelNodeID     = "spawnery.node-id"    // the node that created it
	LabelRole       = "spawnery.role"       // "sidecar" | "agent" (Docker lane: groups the pod's containers)
)

// Resources are the per-container cgroup limits applied to both pod containers.
type Resources struct {
	MemoryBytes int64
	NanoCPUs    int64
	PidsLimit   int64
}

// PodSpec describes the pod sandbox + its sidecar container (started by StartPod).
type PodSpec struct {
	ID           string // spawn id
	SidecarImage string
	SidecarEnv   []string
	Resources    Resources
	Runtime      string            // OCI runtime; "" = default, e.g. "runsc"
	Labels       map[string]string // applied to the sandbox + sidecar (managed/spawn-id/generation/node-id)
}

// AgentSpec describes the agent container (started by StartAgent into the existing pod).
type AgentSpec struct {
	Image          string
	Cmd            []string // overrides the image's default command (the runnable's launch argv); nil = image default
	Env            []string
	Mounts         []Mount
	Resources      Resources
	Runtime        string
	DropAllCaps    bool
	ReadonlyRootfs bool
	Labels         map[string]string // applied to the agent container
}

// ManagedPod is one spawnery-managed pod the backend currently sees running (from its labels), used
// for orphan reconciliation. SpawnID/Generation come from the labels; the *ID fields drive teardown.
type ManagedPod struct {
	SpawnID    string
	Generation uint64
	NodeID     string
	SidecarID  string // Docker backend
	AgentID    string // Docker backend
	SandboxID  string // CRI backend
}

// PodHandle identifies a running pod. PodIP (for the egress floor) and NetnsPath (for the ACP
// attach) are read by the Manager; the *ID fields are backend-specific identifiers the Manager
// persists on the Spawn and hands back to Stop.
type PodHandle struct {
	PodIP     string
	NetnsPath string
	SidecarID string // Docker backend: the sidecar container id (netns owner)
	AgentID   string // Docker backend: the agent container id (set by StartAgent)
	SandboxID string // CRI backend: the pod sandbox id (Docker backend leaves empty)
}

// PodBackend runs a spawn pod: a sidecar + an agent sharing one network namespace, with the model
// key kept isolated in the sidecar. It is two-phase (StartPod then StartAgent) so the egress floor
// can be applied after the pod IP exists and before the untrusted agent starts.
type PodBackend interface {
	Ping(ctx context.Context) error
	Preflight(ctx context.Context) error
	StartPod(ctx context.Context, spec PodSpec) (*PodHandle, error)
	StartAgent(ctx context.Context, h *PodHandle, spec AgentSpec) error
	Stop(ctx context.Context, h *PodHandle) error
	// Attach returns the agent's ACP stdio stream. Docker backend = Docker stdio attach (no root);
	// CRI backend = the in-pod UDS via AttachACP (Linux + CAP_SYS_ADMIN).
	Attach(ctx context.Context, h *PodHandle) (*AttachedStream, error)
	// ListManaged returns every spawnery-managed pod the backend currently sees (from its labels), so
	// the Manager can reap orphans on startup and report a running inventory to the CP.
	ListManaged(ctx context.Context) ([]ManagedPod, error)
}
