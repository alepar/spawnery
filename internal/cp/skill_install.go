package cp

import (
	"sync"

	cpv1 "spawnery/gen/cp/v1"
)

// skillInstallEntry is the CP's copy of one node.v1.SkillInstallEntry (sp-mwco.2.7), decoupled
// from the wire proto so the map/tests don't need gen/node/v1 imports.
type skillInstallEntry struct {
	Agent, Kind, Name, Status, Reason, Bundle string
}

// skillInstallState is the latest known skill-install report for one spawn.
type skillInstallState struct {
	generation uint64
	entries    []skillInstallEntry
}

// skillInstalls holds the latest per-spawn skill-install report (sp-mwco.2.7), mirroring
// provisioningProgress's shape (ephemeral, in-memory, lost on CP restart — the fatal
// all-or-nothing-bundle-failure case survives restart anyway via the persisted
// error_step/error_detail). Unlike provisioningProgress it is NOT cleared when the spawn reaches
// ACTIVE: "per-skill status visible on the spawn" (acceptance criteria) means it stays visible
// for the life of the spawn, not just while STARTING.
//
// Generation-fenced: set() drops a report whose generation is older than the currently-stored
// one — a late reply from a superseded episode must not clobber a newer episode's report. This
// needs no cross-reference to store state: generations are monotonic per spawn, so "older than
// what's already here" is sufficient without a live-container lookup.
type skillInstalls struct {
	mu sync.Mutex
	m  map[string]skillInstallState
}

func newSkillInstalls() *skillInstalls { return &skillInstalls{m: map[string]skillInstallState{}} }

// set records entries for spawnID at generation gen, dropping the update if a newer generation is
// already recorded for that spawn. Nil-safe (no-op) so a Server built without NewServer's full
// wiring (some narrowly-scoped tests construct &Server{...} directly) doesn't panic.
func (s *skillInstalls) set(spawnID string, gen uint64, entries []skillInstallEntry) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if existing, ok := s.m[spawnID]; ok && existing.generation > gen {
		return // stale-generation report; drop
	}
	s.m[spawnID] = skillInstallState{generation: gen, entries: entries}
}

// get is nil-safe (see set).
func (s *skillInstalls) get(spawnID string) ([]skillInstallEntry, bool) {
	if s == nil {
		return nil, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	st, ok := s.m[spawnID]
	if !ok {
		return nil, false
	}
	return st.entries, true
}

// clear is nil-safe (see set).
func (s *skillInstalls) clear(spawnID string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	delete(s.m, spawnID)
	s.mu.Unlock()
}

// skillInstallsToProto returns spawnID's latest skill-install entries as wire SkillInstall
// values for SpawnSummary, or nil when none are recorded.
func skillInstallsToProto(installs *skillInstalls, spawnID string) []*cpv1.SkillInstall {
	entries, ok := installs.get(spawnID)
	if !ok || len(entries) == 0 {
		return nil
	}
	out := make([]*cpv1.SkillInstall, 0, len(entries))
	for _, e := range entries {
		out = append(out, &cpv1.SkillInstall{
			Agent: e.Agent, Kind: e.Kind, Name: e.Name,
			Status: skillInstallStatusToProto(e.Status), Reason: e.Reason, Bundle: e.Bundle,
		})
	}
	return out
}

// skillInstallStatusToProto maps the CP's internal lowercase status string to the wire enum.
func skillInstallStatusToProto(status string) cpv1.SkillInstallStatus {
	switch status {
	case "applied":
		return cpv1.SkillInstallStatus_SKILL_INSTALL_STATUS_APPLIED
	case "skipped":
		return cpv1.SkillInstallStatus_SKILL_INSTALL_STATUS_SKIPPED
	case "failed":
		return cpv1.SkillInstallStatus_SKILL_INSTALL_STATUS_FAILED
	default:
		return cpv1.SkillInstallStatus_SKILL_INSTALL_STATUS_UNKNOWN
	}
}
