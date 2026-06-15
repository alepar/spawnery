package spawnlet

import (
	"sync"

	"spawnery/internal/storage"
	"spawnery/internal/storage/journal"
)

type MountFinalizer struct {
	HostDir string
	Backend storage.Backend
}

type Spawn struct {
	ID         string
	Generation uint64 // CP-assigned generation (0 standalone); labels the pod for reconcile/fencing
	SidecarID  string
	AgentID    string
	MountDirs  []string // host dirs backing this spawn's mounts (for Finalize)
	// MountFinalizers records which backend prepared each removable host dir so teardown finalizes
	// through the same backend. Empty on legacy/reconstructed Spawn values; Stop falls back to scratch.
	MountFinalizers []MountFinalizer
	// JournalMounts records the journaled (node-local/owner-sealed, non-secret)
	// mounts of this spawn, so Stop can take the final suspend snapshot. Empty
	// for scratch-only spawns (the guard that leaves existing behavior unchanged).
	JournalMounts []journal.Mount
	// journalWatchers are the per-journaled-mount continuous file watchers driving
	// RequestSnapshot for the lifetime of the spawn (design §2). Started on Create,
	// stopped first thing in teardown. nil for scratch-only spawns / when no
	// journaler is installed. In-memory only (a reconcile-reconstructed Spawn has
	// none, which Stop tolerates).
	journalWatchers []*journal.Watcher
	FloorIP         string // pod bridge IP the egress floor was applied for (for Remove on Stop)
	PodIP           string // pod bridge IP (for the CRI/runsc TCP ACP attach to podIP:acpPort)
	NetnsPath       string // /proc/<sidecar-pid>/ns/net — the pod netns, for the runc-lane AttachACP
	SandboxID       string // CRI backend: the pod sandbox id (for teardown); empty for Docker
	Status          string
	Mode            string // run mode (acp|tmux|served|""); selects the terminal-attach inner command

	// ControlToken is the per-pod bearer secret the sidecar's control endpoint requires
	// (passed to the sidecar as SIDECAR_CONTROL_TOKEN). The node's SetModel handler sends it.
	ControlToken string
	// ControlURL is the node-reachable sidecar control endpoint,
	// "http://<PodIP>:<controlPort>/control/model". Empty if the pod has no IP.
	ControlURL string

	// BaseImageDigest is the content-addressable digest of the base image resolved at create time
	// (spec §4). Set by Manager.Create via ResolveImageDigest; empty when resolution fails (non-fatal).
	// Threaded to the CP on ACTIVE (§4 report-back) so cross-node resume can pin the exact base.
	BaseImageDigest string
	// LaunchImageRef is the image ref the agent was actually started from: the delta tag on a
	// same-node resume (EnsureImage returned the existing delta), or the base ref on a fresh create.
	// Passed as PodHandle.BaseImageRef in teardown so CaptureDelta's moby#47065 layer-count guard
	// compares against the correct parent — not the original base — on chained suspends.
	LaunchImageRef string
	// DeltaImageRef is the local Docker image tag ("spawnery/delta:<id>") set after a successful
	// CaptureDelta on suspend. In-memory only: same-node resume reads the tag directly from the
	// backend (EnsureImage probes it); cross-node resume is a stage-2 concern.
	// BaseImageDigest is reported to the CP at the ACTIVE transition (node statusActive);
	// DeltaImageRef stays node-local (same-node resume) until stage-2 migration ships it.
	DeltaImageRef string

	// DeltaDepth is the number of CaptureDelta calls committed for this spawn so far
	// (i.e. the length of the delta commit chain above the base image). Loaded from
	// deltaStateStore on Create (resume continuation) and incremented on each successful
	// Suspend capture. When DeltaDepth reaches ManagerConfig.DeltaSquashDepth the
	// manager surfaces a SQUASH-NEEDED warning (squash execution is deferred until a
	// backend layer-export method is available).
	DeltaDepth int
}

type Store struct {
	mu sync.Mutex
	m  map[string]*Spawn
}

func NewStore() *Store { return &Store{m: map[string]*Spawn{}} }

func (s *Store) Put(sp *Spawn) { s.mu.Lock(); s.m[sp.ID] = sp; s.mu.Unlock() }
func (s *Store) Get(id string) (*Spawn, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sp, ok := s.m[id]
	return sp, ok
}
func (s *Store) Delete(id string) { s.mu.Lock(); delete(s.m, id); s.mu.Unlock() }

// Claim atomically removes the spawn from the store and returns it.  If the id is
// unknown (or was already claimed by a concurrent caller), it returns nil, false.
// Use this instead of Get+Delete when starting a teardown: only one concurrent caller
// can successfully claim a given spawn, preventing double-teardown races (e.g. the
// quota watchdog and a CP-driven stop firing simultaneously).
func (s *Store) Claim(id string) (*Spawn, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sp, ok := s.m[id]
	if ok {
		delete(s.m, id)
	}
	return sp, ok
}

// List returns a snapshot of all live spawns (for the running inventory).
func (s *Store) List() []*Spawn {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*Spawn, 0, len(s.m))
	for _, sp := range s.m {
		out = append(out, sp)
	}
	return out
}
