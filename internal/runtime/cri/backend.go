package cri

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"strconv"
	"strings"
	"sync"

	"spawnery/internal/runtime"

	runtimeapi "k8s.io/cri-api/pkg/apis/runtime/v1"
)

// acpPort is the TCP port the agent's acpadapter listens on (on the pod IP) for the CRI/runsc lane.
// The node dials podIP:acpPort because gVisor isolates the in-sandbox abstract-UDS namespace from the
// host (so the runc-lane setns+UDS attach cannot work under runsc).
const acpPort = runtime.ACPPort // single source of truth

// CRIPodBackend runs a spawn pod as one CRI sandbox (handler=runsc) with two containers
// (sidecar + agent) sharing the pod network namespace. Implements runtime.PodBackend.
type CRIPodBackend struct {
	c              *Client
	runtimeHandler string // e.g. "runsc"

	// DNSServers, if set, become the pod sandbox's resolv.conf nameservers. Without a kubelet the CRI
	// pod otherwise inherits the host's /etc/resolv.conf, which on a systemd-resolved host is the
	// 127.0.0.53 stub — unreachable from inside the pod, so the sidecar can't resolve the model
	// upstream. The egress floor's :53 carve-out allows reaching an RFC1918 resolver here.
	DNSServers []string

	mu          sync.Mutex
	sandboxCfgs map[string]*runtimeapi.PodSandboxConfig // sandboxID -> config (CreateContainer needs it)
}

// NewCRIPodBackend builds a backend over a Client, running pods under runtimeHandler.
func NewCRIPodBackend(c *Client, runtimeHandler string) *CRIPodBackend {
	return &CRIPodBackend{c: c, runtimeHandler: runtimeHandler, sandboxCfgs: map[string]*runtimeapi.PodSandboxConfig{}}
}

// Ping checks the CRI runtime is reachable.
func (b *CRIPodBackend) Ping(ctx context.Context) error {
	_, err := b.c.runtime.Status(ctx, &runtimeapi.StatusRequest{})
	return err
}

// Preflight asserts the runtime + network are ready (caught at startup, not first spawn).
func (b *CRIPodBackend) Preflight(ctx context.Context) error {
	resp, err := b.c.runtime.Status(ctx, &runtimeapi.StatusRequest{})
	if err != nil {
		return fmt.Errorf("cri status: %w", err)
	}
	for _, cond := range resp.GetStatus().GetConditions() {
		if (cond.Type == "RuntimeReady" || cond.Type == "NetworkReady") && !cond.Status {
			return fmt.Errorf("cri not ready: %s (%s)", cond.Type, cond.Reason)
		}
	}
	return nil
}

// StartPod runs the pod sandbox and starts the (trusted) sidecar, returning a handle with the pod IP
// (for the egress floor) and netns path (for the ACP attach). The agent is not started yet.
func (b *CRIPodBackend) StartPod(ctx context.Context, spec runtime.PodSpec) (*runtime.PodHandle, error) {
	sandboxCfg := &runtimeapi.PodSandboxConfig{
		Metadata: &runtimeapi.PodSandboxMetadata{Name: spec.ID, Uid: spec.ID, Namespace: "spawnery"},
		Linux:    &runtimeapi.LinuxPodSandboxConfig{},
		Labels:   spec.Labels, // spawnery.managed/spawn-id/generation/node-id — drives ListManaged + reconcile
	}
	if len(b.DNSServers) > 0 {
		sandboxCfg.DnsConfig = &runtimeapi.DNSConfig{Servers: b.DNSServers}
	}
	sb, err := b.c.runtime.RunPodSandbox(ctx, &runtimeapi.RunPodSandboxRequest{Config: sandboxCfg, RuntimeHandler: b.runtimeHandler})
	if err != nil {
		return nil, fmt.Errorf("run pod sandbox: %w", err)
	}
	sandboxID := sb.PodSandboxId
	cleanup := func() { b.removeSandbox(context.WithoutCancel(ctx), sandboxID) }

	if err := b.pullImage(ctx, spec.SidecarImage); err != nil {
		cleanup()
		return nil, err
	}
	sidecarID, err := b.createAndStart(ctx, sandboxID, sandboxCfg, &runtimeapi.ContainerConfig{
		Metadata: &runtimeapi.ContainerMetadata{Name: "sidecar"},
		Image:    &runtimeapi.ImageSpec{Image: spec.SidecarImage},
		Envs:     toKeyValues(spec.SidecarEnv),
		Labels:   spec.Labels,
		Linux:    linuxContainer(spec.Resources, false, false),
	})
	if err != nil {
		cleanup()
		return nil, fmt.Errorf("sidecar: %w", err)
	}

	st, err := b.c.runtime.PodSandboxStatus(ctx, &runtimeapi.PodSandboxStatusRequest{PodSandboxId: sandboxID, Verbose: true})
	if err != nil {
		cleanup()
		return nil, fmt.Errorf("pod sandbox status: %w", err)
	}
	ip := st.GetStatus().GetNetwork().GetIp()
	if ip == "" {
		cleanup()
		return nil, fmt.Errorf("pod sandbox %s has no IP", sandboxID)
	}
	netns, err := netnsPathFromInfo(st.Info)
	if err != nil {
		cleanup()
		return nil, err
	}

	b.mu.Lock()
	b.sandboxCfgs[sandboxID] = sandboxCfg
	b.mu.Unlock()
	return &runtime.PodHandle{PodIP: ip, NetnsPath: netns, SidecarID: sidecarID, SandboxID: sandboxID}, nil
}

