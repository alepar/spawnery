package spawnlet

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
)

// deltaRecord is the node-local, durable record for a spawn's delta chain depth.
// It lets a resumed spawn continue counting from where it left off, and lets the
// squash-needed heuristic fire at the correct threshold across node restarts.
type deltaRecord struct {
	Depth int `json:"depth"`
}

// deltaStateStore persists deltaRecords as <dir>/<spawnID>.delta.json (0600).
// It mirrors the structure of journalStateStore exactly.
type deltaStateStore struct{ dir string }

func (s *deltaStateStore) path(spawnID string) string {
	return filepath.Join(s.dir, spawnID+".delta.json")
}

// Save writes (overwrites) the spawn's delta chain depth record.
// The next suspend increments and overwrites it; a same-node resume reads it.
func (s *deltaStateStore) Save(spawnID string, rec deltaRecord) error {
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
func (s *deltaStateStore) Load(spawnID string) (rec deltaRecord, found bool, err error) {
	b, err := os.ReadFile(s.path(spawnID))
	if errors.Is(err, os.ErrNotExist) {
		return deltaRecord{}, false, nil
	}
	if err != nil {
		return deltaRecord{}, false, err
	}
	if err := json.Unmarshal(b, &rec); err != nil {
		return deltaRecord{}, false, err
	}
	return rec, true, nil
}

// Delete removes the record (on spawn GC). Absent is success.
func (s *deltaStateStore) Delete(spawnID string) error {
	err := os.Remove(s.path(spawnID))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}
