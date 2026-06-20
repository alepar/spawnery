package spawnlet

import (
	"context"
	"fmt"
	"log"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"time"
)

// dummyGitHubToken is the fixed dummy credential injected into the agent for GitHub access.
// The MITM proxy overwrites Authorization unconditionally on GitHub hosts (§2.2), so the dummy is
// belt-and-suspenders — its value is functionally irrelevant. Used for GH_TOKEN/GITHUB_TOKEN and the
// inline git credential helper.
const dummyGitHubToken = "x-spawnery-proxy-dummy"

// sidecarProxyPortOffset is the offset from SidecarPort for the MITM forward proxy port.
// Inference = SidecarPort, control = +1, GetToken-TCP = +2, MITM proxy = +3.
const sidecarProxyPortOffset = 3

// SidecarProxyAddrEnv is the sidecar env var carrying the loopback proxy address for the MITM
// forward proxy. The node injects this so the sidecar knows which port to bind. The NAME MUST match
// exactly what the sidecar reads — cmd/sidecar/main.go → internal/sidecar.StartGitHubProxy reads
// getenv("SIDECAR_GITHUB_PROXY_ADDR"); a divergence here silently disables the proxy
// ("sidecar github proxy disabled"). (Cross-task contract: sp-n7iy.5 set this, sp-n7iy.4 reads it.)
const SidecarProxyAddrEnv = "SIDECAR_GITHUB_PROXY_ADDR"

// SpawnCACertName is the filename of the per-spawn CA public certificate in the git-env dir.
const SpawnCACertName = "spawn-ca.crt"

// CABundleName is the filename of the combined CA bundle (system roots ++ spawn CA) in the git-env dir.
// The bundle is used by agent env vars (GIT_SSL_CAINFO, SSL_CERT_FILE, etc.) so all tools trust the
// per-spawn CA without replacing the real roots that tunneled legs (LFS object store) need (§2.5).
const CABundleName = "ca-bundle.crt"

// systemCABundlePaths is the ordered probe list for the node's system CA bundle. The first existing
// file wins and is prepended to the spawn CA in ca-bundle.crt. Package-level var so tests can redirect.
var systemCABundlePaths = []string{
	"/etc/ssl/certs/ca-certificates.crt",
	"/etc/pki/tls/certs/ca-bundle.crt",
	"/etc/ssl/ca-bundle.pem",
	"/etc/ssl/cert.pem",
}

// sidecarReadyTimeout bounds the sidecar-readiness probe. Package-level var so tests can shorten it.
var sidecarReadyTimeout = 10 * time.Second

// sidecarReadyProbe is the seam for the sidecar-readiness gate before StartAgent. Replace in tests
// to avoid real TCP dials (fakePodBackend returns the unreachable PodIP 10.0.0.5).
var sidecarReadyProbe = defaultSidecarReadyProbe

// proxyAddr returns the MITM proxy loopback address for the given sidecar port (port+sidecarProxyPortOffset).
func proxyAddr(sidecarPort int) string {
	return fmt.Sprintf("127.0.0.1:%d", sidecarPort+sidecarProxyPortOffset)
}

// renderGitProxy writes the spawn CA cert + combined CA bundle into gitEnvDir, and writes the proxy
// gitconfig (if absent). proxyAddress is the MITM proxy addr (e.g. "127.0.0.1:8083"). caPEM is the
// per-spawn CA public certificate from ghControl.SpawnCACert.
//
// Write policy:
//   - spawn-ca.crt and ca-bundle.crt are (re)written every call (idempotent; CA is stable per spawn).
//   - gitconfig is written ONLY if absent (preserves the agent's own edits and sp-m859.1's [user] section).
func renderGitProxy(gitEnvDir, proxyAddress string, caPEM []byte) error {
	// Write per-spawn CA cert (agent-readable).
	caCertPath := filepath.Join(gitEnvDir, SpawnCACertName)
	if err := writeFileAtomic(caCertPath, caPEM, 0o644); err != nil {
		return fmt.Errorf("write %s: %w", SpawnCACertName, err)
	}

	// Build combined bundle: system roots first, then spawn CA (§2.5/S2 finding: never replace real
	// roots, or tunneled object-store legs break LFS). If no system bundle found, warn and use spawn CA alone.
	var bundleBytes []byte
	for _, p := range systemCABundlePaths {
		data, err := os.ReadFile(p)
		if err == nil {
			bundleBytes = append(bundleBytes, data...)
			// Ensure a newline separator so the subsequent PEM block is valid.
			if len(bundleBytes) > 0 && bundleBytes[len(bundleBytes)-1] != '\n' {
				bundleBytes = append(bundleBytes, '\n')
			}
			break
		}
	}
	if len(bundleBytes) == 0 {
		log.Printf("git-proxy: no system CA bundle found (tried %v); spawn-ca used alone — tunneled TLS (LFS object store) is degraded", systemCABundlePaths)
	}
	bundleBytes = append(bundleBytes, caPEM...)
	if len(bundleBytes) > 0 && bundleBytes[len(bundleBytes)-1] != '\n' {
		bundleBytes = append(bundleBytes, '\n')
	}
	bundlePath := filepath.Join(gitEnvDir, CABundleName)
	if err := writeFileAtomic(bundlePath, bundleBytes, 0o644); err != nil {
		return fmt.Errorf("write %s: %w", CABundleName, err)
	}

	// Write gitconfig only if absent (write-if-absent preserves agent edits and sp-m859.1's [user]).
	gitconfigPath := filepath.Join(gitEnvDir, GitConfigName)
	if _, err := os.Stat(gitconfigPath); err == nil {
		return nil // already exists; skip
	}
	gitconfig := fmt.Sprintf("[http]\n\tproxy = http://%s\n[url \"https://github.com/\"]\n\tinsteadOf = git@github.com:\n\tinsteadOf = ssh://git@github.com/\n[credential \"https://github.com\"]\n\thelper = \"!f() { echo username=x-access-token; echo password=%s; }; f\"\n",
		proxyAddress, dummyGitHubToken)
	if err := writeFileAtomic(gitconfigPath, []byte(gitconfig), 0o644); err != nil {
		return fmt.Errorf("write gitconfig: %w", err)
	}
	return nil
}

