package cp

import (
	"fmt"
	"regexp"
	"strings"

	cpv1 "spawnery/gen/cp/v1"
)

var (
	idSideRe = regexp.MustCompile(`^[a-z0-9._-]+$`)
	semverRe = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?$`)
)

// validateManifest runs the structural checks (E5 §5) on a submitted app-version manifest.
// Pure function; returns a single descriptive error or nil.
func validateManifest(m *cpv1.AppManifest, version, ref string) error {
	if m == nil {
		return fmt.Errorf("manifest is required")
	}
	if m.ApiVersion != "spawnery/v1" {
		return fmt.Errorf("apiVersion must be \"spawnery/v1\", got %q", m.ApiVersion)
	}
	parts := strings.Split(m.Id, "/")
	if len(parts) != 2 || !idSideRe.MatchString(parts[0]) || !idSideRe.MatchString(parts[1]) {
		return fmt.Errorf("id must be \"creator/app\" (lowercase [a-z0-9._-]), got %q", m.Id)
	}
	if strings.TrimSpace(m.Title) == "" {
		return fmt.Errorf("title is required")
	}
	if !semverRe.MatchString(version) {
		return fmt.Errorf("version must be semver MAJOR.MINOR.PATCH, got %q", version)
	}
	if strings.TrimSpace(ref) == "" {
		return fmt.Errorf("ref is required")
	}
	switch m.Visibility {
	case "open":
	case "private":
		return fmt.Errorf("private apps are post-MVP; visibility must be \"open\"")
	default:
		return fmt.Errorf("visibility must be \"open\", got %q", m.Visibility)
	}
	seen := map[string]bool{}
	for _, mt := range m.Mounts {
		if strings.TrimSpace(mt.Name) == "" {
			return fmt.Errorf("mount name is required")
		}
		if seen[mt.Name] {
			return fmt.Errorf("duplicate mount name %q", mt.Name)
		}
		seen[mt.Name] = true
		if strings.TrimSpace(mt.Path) == "" {
			return fmt.Errorf("mount %q: path is required", mt.Name)
		}
		if mt.GetGithub() {
			switch strings.TrimSpace(mt.Durability) {
			case "node-local", "owner-sealed":
				// journaled — ok; a github clone survives suspend/resume (§16.7).
			default:
				return fmt.Errorf("github mount %q: durability must be \"node-local\" or \"owner-sealed\" (got %q); a github slot must be journaled", mt.Name, mt.Durability)
			}
		}
	}
	return nil
}
