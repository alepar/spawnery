package agentinstall

import (
	"os"
	"path/filepath"
	"time"
)

// waitForSecrets polls secretsDir for each ref in refs until all are present or timeout expires.
// If timeout <= 0, performs a single check without polling.
// Invalid ref names (checked via validateMCPName) are treated as permanently missing.
// Returns the list of refs that were still absent at the deadline.
func waitForSecrets(secretsDir string, refs []string, timeout time.Duration) []string {
	check := func() []string {
		var missing []string
		for _, ref := range refs {
			if err := validateMCPName(ref); err != nil {
				missing = append(missing, ref) // invalid name can never be present
				continue
			}
			if _, err := os.Stat(filepath.Join(secretsDir, ref)); err != nil {
				missing = append(missing, ref)
			}
		}
		return missing
	}

	if timeout <= 0 {
		return check()
	}

	deadline := time.Now().Add(timeout)
	for {
		missing := check()
		if len(missing) == 0 {
			return nil
		}
		if time.Now().After(deadline) {
			return missing
		}
		time.Sleep(50 * time.Millisecond)
	}
}
