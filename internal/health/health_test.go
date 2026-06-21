package health_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"spawnery/internal/health"
)

func TestHealthz(t *testing.T) {
	mux := http.NewServeMux()
	health.Register(mux, nil)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/healthz")
	if err != nil {
		t.Fatalf("/healthz: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/healthz: want 200, got %d", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "ok" {
		t.Fatalf("/healthz body: want %q, got %q", "ok", string(body))
	}
}

func TestReadyz_Healthy(t *testing.T) {
	mux := http.NewServeMux()
	health.Register(mux, func(_ context.Context) error { return nil })
	srv := httptest.NewServer(mux)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/readyz")
	if err != nil {
		t.Fatalf("/readyz: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/readyz healthy: want 200, got %d", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "ok" {
		t.Fatalf("/readyz body: want %q, got %q", "ok", string(body))
	}
}

func TestReadyz_StoreDown(t *testing.T) {
	mux := http.NewServeMux()
	health.Register(mux, func(_ context.Context) error { return errors.New("db closed") })
	srv := httptest.NewServer(mux)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/readyz")
	if err != nil {
		t.Fatalf("/readyz: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("/readyz store-down: want 503, got %d", resp.StatusCode)
	}
}

func TestReadyz_NilProbe(t *testing.T) {
	mux := http.NewServeMux()
	health.Register(mux, nil)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/readyz")
	if err != nil {
		t.Fatalf("/readyz: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/readyz nil probe: want 200, got %d", resp.StatusCode)
	}
}
