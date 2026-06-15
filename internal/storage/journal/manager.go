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
	// Custody custodies per-spawn repo passwords (node-local default). Required.
	Custody PasswordProvider
	// OwnerSealed is the owner-sealed receiving custody (design §4), holding
	// passwords DELIVERED to this node for cross-node resume / migration. Optional:
	// when set, a spawn for which a key has been delivered is routed here instead
	// of Custody (see passwordFor). When nil, only node-local custody is available
	// and DeliverKey/WaitDelivered error.
	OwnerSealed *OwnerSealedCustody
	// Clock is the time source for the adaptive debounce; nil uses the system
	// clock.
	Clock Clock
	// DebounceK / DebounceMin tune the adaptive debounce (design §2, roast M9).
	// Zero values default to k=2, min=1s.
	DebounceK   float64
	DebounceMin time.Duration
	// Telemetry receives per-snapshot telemetry (design §5). Optional; nil = off.
	Telemetry Telemetry
}

// Manager is the node-daemon journaler service (design §1b): one per spawnlet,
// holding a per-spawn Kopia repo + per-mount serialized snapshot queue + adaptive
// debounce. It implements JournalManager.
type Manager struct {
	cfg   Config
	clock Clock

	mu          sync.Mutex
	spawns      map[string]*spawnState
	ownerSealed map[string]bool // spawnID -> routed to owner-sealed custody
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
	return &Manager{cfg: cfg, clock: clock, spawns: map[string]*spawnState{}, ownerSealed: map[string]bool{}}, nil
}

var _ JournalManager = (*Manager)(nil)

// passwordFor resolves the per-spawn repo password, routing an owner-sealed spawn
// to its DELIVERED key (cross-node resume / migration target) and otherwise to
// the node-local default custody (origin / same-node). The presence of a
// delivered key is the routing signal: the §4 "node-local -> owner-sealed =
// same password" model means the ORIGIN node mints + journals under node-local
// custody, while a resume TARGET receives the same password by delivery and is
// routed here. A node that is supposed to be owner-sealed but has not yet
// received its key surfaces ErrNotDelivered, so the repo is never opened with a
// freshly-minted (wrong) password.
//
// Precondition: the caller holds m.mu (passwordFor is only invoked from state(),
// which locks it). It therefore reads m.ownerSealed WITHOUT re-locking — the
// mutex is not reentrant.
func (m *Manager) passwordFor(spawnID string) (string, error) {
	if m.cfg.OwnerSealed != nil {
		if _, ok := m.cfg.OwnerSealed.Delivered(spawnID); ok {
			return m.cfg.OwnerSealed.PasswordFor(spawnID)
		}
		// Marked owner-sealed but the key has NOT arrived yet: surface
		// ErrNotDelivered rather than minting a fresh node-local password (which
		// would fork the shared repo under a wrong key). A delivered key flips the
		// branch above.
		if m.ownerSealed[spawnID] {
			return "", ErrNotDelivered
		}
	}
	return m.cfg.Custody.PasswordFor(spawnID)
}

// MarkOwnerSealed records that spawnID's repo password is owner-sealed (custodied
// by the owner, delivered to this node), so passwordFor routes it to the
// OwnerSealed custody and never to a node-local mint. The spawnlet marks a spawn
// when resuming an owner-sealed mount cross-node; DeliverKey marks it implicitly.
// No-op when no owner-sealed custody is configured.
func (m *Manager) MarkOwnerSealed(spawnID string) {
	if m.cfg.OwnerSealed == nil {
		return
	}
	m.mu.Lock()
	m.ownerSealed[spawnID] = true
	m.mu.Unlock()
}

// DeliverKey injects an owner-delivered repo password for spawnID at generation
// gen into the owner-sealed custody. The node's SecretDelivery handler calls this
// for a journal-key secret (secret_id prefix journalkey.Prefix) after it has
// OpenDelivered the ciphertext. Errors if no owner-sealed custody is configured.
// Generation-fenced via OwnerSealedCustody.Deliver.
func (m *Manager) DeliverKey(spawnID string, gen uint64, password string) error {
	if m.cfg.OwnerSealed == nil {
		return fmt.Errorf("journal: no owner-sealed custody configured")
	}
	m.MarkOwnerSealed(spawnID)
	return m.cfg.OwnerSealed.Deliver(spawnID, gen, password)
}

