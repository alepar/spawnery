package cri

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

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

	// delta is the engine used by CaptureDelta/ReleaseDelta. Nil until first use (lazy-built from
	// the shared CRI conn) or injected via WithDeltaEngine (tests).
	delta     deltaEngine
	deltaOnce sync.Once
	deltaErr  error
}

// NewCRIPodBackend builds a backend over a Client, running pods under runtimeHandler.
// Optional opts (e.g. WithDeltaEngine) configure the backend; production callers pass none.
func NewCRIPodBackend(c *Client, runtimeHandler string, opts ...Option) *CRIPodBackend {
	b := &CRIPodBackend{
		c:              c,
		runtimeHandler: runtimeHandler,
		sandboxCfgs:    map[string]*runtimeapi.PodSandboxConfig{},
	}
	for _, o := range opts {
		o(b)
	}
	return b
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
	if err != nil && isSandboxNameReserved(err) {
		// A prior teardown left a stale sandbox under this deterministic name (e.g. a suspend whose
		// RemovePodSandbox was cut short by the criStopTimeout while the gofer held a bind mount). The
		// container that held the mount is gone by now, so reap the orphan and retry once so a resume
		// can re-create the pod under the same name.
		b.reapStaleSandbox(ctx, spec.ID)
		sb, err = b.c.runtime.RunPodSandbox(ctx, &runtimeapi.RunPodSandboxRequest{Config: sandboxCfg, RuntimeHandler: b.runtimeHandler})
	}
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
		Labels:   runtime.WithRole(spec.Labels, runtime.RoleSidecar),
		Linux:    linuxContainer(spec.Resources, runtime.CapDefaultSet),
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

// isSandboxNameReserved reports whether a RunPodSandbox error is the CRI "name already taken by a
// still-present sandbox" precondition (a leftover from a prior wedged/incomplete teardown).
func isSandboxNameReserved(err error) bool {
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "reserve sandbox name") ||
		strings.Contains(msg, "is reserved for") ||
		strings.Contains(msg, "already in use") ||
		strings.Contains(msg, "already exists")
}

// reapStaleSandbox best-effort stops+removes any pod sandbox whose metadata name/uid matches name
// (the spawn id, set deterministically in StartPod). Used to clear an orphan a prior wedged teardown
// left behind so a resume can re-create the pod under the same name. Each removal is bounded by
// criStopTimeout so this cannot itself hang the resume.
func (b *CRIPodBackend) reapStaleSandbox(ctx context.Context, name string) {
	resp, err := b.c.runtime.ListPodSandbox(ctx, &runtimeapi.ListPodSandboxRequest{})
	if err != nil {
		return
	}
	for _, sb := range resp.GetItems() {
		md := sb.GetMetadata()
		if md.GetName() != name && md.GetUid() != name {
			continue
		}
		rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), criStopTimeout)
		_, _ = b.c.runtime.StopPodSandbox(rctx, &runtimeapi.StopPodSandboxRequest{PodSandboxId: sb.GetId()})
		_, _ = b.c.runtime.RemovePodSandbox(rctx, &runtimeapi.RemovePodSandboxRequest{PodSandboxId: sb.GetId()})
		cancel()
	}
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

// ContainerEnv returns id's environment, exactly as it was started with — read back via
// ContainerStatus(Verbose) since the CRI API has no dedicated "inspect env" call. Re-adoption uses
// this to recover the per-pod secrets minted by a previous node process that live only in the
// still-running sidecar's env.
func (b *CRIPodBackend) ContainerEnv(ctx context.Context, id string) ([]string, error) {
	resp, err := b.c.runtime.ContainerStatus(ctx, &runtimeapi.ContainerStatusRequest{ContainerId: id, Verbose: true})
	if err != nil {
		return nil, fmt.Errorf("container status %s: %w", id, err)
	}
	return envFromContainerInfo(resp.GetInfo())
}