func (b *CRIPodBackend) createAndStart(ctx context.Context, sandboxID string, sandboxCfg *runtimeapi.PodSandboxConfig, cfg *runtimeapi.ContainerConfig) (string, error) {
	cr, err := b.c.runtime.CreateContainer(ctx, &runtimeapi.CreateContainerRequest{PodSandboxId: sandboxID, Config: cfg, SandboxConfig: sandboxCfg})
	if err != nil {
		return "", fmt.Errorf("create container: %w", err)
	}
	if _, err := b.c.runtime.StartContainer(ctx, &runtimeapi.StartContainerRequest{ContainerId: cr.ContainerId}); err != nil {
		return "", fmt.Errorf("start container: %w", err)
	}
	return cr.ContainerId, nil
}

// pullImage pulls the image if not already present in the CRI (k8s.io) image store.
func (b *CRIPodBackend) pullImage(ctx context.Context, image string) error {
	spec := &runtimeapi.ImageSpec{Image: image}
	if st, err := b.c.image.ImageStatus(ctx, &runtimeapi.ImageStatusRequest{Image: spec}); err == nil && st.GetImage() != nil {
		return nil
	}
	if _, err := b.c.image.PullImage(ctx, &runtimeapi.PullImageRequest{Image: spec}); err != nil {
		return fmt.Errorf("pull image %s: %w", image, err)
	}
	return nil
}

func (b *CRIPodBackend) removeSandbox(ctx context.Context, sandboxID string) {
	_, _ = b.c.runtime.StopPodSandbox(ctx, &runtimeapi.StopPodSandboxRequest{PodSandboxId: sandboxID})
	_, _ = b.c.runtime.RemovePodSandbox(ctx, &runtimeapi.RemovePodSandboxRequest{PodSandboxId: sandboxID})
	b.mu.Lock()
	delete(b.sandboxCfgs, sandboxID)
	b.mu.Unlock()
}

// netnsPathFromInfo extracts the sandbox pid from CRI verbose Info and returns its net ns path.
// NOTE: the Info["info"] = {"pid":N} shape is a containerd-specific contract (the CRI verbose Info
// map is not standardized by the proto); validated against real containerd in slice 5 (sp-ghx).
func netnsPathFromInfo(info map[string]string) (string, error) {
	raw, ok := info["info"]
	if !ok {
		return "", fmt.Errorf("pod sandbox status missing verbose info")
	}
	var v struct {
		Pid int `json:"pid"`
	}
	if err := json.Unmarshal([]byte(raw), &v); err != nil {
		return "", fmt.Errorf("parse sandbox info: %w", err)
	}
	if v.Pid == 0 {
		return "", fmt.Errorf("pod sandbox info has no pid")
	}
	return fmt.Sprintf("/proc/%d/ns/net", v.Pid), nil
}

func toKeyValues(env []string) []*runtimeapi.KeyValue {
	out := make([]*runtimeapi.KeyValue, 0, len(env))
	for _, e := range env {
		k, v, _ := strings.Cut(e, "=")
		out = append(out, &runtimeapi.KeyValue{Key: k, Value: v})
	}
	return out
}

func toCRIMounts(ms []runtime.Mount) []*runtimeapi.Mount {
	out := make([]*runtimeapi.Mount, 0, len(ms))
	for _, m := range ms {
		out = append(out, &runtimeapi.Mount{ContainerPath: m.ContainerPath, HostPath: m.HostPath, Readonly: m.ReadOnly})
	}
	return out
}

// linuxContainer maps our Resources + hardening flags to the CRI LinuxContainerConfig. Pids has no
// dedicated CRI field, so it goes through the cgroup-v2 Unified map ("pids.max").
func linuxContainer(res runtime.Resources, dropCaps, roRootfs bool) *runtimeapi.LinuxContainerConfig {
	r := &runtimeapi.LinuxContainerResources{}
	if res.MemoryBytes > 0 {
		r.MemoryLimitInBytes = res.MemoryBytes
	}
	if res.NanoCPUs > 0 {
		const period = 100000 // 100ms, in microseconds
		r.CpuPeriod = period
		r.CpuQuota = res.NanoCPUs * period / 1_000_000_000
	}
	if res.PidsLimit > 0 {
		r.Unified = map[string]string{"pids.max": strconv.FormatInt(res.PidsLimit, 10)}
	}
	lc := &runtimeapi.LinuxContainerConfig{Resources: r}
	if dropCaps || roRootfs {
		lc.SecurityContext = &runtimeapi.LinuxContainerSecurityContext{}
		if dropCaps {
			lc.SecurityContext.Capabilities = &runtimeapi.Capability{DropCapabilities: []string{"ALL"}}
		}
		if roRootfs {
			lc.SecurityContext.ReadonlyRootfs = true
		}
	}
	return lc
}

