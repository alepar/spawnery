package sidecar

import (
	"context"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/elazarl/goproxy"
)

// GitHubProxyConfig holds the parameters for the GitHub MITM forward proxy.
type GitHubProxyConfig struct {
	// CA is the per-spawn CA used to sign JIT leaf certs for MITM'd hosts.
	CA *spawnCA
	// Control is the node credential client; used to pull the real token before each MITM request.
	Control *githubControl
	// UpstreamTransport is the HTTP transport used for the proxy→github upstream leg.
	// Default (nil): a strict transport (no InsecureSkipVerify, Proxy=nil, HTTP/1.1 only) — T2 (§2.3).
	// Tests inject a transport whose TLSClientConfig trusts the test upstream's cert.
	UpstreamTransport *http.Transport
}

// defaultUpstreamTransport is the strict upstream transport: no InsecureSkipVerify, no
// custom CA pool, no proxy env, HTTP/1.1 only (S2: sufficient). T2 invariant (§2.3).
func newDefaultUpstreamTransport() *http.Transport {
	return &http.Transport{
		Proxy:             nil, // never honor HTTPS_PROXY/NO_PROXY on the upstream leg (T2)
		ForceAttemptHTTP2: false,
		TLSClientConfig: &tls.Config{
			MinVersion: tls.VersionTLS12,
			NextProtos: []string{"http/1.1"},
		},
	}
}

// applyGitHubAuth overwrites the Authorization header on h according to action.
// actionMitmBasic → Basic base64("x-access-token:"+token).
// actionMitmBearer → Bearer <token>.
// actionTunnel should never be called on the MITM path; it is a no-op here.
func applyGitHubAuth(h http.Header, action ghAction, token string) {
	switch action {
	case actionMitmBasic:
		cred := base64.StdEncoding.EncodeToString([]byte("x-access-token:" + token))
		h.Set("Authorization", "Basic "+cred)
	case actionMitmBearer:
		h.Set("Authorization", "Bearer "+token)
	}
}

// newGitHubProxy builds a goproxy-based forward proxy that:
//   - MITMs GitHub inject hosts (github.com, api.github.com, etc.) using per-spawn JIT leaf
//     certs from cfg.CA and overwrites Authorization with the real token from cfg.Control;
//   - CONNECT-tunnels presigned object stores and all non-GitHub hosts untouched;
//   - uses cfg.UpstreamTransport for the upstream TLS leg — default is strict (T2 §2.3).
func newGitHubProxy(cfg GitHubProxyConfig) http.Handler {
	upstream := cfg.UpstreamTransport
	if upstream == nil {
		upstream = newDefaultUpstreamTransport()
	}

	proxy := goproxy.NewProxyHttpServer()
	proxy.Verbose = false
	proxy.Tr = upstream

	// HandleConnect: decide per host whether to MITM or plain-tunnel.
	proxy.OnRequest().HandleConnect(goproxy.FuncHttpsHandler(func(host string, _ *goproxy.ProxyCtx) (*goproxy.ConnectAction, string) {
		action := classifyGitHubHost(host)
		switch action {
		case actionMitmBasic, actionMitmBearer:
			hostname := hostOnly(host)
			leaf, err := cfg.CA.leafFor(hostname)
			if err != nil {
				slog.Warn("githubproxy: leafFor failed, falling back to plain tunnel", "host", hostname, "err", err)
				return goproxy.OkConnect, host
			}
			tlsCfg := &tls.Config{
				Certificates: []tls.Certificate{*leaf},
				MinVersion:   tls.VersionTLS12,
				NextProtos:   []string{"http/1.1"},
			}
			return &goproxy.ConnectAction{
				Action: goproxy.ConnectMitm,
				// TLSConfig is func(host string, ctx *ProxyCtx) (*tls.Config, error) — set directly.
				TLSConfig: func(_ string, _ *goproxy.ProxyCtx) (*tls.Config, error) {
					return tlsCfg, nil
				},
			}, host
		default:
			// Plain CONNECT tunnel: no intercept, no inject.
			return goproxy.OkConnect, host
		}
	}))

	// OnRequest DoFunc: rewrite Authorization on the decrypted MITM leg.
	proxy.OnRequest().DoFunc(func(req *http.Request, _ *goproxy.ProxyCtx) (*http.Request, *http.Response) {
		host := req.Host
		if host == "" {
			host = req.URL.Host
		}
		action := classifyGitHubHost(host)
		if action == actionTunnel {
			// Not a MITM target — pass through (also covers non-GitHub hosts on plain HTTP).
			return req, nil
		}

		tok, err := cfg.Control.Token(req.Context())
		if err != nil {
			body := "github proxy: GetToken failed"
			var ctrlErr *ControlTokenError
			if errors.As(err, &ctrlErr) {
				body = fmt.Sprintf("github proxy: GetToken %d: %s", ctrlErr.StatusCode, strings.TrimSpace(ctrlErr.Body))
			}
			slog.Warn("githubproxy: GetToken failed", "detail", body)
			resp := &http.Response{
				StatusCode: http.StatusBadGateway,
				Status:     "502 Bad Gateway",
				Proto:      "HTTP/1.1",
				ProtoMajor: 1,
				ProtoMinor: 1,
				Header:     make(http.Header),
				Body:       io.NopCloser(strings.NewReader(body + "\n")),
				Request:    req,
			}
			resp.Header.Set("Content-Type", "text/plain; charset=utf-8")
			return req, resp
		}

		// Clone the request so we don't mutate the shared header map.
		req2 := req.Clone(req.Context())
		applyGitHubAuth(req2.Header, action, tok)
		return req2, nil
	})

	return proxy
}

