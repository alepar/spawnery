package spawnlet

import (
	"context"
	"fmt"
	"log"
	"path/filepath"

	"spawnery/internal/manifest"
	"spawnery/internal/runtime"
	"spawnery/internal/spawnlet/firewall"
	"spawnery/internal/storage"
)

type ManagerConfig struct {
	AgentImage, SidecarImage, OpenRouterKey, DataRoot string
	SidecarPort                                       int // default 8080

	NodeClass        string // "cloud" (always enforces) or "self-hosted" (honors EgressEnforce)
	EgressEnforce    bool   // self-hosted opt-out switch; ignored on cloud
	EgressAllowCIDRs []string

	MemLimitMB       int64   // memory limit in MiB; default 1024
	CPULimit         float64 // CPU cores; default 1.0
	PidsLimit        int64   // max pids per container; default 256
	ContainerRuntime string  // OCI runtime name; "" = Docker default
	HardenRootfs     bool    // if true, run agent with read-only rootfs + /tmp tmpfs
}

type Manager struct {
	pod     runtime.PodBackend
	cfg     ManagerConfig
	store   *Store
	backend storage.Backend
	fw      firewall.Applier
}

// NewManager builds a Manager on the Docker/runc path: the Docker pod backend + the DOCKER-USER
// egress floor. (cmd/spawnlet uses NewManagerWithBackend for the runsc/CRI path.)
func NewManager(rt runtime.ContainerRuntime, cfg ManagerConfig) *Manager {
	return NewManagerWithBackend(
		runtime.NewDockerPodBackend(rt, cfg.ContainerRuntime, cfg.AgentImage),
		firewall.HostFloorApplier{},
		cfg,
	)
}

// NewManagerWithBackend builds a Manager around an explicit pod backend + egress-floor applier,
// applying config defaults. Used to select the runc (Docker + DOCKER-USER) vs runsc (CRI +
// SPAWNLET-EGRESS) stacks by CONTAINER_RUNTIME.
func NewManagerWithBackend(pod runtime.PodBackend, fw firewall.Applier, cfg ManagerConfig) *Manager {
	if cfg.SidecarPort == 0 {
		cfg.SidecarPort = 8080
	}
	if cfg.MemLimitMB == 0 {
		cfg.MemLimitMB = 1024
	}
	if cfg.CPULimit == 0 {
		cfg.CPULimit = 1.0
	}
	if cfg.PidsLimit == 0 {
		cfg.PidsLimit = 256
	}
	return &Manager{
		pod:     pod,
		cfg:     cfg,
		store:   NewStore(),
		backend: storage.NewScratch(cfg.DataRoot),
		fw:      fw,
	}
}

// egressEnforced reports whether the egress floor must be applied: cloud nodes always enforce
// (non-disableable); self-hosted honors the operator's EgressEnforce choice.
func (m *Manager) egressEnforced() bool {
	return m.cfg.NodeClass == "cloud" || m.cfg.EgressEnforce
}

func (m *Manager) Store() *Store { return m.store }

// Attach returns the agent's ACP stdio for a spawn, dispatching to the backend's transport (Docker
// stdio attach for the Docker lane, the in-pod UDS for the CRI lane).
func (m *Manager) Attach(ctx context.Context, sp *Spawn) (*runtime.AttachedStream, error) {
	return m.pod.Attach(ctx, &runtime.PodHandle{
		AgentID:   sp.AgentID,
		NetnsPath: sp.NetnsPath,
		SidecarID: sp.SidecarID,
		SandboxID: sp.SandboxID,
	})
}

