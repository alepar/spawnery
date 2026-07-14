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

// TestSidecarEnvNamesMatchSidecarReads pins the sidecar control env var NAMES the node injects.
//
// NOTE what this does and does not prove: it compares each constant to a string LITERAL, so it catches a
// rename of the constant's value — it does NOT prove the sidecar reads it. SIDECAR_SPAWN_ID was pinned
// here for exactly that reason while the sidecar read it nowhere; the pin gave the illusion of a
// name-to-consumer contract and kept a dead var alive. If you add a name here, make sure something on the
// sidecar side actually consumes it.
func TestSidecarEnvNamesMatchSidecarReads(t *testing.T) {
	want := map[string]string{
		"proxy-addr": "SIDECAR_GITHUB_PROXY_ADDR",
	}
	got := map[string]string{
		"proxy-addr": SidecarProxyAddrEnv,
	}
	for k, w := range want {
		if got[k] != w {
			t.Errorf("%s env name = %q, sidecar reads %q", k, got[k], w)
		}
	}
}
