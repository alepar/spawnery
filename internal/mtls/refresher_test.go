package mtls

import (
	"context"
	"crypto/x509"
	"errors"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"spawnery/internal/pki"
)

func TestCRLRefresherAppliesOnlyValidHigherSignedLists(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	root, _ := pki.NewRootCA("root")
	issuer, _ := root.NewIntermediate(pki.IssuerService, "prod.spawnery.internal")
	other, _ := root.NewIntermediate(pki.IssuerCloudNode, "prod.spawnery.internal")
	state := openRefresherState(t, issuer.Cert, now)
	serial := big.NewInt(41)
	valid := refresherCRL(t, issuer, 2, now, serial)

	var body []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(body)
	}))
	t.Cleanup(server.Close)
	refresher := NewCRLRefresher(server.Client(), []CRLSource{{Issuer: issuer.Cert, URL: server.URL}}, state, time.Minute)

	body = valid
	if err := refresher.Refresh(t.Context()); err != nil {
		t.Fatalf("valid refresh: %v", err)
	}
	if !state.IsRevoked(issuer.Cert.SerialNumber, serial) {
		t.Fatal("valid refresh did not publish revocation")
	}

	for _, test := range []struct {
		name string
		body []byte
	}{
		{name: "wrong signature", body: refresherCRL(t, other, 3, now, big.NewInt(42))},
		{name: "corrupt", body: []byte("not a CRL")},
		{name: "stale", body: refresherCRL(t, issuer, 1, now, big.NewInt(43))},
	} {
		t.Run(test.name, func(t *testing.T) {
			body = test.body
			if err := refresher.Refresh(t.Context()); err == nil {
				t.Fatal("invalid refresh succeeded")
			}
			if got, ok := state.HighestNumber(issuer.Cert.SerialNumber); !ok || got.Cmp(big.NewInt(2)) != 0 {
				t.Fatalf("highest number changed: %v, %v", got, ok)
			}
			if !state.IsRevoked(issuer.Cert.SerialNumber, serial) {
				t.Fatal("invalid refresh replaced last good snapshot")
			}
		})
	}
}

func TestCRLRefresherRejectsNetworkStatusAndOversizedResponses(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	root, _ := pki.NewRootCA("root")
	issuer, _ := root.NewIntermediate(pki.IssuerService, "prod.spawnery.internal")
	state := openRefresherState(t, issuer.Cert, now)

	statusServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	t.Cleanup(statusServer.Close)
	oversizedServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(strings.Repeat("x", MaxCRLResponseSize+1)))
	}))
	t.Cleanup(oversizedServer.Close)

	tests := []struct {
		name string
		url  string
		want error
	}{
		{name: "network", url: "http://127.0.0.1:1/crl", want: ErrCRLFetch},
		{name: "status", url: statusServer.URL, want: ErrCRLStatus},
		{name: "size", url: oversizedServer.URL, want: ErrCRLResponseTooLarge},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			refresher := NewCRLRefresher(http.DefaultClient, []CRLSource{{Issuer: issuer.Cert, URL: test.url}}, state, time.Minute)
			err := refresher.Refresh(t.Context())
			if !errors.Is(err, test.want) {
				t.Fatalf("refresh error = %v, want %v", err, test.want)
			}
		})
	}
	if _, ok := state.HighestNumber(issuer.Cert.SerialNumber); ok {
		t.Fatal("failed refresh created trusted state")
	}
}

func TestCRLRefresherRestartRejectsRollbackFromSource(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	root, _ := pki.NewRootCA("root")
	issuer, _ := root.NewIntermediate(pki.IssuerService, "prod.spawnery.internal")
	path := filepath.Join(t.TempDir(), "revocations", "state.json")
	state, err := pki.OpenRevocationState(path, []*x509.Certificate{issuer.Cert}, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	if err := state.ApplyPEM(refresherCRL(t, issuer, 5, now, big.NewInt(50))); err != nil {
		t.Fatal(err)
	}
	if err := state.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := pki.OpenRevocationState(path, []*x509.Certificate{issuer.Cert}, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(refresherCRL(t, issuer, 4, now, big.NewInt(40)))
	}))
	t.Cleanup(server.Close)
	refresher := NewCRLRefresher(server.Client(), []CRLSource{{Issuer: issuer.Cert, URL: server.URL}}, reopened, time.Minute)
	if err := refresher.Refresh(context.Background()); !errors.Is(err, pki.ErrCRLRollback) {
		t.Fatalf("restart rollback error = %v", err)
	}
	if got, ok := reopened.HighestNumber(issuer.Cert.SerialNumber); !ok || got.Cmp(big.NewInt(5)) != 0 {
		t.Fatalf("restart highest number = %v, %v", got, ok)
	}
}

func TestCRLRefresherAppliesBoundedFileSource(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	root, _ := pki.NewRootCA("root")
	issuer, _ := root.NewIntermediate(pki.IssuerService, "prod.spawnery.internal")
	state := openRefresherState(t, issuer.Cert, now)
	path := filepath.Join(t.TempDir(), "issuer.crl")
	if err := os.WriteFile(path, refresherCRL(t, issuer, 1, now, big.NewInt(9)), 0o644); err != nil {
		t.Fatal(err)
	}
	refresher := NewCRLRefresher(nil, []CRLSource{{Issuer: issuer.Cert, Path: path}}, state, time.Minute)
	if err := refresher.Refresh(t.Context()); err != nil {
		t.Fatal(err)
	}
	if !state.IsRevoked(issuer.Cert.SerialNumber, big.NewInt(9)) {
		t.Fatal("file source did not apply CRL")
	}
	if err := os.WriteFile(path, []byte(strings.Repeat("x", MaxCRLResponseSize+1)), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := refresher.Refresh(t.Context()); !errors.Is(err, ErrCRLResponseTooLarge) {
		t.Fatalf("oversized file error = %v", err)
	}
}

func openRefresherState(t *testing.T, issuer *x509.Certificate, now time.Time) *pki.RevocationState {
	t.Helper()
	state, err := pki.OpenRevocationState(filepath.Join(t.TempDir(), "revocations", "state.json"), []*x509.Certificate{issuer}, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = state.Close() })
	return state
}

func refresherCRL(t *testing.T, issuer *pki.CA, number int64, now time.Time, serials ...*big.Int) []byte {
	t.Helper()
	entries := make([]x509.RevocationListEntry, 0, len(serials))
	for _, serial := range serials {
		entries = append(entries, x509.RevocationListEntry{SerialNumber: serial, RevocationTime: now})
	}
	list, err := issuer.CreateCRL(big.NewInt(number), entries, now, now.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	return pki.MarshalCRLPEM(list)
}
