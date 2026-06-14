package agentinstall

import "testing"

// TestCheckRuntime verifies that checkRuntime correctly detects presence/absence
// of commands on PATH.
func TestCheckRuntime(t *testing.T) {
	tests := []struct {
		name    string
		cmd     string
		present bool
	}{
		{
			name:    "sh is present",
			cmd:     "sh",
			present: true,
		},
		{
			name:    "nonexistent command absent",
			cmd:     "definitely-nonexistent-agentinstall-binary-xyzzy-7f3a9b",
			present: false,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := checkRuntime(tc.cmd)
			if got != tc.present {
				t.Errorf("checkRuntime(%q) = %v, want %v", tc.cmd, got, tc.present)
			}
		})
	}
}
