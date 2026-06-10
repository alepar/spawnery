package spawnlet

import (
	"sync"

	"spawnery/internal/storage/journal"
)

type Spawn struct {
	ID         string
	Generation uint64 // CP-assigned generation (0 standalone); labels the pod for reconcile/fencing
	SidecarID  string
	AgentID    string
	MountDirs  []string // host dirs backing this spawn's mounts (for Finalize)
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
