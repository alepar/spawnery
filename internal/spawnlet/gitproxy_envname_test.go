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

// TestSidecarEnvNamesMatchSidecarReads pins every sidecar control env var NAME the node injects to
// exactly what internal/sidecar reads. Divergence (or a missing inject) silently breaks the github
// path: SIDECAR_SPAWN_ID empty → wrong CA/token (signer-not-trusted); GETTOKEN_* wrong → no token.
func TestSidecarEnvNamesMatchSidecarReads(t *testing.T) {
	want := map[string]string{
		"proxy-addr":      "SIDECAR_GITHUB_PROXY_ADDR",
		"spawn-id":        "SIDECAR_SPAWN_ID",
		"gettoken-uds":    "SIDECAR_GETTOKEN_UDS",
		"gettoken-addr":   "SIDECAR_GETTOKEN_ADDR",
		"gettoken-bearer": "SIDECAR_GETTOKEN_BEARER",
	}
	got := map[string]string{
		"proxy-addr":      SidecarProxyAddrEnv,
		"spawn-id":        SidecarSpawnIDEnv,
		"gettoken-uds":    SidecarGetTokenUDSEnv,
		"gettoken-addr":   SidecarGetTokenAddrEnv,
		"gettoken-bearer": SidecarGetTokenBearerEnv,
	}
	for k, w := range want {
		if got[k] != w {
			t.Errorf("%s env name = %q, sidecar reads %q", k, got[k], w)
		}
	}
}
