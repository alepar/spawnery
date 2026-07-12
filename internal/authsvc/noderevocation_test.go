package authsvc_test

import (
	"context"
	"crypto/x509"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
	"time"

	"spawnery/internal/authsvc"
	"spawnery/internal/authsvc/store"
	"spawnery/internal/mtls"
	"spawnery/internal/pki"
)

func newNodeRevocationSvc(t *testing.T) (*authsvc.Service, store.Store, *pki.CA, <-chan []byte, time.Time) {
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
	published := make(chan []byte, 4)
	now := time.Now().UTC().Truncate(time.Second)
	svc := authsvc.New(root.Cert, inter,
		authsvc.WithClock(func() time.Time { return now }),
		authsvc.WithNodeRevocationStore(st, func(pem []byte) error {
			published <- append([]byte(nil), pem...)
			return nil
		}),
	)
	return svc, st, inter, published, now
}

func TestNodeRevocationsEndpointReturnsSortedList(t *testing.T) {
	svc, st, _, _, now := newNodeRevocationSvc(t)
	if _, err := st.NodeRevocations().Revoke(context.Background(), store.NodeRevocation{NodeID: "node-b", IssuerSerial: "aa", LeafSerial: "bb", Reason: "stolen", RevokedAt: 200}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.NodeRevocations().Revoke(context.Background(), store.NodeRevocation{NodeID: "node-a", IssuerSerial: "aa", LeafSerial: "cc", Reason: "lost", RevokedAt: 100}); err != nil {
		t.Fatal(err)
	}

	srv := httptest.NewServer(svc.InternalHandler(mtls.Policy{"anonymous": {"authsvc.node-revocations": {}}}))
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
	if body.GeneratedAt != now.Unix() {
		t.Fatalf("generated_at = %d", body.GeneratedAt)
	}
}

func TestNodeRevocationsEndpointEmptyList(t *testing.T) {
	svc, _, _, _, _ := newNodeRevocationSvc(t)
	srv := httptest.NewServer(svc.InternalHandler(mtls.Policy{"anonymous": {"authsvc.node-revocations": {}}}))
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

func TestNodeRevocationIssuesMonotonicSelfHostedCRL(t *testing.T) {
	svc, st, issuer, published, now := newNodeRevocationSvc(t)
	first, err := svc.IssueSelfHostedNode("node-a", "acct", now.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	sibling, err := svc.IssueSelfHostedNode("node-b", "acct", now.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.RevokeNodeCertificate(t.Context(), "node-a", issuer.Cert.SerialNumber, first.Cert.SerialNumber, "stolen"); err != nil {
		t.Fatal(err)
	}
	firstCRL := parsePublishedCRL(t, <-published, issuer.Cert, now)
	if firstCRL.Number.Cmp(big.NewInt(1)) != 0 || len(firstCRL.RevokedCertificateEntries) != 1 || firstCRL.RevokedCertificateEntries[0].SerialNumber.Cmp(first.Cert.SerialNumber) != 0 {
		t.Fatalf("first CRL = %+v", firstCRL)
	}
	rows, err := st.NodeRevocations().List(t.Context())
	if err != nil || len(rows) != 1 || rows[0].IssuerSerial != issuer.Cert.SerialNumber.Text(16) || rows[0].LeafSerial != first.Cert.SerialNumber.Text(16) {
		t.Fatalf("stored revocation = %+v, %v", rows, err)
	}
	if containsCRLSerial(firstCRL, sibling.Cert.SerialNumber) {
		t.Fatal("revoking one node revoked its sibling")
	}

	if err := svc.RevokeNodeCertificate(t.Context(), "node-b", issuer.Cert.SerialNumber, sibling.Cert.SerialNumber, "retired"); err != nil {
		t.Fatal(err)
	}
	secondCRL := parsePublishedCRL(t, <-published, issuer.Cert, now)
	if secondCRL.Number.Cmp(big.NewInt(2)) != 0 || !containsCRLSerial(secondCRL, first.Cert.SerialNumber) || !containsCRLSerial(secondCRL, sibling.Cert.SerialNumber) {
		t.Fatalf("second CRL = %+v", secondCRL)
	}
}

func parsePublishedCRL(t *testing.T, pem []byte, issuer *x509.Certificate, now time.Time) *x509.RevocationList {
	t.Helper()
	list, err := pki.ParseCRLPEM(pem)
	if err != nil {
		t.Fatal(err)
	}
	if err := pki.VerifyCRL(list, issuer, now); err != nil {
		t.Fatal(err)
	}
	return list
}

func containsCRLSerial(list *x509.RevocationList, serial *big.Int) bool {
	for _, entry := range list.RevokedCertificateEntries {
		if entry.SerialNumber.Cmp(serial) == 0 {
			return true
		}
	}
	return false
}