// NodeLocalPassword returns the node-local repo password for spawnID from the
// Custody PasswordProvider. Used by the UpgradeToOwnerSealed node handler to
// read the password and seal it to the owner's device set (the node-local →
// owner-sealed upgrade seam, design §4). It calls Custody.PasswordFor which
// generates-and-seals a fresh password on first call, so calling this before
// any snapshot is safe. Returns an error if the custody call fails.
func (m *Manager) NodeLocalPassword(spawnID string) (string, error) {
	return m.cfg.Custody.PasswordFor(spawnID)
}

// WaitDelivered blocks until an owner-sealed key has been delivered for spawnID
// or ctx is done — the "wait for the delivered key before Restore" hook on the
// cross-node resume path (design §4/§5). Errors if no owner-sealed custody is
// configured.
func (m *Manager) WaitDelivered(ctx context.Context, spawnID string) error {
	if m.cfg.OwnerSealed == nil {
		return fmt.Errorf("journal: no owner-sealed custody configured")
	}
	return m.cfg.OwnerSealed.WaitDelivered(ctx, spawnID)
}

// state returns the live state for spawnID, opening its repo on first use.
func (m *Manager) state(ctx context.Context, spawnID string) (*spawnState, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if s, ok := m.spawns[spawnID]; ok {
		return s, nil
	}
	pw, err := m.passwordFor(spawnID)
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
		dur := m.clock.Now().Sub(start)
		deb.RecordScan(dur)
		deb.MarkFired(m.clock.Now())
		m.emit(spawnID, mt.Name, gen, SnapshotContinuous, dur, id, err)
		return id, err
	}
	q = newSerialQueue(context.Background(), action)
	s.queues[mt.Name] = q
	s.debs[mt.Name] = deb
	return q, deb
}

// emit forwards a snapshot telemetry event when a sink is configured. A
// debounce-skip (empty id, nil err) is suppressed — it is not a real snapshot.
func (m *Manager) emit(spawnID, mount string, gen uint64, kind SnapshotKind, scan time.Duration, id ManifestID, err error) {
	if m.cfg.Telemetry == nil {
		return
	}
	if id == "" && err == nil {
		return
	}
	m.cfg.Telemetry.SnapshotDone(spawnID, mount, gen, kind, scan, id, err)
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

// WarmSnapshot implements JournalManager: drain queued work and take one
// immediate snapshot per journaled mount without suspending future requests.
func (m *Manager) WarmSnapshot(ctx context.Context, spawnID string, gen uint64, mounts []Mount) (map[string]ManifestID, error) {
	var journaled []Mount
	for _, mt := range mounts {
		if mt.shouldJournal() {
			journaled = append(journaled, mt)
		}
	}
	if len(journaled) == 0 {
		return nil, nil
	}

	s, err := m.state(ctx, spawnID)
	if err != nil {
		return nil, err
	}

	out := make(map[string]ManifestID, len(journaled))
	for _, mt := range journaled {
		q, _ := m.mountState(s, spawnID, gen, mt)
		warm := func(ctx context.Context) (ManifestID, error) {
			start := m.clock.Now()
			id, err := s.repo.snapshotMount(ctx, mt.Name, mt.HostDir, gen)
			m.emit(spawnID, mt.Name, gen, SnapshotWarm, m.clock.Now().Sub(start), id, err)
			return id, err
		}
		id, err := q.WarmSnapshot(ctx, warm)
		if err != nil {
			return out, fmt.Errorf("journal: warm snapshot mount %q: %w", mt.Name, err)
		}
		out[mt.Name] = id
	}
	return out, nil
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
			start := m.clock.Now()
			id, err := s.repo.snapshotMount(ctx, mt.Name, mt.HostDir, gen)
			m.emit(spawnID, mt.Name, gen, SnapshotFinal, m.clock.Now().Sub(start), id, err)
			return id, err
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

// FullMaintenance runs full (deleting) maintenance on the spawn's repo (design §2
// roast M5). It is intentionally OFF the JournalManager interface: full
// maintenance is CP-commanded only (a CP→node command run after the spawn is
// suspended), never on the node's own cadence, so only the node's command handler
// — holding a concrete *Manager — invokes it. (The CP→node command wiring is a
// sp-u53.5.2 follow-up; this is the node-side primitive it calls.)
func (m *Manager) FullMaintenance(ctx context.Context, spawnID string) error {
	s, err := m.state(ctx, spawnID)
	if err != nil {
		return err
	}
	return s.repo.fullMaintenance(ctx)
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
