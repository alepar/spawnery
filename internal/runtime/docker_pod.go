package runtime

import (
	"context"
	"fmt"
	"io"
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
		Mounts:      spec.SidecarMounts,
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
	// Map the AgentSpec bool to the Docker-lane CapPolicy.
	// AgentSpec.DropAllCaps is the shared interface field (also consumed by the CRI backend);
	// ContainerSpec.CapPolicy is Docker-lane-only and drives buildHostConfig.
	capPolicy := CapDefaultSet
	if spec.DropAllCaps {
		capPolicy = CapDropAll
	}
	agentID, err := d.rt.StartContainer(ctx, ContainerSpec{
		Image:   spec.Image,
		Cmd:     spec.Cmd,
		NetnsOf: h.SidecarID,
		// The adapter listens on TCP for the node (both lanes now); no stdio ACP channel.
		// TMUX_TMPDIR=/dev/shm lets tmux-mode agents recover even from legacy delta images
		// whose scrubbed rootfs is missing /tmp; Docker/CRI provide /dev/shm as a writable tmpfs.
		Env: append([]string{
			fmt.Sprintf("ACP_LISTEN=tcp://0.0.0.0:%d", d.port()),
			"TMUX_TMPDIR=/dev/shm",
		}, spec.Env...),
		Mounts:      spec.Mounts,
		AttachStdio: false,
		MemoryBytes: spec.Resources.MemoryBytes,
		NanoCPUs:    spec.Resources.NanoCPUs,
		PidsLimit:   spec.Resources.PidsLimit,
		Runtime:     spec.Runtime,
		CapPolicy:   capPolicy,
		Labels:      withRole(spec.Labels, "agent"),
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

// DeltaTag returns the local Docker image tag for a spawn's delta image ("spawnery/delta:<id>").
// This is the single source of truth for the tag format — both the backend and Manager use this.
func DeltaTag(spawnID string) string { return "spawnery/delta:" + spawnID }

// ResolveImageDigest returns the content-addressable digest of ref: RepoDigests[0] when present,
// fallback to the image Id. Used by Manager.Create to pin the base image (spec §4).
func (d *DockerPodBackend) ResolveImageDigest(ctx context.Context, ref string) (string, error) {
	info, ok, err := d.rt.InspectImage(ctx, ref)
	if err != nil {
		return "", fmt.Errorf("inspect %q: %w", ref, err)
	}
	if !ok {
		return "", fmt.Errorf("image %q not found", ref)
	}
	if len(info.RepoDigests) > 0 {
		return info.RepoDigests[0], nil
	}
	return info.ID, nil
}

// EnsureImage returns the image ref to launch the agent from. If deltaRef is non-empty and
// present locally it is returned (resume from delta); otherwise baseRef is returned (fresh create
// or delta not yet available). Base image pull is a stage-2 concern; dev lane has the base locally.
func (d *DockerPodBackend) EnsureImage(ctx context.Context, baseRef, deltaRef string) (string, error) {
	if deltaRef != "" {
		if _, ok, err := d.rt.InspectImage(ctx, deltaRef); err == nil && ok {
			return deltaRef, nil
		}
	}
	return baseRef, nil
}

// CaptureDelta stops+commits the agent container to "spawnery/delta:<h.SpawnID>", validates the
// committed image has more layers than the base (moby#47065 zero-layer guard), and returns the
// delta tag. The container is left stopped (not removed); the normal pod Stop path removes it.
func (d *DockerPodBackend) CaptureDelta(ctx context.Context, h *PodHandle) (string, error) {
	return d.captureDelta(ctx, h, h.SpawnID, false)
}

// CaptureDeltaAs commits h's source agent container to the delta tag for targetSpawnID without
// stopping or removing the source container. Callers that need source liveness pause/unpause around
// this method.
func (d *DockerPodBackend) CaptureDeltaAs(ctx context.Context, h *PodHandle, targetSpawnID string) (string, error) {
	return d.captureDelta(ctx, h, targetSpawnID, true)
}

func (d *DockerPodBackend) captureDelta(ctx context.Context, h *PodHandle, targetSpawnID string, preserveSource bool) (string, error) {
	tag := DeltaTag(targetSpawnID)
	// Derive base layer count for the moby#47065 guard.
	baseLayers := 0
	if h.BaseImageRef != "" {
		if bi, ok, err := d.rt.InspectImage(ctx, h.BaseImageRef); err == nil && ok {
			baseLayers = bi.Layers
		}
	}

	var err error
	if preserveSource {
		_, err = d.rt.CommitContainerPreserving(ctx, h.AgentID, tag)
	} else {
		_, err = d.rt.CommitContainer(ctx, h.AgentID, tag)
	}
	if err != nil {
		return "", fmt.Errorf("commit delta for %s as %s: %w", h.SpawnID, targetSpawnID, err)
	}
	ni, ok, err := d.rt.InspectImage(ctx, tag)
	if err != nil || !ok {
		return "", fmt.Errorf("inspect committed delta %s: %w", tag, err)
	}
	if ni.Layers <= baseLayers {
		return "", fmt.Errorf("delta capture for %s produced %d layers <= base %d "+
			"(moby#47065 zero-layer guard)", targetSpawnID, ni.Layers, baseLayers)
	}
	return tag, nil
}

// ReleaseDelta removes the per-spawn delta tag (GC hook). Task .12 wires the callers.
func (d *DockerPodBackend) ReleaseDelta(ctx context.Context, spawnID string) error {
	return d.rt.RemoveImage(ctx, DeltaTag(spawnID))
}

func (d *DockerPodBackend) ExportDelta(ctx context.Context, spawnID string, w io.Writer) error {
	tag := DeltaTag(spawnID)
	if _, ok, err := d.rt.InspectImage(ctx, tag); err != nil {
		return fmt.Errorf("inspect delta %s: %w", tag, err)
	} else if !ok {
		return fmt.Errorf("delta image %s not found", tag)
	}
	if err := d.rt.ExportTopLayer(ctx, tag, w); err != nil {
		return fmt.Errorf("export delta %s: %w", tag, err)
	}
	return nil
}

// Pause pauses the AGENT container (quiesces agent writes before the final snapshot, spec §3).
// Empty AgentID is a caller bug, not a best-effort teardown case — returns an error.
func (d *DockerPodBackend) Pause(ctx context.Context, h *PodHandle) error {
	if h.AgentID == "" {
		return fmt.Errorf("docker pause: no agent container id")
	}
	return d.rt.PauseContainer(ctx, h.AgentID)
}

// Unpause resumes a previously-paused agent container.
func (d *DockerPodBackend) Unpause(ctx context.Context, h *PodHandle) error {
	if h.AgentID == "" {
		return fmt.Errorf("docker unpause: no agent container id")
	}
	return d.rt.UnpauseContainer(ctx, h.AgentID)
}

var _ PodBackend = (*DockerPodBackend)(nil)

func (d *DockerPodBackend) ImportDelta(ctx context.Context, spawnID, baseRef string, r io.Reader) (string, error) {
	tag := DeltaTag(spawnID)
	// Reassemble base + the shipped single delta layer into the deterministic delta tag. The base
	// must already be present locally (CP-pinned by digest); AssembleOnBase reads it, appends the
	// layer, and writes the tag back via the daemon — no full-image archive crosses the wire.
	if err := d.rt.AssembleOnBase(ctx, baseRef, tag, r); err != nil {
		return "", fmt.Errorf("assemble delta %s on base %s: %w", tag, baseRef, err)
	}
	return tag, nil
}
