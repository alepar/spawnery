package githubcred

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
)

// MinGHVersion is the minimum gh CLI version required for the clone2leak (CVE-2024-53858) fix.
// gh < 2.63.0 can be exploited via repo-injected credential helpers in .gitmodules or .git/config
// when gh internally spawns git. Pin the image to >= this version.
const MinGHVersion = "2.63.0"

// MinGHVersionTuple is [major, minor, patch] for comparison.
var MinGHVersionTuple = [3]int{2, 63, 0}

var ghVersionRe = regexp.MustCompile(`gh version (\d+)\.(\d+)\.(\d+)`)

// ParseGHVersion parses a semver string "MAJOR.MINOR.PATCH" (as appears in `gh version` output).
// It is lenient: only the first match of the pattern is used, so the full `gh version` output
// (e.g. "gh version 2.63.0 (2024-11-27)\nhttps://...") is also accepted.
func ParseGHVersion(s string) ([3]int, error) {
	m := ghVersionRe.FindStringSubmatch(s)
	if m == nil {
		// fall back: try bare "MAJOR.MINOR.PATCH"
		parts := strings.Split(strings.TrimSpace(s), ".")
		if len(parts) == 3 {
			var v [3]int
			for i, p := range parts {
				n, err := strconv.Atoi(p)
				if err != nil {
					break
				}
				v[i] = n
				if i == 2 {
					return v, nil
				}
			}
		}
		return [3]int{}, fmt.Errorf("ghversion: cannot parse %q", s)
	}
	var v [3]int
	for i, s := range m[1:4] {
		n, err := strconv.Atoi(s)
		if err != nil {
			return [3]int{}, fmt.Errorf("ghversion: parse component %q: %w", s, err)
		}
		v[i] = n
	}
	return v, nil
}

// GHVersionAtLeast reports whether version a >= b (semver comparison, major.minor.patch).
func GHVersionAtLeast(a, b [3]int) bool {
	for i := range a {
		if a[i] > b[i] {
			return true
		}
		if a[i] < b[i] {
			return false
		}
	}
	return true // equal
}

// CheckGHVersion runs `gh version` and returns an error if the installed gh binary is not present
// or is older than MinGHVersion. Used as a startup/health check for the credential path.
func CheckGHVersion(ctx context.Context) error {
	out, err := exec.CommandContext(ctx, "gh", "version").Output()
	if err != nil {
		return fmt.Errorf("ghversion: gh not found or failed: %w", err)
	}
	v, err := ParseGHVersion(string(bytes.TrimSpace(out)))
	if err != nil {
		return fmt.Errorf("ghversion: parse output %q: %w", out, err)
	}
	if !GHVersionAtLeast(v, MinGHVersionTuple) {
		return fmt.Errorf("ghversion: gh %d.%d.%d is below minimum %s (CVE-2024-53858 / clone2leak fix requires >= %s)",
			v[0], v[1], v[2], MinGHVersion, MinGHVersion)
	}
	return nil
}
