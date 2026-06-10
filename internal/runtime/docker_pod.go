package runtime

import (
	"context"
	"fmt"
	"net"
	"strconv"
)

// withRole returns a copy of base with the spawnery.role label set (so a Docker pod's sidecar +
// agent are distinguishable when reconciling). nil base => a labels map with just the role.
func withRole(base map[string]string, role string) map[string]string {
	out := make(map[string]string, len(base)+1)
	for k, v := range base {
		out[k] = v
	}
	out[LabelRole] = role
	return out
}

// DockerPodBackend implements PodBackend over the per-container ContainerRuntime (Docker): the
// sidecar owns the pod network namespace and the agent joins it via NetnsOf. This is the runc path.
type DockerPodBackend struct {
	rt          ContainerRuntime
	runtimeName string // OCI runtime smoke-tested by Preflight ("" = default, skip)
	smokeImage  string // image for the preflight smoke container
	acpPort     int    // 0 => ACPPort; overridable in tests
}

// port returns the ACP TCP port the adapter listens on (ACPPort unless overridden).
func (d *DockerPodBackend) port() int {
	if d.acpPort != 0 {
		return d.acpPort
	}
	return ACPPort
}

// NewDockerPodBackend wraps a ContainerRuntime. runtimeName + smokeImage drive Preflight.
func NewDockerPodBackend(rt ContainerRuntime, runtimeName, smokeImage string) *DockerPodBackend {
	return &DockerPodBackend{rt: rt, runtimeName: runtimeName, smokeImage: smokeImage}
}

func (d *DockerPodBackend) Ping(ctx context.Context) error { return d.rt.Ping(ctx) }

// Preflight smoke-runs `true` under the configured runtime so a misconfigured runsc is caught at
// startup, not at first spawn. No-op when no non-default runtime is configured.
func (d *DockerPodBackend) Preflight(ctx context.Context) error {
	if d.runtimeName == "" {
		return nil
	}
	id, err := d.rt.StartContainer(ctx, ContainerSpec{
		Image:   d.smokeImage,
		Cmd:     []string{"true"},
		Runtime: d.runtimeName,
	})
	if err != nil {
		return fmt.Errorf("runtime %q preflight: %w", d.runtimeName, err)
	}
	_ = d.rt.StopContainer(context.WithoutCancel(ctx), id)
	return nil
}

// StartPod starts the sidecar (which owns the pod netns) and returns a handle carrying the pod IP
// (for the floor) and netns path (for the ACP attach). The agent is not started yet.
func (d *DockerPodBackend) StartPod(ctx context.Context, spec PodSpec) (*PodHandle, error) {
	sidecarID, err := d.rt.StartContainer(ctx, ContainerSpec{
		Image:       spec.SidecarImage,
		Env:         spec.SidecarEnv,
		MemoryBytes: spec.Resources.MemoryBytes,
		NanoCPUs:    spec.Resources.NanoCPUs,
		PidsLimit:   spec.Resources.PidsLimit,
		Runtime:     spec.Runtime,
		Labels:      withRole(spec.Labels, "sidecar"),
	})
	if err != nil {
		return nil, fmt.Errorf("sidecar: %w", err)
	}
	// Best-effort: rootless Podman (slirp4netns/pasta) has no bridge IP, and the Docker lane attaches
	// via the Docker API (not setns), so a missing IP/PID is not fatal here. The Manager fail-closes
	// later only if the egress floor is enforced and there's no IP to scope it.
	ip, _ := d.rt.ContainerIP(ctx, sidecarID)
	var netnsPath string
	if pid, perr := d.rt.ContainerPID(ctx, sidecarID); perr == nil {
		netnsPath = fmt.Sprintf("/proc/%d/ns/net", pid)
	}
	return &PodHandle{
		PodIP:     ip,
		NetnsPath: netnsPath,
		SidecarID: sidecarID,
	}, nil
}

// StartAgent starts the agent container in the sidecar's netns and records its id on the handle.
func (d *DockerPodBackend) StartAgent(ctx context.Context, h *PodHandle, spec AgentSpec) error {
	agentID, err := d.rt.StartContainer(ctx, ContainerSpec{
		Image:   spec.Image,
		Cmd:     spec.Cmd,
		NetnsOf: h.SidecarID,
		// The adapter listens on TCP for the node (both lanes now); no stdio ACP channel.
		Env:            append([]string{fmt.Sprintf("ACP_LISTEN=tcp://0.0.0.0:%d", d.port())}, spec.Env...),
		Mounts:         spec.Mounts,
		AttachStdio:    false,
		MemoryBytes:    spec.Resources.MemoryBytes,
		NanoCPUs:       spec.Resources.NanoCPUs,
		PidsLimit:      spec.Resources.PidsLimit,
		Runtime:        spec.Runtime,
		DropAllCaps:    spec.DropAllCaps,
		ReadonlyRootfs: spec.ReadonlyRootfs,
		Labels:         withRole(spec.Labels, "agent"),
	})
	if err != nil {
		return fmt.Errorf("agent: %w", err)
	}
	h.AgentID = agentID
	return nil
}

// Attach dials the agent's ACP adapter over TCP on the pod IP. The agent shares the sidecar's netns,
// so the sidecar/pod bridge IP reaches the adapter's port. Unified with the CRI lane; the old
// Docker-API stdio hijack is gone because the opencode adapter has no stdio ACP channel.
//
// NOTE: requires the pod to have a bridge IP. Rootful Docker and the CRI/CNI lane provide one;
// rootless Podman with slirp4netns/pasta may not, in which case the node cannot reach the adapter
// without published-port plumbing (a follow-up if rootless-without-bridge must be supported).
func (d *DockerPodBackend) Attach(ctx context.Context, h *PodHandle) (*AttachedStream, error) {
	if h.PodIP == "" {
		return nil, fmt.Errorf("docker attach: pod has no IP (rootless-without-bridge is unsupported for TCP ACP)")
	}
	return AttachTCP(ctx, net.JoinHostPort(h.PodIP, strconv.Itoa(d.port())))
}

// ListManaged groups all spawnery-managed containers by spawn id into ManagedPods (sidecar + agent
// by role label), reading the generation/node-id off the labels.
func (d *DockerPodBackend) ListManaged(ctx context.Context) ([]ManagedPod, error) {
	cs, err := d.rt.ListByLabel(ctx, LabelManaged, "true")
	if err != nil {
		return nil, err
	}
	pods := map[string]*ManagedPod{}
	for _, c := range cs {
		sid := c.Labels[LabelSpawnID]
		if sid == "" {
			continue
		}
		p := pods[sid]
		if p == nil {
			gen, _ := strconv.ParseUint(c.Labels[LabelGeneration], 10, 64)
			p = &ManagedPod{SpawnID: sid, Generation: gen, NodeID: c.Labels[LabelNodeID]}
			pods[sid] = p
		}
		if c.Labels[LabelRole] == "agent" {
			p.AgentID = c.ID
		} else {
			p.SidecarID = c.ID
		}
	}
	out := make([]ManagedPod, 0, len(pods))
	for _, p := range pods {
		out = append(out, *p)
	}
	return out, nil
}

// Stop tears down the agent then the sidecar. Empty ids (e.g. agent not yet started on the
// fail-closed floor path) are skipped.
func (d *DockerPodBackend) Stop(ctx context.Context, h *PodHandle) error {
	ctx = context.WithoutCancel(ctx)
	if h.AgentID != "" {
		_ = d.rt.StopContainer(ctx, h.AgentID)
	}
	if h.SidecarID != "" {
		_ = d.rt.StopContainer(ctx, h.SidecarID)
	}
	return nil
}
