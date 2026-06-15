package subkey_test

import (
	"encoding/json"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"net/http"
	"net/http/httptest"

	"spawnery/internal/pki"
	"spawnery/internal/secrets/subkey"
)

func TestASRevocationCheckerAllowsUnlistedNode(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/node-revocations" {
			t.Fatalf("path = %s", r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"revoked_node_ids": []string{"node-b"}})
	}))
	defer srv.Close()

	c := subkey.NewASRevocationChecker(srv.URL+"/node-revocations", srv.Client(), 0)
	revoked, err := c.IsRevoked(pki.Identity{NodeID: "node-a"})
	if err != nil {
		t.Fatal(err)
	}
	if revoked {
		t.Fatal("node-a should not be revoked")
	}
}

func TestASRevocationCheckerRejectsRevokedNode(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"revoked_node_ids": []string{"node-a"}})
	}))
	defer srv.Close()

	c := subkey.NewASRevocationChecker(srv.URL+"/node-revocations", srv.Client(), 0)
	revoked, err := c.IsRevoked(pki.Identity{NodeID: "node-a"})
	if err != nil {
		t.Fatal(err)
	}
	if !revoked {
		t.Fatal("node-a should be revoked")
	}
}

func TestASRevocationCheckerFailsClosedOnHTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "down", http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	c := subkey.NewASRevocationChecker(srv.URL+"/node-revocations", srv.Client(), 0)
	_, err := c.IsRevoked(pki.Identity{NodeID: "node-a"})
	if err == nil || !strings.Contains(err.Error(), "503") {
		t.Fatalf("err = %v", err)
	}
}

func TestASRevocationCheckerFailsClosedOnMalformedJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"revoked_node_ids":[1]}`))
	}))
	defer srv.Close()

	c := subkey.NewASRevocationChecker(srv.URL+"/node-revocations", srv.Client(), 0)
	_, err := c.IsRevoked(pki.Identity{NodeID: "node-a"})
	if err == nil || !strings.Contains(err.Error(), "malformed") {
		t.Fatalf("err = %v", err)
	}
}

func TestASRevocationCheckerFailsClosedOnTrailingJSONGarbage(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"revoked_node_ids":[]} junk`))
	}))
	defer srv.Close()

	c := subkey.NewASRevocationChecker(srv.URL+"/node-revocations", srv.Client(), 0)
	_, err := c.IsRevoked(pki.Identity{NodeID: "node-a"})
	if err == nil || !strings.Contains(err.Error(), "malformed") {
		t.Fatalf("err = %v", err)
	}
}

func TestASRevocationCheckerFailsClosedOnMissingOrNullRevokedNodeIDs(t *testing.T) {
	for _, body := range []string{`{}`, `{"revoked_node_ids":null}`} {
		t.Run(body, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(body))
			}))
			defer srv.Close()

			c := subkey.NewASRevocationChecker(srv.URL+"/node-revocations", srv.Client(), 0)
			_, err := c.IsRevoked(pki.Identity{NodeID: "node-a"})
			if err == nil || !strings.Contains(err.Error(), "malformed") {
				t.Fatalf("err = %v", err)
			}
		})
	}
}

func TestASRevocationCheckerDefaultFetchesEveryTimeAndFailsClosed(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			_ = json.NewEncoder(w).Encode(map[string]any{"revoked_node_ids": []string{}})
			return
		}
		http.Error(w, "down", http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	c := subkey.NewASRevocationChecker(srv.URL+"/node-revocations", srv.Client(), 0)
	revoked, err := c.IsRevoked(pki.Identity{NodeID: "node-a"})
	if err != nil {
		t.Fatal(err)
	}
	if revoked {
		t.Fatal("node-a should not be revoked")
	}
	_, err = c.IsRevoked(pki.Identity{NodeID: "node-a"})
	if err == nil || !strings.Contains(err.Error(), "503") {
		t.Fatalf("err = %v", err)
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("calls = %d", got)
	}
}

func TestASRevocationCheckerCachesSuccessfulResponsesWhenTTLIsOptedIn(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		_ = json.NewEncoder(w).Encode(map[string]any{"revoked_node_ids": []string{"node-a"}})
	}))
	defer srv.Close()

	c := subkey.NewASRevocationChecker(srv.URL+"/node-revocations", srv.Client(), time.Minute)
	revoked, err := c.IsRevoked(pki.Identity{NodeID: "node-a"})
	if err != nil {
		t.Fatal(err)
	}
	if !revoked {
		t.Fatal("node-a should be revoked")
	}
	revoked, err = c.IsRevoked(pki.Identity{NodeID: "node-b"})
	if err != nil {
		t.Fatal(err)
	}
	if revoked {
		t.Fatal("node-b should not be revoked")
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("calls = %d", got)
	}
}
