package agentinstall

import "os/exec"

// checkRuntime returns true if the given command is found on PATH.
// Used to set Report.RuntimeDepMissing for MCP stdio commands.
func checkRuntime(cmd string) bool {
	_, err := exec.LookPath(cmd)
	return err == nil
}
