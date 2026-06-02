package runtime

import (
	"context"
	"fmt"
)

// DockerPodBackend implements PodBackend over the per-container ContainerRuntime (Docker): the
// sidecar owns the pod network namespace and the agent joins it via NetnsOf. This is the runc path.
type DockerPodBackend struct {
	rt          ContainerRuntime
	runtimeName string // OCI runtime smoke-tested by Preflight ("" = default, skip)
	smokeImage  string // image for the preflight smoke container
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
		Image:          spec.Image,
		NetnsOf:        h.SidecarID,
		Env:            spec.Env,
		Mounts:         spec.Mounts,
		AttachStdio:    true,
		MemoryBytes:    spec.Resources.MemoryBytes,
		NanoCPUs:       spec.Resources.NanoCPUs,
		PidsLimit:      spec.Resources.PidsLimit,
		Runtime:        spec.Runtime,
		DropAllCaps:    spec.DropAllCaps,
		ReadonlyRootfs: spec.ReadonlyRootfs,
	})
	if err != nil {
		return fmt.Errorf("agent: %w", err)
	}
	h.AgentID = agentID
	return nil
}

// Attach returns the agent's stdio via Docker's attach API — works on Mac (Docker Desktop) and Linux
// (incl. rootless Docker/Podman) without root, since it rides the Docker API, not setns.
func (d *DockerPodBackend) Attach(ctx context.Context, h *PodHandle) (*AttachedStream, error) {
	return d.rt.Attach(ctx, h.AgentID)
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
