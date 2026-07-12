package authsvc_test

import (
	"crypto/x509"
	"database/sql"
	"errors"
	"math/big"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"spawnery/internal/authsvc"
	"spawnery/internal/authsvc/store"
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
	rows, err := st.NodeRevocations().ListByIssuer(t.Context(), issuer.Cert.SerialNumber.Text(16))
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

func TestNodeCRLPublicationRetriesCommittedStateWithoutRenumbering(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	root, _ := pki.NewRootCA("root")
	issuer, _ := root.NewIntermediate(pki.IssuerSelfHostedNode, "prod.spawnery.internal")
	st := store.NewTestStore(t)
	var attempts atomic.Int32
	published := make(chan []byte, 2)
	sink := func(pem []byte) error {
		if attempts.Add(1) == 1 {
			return errors.New("sink unavailable")
		}
		published <- append([]byte(nil), pem...)
		return nil
	}
	svc := authsvc.New(root.Cert, issuer, authsvc.WithClock(func() time.Time { return now }), authsvc.WithNodeRevocationStore(st, sink))
	leaf, err := svc.IssueSelfHostedNode("node-a", "acct", now.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.RevokeNodeCertificate(t.Context(), "node-a", issuer.Cert.SerialNumber, leaf.Cert.SerialNumber, "lost"); err == nil {
		t.Fatal("sink failure did not surface")
	}
	checkpoint, err := st.NodeRevocations().GetCRL(t.Context(), issuer.Cert.SerialNumber.Text(16))
	if err != nil || checkpoint.Number != "1" {
		t.Fatalf("committed checkpoint = %+v, %v", checkpoint, err)
	}
	if err := svc.RevokeNodeCertificate(t.Context(), "node-a", issuer.Cert.SerialNumber, leaf.Cert.SerialNumber, "lost"); err != nil {
		t.Fatalf("idempotent publication retry: %v", err)
	}
	list := parsePublishedCRL(t, <-published, issuer.Cert, now)
	if list.Number.Cmp(big.NewInt(1)) != 0 {
		t.Fatalf("retry renumbered CRL to %s", list.Number)
	}

	restarted := authsvc.New(root.Cert, issuer, authsvc.WithClock(func() time.Time { return now }), authsvc.WithNodeRevocationStore(st, func(pem []byte) error {
		published <- append([]byte(nil), pem...)
		return nil
	}))
	if err := restarted.RecoverNodeCRLPublication(t.Context()); err != nil {
		t.Fatalf("restart publication recovery: %v", err)
	}
	if list := parsePublishedCRL(t, <-published, issuer.Cert, now); list.Number.Cmp(big.NewInt(1)) != 0 {
		t.Fatalf("restart published CRL %s", list.Number)
	}
}

func TestLegacyNodeRevocationRequiresExactVerifiedCertificateMapping(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	root, _ := pki.NewRootCA("root")
	issuer, _ := root.NewIntermediate(pki.IssuerSelfHostedNode, "prod.spawnery.internal")
	correct, _ := issuer.IssueNode("legacy-node", "acct", pki.RoleSelfHosted, "prod.spawnery.internal", now.Add(time.Hour))
	wrong, _ := issuer.IssueNode("other-node", "acct", pki.RoleSelfHosted, "prod.spawnery.internal", now.Add(time.Hour))
	dsn := "file:" + filepath.Join(t.TempDir(), "identity.db")
	initial, err := store.Open(t.Context(), store.Config{Driver: "sqlite", DSN: dsn})
	if err != nil {
		t.Fatal(err)
	}
	if err := initial.Close(); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO node_revocations (node_id, reason, revoked_at) VALUES ('legacy-node', 'lost', ?)`, now.Unix()); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(t.Context(), store.Config{Driver: "sqlite", DSN: dsn})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	published := make(chan []byte, 1)
	svc := authsvc.New(root.Cert, issuer, authsvc.WithTrustDomain("prod.spawnery.internal"), authsvc.WithClock(func() time.Time { return now }), authsvc.WithNodeRevocationStore(st, func(pem []byte) error {
		published <- append([]byte(nil), pem...)
		return nil
	}))
	if err := svc.ReconcileLegacyNodeRevocations(t.Context(), nil); err == nil {
		t.Fatal("missing legacy mapping accepted")
	}
	if err := svc.ReconcileLegacyNodeRevocations(t.Context(), map[string]*x509.Certificate{"legacy-node": wrong.Cert}); err == nil {
		t.Fatal("wrong-node legacy mapping accepted")
	}
	if err := svc.ReconcileLegacyNodeRevocations(t.Context(), map[string]*x509.Certificate{"legacy-node": correct.Cert}); err != nil {
		t.Fatalf("valid legacy mapping: %v", err)
	}
	list := parsePublishedCRL(t, <-published, issuer.Cert, now)
	if !containsCRLSerial(list, correct.Cert.SerialNumber) {
		t.Fatal("reconciled legacy certificate missing from CRL")
	}
	legacy, err := st.NodeRevocations().ListLegacy(t.Context())
	if err != nil || len(legacy) != 0 {
		t.Fatalf("legacy rows after reconciliation = %+v, %v", legacy, err)
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
