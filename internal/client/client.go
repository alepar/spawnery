// Package client is the canonical Go SDK for driving spawnery's control plane: construction,
// transport, the A4 two-phase intent-signing protocol, and spawn lifecycle/query operations.
// It wraps cpv1connect.SpawnServiceClient, parameterized by an endpoint, a TokenSource, and TLS
// config. See docs/superpowers/specs/2026-07-06-client-sdk-signing-design.md.
package client

import (
	"context"
	"crypto/tls"
	"errors"
	"net"
	"net/http"

	"connectrpc.com/connect"
	"golang.org/x/net/http2"

	"spawnery/gen/cp/v1/cpv1connect"
)

// TokenSource supplies the bearer token for CP calls and forces a refresh on 401. Exactly the
// two methods spawnctl's cpTokenSource already implements.
type TokenSource interface {
	Token(ctx context.Context) (string, error)
	OnUnauthenticated(ctx context.Context) error
}

// Client wraps cpv1connect.SpawnServiceClient with the A4 intent-signing protocol and lifecycle
// helpers. Construct with New.
type Client struct {
	rpc             cpv1connect.SpawnServiceClient
	warn            func(error)
	nodeCredentials NodeCredentialSource
	targetTrust     TargetTrust
}

// Option configures a Client at construction time.
type Option func(*Client)

// WithWarnHandler sets the callback used to surface non-fatal retry notices. Defaults to a no-op.
func WithWarnHandler(fn func(error)) Option {
	return func(c *Client) { c.warn = fn }
}

// WithNodeAuthorization enables methods that construct authorization for verified nodes.
func WithNodeAuthorization(source NodeCredentialSource, trust TargetTrust) Option {
	return func(c *Client) {
		c.nodeCredentials = source
		c.targetTrust = trust
	}
}

// New builds a Client against endpoint (e.g. "http://127.0.0.1:8080" or an https:// CP), using ts
// for bearer-token auth and tlsConf for the TLS leg (nil selects system roots / standard
// verification).
func New(endpoint string, ts TokenSource, tlsConf *tls.Config, opts ...Option) *Client {
	httpc := &http.Client{Transport: newSchemeTransport(tlsConf)}
	rpc := cpv1connect.NewSpawnServiceClient(httpc, endpoint,
		connect.WithGRPC(), connect.WithInterceptors(tokenSourceInterceptor(ts)))
	c := &Client{rpc: rpc, warn: func(error) {}}
	for _, opt := range opts {
		opt(c)
	}
	return c
}

// newSchemeTransport builds the scheme-dispatching RoundTripper. tlsConf is nil in production
// (system roots / standard verification); tests inject a cert pool.
func newSchemeTransport(tlsConf *tls.Config) *schemeTransport {
	return &schemeTransport{
		h2c: &http2.Transport{
			AllowHTTP: true,
			DialTLSContext: func(ctx context.Context, network, addr string, _ *tls.Config) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, network, addr)
			},
		},
		tls: &http2.Transport{TLSClientConfig: tlsConf},
	}
}

// schemeTransport routes each request to the h2c or TLS HTTP/2 transport by URL scheme.
type schemeTransport struct {
	h2c *http2.Transport
	tls *http2.Transport
}

func (t *schemeTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.URL.Scheme == "https" {
		return t.tls.RoundTrip(req)
	}
	return t.h2c.RoundTrip(req)
}

// tokenSourceInterceptor builds a Connect interceptor backed by a TokenSource.
// Unary: sets bearer token; on CodeUnauthenticated, forces refresh and retries once.
// Streaming: proactively refreshes before opening the connection (mid-stream 401 needs reconnect — out of scope).
func tokenSourceInterceptor(src TokenSource) connect.Interceptor {
	return &tsInterceptor{src: src}
}

type tsInterceptor struct{ src TokenSource }

func (t *tsInterceptor) WrapUnary(next connect.UnaryFunc) connect.UnaryFunc {
	return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
		tok, err := t.src.Token(ctx)
		if err != nil {
			return nil, connect.NewError(connect.CodeUnauthenticated, err)
		}
		req.Header().Set("Authorization", "Bearer "+tok)
		resp, err := next(ctx, req)
		if err != nil {
			var connErr *connect.Error
			if errors.As(err, &connErr) && connErr.Code() == connect.CodeUnauthenticated {
				// Force refresh and retry once.
				if refreshErr := t.src.OnUnauthenticated(ctx); refreshErr == nil {
					if newTok, tokErr := t.src.Token(ctx); tokErr == nil {
						req.Header().Set("Authorization", "Bearer "+newTok)
						return next(ctx, req)
					}
				}
			}
		}
		return resp, err
	}
}

func (t *tsInterceptor) WrapStreamingClient(next connect.StreamingClientFunc) connect.StreamingClientFunc {
	return func(ctx context.Context, spec connect.Spec) connect.StreamingClientConn {
		conn := next(ctx, spec)
		tok, err := t.src.Token(ctx)
		if err == nil {
			conn.RequestHeader().Set("Authorization", "Bearer "+tok)
		}
		return conn
	}
}

func (t *tsInterceptor) WrapStreamingHandler(next connect.StreamingHandlerFunc) connect.StreamingHandlerFunc {
	return next // server-side: no-op
}

// writerFunc adapts a func([]byte)(int,error) to the io.Writer interface.
type writerFunc func([]byte) (int, error)

func (f writerFunc) Write(b []byte) (int, error) { return f(b) }
