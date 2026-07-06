package client

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"

	"connectrpc.com/connect"
	"golang.org/x/net/http2"
	"golang.org/x/net/http2/h2c"
)

func okHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
}

// TestSchemeTransport_H2C verifies the http:// path keeps working (cleartext HTTP/2).
func TestSchemeTransport_H2C(t *testing.T) {
	srv := httptest.NewUnstartedServer(h2c.NewHandler(okHandler(), &http2.Server{}))
	srv.Start()
	defer srv.Close()

	cl := &http.Client{Transport: newSchemeTransport(nil)}
	resp, err := cl.Get(srv.URL)
	if err != nil {
		t.Fatalf("GET %s: %v", srv.URL, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
	if resp.Proto != "HTTP/2.0" {
		t.Errorf("proto = %q, want HTTP/2.0 (h2c sub-transport should have served this)", resp.Proto)
	}
}

// TestSchemeTransport_TLS verifies the https:// path performs a real TLS handshake with ALPN h2.
func TestSchemeTransport_TLS(t *testing.T) {
	srv := httptest.NewUnstartedServer(okHandler())
	srv.EnableHTTP2 = true
	srv.StartTLS()
	defer srv.Close()

	pool := x509.NewCertPool()
	pool.AddCert(srv.Certificate())

	cl := &http.Client{Transport: newSchemeTransport(&tls.Config{RootCAs: pool})}
	resp, err := cl.Get(srv.URL)
	if err != nil {
		t.Fatalf("GET %s: %v", srv.URL, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
	if resp.Proto != "HTTP/2.0" {
		t.Errorf("proto = %q, want HTTP/2.0 (real TLS+ALPN h2 handshake)", resp.Proto)
	}
}

// TestSchemeTransport_H2CClientCannotDoTLS is a regression witness: a plaintext-only h2c
// transport (AllowHTTP + plain DialTLSContext) cannot reach an https server at all.
func TestSchemeTransport_H2CClientCannotDoTLS(t *testing.T) {
	srv := httptest.NewUnstartedServer(okHandler())
	srv.EnableHTTP2 = true
	srv.StartTLS()
	defer srv.Close()

	plainOnly := &http2.Transport{
		AllowHTTP: true,
		DialTLSContext: func(ctx context.Context, network, addr string, _ *tls.Config) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, network, addr)
		},
	}
	cl := &http.Client{Transport: plainOnly}
	if _, err := cl.Get(srv.URL); err == nil {
		t.Fatal("expected error dialing https:// through a plaintext-only h2c transport, got nil")
	}
}

// fakeTokenSource is a scripted TokenSource for interceptor tests: it returns tokens in
// sequence and counts OnUnauthenticated calls.
type fakeTokenSource struct {
	tokens        []string
	call          int
	refreshCalled int
	refreshErr    error
}

func (f *fakeTokenSource) Token(context.Context) (string, error) {
	tok := f.tokens[f.call]
	if f.call < len(f.tokens)-1 {
		f.call++
	}
	return tok, nil
}

func (f *fakeTokenSource) OnUnauthenticated(context.Context) error {
	f.refreshCalled++
	return f.refreshErr
}

// fakeUnaryFunc lets a test script a sequence of connect responses/errors for WrapUnary.
type scriptedUnary struct {
	calls   int
	headers []string // Authorization header seen on each call
	results []error  // error to return per call (nil = success); repeats last entry past the end
}

func (s *scriptedUnary) do(_ context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
	s.headers = append(s.headers, req.Header().Get("Authorization"))
	idx := s.calls
	if idx >= len(s.results) {
		idx = len(s.results) - 1
	}
	s.calls++
	if s.results[idx] != nil {
		return nil, s.results[idx]
	}
	return connect.NewResponse(&struct{}{}), nil
}

func TestTokenSourceInterceptor_SetsBearer(t *testing.T) {
	src := &fakeTokenSource{tokens: []string{"tok-1"}}
	ic := &tsInterceptor{src: src}
	script := &scriptedUnary{results: []error{nil}}
	wrapped := ic.WrapUnary(script.do)

	req := connect.NewRequest(&struct{}{})
	if _, err := wrapped(context.Background(), req); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(script.headers) != 1 || script.headers[0] != "Bearer tok-1" {
		t.Fatalf("headers = %v, want [Bearer tok-1]", script.headers)
	}
	if src.refreshCalled != 0 {
		t.Fatalf("refreshCalled = %d, want 0 (no 401)", src.refreshCalled)
	}
}

func TestTokenSourceInterceptor_RefreshesAndRetriesOnceOnUnauthenticated(t *testing.T) {
	src := &fakeTokenSource{tokens: []string{"stale-tok", "fresh-tok"}}
	ic := &tsInterceptor{src: src}
	script := &scriptedUnary{results: []error{
		connect.NewError(connect.CodeUnauthenticated, errors.New("expired")),
		nil,
	}}
	wrapped := ic.WrapUnary(script.do)

	req := connect.NewRequest(&struct{}{})
	if _, err := wrapped(context.Background(), req); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if script.calls != 2 {
		t.Fatalf("calls = %d, want 2 (initial + retry)", script.calls)
	}
	if src.refreshCalled != 1 {
		t.Fatalf("refreshCalled = %d, want 1", src.refreshCalled)
	}
	if len(script.headers) != 2 || script.headers[0] != "Bearer stale-tok" || script.headers[1] != "Bearer fresh-tok" {
		t.Fatalf("headers = %v, want [Bearer stale-tok, Bearer fresh-tok]", script.headers)
	}
}

func TestTokenSourceInterceptor_DoesNotRetryOnOtherErrors(t *testing.T) {
	src := &fakeTokenSource{tokens: []string{"tok-1"}}
	ic := &tsInterceptor{src: src}
	wantErr := connect.NewError(connect.CodeInternal, errors.New("boom"))
	script := &scriptedUnary{results: []error{wantErr}}
	wrapped := ic.WrapUnary(script.do)

	req := connect.NewRequest(&struct{}{})
	_, err := wrapped(context.Background(), req)
	if err == nil {
		t.Fatal("expected error")
	}
	if script.calls != 1 {
		t.Fatalf("calls = %d, want 1 (no retry on non-Unauthenticated error)", script.calls)
	}
	if src.refreshCalled != 0 {
		t.Fatalf("refreshCalled = %d, want 0", src.refreshCalled)
	}
}