// StartAgent starts the (untrusted) agent container in the existing pod sandbox.
func (b *CRIPodBackend) StartAgent(ctx context.Context, h *runtime.PodHandle, spec runtime.AgentSpec) error {
	b.mu.Lock()
	sandboxCfg := b.sandboxCfgs[h.SandboxID]
	b.mu.Unlock()
	if sandboxCfg == nil {
		return fmt.Errorf("unknown sandbox %s", h.SandboxID)
	}
	if err := b.pullImage(ctx, spec.Image); err != nil {
		return err
	}
	agentID, err := b.createAndStart(ctx, h.SandboxID, sandboxCfg, &runtimeapi.ContainerConfig{
		Metadata: &runtimeapi.ContainerMetadata{Name: "agent"},
		Image:    &runtimeapi.ImageSpec{Image: spec.Image},
		Command:  spec.Cmd,
		Envs:     toKeyValues(append([]string{"ACP_ADAPTER=1", fmt.Sprintf("ACP_LISTEN=tcp://0.0.0.0:%d", acpPort)}, spec.Env...)),
		Mounts:   toCRIMounts(spec.Mounts),
		Labels:   spec.Labels,
		Linux:    linuxContainer(spec.Resources, spec.DropAllCaps, spec.ReadonlyRootfs),
	})
	if err != nil {
		return fmt.Errorf("agent: %w", err)
	}
	h.AgentID = agentID
	return nil
}

// Stop tears down the agent + sidecar, then stops and removes the pod sandbox. Best-effort; empty
// ids are skipped (e.g. agent never started on the fail-closed floor path).
func (b *CRIPodBackend) Stop(ctx context.Context, h *runtime.PodHandle) error {
	ctx = context.WithoutCancel(ctx)
	if h.AgentID != "" {
		_, _ = b.c.runtime.StopContainer(ctx, &runtimeapi.StopContainerRequest{ContainerId: h.AgentID})
	}
	if h.SidecarID != "" {
		_, _ = b.c.runtime.StopContainer(ctx, &runtimeapi.StopContainerRequest{ContainerId: h.SidecarID})
	}
	if h.SandboxID != "" {
		b.removeSandbox(ctx, h.SandboxID)
	}
	return nil
}

// Attach returns the agent's ACP stdio over TCP on the pod IP. Under gVisor/runsc the in-sandbox
// abstract-UDS namespace is isolated from the host, so the runc-lane setns+UDS attach cannot reach
// the adapter; the adapter listens on tcp://0.0.0.0:acpPort and the node dials the pod IP (reachable
// via the CNI bridge; other pods are blocked by the SPAWNLET-EGRESS floor's RFC1918 drop).
func (b *CRIPodBackend) Attach(ctx context.Context, h *runtime.PodHandle) (*runtime.AttachedStream, error) {
	if h.PodIP == "" {
		return nil, fmt.Errorf("cri attach: pod has no IP")
	}
	return runtime.AttachTCP(ctx, net.JoinHostPort(h.PodIP, strconv.Itoa(acpPort)))
}

// ListManaged returns every spawnery-managed pod sandbox (by label), so the Manager can reap
// orphans and reconcile. Reaping a CRI pod is RemovePodSandbox(SandboxID) (removes its containers).
func (b *CRIPodBackend) ListManaged(ctx context.Context) ([]runtime.ManagedPod, error) {
	resp, err := b.c.runtime.ListPodSandbox(ctx, &runtimeapi.ListPodSandboxRequest{
		Filter: &runtimeapi.PodSandboxFilter{LabelSelector: map[string]string{runtime.LabelManaged: "true"}},
	})
	if err != nil {
		return nil, err
	}
	out := make([]runtime.ManagedPod, 0, len(resp.GetItems()))
	for _, sb := range resp.GetItems() {
		l := sb.GetLabels()
		sid := l[runtime.LabelSpawnID]
		if sid == "" {
			continue
		}
		gen, _ := strconv.ParseUint(l[runtime.LabelGeneration], 10, 64)
		out = append(out, runtime.ManagedPod{SpawnID: sid, Generation: gen, NodeID: l[runtime.LabelNodeID], SandboxID: sb.GetId()})
	}
	return out, nil
}

var _ runtime.PodBackend = (*CRIPodBackend)(nil)
