package spawnlet

import "sync"

type Spawn struct {
	ID        string
	SidecarID string
	AgentID   string
	MountDirs []string // host dirs backing this spawn's mounts (for Finalize)
	FloorIP   string   // pod bridge IP the egress floor was applied for (for Remove on Stop)
	NetnsPath string   // /proc/<sidecar-pid>/ns/net — the pod netns, for AttachACP
	SandboxID string   // CRI backend: the pod sandbox id (for teardown); empty for Docker
	Status    string
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