func (m *Manager) Create(ctx context.Context, id, appPath, model string) (*Spawn, error) {
	if abs, err := filepath.Abs(appPath); err == nil {
		appPath = abs
	}
	mf, err := manifest.Parse(appPath)
	if err != nil {
		return nil, fmt.Errorf("manifest: %w", err)
	}

	// /app is read-only; each declared mount is a rw overlay at /app/<path>,
	// backed (slice: scratch) by a host dir seeded from /app/<seed>.
	mounts := []runtime.Mount{{HostPath: appPath, ContainerPath: "/app", ReadOnly: true}}
	var mountDirs []string
	finalizeAll := func() {
		for _, d := range mountDirs {
			_ = m.backend.Finalize(ctx, d)
		}
	}
	for _, mt := range mf.Storage.Mounts {
		seedDir := filepath.Join(appPath, mt.Seed)
		hostDir, err := m.backend.Prepare(ctx, id, mt.Name, seedDir)
		if err != nil {
			finalizeAll()
			return nil, fmt.Errorf("prepare mount %q: %w", mt.Name, err)
		}
		mountDirs = append(mountDirs, hostDir)
		mounts = append(mounts, runtime.Mount{HostPath: hostDir, ContainerPath: "/app/" + mt.Path})
	}

	res := runtime.Resources{
		MemoryBytes: m.cfg.MemLimitMB << 20,
		NanoCPUs:    int64(m.cfg.CPULimit * 1e9),
		PidsLimit:   m.cfg.PidsLimit,
	}
	addr := fmt.Sprintf("127.0.0.1:%d", m.cfg.SidecarPort)

	// Phase 1: sandbox + sidecar (the trusted, key-holding container).
	h, err := m.pod.StartPod(ctx, runtime.PodSpec{
		ID:           id,
		SidecarImage: m.cfg.SidecarImage,
		SidecarEnv: []string{
			"OPENROUTER_API_KEY=" + m.cfg.OpenRouterKey,
			"SIDECAR_ADDR=" + addr,
		},
		Resources: res,
		Runtime:   m.cfg.ContainerRuntime,
	})
	if err != nil {
		finalizeAll()
		return nil, err
	}

	// Egress floor: applied after the pod IP exists, before the untrusted agent starts (fail-closed).
	var floorIP string
	if m.egressEnforced() {
		if ferr := m.fw.Apply(ctx, firewall.Rules(h.PodIP, m.cfg.EgressAllowCIDRs)); ferr != nil {
			_ = m.pod.Stop(ctx, h)
			finalizeAll()
			return nil, fmt.Errorf("egress floor (fail-closed): %w", ferr)
		}
		floorIP = h.PodIP
	}

	// Phase 2: the untrusted agent, into the existing pod.
	if err := m.pod.StartAgent(ctx, h, runtime.AgentSpec{
		Image: m.cfg.AgentImage,
		Env: []string{
			"OPENAI_BASE_URL=http://" + addr + "/v1",
			"SPAWN_MODEL=" + model,
		},
		Mounts:         mounts,
		Resources:      res,
		Runtime:        m.cfg.ContainerRuntime,
		DropAllCaps:    true,
		ReadonlyRootfs: m.cfg.HardenRootfs,
	}); err != nil {
		_ = m.pod.Stop(ctx, h)
		finalizeAll()
		return nil, err
	}

	sp := &Spawn{ID: id, SidecarID: h.SidecarID, AgentID: h.AgentID, MountDirs: mountDirs, FloorIP: floorIP, NetnsPath: h.NetnsPath, SandboxID: h.SandboxID, Status: "ready"}
	m.store.Put(sp)
	return sp, nil
}

// PreflightRuntime validates a configured non-default container runtime at startup (delegates to the
// backend's smoke check). Callers should fail hard rather than discover a broken runtime at first spawn.
func (m *Manager) PreflightRuntime(ctx context.Context) error {
	return m.pod.Preflight(ctx)
}

func (m *Manager) Stop(ctx context.Context, id string) error {
	sp, ok := m.store.Get(id)
	if !ok {
		return fmt.Errorf("unknown spawn %s", id)
	}
	_ = m.pod.Stop(ctx, &runtime.PodHandle{SidecarID: sp.SidecarID, AgentID: sp.AgentID, SandboxID: sp.SandboxID})
	if sp.FloorIP != "" {
		if err := m.fw.Remove(ctx, firewall.Rules(sp.FloorIP, m.cfg.EgressAllowCIDRs)); err != nil {
			log.Printf("egress floor cleanup for %s (ip %s): %v", id, sp.FloorIP, err)
		}
	}
	for _, d := range sp.MountDirs {
		_ = m.backend.Finalize(ctx, d)
	}
	m.store.Delete(id)
	return nil
}
