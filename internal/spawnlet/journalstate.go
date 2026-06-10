package spawnlet

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"

	"spawnery/internal/storage/journal"
)

// journalRecord is the node-local, durable record that lets the spawnlet resume
// a node-local journaled spawn on the SAME node WITHOUT any CP protocol: it pins
// the per-mount manifest ids captured at the last suspend. Under node-local
// custody the repo password (internal/storage/journal.NodeLocalCustody) and
// these manifest ids both live on the node, so a same-node resume is fully
// self-contained — the CP-threaded pin (design §3) is only needed for the
// owner-sealed cross-node / migration path (transient-tier §4).
type journalRecord struct {
	Generation uint64                        `json:"generation"`
	Manifests  map[string]journal.ManifestID `json:"manifests"` // mount name -> pinned manifest id
}

// journalStateStore persists journalRecords as <dir>/<spawnID>.json (0600).
type journalStateStore struct{ dir string }

func (s *journalStateStore) path(spawnID string) string {
	return filepath.Join(s.dir, spawnID+".json")
}

// Save writes (overwrites) the spawn's pinned-manifest record. The next clean
// suspend overwrites it; a same-node resume reads it.
func (s *journalStateStore) Save(spawnID string, rec journalRecord) error {
	if err := os.MkdirAll(s.dir, 0o700); err != nil {
		return err
	}
	b, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	return os.WriteFile(s.path(spawnID), b, 0o600)
}

// Load returns the spawn's record and whether one exists. A missing record
// (found=false) is the fresh-create case, not an error.
func (s *journalStateStore) Load(spawnID string) (rec journalRecord, found bool, err error) {
	b, err := os.ReadFile(s.path(spawnID))
	if errors.Is(err, os.ErrNotExist) {
		return journalRecord{}, false, nil
	}
	if err != nil {
		return journalRecord{}, false, err
	}
	if err := json.Unmarshal(b, &rec); err != nil {
		return journalRecord{}, false, err
	}
	return rec, true, nil
}

// Delete removes the record (on spawn destroy). Absent is success.
func (s *journalStateStore) Delete(spawnID string) error {
	err := os.Remove(s.path(spawnID))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}
