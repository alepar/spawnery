package runtime

import "context"

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
	Runtime      string // OCI runtime; "" = default, e.g. "runsc"
}

// AgentSpec describes the agent container (started by StartAgent into the existing pod).
type AgentSpec struct {
	Image          string
	Env            []string
	Mounts         []Mount
	Resources      Resources
	Runtime        string
	DropAllCaps    bool
	ReadonlyRootfs bool
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
}
