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
}

type Manager struct {
	rt      runtime.ContainerRuntime
	cfg     ManagerConfig
	store   *Store
	backend storage.Backend
	fw      firewall.Applier
}

func NewManager(rt runtime.ContainerRuntime, cfg ManagerConfig) *Manager {
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
	return &Manager{rt: rt, cfg: cfg, store: NewStore(), backend: storage.NewScratch(cfg.DataRoot), fw: firewall.HostFloorApplier{}}
}

// egressEnforced reports whether the egress floor must be applied: cloud nodes always enforce
// (non-disableable); self-hosted honors the operator's EgressEnforce choice.
func (m *Manager) egressEnforced() bool {
	return m.cfg.NodeClass == "cloud" || m.cfg.EgressEnforce
}

func (m *Manager) Store() *Store { return m.store }

func (m *Manager) Runtime() runtime.ContainerRuntime { return m.rt }

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

	mem := m.cfg.MemLimitMB << 20
	cpus := int64(m.cfg.CPULimit * 1e9)
	pids := m.cfg.PidsLimit
	rtName := m.cfg.ContainerRuntime

	addr := fmt.Sprintf("127.0.0.1:%d", m.cfg.SidecarPort)
	sidecarID, err := m.rt.StartContainer(ctx, runtime.ContainerSpec{
		Image: m.cfg.SidecarImage,
		Env: []string{
			"OPENROUTER_API_KEY=" + m.cfg.OpenRouterKey,
			"SIDECAR_ADDR=" + addr,
		},
		MemoryBytes: mem,
		NanoCPUs:    cpus,
		PidsLimit:   pids,
		Runtime:     rtName,
	})
	if err != nil {
		finalizeAll()
		return nil, fmt.Errorf("sidecar: %w", err)
	}

	var floorIP string
	if m.egressEnforced() {
		ip, ferr := m.rt.ContainerIP(ctx, sidecarID)
		if ferr == nil {
			floorIP = ip
			ferr = m.fw.Apply(ctx, firewall.Rules(ip, m.cfg.EgressAllowCIDRs))
		}
		if ferr != nil {
			_ = m.rt.StopContainer(ctx, sidecarID)
			finalizeAll()
			return nil, fmt.Errorf("egress floor (fail-closed): %w", ferr)
		}
	}

	agentID, err := m.rt.StartContainer(ctx, runtime.ContainerSpec{
		Image:   m.cfg.AgentImage,
		NetnsOf: sidecarID,
		Env: []string{
			"OPENAI_BASE_URL=http://" + addr + "/v1",
			"SPAWN_MODEL=" + model,
		},
		Mounts:      mounts,
		AttachStdio: true,
		MemoryBytes: mem,
		NanoCPUs:    cpus,
		PidsLimit:   pids,
		Runtime:     rtName,
	})
	if err != nil {
		_ = m.rt.StopContainer(ctx, sidecarID)
		finalizeAll()
		return nil, fmt.Errorf("agent: %w", err)
	}

	sp := &Spawn{ID: id, SidecarID: sidecarID, AgentID: agentID, MountDirs: mountDirs, FloorIP: floorIP, Status: "ready"}
	m.store.Put(sp)
	return sp, nil
}

func (m *Manager) Stop(ctx context.Context, id string) error {
	sp, ok := m.store.Get(id)
	if !ok {
		return fmt.Errorf("unknown spawn %s", id)
	}
	_ = m.rt.StopContainer(ctx, sp.AgentID)
	_ = m.rt.StopContainer(ctx, sp.SidecarID)
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
