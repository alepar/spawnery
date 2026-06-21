package metrics_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"spawnery/internal/metrics"
)

func TestHandler_StatusOK(t *testing.T) {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	metrics.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
}

func TestHandler_GoCollector(t *testing.T) {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	metrics.Handler().ServeHTTP(rec, req)
	body := rec.Body.String()
	if !strings.Contains(body, "go_goroutines") {
		t.Fatalf("expected go_goroutines in body; got:\n%s", body)
	}
}

func TestHandler_ProcessCollector(t *testing.T) {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	metrics.Handler().ServeHTTP(rec, req)
	body := rec.Body.String()
	if !strings.Contains(body, "process_start_time_seconds") {
		t.Fatalf("expected process_start_time_seconds in body; got:\n%s", body)
	}
}
