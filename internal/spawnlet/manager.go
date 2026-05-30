package spawnlet

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"spawnery/internal/runtime"
)

type ManagerConfig struct {
	AgentImage, SidecarImage, OpenRouterKey, DataRoot string
	SidecarPort                                       int // default 8080
}

type Manager struct {
	rt    runtime.ContainerRuntime
	cfg   ManagerConfig
	store *Store
}

func NewManager(rt runtime.ContainerRuntime, cfg ManagerConfig) *Manager {
	if cfg.SidecarPort == 0 {
		cfg.SidecarPort = 8080
	}
	return &Manager{rt: rt, cfg: cfg, store: NewStore()}
}

func (m *Manager) Store() *Store { return m.store }

func (m *Manager) Create(ctx context.Context, id, appPath, dataPath, model string) (*Spawn, error) {
	dataDir := dataPath
	if dataDir == "" {
		dataDir = filepath.Join(m.cfg.DataRoot, id, "data")
	}
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return nil, fmt.Errorf("data dir: %w", err)
	}
	copySeed(appPath, dataDir) // best-effort scaffold

	addr := fmt.Sprintf("127.0.0.1:%d", m.cfg.SidecarPort)
	sidecarID, err := m.rt.StartContainer(ctx, runtime.ContainerSpec{
		Image: m.cfg.SidecarImage,
		Env: []string{
			"OPENROUTER_API_KEY=" + m.cfg.OpenRouterKey,
			"SIDECAR_ADDR=" + addr,
		},
	})
	if err != nil {
		return nil, fmt.Errorf("sidecar: %w", err)
	}

	agentID, err := m.rt.StartContainer(ctx, runtime.ContainerSpec{
		Image:   m.cfg.AgentImage,
		NetnsOf: sidecarID,
		Env: []string{
			"OPENAI_BASE_URL=http://" + addr + "/v1",
			"SPAWN_MODEL=" + model,
		},
		Mounts: []runtime.Mount{
			{HostPath: appPath, ContainerPath: "/app", ReadOnly: true},
			{HostPath: dataDir, ContainerPath: "/data"},
		},
		AttachStdio: true,
	})
	if err != nil {
		_ = m.rt.StopContainer(ctx, sidecarID) // rollback
		return nil, fmt.Errorf("agent: %w", err)
	}

	sp := &Spawn{ID: id, SidecarID: sidecarID, AgentID: agentID, DataDir: dataDir, Status: "ready"}
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
	m.store.Delete(id)
	return nil
}

func copySeed(appPath, dataDir string) {
	seed := filepath.Join(appPath, "seed")
	entries, err := os.ReadDir(seed)
	if err != nil {
		return
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		b, err := os.ReadFile(filepath.Join(seed, e.Name()))
		if err == nil {
			_ = os.WriteFile(filepath.Join(dataDir, e.Name()), b, 0o644)
		}
	}
}
