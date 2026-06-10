//go:build e2e

package cp_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"sync"
	"testing"
	"time"
)

// skipWithoutDocker skips (does not fail) when Docker is unavailable, mirroring
// the SKIP_DOCKER pattern in internal/node/tmuxrelay_test.go. Unlike the
// requireDocker helper (which Fatals), this test is explicitly "skips w/o
// Docker" per its beads title, so absence of Docker is a clean skip.
func skipWithoutDocker(t *testing.T) {
	t.Helper()
	if os.Getenv("SKIP_DOCKER") != "" {
		t.Skip("SKIP_DOCKER set")
	}
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker not in PATH")
	}
}

// recordingUpstream is a stub OpenRouter that records the top-level "model" of
// every chat-completions request the sidecar forwards, and returns a minimal
// canned OpenAI completion so the caller's HTTP round-trip succeeds.
type recordingUpstream struct {
	mu     sync.Mutex
	models []string
}

func (u *recordingUpstream) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	var req struct {
		Model string `json:"model"`
	}
	_ = json.Unmarshal(body, &req)
	u.mu.Lock()
	u.models = append(u.models, req.Model)
	u.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{"id":"stub","object":"chat.completion",` +
		`"choices":[{"index":0,"message":{"role":"assistant","content":"ok"},` +
		`"finish_reason":"stop"}]}`))
}

func (u *recordingUpstream) lastModel(t *testing.T) string {
	t.Helper()
	u.mu.Lock()
	defer u.mu.Unlock()
	if len(u.models) == 0 {
		t.Fatal("stub upstream recorded no forwarded requests")
	}
	return u.models[len(u.models)-1]
}

// TestSidecarSeamlessModelSwitch is the one true seamless-switch proof. It runs
// the REAL spawnery/sidecar:dev container pointed at a test-controlled stub
// upstream, then:
//
//	(A) sends an inference request naming OLD  -> stub records OLD (passthrough)
//	(B) POSTs the override (NEW) to the token-gated /control/model endpoint
//	(C) sends another inference request whose body STILL names OLD
//	    -> stub records NEW, proving the sidecar rewrote the live request
//	       mid-session with no restart.
//
// No OPENROUTER_API_KEY / live OpenRouter needed: the stub returns canned JSON
// (OPENROUTER_API_KEY is set to a dummy only because cmd/sidecar/main.go
// requires it non-empty). Skips cleanly without Docker (SKIP_DOCKER or docker
// not in PATH); a missing spawnery/sidecar:dev image is a loud failure (broken
// env), consistent with the repo's requireDocker convention.
func TestSidecarSeamlessModelSwitch(t *testing.T) {
	skipWithoutDocker(t)

	const (
		oldModel    = "openai/gpt-oss-120b:free"
		newModel    = "anthropic/claude-3.5-sonnet"
		token       = "test-control-token-abc123"
		controlPort = "8090"
	)

	// Stub upstream, bound on all interfaces so the container can reach it via
	// the docker host-gateway. httptest.NewServer binds 127.0.0.1 only, so use
	// an explicit 0.0.0.0 listener.
	up := &recordingUpstream{}
	ln, err := net.Listen("tcp", "0.0.0.0:0")
	if err != nil {
		t.Fatalf("listen stub upstream: %v", err)
	}
	stub := &http.Server{Handler: up}
	go func() { _ = stub.Serve(ln) }()
	t.Cleanup(func() {
		ctx, c := context.WithTimeout(context.Background(), 3*time.Second)
		defer c()
		_ = stub.Shutdown(ctx)
	})
	stubPort := ln.Addr().(*net.TCPAddr).Port

	// Run the real sidecar, pointed at the stub via host-gateway. Bind both the
	// inference (SIDECAR_ADDR) and control (SIDECAR_CONTROL_ADDR) listeners on
	// 0.0.0.0 so the host test can reach them via the container's bridge IP.
	//
	// Container -> host reachability uses --add-host host.docker.internal:
	// host-gateway (Docker 20.10+). If host-gateway does not resolve on a given
	// rootless setup, fallbacks are: point SIDECAR_UPSTREAM at the docker bridge
	// gateway IP, or run the stub as a sibling container on the same bridge.
	sidecar := dockerRunD(t, []string{
		"--add-host", "host.docker.internal:host-gateway",
		"-e", "OPENROUTER_API_KEY=dummy", // required non-empty by cmd/sidecar/main.go
		"-e", "SIDECAR_ADDR=0.0.0.0:8080",
		"-e", "SIDECAR_UPSTREAM=http://host.docker.internal:" + strconv.Itoa(stubPort),
		"-e", "SIDECAR_CONTROL_TOKEN=" + token,
		"-e", "SIDECAR_CONTROL_ADDR=0.0.0.0:" + controlPort,
		"spawnery/sidecar:dev",
	})
	t.Cleanup(func() { _ = exec.Command("docker", "rm", "-f", sidecar).Run() })

	sidecarIP := dockerIP(t, sidecar)
	if sidecarIP == "" {
		t.Fatal("sidecar has no bridge IP (rootless-without-bridge unsupported for this test)")
	}
	inferURL := "http://" + net.JoinHostPort(sidecarIP, "8080") + "/v1/chat/completions"
	controlURL := "http://" + net.JoinHostPort(sidecarIP, controlPort) + "/control/model"

	hc := &http.Client{Timeout: 10 * time.Second}
	waitListening(t, net.JoinHostPort(sidecarIP, "8080"))
	waitListening(t, net.JoinHostPort(sidecarIP, controlPort))

	infer := func(model string) {
		t.Helper()
		body := `{"model":"` + model + `","messages":[{"role":"user","content":"hi"}]}`
		resp, err := hc.Post(inferURL, "application/json", bytes.NewBufferString(body))
		if err != nil {
			t.Fatalf("inference POST: %v", err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("inference status = %d, want 200", resp.StatusCode)
		}
	}

	// (A) Before any switch: override unset -> passthrough, stub sees OLD.
	infer(oldModel)
	if got := up.lastModel(t); got != oldModel {
		t.Fatalf("pre-switch forwarded model = %q, want %q (passthrough broken)", got, oldModel)
	}

	// (B) Switch via the token-gated control endpoint.
	req, _ := http.NewRequest(http.MethodPost, controlURL,
		bytes.NewBufferString(`{"model":"`+newModel+`"}`))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	cresp, err := hc.Do(req)
	if err != nil {
		t.Fatalf("control POST: %v", err)
	}
	_, _ = io.Copy(io.Discard, cresp.Body)
	_ = cresp.Body.Close()
	if cresp.StatusCode != http.StatusOK {
		t.Fatalf("control/model status = %d, want 200", cresp.StatusCode)
	}

	// (C) The proof: the agent's request STILL names OLD, but the live sidecar
	// rewrites it -> stub records NEW. Seamless, same sidecar, no restart.
	infer(oldModel)
	if got := up.lastModel(t); got != newModel {
		t.Fatalf("post-switch forwarded model = %q, want %q "+
			"(override did not rewrite the next request)", got, newModel)
	}
	t.Logf("seamless switch proven: next forwarded request carried %q", newModel)
}

// waitListening dials addr until it accepts (sidecar listeners come up shortly
// after the container starts), failing after a bounded deadline.
func waitListening(t *testing.T, addr string) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", addr, time.Second)
		if err == nil {
			_ = conn.Close()
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("sidecar listener %s did not come up within 30s", addr)
}
