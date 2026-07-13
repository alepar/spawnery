package agentinstall

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// CanonicalSkillsDir returns the canonical skills root shared by every agent:
// ~/.agents/skills. Exported so callers outside this package (e.g. sp-mwco.1's
// e2e suite) can locate the canonical install without re-deriving the path.
func CanonicalSkillsDir(homeDir string) string {
	return filepath.Join(homeDir, ".agents", "skills")
}

// skillMarkerName is the ownership marker written inside every installed skill
// directory (both the canonical copy and any agent-native copy) before the
// atomic rename. Its presence at a destination is what distinguishes a prior
// spawnery-managed install (safe to overwrite) from a name-squat (reject).
const skillMarkerName = ".spawnery-skill.json"

// skillMarker records provenance for an installed skill tree.
type skillMarker struct {
	Name           string `json:"name"`
	ProfileID      string `json:"profileId,omitempty"`
	ProfileVersion string `json:"profileVersion,omitempty"`
	InstalledBy    string `json:"installedBy"`
	// Source* record the skill's untrusted-external provenance (sp-mwco.2.8 §4.6), mirroring
	// spec.SkillSource; omitted (no source keys) when the skill has no Source (operator-authored
	// custom/inline content).
	SourceURL    string `json:"sourceUrl,omitempty"`
	SourceRef    string `json:"sourceRef,omitempty"`
	SourceCommit string `json:"sourceCommit,omitempty"`
	SourceSubdir string `json:"sourceSubdir,omitempty"`
}

// writeMarker writes the ownership marker into dir.
func writeMarker(dir string, m skillMarker) error {
	data, err := json.Marshal(m)
	if err != nil {
		return fmt.Errorf("marshal skill marker: %w", err)
	}
	return os.WriteFile(filepath.Join(dir, skillMarkerName), data, 0o600)
}

// destOwnership reports whether dest exists and, if so, whether it carries a
// spawnery ownership marker (i.e. was itself installed by a prior spawnery
// apply, and is therefore safe to overwrite).
func destOwnership(dest string) (exists, managed bool, err error) {
	if _, statErr := os.Lstat(dest); statErr != nil {
		if os.IsNotExist(statErr) {
			return false, false, nil
		}
		return false, false, fmt.Errorf("stat %s: %w", dest, statErr)
	}
	if _, markerErr := os.Stat(filepath.Join(dest, skillMarkerName)); markerErr != nil {
		if os.IsNotExist(markerErr) {
			return true, false, nil
		}
		return true, false, fmt.Errorf("stat skill marker in %s: %w", dest, markerErr)
	}
	return true, true, nil
}

// checkSquat guards every destination in dests against a name-squat: a
// pre-existing, unmanaged directory (not previously installed by spawnery)
// occupying the name a skill is about to be installed under. It runs before
// any temp dir is created, and before any destination is touched, so a squat
// on one of two destinations (e.g. the claude copy) blocks the whole install —
// including the canonical copy — rather than partially applying.
func checkSquat(dests []string) error {
	for _, dest := range dests {
		exists, managed, err := destOwnership(dest)
		if err != nil {
			return err
		}
		if exists && !managed {
			return fmt.Errorf("skill destination %s already exists and was not installed by spawnery (name-squat guard)", dest)
		}
	}
	return nil
}
