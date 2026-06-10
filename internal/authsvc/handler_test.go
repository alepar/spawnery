package authsvc_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"spawnery/internal/authsvc"
	"spawnery/internal/pki"
)

// The AS exposes a health endpoint and serves its Root CA for distribution (clients pin it out-of-band;
// this endpoint is for bootstrap/ops).
func TestHandlerServesHealthAndRootCA(t *testing.T) {
	root, _ := pki.NewRootCA("R")
	inter, _ := root.NewIntermediate(pki.ClassSelfHosted)
	srv := httptest.NewServer(authsvc.New(root.Cert, inter).Handler())
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/healthz")
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("/healthz: %v status=%v", err, resp.StatusCode)
	}

	resp2, err := http.Get(srv.URL + "/ca/root")
	if err != nil {
		t.Fatalf("/ca/root: %v", err)
	}
	body, _ := io.ReadAll(resp2.Body)
	got, err := pki.ParseCertPEM(body)
	if err != nil {
		t.Fatalf("/ca/root did not return a PEM cert: %v", err)
	}
	if !got.Equal(root.Cert) {
		t.Fatal("/ca/root returned a different cert than the AS root")
	}
}