// envFromContainerInfo extracts the container's env from CRI verbose Info as K=V strings. It tries
// containerd's config.envs shape first (the CRI ContainerConfig.Envs, echoed back verbatim), falling
// back to the OCI runtimeSpec.process.env shape (["K=V"]) — the same "containerd-specific verbose-Info
// contract" caveat netnsPathFromInfo already documents; validated against real containerd by the
// cri_delta_e2e contract arm.
func envFromContainerInfo(info map[string]string) ([]string, error) {
	raw, ok := info["info"]
	if !ok {
		return nil, fmt.Errorf("container status missing verbose info")
	}
	var v struct {
		Config struct {
			Envs []struct {
				Key   string `json:"key"`
				Value string `json:"value"`
			} `json:"envs"`
		} `json:"config"`
		RuntimeSpec struct {
			Process struct {
				Env []string `json:"env"`
			} `json:"process"`
		} `json:"runtimeSpec"`
	}
	if err := json.Unmarshal([]byte(raw), &v); err != nil {
		return nil, fmt.Errorf("parse container info: %w", err)
	}
	if len(v.Config.Envs) > 0 {
		out := make([]string, 0, len(v.Config.Envs))
		for _, e := range v.Config.Envs {
			out = append(out, e.Key+"="+e.Value)
		}
		return out, nil
	}
	return append([]string(nil), v.RuntimeSpec.Process.Env...), nil
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

// linuxContainer maps our Resources + CapPolicy to the CRI LinuxContainerConfig. Pids has no
// dedicated CRI field, so it goes through the cgroup-v2 Unified map ("pids.max").
// ReadonlyRootfs is retired (spec §6 — writable rootfs survival replaces it).
//
// CapPolicy emission:
//   - CapDefaultSet → emit NO SecurityContext capability modifications; the CRI runtime's default
//     capability set applies. Under runsc the sentry virtualizes privilege, so apt/useradd/chown
//     pass without kernel userns (spike 3 result 3).
//   - CapDropAll → emit DropCapabilities=["ALL"]; used when no kernel/sentry isolation is present.
func linuxContainer(res runtime.Resources, policy runtime.CapPolicy) *runtimeapi.LinuxContainerConfig {
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
	switch policy {
	case runtime.CapDropAll:
		lc.SecurityContext = &runtimeapi.LinuxContainerSecurityContext{
			Capabilities: &runtimeapi.Capability{DropCapabilities: []string{"ALL"}},
		}
	default: // CapDefaultSet: emit NO capability modifications — runtime default set.
		// Under runsc the sentry virtualizes privilege (spike 3 result 3).
	}
	return lc
}

// assertNoAddedCaps rejects a security context that requests added capabilities. The agent
// path never sets AddCapabilities; this is a defensive guard mirroring the Docker lane
// (spec §7) — CAP_NET_ADMIN in the shared pod netns lets the agent flush the egress floor
// (spike T5b).
func assertNoAddedCaps(sc *runtimeapi.LinuxContainerSecurityContext) error {
	if sc.GetCapabilities() == nil || len(sc.GetCapabilities().GetAddCapabilities()) == 0 {
		return nil
	}
	return fmt.Errorf("capability add rejected: agent containers must not receive extra "+
		"capabilities (got %v) — granting CAP_NET_ADMIN or similar lets the agent flush the "+
		"egress floor in the shared pod netns (spec §7 floor-defeat guard)",
		sc.GetCapabilities().GetAddCapabilities())
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
	// Map DropAllCaps bool → CapPolicy, mirroring the Docker lane (docker_pod.go).
	capPolicy := runtime.CapDefaultSet
	if spec.DropAllCaps {
		capPolicy = runtime.CapDropAll
	}
	lc := linuxContainer(spec.Resources, capPolicy)
	if err := assertNoAddedCaps(lc.GetSecurityContext()); err != nil {
		return fmt.Errorf("agent: %w", err)
	}
	agentID, err := b.createAndStart(ctx, h.SandboxID, sandboxCfg, &runtimeapi.ContainerConfig{
		Metadata: &runtimeapi.ContainerMetadata{Name: "agent"},
		Image:    &runtimeapi.ImageSpec{Image: spec.Image},
		// CRI Args maps to the image CMD while preserving its ENTRYPOINT. Command would replace the
		// dispatcher entrypoint and attempt to execute the runnable ID as a binary.
		Args: spec.Cmd,
		Envs: toKeyValues(append([]string{
			"ACP_ADAPTER=1",
			fmt.Sprintf("ACP_LISTEN=tcp://0.0.0.0:%d", acpPort),
			"TMUX_TMPDIR=/dev/shm",
		}, spec.Env...)),
		Mounts: toCRIMounts(spec.Mounts),
		Labels: runtime.WithRole(spec.Labels, runtime.RoleAgent),
		Linux:  lc,
	})
	if err != nil {
		return fmt.Errorf("agent: %w", err)
	}
	h.AgentID = agentID
	return nil
}

// criStopTimeout bounds the whole teardown (all StopContainer + StopPodSandbox/RemovePodSandbox
// calls share this one deadline). It must stay under the CP suspend stall window (30s) so a wedged
// stop fails the teardown fast enough for suspend to still complete rather than trip the detector.
const criStopTimeout = 20 * time.Second

// Stop tears down the agent + sidecar, then stops and removes the pod sandbox. Best-effort; empty
// ids are skipped (e.g. agent never started on the fail-closed floor path).
func (b *CRIPodBackend) Stop(ctx context.Context, h *runtime.PodHandle) error {
	// Best-effort teardown, but BOUNDED. WithoutCancel detaches from a possibly-cancelled caller ctx;
	// the timeout then caps a wedged CRI call so teardown can never hang suspend/stop forever. Without
	// this a paused agent task (StopContainer cannot signal a frozen gVisor task) or a bind mount the
	// gofer can't release (StopPodSandbox) blocks indefinitely and trips the CP suspend stall window.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), criStopTimeout)
	defer cancel()
	if h.AgentID != "" {
		// Resume a possibly-paused agent first so StopContainer's signal is delivered (mirrors the
		// resume-before-stop in the delta-capture path).
		if eng, err := b.engine(); err == nil {
			_ = eng.Resume(ctx, h.AgentID)
		}
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

// ListManaged returns every spawnery-managed pod sandbox (by label) WITH the ids and the pod IP a
// restarted node needs to re-adopt it: the sidecar id, the agent id, the sandbox id and the pod IP
// (SE3 design §4.5). Listing sandboxes alone is not enough — the Manager cannot name, capture, pause,
// tear down or re-floor a pod it only knows a sandbox id for.
//
// A CRI API error is returned, NOT swallowed: reconcile treats "cannot rebuild" as reap-with-capture,
// so a transient CRI blip must never be laundered into a destructive decision. An empty pod IP from a
// SUCCESSFUL status (a NOT_READY sandbox) is information, and is reported as such.
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
		mp := runtime.ManagedPod{
			SpawnID:    sid,
			Generation: gen,
			NodeID:     l[runtime.LabelNodeID],
			SandboxID:  sb.GetId(),
		}
		if err := b.fillContainers(ctx, &mp); err != nil {
			return nil, fmt.Errorf("list containers of sandbox %s (spawn %s): %w", mp.SandboxID, sid, err)
		}
		st, err := b.c.runtime.PodSandboxStatus(ctx, &runtimeapi.PodSandboxStatusRequest{PodSandboxId: mp.SandboxID})
		if err != nil {
			return nil, fmt.Errorf("pod sandbox status %s (spawn %s): %w", mp.SandboxID, sid, err)
		}
		mp.PodIP = st.GetStatus().GetNetwork().GetIp()
		out = append(out, mp)
	}
	return out, nil
}

// fillContainers sets mp.SidecarID/AgentID from the sandbox's containers.
func (b *CRIPodBackend) fillContainers(ctx context.Context, mp *runtime.ManagedPod) error {
	resp, err := b.c.runtime.ListContainers(ctx, &runtimeapi.ListContainersRequest{
		Filter: &runtimeapi.ContainerFilter{PodSandboxId: mp.SandboxID},
	})
	if err != nil {
		return err
	}
	var sidecar, agent *runtimeapi.Container
	for _, c := range resp.GetContainers() {
		switch containerRole(c) {
		case runtime.RoleSidecar:
			sidecar = preferred(sidecar, c)
		case runtime.RoleAgent:
			agent = preferred(agent, c)
		}
	}
	if sidecar != nil {
		mp.SidecarID = sidecar.GetId()
	}
	if agent != nil {
		mp.AgentID = agent.GetId()
	}
	return nil
}

// containerRole resolves a CRI container's role from the spawnery.role label, falling back to its CRI
// Metadata.Name. The fallback is load-bearing for re-adoption: the pods that survive a node UPGRADE were
// created by the previous binary, which did not stamp the role label — those are exactly the pods this
// exists to save. StartPod/StartAgent have always named the containers "sidecar"/"agent".
func containerRole(c *runtimeapi.Container) string {
	if r := c.GetLabels()[runtime.LabelRole]; r != "" {
		return r
	}
	return c.GetMetadata().GetName()
}

// preferred picks the container a pod should be ADDRESSED by for a role: a RUNNING one beats a
// non-running one, and between two equally-running ones the newer wins. A sandbox can legitimately hold
// a crashed predecessor containerd has not GC'd — re-adopting THAT id would pause/capture/tear down a
// dead container and leave the live one unmanaged.
func preferred(cur, cand *runtimeapi.Container) *runtimeapi.Container {
	if cur == nil {
		return cand
	}
	curRunning := cur.GetState() == runtimeapi.ContainerState_CONTAINER_RUNNING
	candRunning := cand.GetState() == runtimeapi.ContainerState_CONTAINER_RUNNING
	if curRunning != candRunning {
		if candRunning {
			return cand
		}
		return cur
	}
	if cand.GetCreatedAt() > cur.GetCreatedAt() {
		return cand
	}
	return cur
}

var _ runtime.PodBackend = (*CRIPodBackend)(nil)