// agentGitProxyEnv returns the agent env var slice for the MITM proxy + CA + dummy credential wiring.
// proxyAddress is the MITM proxy addr (e.g. "127.0.0.1:8083").
// Sets both upper- and lower-case proxy vars (curl/python read lowercase). CA bundle path covers:
// git (GIT_SSL_CAINFO), Go/gh (SSL_CERT_FILE), node (NODE_EXTRA_CA_CERTS), python (REQUESTS_CA_BUNDLE),
// curl (CURL_CA_BUNDLE). NO_PROXY covers the loopback inference addr so OPENAI_BASE_URL is not
// double-proxied (§2.1).
func agentGitProxyEnv(proxyAddress string) []string {
	proxyURL := "http://" + proxyAddress
	caBundle := GitEnvMountPath + "/" + CABundleName
	return []string{
		"HTTPS_PROXY=" + proxyURL,
		"https_proxy=" + proxyURL,
		"HTTP_PROXY=" + proxyURL,
		"http_proxy=" + proxyURL,
		"ALL_PROXY=" + proxyURL,
		"all_proxy=" + proxyURL,
		"NO_PROXY=127.0.0.1,localhost",
		"no_proxy=127.0.0.1,localhost",
		"GH_TOKEN=" + dummyGitHubToken,
		"GITHUB_TOKEN=" + dummyGitHubToken,
		"GIT_SSL_CAINFO=" + caBundle,
		"SSL_CERT_FILE=" + caBundle,
		"NODE_EXTRA_CA_CERTS=" + caBundle,
		"REQUESTS_CA_BUNDLE=" + caBundle,
		"CURL_CA_BUNDLE=" + caBundle,
	}
}

// defaultSidecarReadyProbe performs a bounded-retry TCP dial to confirm the sidecar's control listener
// (at podIP:port) is up before the agent starts (§2.6). Returns nil on first successful connect, a
// wrapped error on timeout or context cancellation. sidecarReadyTimeout governs the overall budget.
func defaultSidecarReadyProbe(ctx context.Context, podIP string, port int) error {
	addr := net.JoinHostPort(podIP, strconv.Itoa(port))
	deadline := time.Now().Add(sidecarReadyTimeout)
	const interval = 200 * time.Millisecond
	dialer := &net.Dialer{}
	var lastErr error
	for {
		dialCtx, cancel := context.WithDeadline(ctx, deadline)
		conn, err := dialer.DialContext(dialCtx, "tcp", addr)
		cancel()
		if err == nil {
			_ = conn.Close()
			return nil
		}
		lastErr = err
		if time.Now().After(deadline) {
			return fmt.Errorf("sidecar readiness probe timeout after %s dialing %s: %w", sidecarReadyTimeout, addr, lastErr)
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("sidecar readiness probe cancelled: %w", ctx.Err())
		case <-time.After(interval):
		}
	}
}

// writeFileAtomic writes data to path with mode perm atomically (temp file in same dir + rename).
// Explicit chmod is applied so the perm is accurate regardless of umask.
func writeFileAtomic(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".tmp-"+filepath.Base(path)+"-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	removeOnErr := true
	defer func() {
		if removeOnErr {
			_ = os.Remove(tmpPath)
		}
	}()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmpPath, perm); err != nil {
		return err
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return err
	}
	removeOnErr = false
	return nil
}
