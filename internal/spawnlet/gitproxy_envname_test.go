package spawnlet

import "testing"

// TestSidecarProxyAddrEnvName pins the env var NAME the node injects to exactly what the sidecar
// reads (cmd/sidecar/main.go → internal/sidecar.StartGitHubProxy: getenv("SIDECAR_GITHUB_PROXY_ADDR")).
// A divergence here silently disables the MITM proxy ("sidecar github proxy disabled") — the
// cross-task contract break (sp-n7iy.5 set, sp-n7iy.4 read) that shipped from the parallel
// implementation. If the sidecar's getenv key changes, change this too.
func TestSidecarProxyAddrEnvName(t *testing.T) {
	const wantSidecarReads = "SIDECAR_GITHUB_PROXY_ADDR"
	if SidecarProxyAddrEnv != wantSidecarReads {
		t.Fatalf("SidecarProxyAddrEnv = %q, but the sidecar reads %q — the MITM proxy would be silently disabled",
			SidecarProxyAddrEnv, wantSidecarReads)
	}
}
