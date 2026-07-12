package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"golang.org/x/net/http2"
	"golang.org/x/net/http2/h2c"
)

func TestCreateWaitsForLateAuthorizationFailureAfterActive(t *testing.T) {
	started := time.Now()
	_, err := awaitCreateAuthorization(context.Background(), func(context.Context) error {
		time.Sleep(25 * time.Millisecond)
		return errors.New("late create authorization failure")
	}, func(context.Context) (uint64, error) {
		return 7, nil
	})
	if err == nil || err.Error() != "late create authorization failure" {
		t.Fatalf("error = %v", err)
	}
	if time.Since(started) < 20*time.Millisecond {
		t.Fatal("create returned before authorization completed")
	}
}

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

// TestSchemeTransport_TLS verifies the https:// path performs a real TLS handshake with
// ALPN h2 — the bug this task fixes (the old h2c-only client could never reach an https CP).
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

// TestSchemeTransport_H2CClientCannotDoTLS is a regression witness: the old plaintext-only
// h2c transport (AllowHTTP + plain DialTLSContext) cannot reach an https server at all —
// this is exactly why -cp https://... was broken before this change.
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
