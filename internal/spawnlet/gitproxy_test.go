package spawnlet

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// --- renderGitProxy tests ---

func TestRenderGitProxyGitconfigSections(t *testing.T) {
	dir := t.TempDir()
	caPEM := []byte("-----BEGIN CERTIFICATE-----\nFAKECA\n-----END CERTIFICATE-----\n")
	if err := renderGitProxy(dir, "127.0.0.1:8083", caPEM); err != nil {
		t.Fatalf("renderGitProxy: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(dir, GitConfigName))
	if err != nil {
		t.Fatalf("read gitconfig: %v", err)
	}
	content := string(data)
	for _, want := range []string{
		"proxy = http://127.0.0.1:8083",
		`[url "https://github.com/"]`,
		"insteadOf = git@github.com:",
		"insteadOf = ssh://git@github.com/",
		`[credential "https://github.com"]`,
		"username=x-access-token",
		"password=" + dummyGitHubToken,
	} {
		if !strings.Contains(content, want) {
			t.Errorf("gitconfig missing %q\ncontent:\n%s", want, content)
		}
	}
}

func TestRenderGitProxyCAFiles(t *testing.T) {
	dir := t.TempDir()
	caPEM := []byte("-----BEGIN CERTIFICATE-----\nFAKECA\n-----END CERTIFICATE-----\n")

	// Point systemCABundlePaths at a temp file with a sentinel.
	sysCA := filepath.Join(t.TempDir(), "system-ca.crt")
	sentinel := "-----FAKE-ROOT-----"
	if err := os.WriteFile(sysCA, []byte(sentinel+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	origPaths := systemCABundlePaths
	systemCABundlePaths = []string{sysCA}
	t.Cleanup(func() { systemCABundlePaths = origPaths })

	if err := renderGitProxy(dir, "127.0.0.1:8083", caPEM); err != nil {
		t.Fatalf("renderGitProxy: %v", err)
	}

	// spawn-ca.crt must equal caPEM exactly.
	gotCA, err := os.ReadFile(filepath.Join(dir, SpawnCACertName))
	if err != nil {
		t.Fatalf("read spawn-ca.crt: %v", err)
	}
	if string(gotCA) != string(caPEM) {
		t.Errorf("spawn-ca.crt = %q, want %q", gotCA, caPEM)
	}

	// ca-bundle.crt must contain the sentinel (system roots) AND the spawn CA.
	gotBundle, err := os.ReadFile(filepath.Join(dir, CABundleName))
	if err != nil {
		t.Fatalf("read ca-bundle.crt: %v", err)
	}
	bundleStr := string(gotBundle)
	if !strings.Contains(bundleStr, sentinel) {
		t.Errorf("ca-bundle.crt missing system root sentinel %q", sentinel)
	}
	if !strings.Contains(bundleStr, string(caPEM)) {
		t.Errorf("ca-bundle.crt missing spawn caPEM")
	}
	// System roots must appear before spawn CA in the bundle.
	if idx1, idx2 := strings.Index(bundleStr, sentinel), strings.Index(bundleStr, string(caPEM)); idx1 > idx2 {
		t.Errorf("system roots (idx %d) must precede spawn CA (idx %d) in bundle", idx1, idx2)
	}
}

func TestRenderGitProxyBundleDegraded(t *testing.T) {
	dir := t.TempDir()
	caPEM := []byte("-----BEGIN CERTIFICATE-----\nONLYSPAWNCA\n-----END CERTIFICATE-----\n")

	// No system CA paths exist.
	origPaths := systemCABundlePaths
	systemCABundlePaths = []string{
		filepath.Join(t.TempDir(), "nonexistent1.crt"),
		filepath.Join(t.TempDir(), "nonexistent2.crt"),
	}
	t.Cleanup(func() { systemCABundlePaths = origPaths })

	// renderGitProxy must not return an error even when system CA is missing.
	if err := renderGitProxy(dir, "127.0.0.1:8083", caPEM); err != nil {
		t.Fatalf("renderGitProxy (degraded): %v", err)
	}

	got, err := os.ReadFile(filepath.Join(dir, CABundleName))
	if err != nil {
		t.Fatalf("read ca-bundle.crt: %v", err)
	}
	if !strings.Contains(string(got), string(caPEM)) {
		t.Errorf("degraded bundle does not contain spawn CA\ngot:\n%s", got)
	}
}

func TestRenderGitProxyGitconfigIfAbsent(t *testing.T) {
	dir := t.TempDir()
	caPEM := []byte("-----BEGIN CERTIFICATE-----\nFAKECA\n-----END CERTIFICATE-----\n")

	// Pre-write gitconfig (simulating sp-m859.1 or agent edit).
	preExisting := "[user]\n\temail = agent@example.com\n"
	gitconfigPath := filepath.Join(dir, GitConfigName)
	if err := os.WriteFile(gitconfigPath, []byte(preExisting), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := renderGitProxy(dir, "127.0.0.1:8083", caPEM); err != nil {
		t.Fatalf("renderGitProxy: %v", err)
	}

	// gitconfig must NOT be overwritten.
	got, err := os.ReadFile(gitconfigPath)
	if err != nil {
		t.Fatalf("read gitconfig: %v", err)
	}
	if string(got) != preExisting {
		t.Errorf("gitconfig was overwritten; got %q, want %q", got, preExisting)
	}

	// CA files ARE refreshed even when gitconfig is skipped.
	if _, err := os.Stat(filepath.Join(dir, SpawnCACertName)); err != nil {
		t.Errorf("spawn-ca.crt missing after gitconfig-skip: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, CABundleName)); err != nil {
		t.Errorf("ca-bundle.crt missing after gitconfig-skip: %v", err)
	}
}

// --- agentGitProxyEnv tests ---

// gitProxyEnvMap converts a KEY=VALUE slice to a map for easier assertion.
func gitProxyEnvMap(env []string) map[string]string {
	m := make(map[string]string, len(env))
	for _, e := range env {
		idx := strings.IndexByte(e, '=')
		if idx < 0 {
			m[e] = ""
			continue
		}
		m[e[:idx]] = e[idx+1:]
	}
	return m
}

func TestAgentGitProxyEnvVars(t *testing.T) {
	env := agentGitProxyEnv("127.0.0.1:8083")
	m := gitProxyEnvMap(env)

	proxyURL := "http://127.0.0.1:8083"
	caBundle := GitEnvMountPath + "/" + CABundleName
	noProxy := "127.0.0.1,localhost"

	cases := []struct{ key, want string }{
		{"HTTPS_PROXY", proxyURL},
		{"https_proxy", proxyURL},
		{"HTTP_PROXY", proxyURL},
		{"http_proxy", proxyURL},
		{"ALL_PROXY", proxyURL},
		{"all_proxy", proxyURL},
		{"NO_PROXY", noProxy},
		{"no_proxy", noProxy},
		{"GH_TOKEN", dummyGitHubToken},
		{"GITHUB_TOKEN", dummyGitHubToken},
		{"GIT_SSL_CAINFO", caBundle},
		{"SSL_CERT_FILE", caBundle},
		{"NODE_EXTRA_CA_CERTS", caBundle},
		{"REQUESTS_CA_BUNDLE", caBundle},
		{"CURL_CA_BUNDLE", caBundle},
	}
	for _, tc := range cases {
		got, ok := m[tc.key]
		if !ok {
			t.Errorf("agentGitProxyEnv: missing %s", tc.key)
		} else if got != tc.want {
			t.Errorf("agentGitProxyEnv %s = %q, want %q", tc.key, got, tc.want)
		}
	}
}

// --- defaultSidecarReadyProbe tests ---

func TestSidecarReadyProbeReady(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	host, portStr, _ := net.SplitHostPort(ln.Addr().String())
	port := 0
	if _, err := fmt.Sscanf(portStr, "%d", &port); err != nil {
		t.Fatalf("parse port %q: %v", portStr, err)
	}

	origTimeout := sidecarReadyTimeout
	sidecarReadyTimeout = 2 * time.Second
	t.Cleanup(func() { sidecarReadyTimeout = origTimeout })

	if err := defaultSidecarReadyProbe(context.Background(), host, port); err != nil {
		t.Fatalf("probe should return nil for a live listener; got: %v", err)
	}
}

func TestSidecarReadyProbeTimeout(t *testing.T) {
	// Grab a free port and close it so nothing is listening.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()

	host, portStr, _ := net.SplitHostPort(addr)
	port := 0
	if _, err := fmt.Sscanf(portStr, "%d", &port); err != nil {
		t.Fatalf("parse port %q: %v", portStr, err)
	}

	origTimeout := sidecarReadyTimeout
	sidecarReadyTimeout = 200 * time.Millisecond
	t.Cleanup(func() { sidecarReadyTimeout = origTimeout })

	if err := defaultSidecarReadyProbe(context.Background(), host, port); err == nil {
		t.Fatal("probe should return an error for an unreachable port")
	}
}

// --- manager-level wiring tests ---

// overrideSidecarReadyProbe replaces sidecarReadyProbe with a recorder that returns retErr.
// Returns pointers to the last observed (podIP, port). Restores original via t.Cleanup.
func overrideSidecarReadyProbe(t *testing.T, retErr error) (*string, *int) {
	t.Helper()
	podIP := new(string)
	port := new(int)
	orig := sidecarReadyProbe
	sidecarReadyProbe = func(_ context.Context, ip string, p int) error {
		*podIP, *port = ip, p
		return retErr
	}
	t.Cleanup(func() { sidecarReadyProbe = orig })
	return podIP, port
}

func TestManagerSidecarProxyEnv(t *testing.T) {
	fb := &fakePodBackend{}
	mock := &mockGitHubControlServer{}
	overrideSidecarReadyProbe(t, nil)

	m := NewManagerWithBackend(fb, &fakeApplier{}, ManagerConfig{
		AgentImage:   "a",
		SidecarImage: "s",
		DataRoot:     t.TempDir(),
		UsernsMode:   "remap",
		SidecarPort:  8080,
	})
	m.SetGitHubControlServer(mock)

	if _, err := m.Create(context.Background(), "sp-proxy-env", writeApp(t), "model", "", "", 0); err != nil {
		t.Fatalf("Create: %v", err)
	}

	wantProxyAddr := proxyAddr(8080) // "127.0.0.1:8083"
	got := sidecarEnvVal(fb.podSpec.SidecarEnv, SidecarProxyAddrEnv)
	if got != wantProxyAddr {
		t.Errorf("SIDECAR_PROXY_ADDR in sidecar env = %q, want %q", got, wantProxyAddr)
	}
}

func TestManagerAgentProxyEnv(t *testing.T) {
	fb := &fakePodBackend{}
	mock := &mockGitHubControlServer{}
	overrideSidecarReadyProbe(t, nil)

	dataRoot := t.TempDir()
	m := NewManagerWithBackend(fb, &fakeApplier{}, ManagerConfig{
		AgentImage:   "a",
		SidecarImage: "s",
		DataRoot:     dataRoot,
		UsernsMode:   "remap",
		SidecarPort:  8080,
	})
	m.SetGitHubControlServer(mock)

	if _, err := m.Create(context.Background(), "sp-agent-env", writeApp(t), "model", "", "", 0); err != nil {
		t.Fatalf("Create: %v", err)
	}

	agentEnv := gitProxyEnvMap(fb.agentSpec.Env)

	wantProxyURL := "http://" + proxyAddr(8080)
	for _, key := range []string{"HTTPS_PROXY", "https_proxy", "HTTP_PROXY", "http_proxy"} {
		if got := agentEnv[key]; got != wantProxyURL {
			t.Errorf("agent env %s = %q, want %q", key, got, wantProxyURL)
		}
	}
	if got := agentEnv["GH_TOKEN"]; got != dummyGitHubToken {
		t.Errorf("agent env GH_TOKEN = %q, want %q", got, dummyGitHubToken)
	}
	wantCA := GitEnvMountPath + "/" + CABundleName
	if got := agentEnv["GIT_SSL_CAINFO"]; got != wantCA {
		t.Errorf("agent env GIT_SSL_CAINFO = %q, want %q", got, wantCA)
	}

	// Verify CA files written into git-env.
	gitEnvDir := filepath.Join(dataRoot, "git-env", "sp-agent-env")
	gotCA, err := os.ReadFile(filepath.Join(gitEnvDir, SpawnCACertName))
	if err != nil {
		t.Fatalf("spawn-ca.crt not written: %v", err)
	}
	if string(gotCA) != "fake-ca-cert" {
		t.Errorf("spawn-ca.crt = %q, want \"fake-ca-cert\"", gotCA)
	}
	if _, err := os.Stat(filepath.Join(gitEnvDir, CABundleName)); err != nil {
		t.Errorf("ca-bundle.crt not written: %v", err)
	}
	if _, err := os.Stat(filepath.Join(gitEnvDir, GitConfigName)); err != nil {
		t.Errorf("gitconfig not written: %v", err)
	}
}

func TestManagerReadinessCalled(t *testing.T) {
	fb := &fakePodBackend{}
	mock := &mockGitHubControlServer{}

	var probePodIP string
	var probePort int
	var probeCalledBeforeAgent bool

	orig := sidecarReadyProbe
	sidecarReadyProbe = func(_ context.Context, ip string, p int) error {
		probePodIP, probePort = ip, p
		// At probe time, agentSpec.Image is still "" (StartAgent not yet called).
		probeCalledBeforeAgent = fb.agentSpec.Image == ""
		return nil
	}
	t.Cleanup(func() { sidecarReadyProbe = orig })

	m := NewManagerWithBackend(fb, &fakeApplier{}, ManagerConfig{
		AgentImage:   "a",
		SidecarImage: "s",
		DataRoot:     t.TempDir(),
		UsernsMode:   "remap",
		SidecarPort:  8080,
	})
	m.SetGitHubControlServer(mock)

	if _, err := m.Create(context.Background(), "sp-probe", writeApp(t), "model", "", "", 0); err != nil {
		t.Fatalf("Create: %v", err)
	}

	// fakePodBackend.StartPod returns PodIP="10.0.0.5"; control port = SidecarPort+1 = 8081.
	if probePodIP != "10.0.0.5" {
		t.Errorf("probe podIP = %q, want 10.0.0.5", probePodIP)
	}
	if probePort != 8081 {
		t.Errorf("probe port = %d, want 8081", probePort)
	}
	if !probeCalledBeforeAgent {
		t.Error("probe was not called before StartAgent")
	}
}

func TestManagerNoGHControlNoop(t *testing.T) {
	fb := &fakePodBackend{}

	orig := sidecarReadyProbe
	sidecarReadyProbe = func(_ context.Context, _ string, _ int) error {
		t.Error("sidecarReadyProbe must not be called when ghControl is nil")
		return nil
	}
	t.Cleanup(func() { sidecarReadyProbe = orig })

	m := NewManagerWithBackend(fb, &fakeApplier{}, ManagerConfig{
		AgentImage:   "a",
		SidecarImage: "s",
		DataRoot:     t.TempDir(),
		UsernsMode:   "remap",
	})
	// ghControl intentionally NOT set.

	if _, err := m.Create(context.Background(), "sp-noop", writeApp(t), "model", "", "", 0); err != nil {
		t.Fatalf("Create without ghControl: %v", err)
	}

	// No SIDECAR_PROXY_ADDR in sidecar env.
	if got := sidecarEnvVal(fb.podSpec.SidecarEnv, SidecarProxyAddrEnv); got != "" {
		t.Errorf("SIDECAR_PROXY_ADDR must be absent without ghControl; got %q", got)
	}
	// No proxy/CA vars in agent env.
	agentEnv := gitProxyEnvMap(fb.agentSpec.Env)
	for _, key := range []string{"HTTPS_PROXY", "GH_TOKEN", "GIT_SSL_CAINFO"} {
		if val, ok := agentEnv[key]; ok {
			t.Errorf("agent env %s must be absent without ghControl; got %q", key, val)
		}
	}
}

func TestManagerReadinessFailureFailClosed(t *testing.T) {
	fb := &fakePodBackend{}
	mock := &mockGitHubControlServer{}

	orig := sidecarReadyProbe
	sidecarReadyProbe = func(_ context.Context, _ string, _ int) error {
		return fmt.Errorf("simulated sidecar not ready")
	}
	t.Cleanup(func() { sidecarReadyProbe = orig })

	m := NewManagerWithBackend(fb, &fakeApplier{}, ManagerConfig{
		AgentImage:   "a",
		SidecarImage: "s",
		DataRoot:     t.TempDir(),
		UsernsMode:   "remap",
	})
	m.SetGitHubControlServer(mock)

	_, err := m.Create(context.Background(), "sp-fail", writeApp(t), "model", "", "", 0)
	if err == nil {
		t.Fatal("Create should fail when sidecarReadyProbe returns an error")
	}
	if !strings.Contains(err.Error(), "sidecar readiness gate") {
		t.Errorf("error should mention sidecar readiness gate; got: %v", err)
	}
	// StartAgent must NOT have been called (agentSpec remains zero).
	if fb.agentSpec.Image != "" {
		t.Error("StartAgent must not be called when readiness probe fails")
	}
	// Pod must have been stopped (fail-closed).
	if fb.stopped == nil {
		t.Error("pod.Stop must be called when readiness probe fails")
	}
}