// hostOnly strips the port from a hostport string, returning just the hostname.
func hostOnly(hostport string) string {
	h, _, err := net.SplitHostPort(hostport)
	if err != nil {
		return hostport
	}
	return h
}

// ServeGitHubProxy starts the github proxy on ln in a background goroutine.
func ServeGitHubProxy(ln net.Listener, cfg GitHubProxyConfig) {
	h := newGitHubProxy(cfg)
	srv := &http.Server{Handler: h}
	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Error("githubproxy: serve error", "err", err)
		}
	}()
}

// StartGitHubProxy is the startup helper for cmd/sidecar/main.go: it reads env, binds the
// listener (claiming the port before the agent starts — §2.6), fetches the per-spawn CA with
// bounded retry, parses it, and starts serving the GitHub MITM proxy in a background goroutine.
// When the proxy is disabled (SIDECAR_GITHUB_PROXY_ADDR unset or no control transport) it logs
// a notice and returns — inference proxy continues unaffected (back-compat).
// On unrecoverable startup errors it logs at Error level and calls os.Exit(1).
func StartGitHubProxy(getenv func(string) string) {
	proxyAddr := getenv("SIDECAR_GITHUB_PROXY_ADDR")
	if proxyAddr == "" {
		slog.Info("sidecar github proxy disabled (set SIDECAR_GITHUB_PROXY_ADDR + a SIDECAR_GETTOKEN_* transport to enable)")
		return
	}

	ctrlCfg, ok := ControlTransportFromEnv(getenv)
	if !ok {
		slog.Info("sidecar github proxy disabled (SIDECAR_GITHUB_PROXY_ADDR set but no SIDECAR_GETTOKEN_UDS or SIDECAR_GETTOKEN_ADDR)")
		return
	}

	// Bind the listener first: claim the port before the agent starts (prevents port-squatting §2.6).
	ln, err := ListenAndServeGitHubProxy(proxyAddr)
	if err != nil {
		slog.Error("sidecar github proxy bind failed", "err", err)
		os.Exit(1)
	}
	slog.Info("sidecar github proxy listener bound", "addr", proxyAddr)

	ctrl := newGitHubControl(ctrlCfg)

	// FetchCA with bounded retry/backoff — the node may not have the CA ready the instant we start.
	var ca *spawnCA
	const maxRetries = 10
	for attempt := range maxRetries {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		delivery, fetchErr := ctrl.FetchCA(ctx)
		cancel()
		if fetchErr == nil {
			ca, err = parseSpawnCA(delivery.GetCaCertPem(), delivery.GetCaKeyPem())
			if err != nil {
				slog.Error("sidecar github proxy: parse CA failed", "err", err)
				os.Exit(1)
			}
			break
		}
		if attempt == maxRetries-1 {
			slog.Error("sidecar github proxy: FetchCA failed", "attempts", maxRetries, "err", fetchErr)
			os.Exit(1)
		}
		backoff := time.Duration(attempt+1) * 500 * time.Millisecond
		slog.Warn("sidecar github proxy: FetchCA attempt failed", "attempt", attempt+1, "max", maxRetries, "err", fetchErr, "retry_in", backoff)
		time.Sleep(backoff)
	}

	ServeGitHubProxy(ln, GitHubProxyConfig{CA: ca, Control: ctrl})
	slog.Info("sidecar github proxy serving", "addr", proxyAddr)
}

// ControlTransportFromEnv builds a ControlConfig from environment variables (UDS or TCP lane).
// Returns (zero value, false) when no transport is configured.
func ControlTransportFromEnv(getenv func(string) string) (ControlConfig, bool) {
	spawnID := getenv("SIDECAR_SPAWN_ID")

	if uds := getenv("SIDECAR_GETTOKEN_UDS"); uds != "" {
		return ControlConfig{
			Network: "unix",
			Address: uds,
			SpawnID: spawnID,
		}, true
	}
	if addr := getenv("SIDECAR_GETTOKEN_ADDR"); addr != "" {
		bearer := getenv("SIDECAR_GETTOKEN_BEARER")
		return ControlConfig{
			Network: "tcp",
			Address: addr,
			Bearer:  bearer,
			SpawnID: spawnID,
		}, true
	}
	return ControlConfig{}, false
}

// ListenAndServeGitHubProxy binds the github proxy listener on addr (claiming the port before
// the agent starts — prevents port-squatting §2.6). The caller passes the listener to
// ServeGitHubProxy after the CA is fetched and parsed.
func ListenAndServeGitHubProxy(addr string) (net.Listener, error) {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("github proxy: listen %s: %w", addr, err)
	}
	return ln, nil
}
