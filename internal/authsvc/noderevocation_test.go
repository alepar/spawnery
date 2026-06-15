package authsvc_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
	"time"

	"spawnery/internal/authsvc"
	"spawnery/internal/authsvc/store"
	"spawnery/internal/pki"
)

func newNodeRevocationSvc(t *testing.T) (*authsvc.Service, store.Store) {
	t.Helper()

	root, err := pki.NewRootCA("R")
	if err != nil {
		t.Fatal(err)
	}
	inter, err := root.NewIntermediate(pki.ClassSelfHosted)
	if err != nil {
		t.Fatal(err)
	}
	st := store.NewTestStore(t)
	_, sigKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	svc := authsvc.New(root.Cert, inter,
		authsvc.WithClock(func() time.Time { return time.Unix(1770000000, 0) }),
		authsvc.WithSessionKey(sigKey),
		authsvc.WithNodeRevocations(st.NodeRevocations()),
	)
	return svc, st
}

func TestNodeRevocationsEndpointReturnsSortedList(t *testing.T) {
	svc, st := newNodeRevocationSvc(t)
	if err := st.NodeRevocations().Revoke(context.Background(), "node-b", "stolen", 200); err != nil {
		t.Fatal(err)
	}
	if err := st.NodeRevocations().Revoke(context.Background(), "node-a", "lost", 100); err != nil {
		t.Fatal(err)
	}

	srv := httptest.NewServer(svc.Handler())
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/node-revocations")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if got := resp.Header.Get("Cache-Control"); got != "no-store" {
		t.Fatalf("Cache-Control = %q, want no-store", got)
	}

	var body struct {
		RevokedNodeIDs []string `json:"revoked_node_ids"`
		GeneratedAt    int64    `json:"generated_at"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(body.RevokedNodeIDs, []string{"node-a", "node-b"}) {
		t.Fatalf("revoked ids = %v", body.RevokedNodeIDs)
	}
	if body.GeneratedAt != 1770000000 {
		t.Fatalf("generated_at = %d", body.GeneratedAt)
	}
}

func TestNodeRevocationsEndpointEmptyList(t *testing.T) {
	svc, _ := newNodeRevocationSvc(t)
	srv := httptest.NewServer(svc.Handler())
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/node-revocations")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}

	var body struct {
		RevokedNodeIDs []string `json:"revoked_node_ids"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if len(body.RevokedNodeIDs) != 0 {
		t.Fatalf("revoked ids = %v", body.RevokedNodeIDs)
	}
}
