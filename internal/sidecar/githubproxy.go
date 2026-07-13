package sidecar

import (
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

	"github.com/elazarl/goproxy"
)

// GitHubProxyConfig holds the parameters for the GitHub MITM forward proxy.
type GitHubProxyConfig struct {
	// State is the node-pushed GitHub credential state: the per-spawn MITM CA (used to sign JIT
	// leaf certs) and the current access token. The sidecar NEVER fetches these — the node pushes
	// them to /control/github before the agent starts (§3.1).
	State *GitHubState
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

// proxyErrResp builds a 502 Bad Gateway response with a plain-text diagnostic body.
// The caller MUST return this as the second value from a DoFunc so goproxy streams it.
func proxyErrResp(req *http.Request, body string) *http.Response {
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
	return resp
}

// newGitHubProxy builds a goproxy-based forward proxy that:
//   - MITMs GitHub inject hosts (github.com, api.github.com, etc.) using per-spawn JIT leaf
//     certs from cfg.State and overwrites Authorization with the token pushed to cfg.State;
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
			leaf, err := cfg.State.LeafFor(hostname)
			if err != nil {
				// No CA pushed (or a broken one): REJECT. Falling back to a plain tunnel would send
				// the agent to GitHub with its dummy token and no injection — a confusing 401 and a
				// silent loss of the MITM. Fail closed instead (§3.1: the agent must never run
				// without a working proxy).
				slog.Error("githubproxy: no usable spawn CA, rejecting CONNECT", "host", hostname, "err", err)
				return goproxy.RejectConnect, host
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

	// OnRequest DoFunc: perform the upstream round-trip ourselves (short-circuiting goproxy's own
	// RoundTrip) so we can inspect the response status and record a token rejection. When DoFunc
	// returns a non-nil *http.Response, goproxy skips its own round-trip and streams our response to
	// the client — so we own the full request/response lifecycle here.
	//
	// NOTE: because we short-circuit, goproxy will NOT call RemoveProxyHeaders for us. Before the
	// RoundTrip we MUST set req.RequestURI = "" (else stdlib errors "RequestURI can't be set in
	// client requests") and strip hop-by-hop proxy headers.
	//
	// There is NO in-request retry: the sidecar cannot re-mint a token (it has no channel to ask on
	// — that was the whole point of the inversion). On a 401/403 it records the rejection, the node's
	// long-poll picks it up, forces a re-mint and pushes a fresh token (§3.2). The upstream response
	// is passed through to the agent unchanged.
	proxy.OnRequest().DoFunc(func(req *http.Request, _ *goproxy.ProxyCtx) (*http.Request, *http.Response) {
		host := req.Host
		if host == "" {
			host = req.URL.Host
		}
		action := classifyGitHubHost(host)
		if action == actionTunnel {
			// Not a MITM target — pass through (goproxy handles tunnel/plain-HTTP).
			return req, nil
		}

		tok, _ := cfg.State.Token()
		if tok == "" {
			slog.Warn("githubproxy: no token pushed yet", "host", host, "action", action)
			return req, proxyErrResp(req, "github proxy: no token has been pushed to the sidecar")
		}

		r2 := req.Clone(req.Context())
		applyGitHubAuth(r2.Header, action, tok)
		// Mandatory: stdlib transport errors if RequestURI is non-empty on a client request.
		r2.RequestURI = ""
		// Strip hop-by-hop / proxy headers that goproxy's RemoveProxyHeaders would normally drop.
		r2.Header.Del("Accept-Encoding")
		r2.Header.Del("Proxy-Connection")
		r2.Header.Del("Connection")
		r2.Header.Del("Proxy-Authorization")
		r2.Header.Del("Proxy-Authenticate")

		resp, err := upstream.RoundTrip(r2)
		if err != nil {
			slog.Warn("githubproxy: upstream round-trip failed", "host", host, "action", action, "err", err)
			return req, proxyErrResp(req, "github proxy: upstream error: "+err.Error())
		}

		if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
			// GitHub rejected the pushed token. Latch it for the node's long-poll; pass the response
			// through untouched so the agent's git/gh sees the real error.
			slog.Warn("githubproxy: upstream rejected the token", "host", host, "status", resp.StatusCode)
			cfg.State.RecordRejection(tok)
		}

		return req, resp
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

// StartGitHubProxy is the startup helper for cmd/sidecar/main.go: it reads env, binds the listener
// (claiming the port before the agent starts — §2.6) and starts serving the GitHub MITM proxy in a
// background goroutine, backed by state.
//
// The sidecar NO LONGER FETCHES ANYTHING. The node pushes the CA + token to POST /control/github
// before the agent starts (§3.1), so the proxy comes up credential-less and fails closed (REJECT on
// CONNECT, 502 on request) until the first push lands — which, in the fail-closed create path, is
// always before the agent can issue a request.
//
// When the proxy is disabled (SIDECAR_GITHUB_PROXY_ADDR unset) it logs a notice and returns; the
// inference proxy is unaffected. A bind failure is still fatal (os.Exit(1)): the port must be
// claimed before the agent starts.
func StartGitHubProxy(getenv func(string) string, state *GitHubState) {
	proxyAddr := getenv("SIDECAR_GITHUB_PROXY_ADDR")
	if proxyAddr == "" {
		slog.Info("sidecar github proxy disabled (set SIDECAR_GITHUB_PROXY_ADDR to enable)")
		return
	}
	if state == nil {
		slog.Error("sidecar github proxy: nil GitHubState")
		os.Exit(1)
	}

	// Bind the listener first: claim the port before the agent starts (prevents port-squatting §2.6).
	ln, err := ListenAndServeGitHubProxy(proxyAddr)
	if err != nil {
		slog.Error("sidecar github proxy bind failed", "err", err)
		os.Exit(1)
	}
	slog.Info("sidecar github proxy listener bound", "addr", proxyAddr)

	ServeGitHubProxy(ln, GitHubProxyConfig{State: state})
	slog.Info("sidecar github proxy serving; awaiting a credential push on /control/github", "addr", proxyAddr)
}

// ListenAndServeGitHubProxy binds the github proxy listener on addr (claiming the port before
// the agent starts — prevents port-squatting §2.6). The caller passes the listener to
// ServeGitHubProxy, which serves immediately: the proxy comes up credential-less and fails
// closed until the node's first push lands on /control/github.
func ListenAndServeGitHubProxy(addr string) (net.Listener, error) {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("github proxy: listen %s: %w", addr, err)
	}
	return ln, nil
}
