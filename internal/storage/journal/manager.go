package journal

import (
	"context"
	"fmt"
	"sync"
	"time"
)

// Config configures a Manager.
type Config struct {
	// RepoRoot holds per-spawn Kopia config files + (for FilesystemBackend) the
	// repo blobs. One sub-dir per spawn.
	RepoRoot string
	// Backend opens the blob storage for each spawn's repo (filesystem here;
	// Garage/S3 later). Required.
	Backend BlobBackend
	// Custody custodies per-spawn repo passwords (node-local here). Required.
	Custody PasswordProvider
	// Clock is the time source for the adaptive debounce; nil uses the system
	// clock.
	Clock Clock
	// DebounceK / DebounceMin tune the adaptive debounce (design §2, roast M9).
	// Zero values default to k=2, min=1s.
	DebounceK   float64
	DebounceMin time.Duration
}

// Manager is the node-daemon journaler service (design §1b): one per spawnlet,
// holding a per-spawn Kopia repo + per-mount serialized snapshot queue + adaptive
// debounce. It implements JournalManager.
type Manager struct {
	cfg   Config
	clock Clock

	mu     sync.Mutex
	spawns map[string]*spawnState
}

// spawnState is the live per-spawn journaler state.
type spawnState struct {
	repo   *spawnRepo
	queues map[string]*serialQueue // per mount
	debs   map[string]*Debouncer   // per mount
}

// NewManager builds a Manager. Backend and Custody are required.
func NewManager(cfg Config) (*Manager, error) {
	if cfg.Backend == nil {
		return nil, fmt.Errorf("journal: Config.Backend is required")
	}
	if cfg.Custody == nil {
		return nil, fmt.Errorf("journal: Config.Custody is required")
	}
	if cfg.RepoRoot == "" {
		return nil, fmt.Errorf("journal: Config.RepoRoot is required")
	}
	clock := cfg.Clock
	if clock == nil {
		clock = systemClock{}
	}
	return &Manager{cfg: cfg, clock: clock, spawns: map[string]*spawnState{}}, nil
}

var _ JournalManager = (*Manager)(nil)

// state returns the live state for spawnID, opening its repo on first use.
func (m *Manager) state(ctx context.Context, spawnID string) (*spawnState, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if s, ok := m.spawns[spawnID]; ok {
		return s, nil
	}
	pw, err := m.cfg.Custody.PasswordFor(spawnID)
	if err != nil {
		return nil, err
	}
	r, err := openOrCreateRepo(ctx, spawnID, m.cfg.RepoRoot, pw, m.cfg.Backend)
	if err != nil {
		return nil, err
	}
	s := &spawnState{repo: r, queues: map[string]*serialQueue{}, debs: map[string]*Debouncer{}}
	m.spawns[spawnID] = s
	return s, nil
}

// mountState returns the per-mount queue + debouncer, creating them on first use.
// The queue's action snapshots the mount through the debounce + scan-duration
// recording. Must be called with m.mu held NOT required — uses its own sync via
// the state's maps under m.mu.
func (m *Manager) mountState(s *spawnState, spawnID string, gen uint64, mt Mount) (*serialQueue, *Debouncer) {
	m.mu.Lock()
	defer m.mu.Unlock()
	q, ok := s.queues[mt.Name]
	if ok {
		return q, s.debs[mt.Name]
	}
	deb := NewDebouncer(m.cfg.DebounceK, m.cfg.DebounceMin)
	action := func(ctx context.Context) (ManifestID, error) {
		// Honor the adaptive debounce: only fire when the cooldown has elapsed.
		if !deb.Ready(m.clock.Now()) {
			return "", nil
		}
		start := m.clock.Now()
		id, err := s.repo.snapshotMount(ctx, mt.Name, mt.HostDir, gen)
		deb.RecordScan(m.clock.Now().Sub(start))
		deb.MarkFired(m.clock.Now())
		return id, err
	}
	q = newSerialQueue(context.Background(), action)
	s.queues[mt.Name] = q
	s.debs[mt.Name] = deb
	return q, deb
}

// RequestSnapshot implements JournalManager.
func (m *Manager) RequestSnapshot(ctx context.Context, spawnID string, gen uint64, mt Mount) {
	if !mt.shouldJournal() {
		return
	}
	s, err := m.state(ctx, spawnID)
	if err != nil {
		return // best-effort; periodic + suspend snapshots are the safety net
	}
	q, _ := m.mountState(s, spawnID, gen, mt)
	q.Request()
}

// FinalSnapshot implements JournalManager: the suspend barrier + final snapshot
// per journaled mount, returning the pinned manifest ids.
func (m *Manager) FinalSnapshot(ctx context.Context, spawnID string, gen uint64, mounts []Mount) (map[string]ManifestID, error) {
	var journaled []Mount
	for _, mt := range mounts {
		if mt.shouldJournal() {
			journaled = append(journaled, mt)
		}
	}
	if len(journaled) == 0 {
		return nil, nil // no journaled mounts — scratch-only spawn, nothing to do
	}

	s, err := m.state(ctx, spawnID)
	if err != nil {
		return nil, err
	}

	out := make(map[string]ManifestID, len(journaled))
	for _, mt := range journaled {
		q, _ := m.mountState(s, spawnID, gen, mt)
		// The final snapshot bypasses the debounce (it must always run).
		final := func(ctx context.Context) (ManifestID, error) {
			return s.repo.snapshotMount(ctx, mt.Name, mt.HostDir, gen)
		}
		id, err := q.Suspend(ctx, final)
		if err != nil {
			return out, fmt.Errorf("journal: final snapshot mount %q: %w", mt.Name, err)
		}
		out[mt.Name] = id
	}
	return out, nil
}

// Restore implements JournalManager.
func (m *Manager) Restore(ctx context.Context, spawnID, mountName string, id ManifestID, hostDir string) error {
	s, err := m.state(ctx, spawnID)
	if err != nil {
		return err
	}
	return s.repo.restore(ctx, id, hostDir)
}

// LatestForGeneration implements JournalManager.
func (m *Manager) LatestForGeneration(ctx context.Context, spawnID, mountName string, gen uint64) (ManifestID, error) {
	s, err := m.state(ctx, spawnID)
	if err != nil {
		return "", err
	}
	return s.repo.latestForGeneration(ctx, mountName, gen)
}

// QuickMaintenance implements JournalManager.
func (m *Manager) QuickMaintenance(ctx context.Context, spawnID string) error {
	s, err := m.state(ctx, spawnID)
	if err != nil {
		return err
	}
	return s.repo.quickMaintenance(ctx)
}

// Close implements JournalManager: release the spawn's repo handle + scheduler
// state. Does NOT forget the sealed password (that is Forget, on spawn delete).
func (m *Manager) Close(ctx context.Context, spawnID string) error {
	m.mu.Lock()
	s, ok := m.spawns[spawnID]
	if ok {
		delete(m.spawns, spawnID)
	}
	m.mu.Unlock()
	if !ok {
		return nil
	}
	return s.repo.close(ctx)
}
