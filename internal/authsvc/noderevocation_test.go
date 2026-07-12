package authsvc_test

import (
	"context"
	"crypto/x509"
	"database/sql"
	"errors"
	"math/big"
	"path/filepath"
	"strings"
	"sync"
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

func TestEnsureCurrentNodeCRLBootstrapsRenewsAndRecoversAfterDowntime(t *testing.T) {
	t0 := time.Now().UTC().Truncate(time.Second)
	now := t0
	root, _ := pki.NewRootCA("root")
	issuer, _ := root.NewIntermediate(pki.IssuerSelfHostedNode, "prod.spawnery.internal")
	st := store.NewTestStore(t)
	published := make(chan []byte, 4)
	newService := func() *authsvc.Service {
		return authsvc.New(root.Cert, issuer,
			authsvc.WithClock(func() time.Time { return now }),
			authsvc.WithNodeRevocationStore(st, func(pem []byte) error {
				published <- append([]byte(nil), pem...)
				return nil
			}),
		)
	}

	svc := newService()
	if err := svc.EnsureCurrentNodeCRL(t.Context(), 6*time.Hour); err != nil {
		t.Fatalf("bootstrap empty CRL: %v", err)
	}
	first := parsePublishedCRL(t, <-published, issuer.Cert, now)
	if first.Number.Cmp(big.NewInt(1)) != 0 || len(first.RevokedCertificateEntries) != 0 {
		t.Fatalf("bootstrap CRL = number %s entries %d", first.Number, len(first.RevokedCertificateEntries))
	}

	now = t0.Add(17 * time.Hour)
	if err := svc.EnsureCurrentNodeCRL(t.Context(), 6*time.Hour); err != nil {
		t.Fatalf("publish still-current CRL: %v", err)
	}
	if got := parsePublishedCRL(t, <-published, issuer.Cert, now); got.Number.Cmp(big.NewInt(1)) != 0 {
		t.Fatalf("early renewal advanced to %s", got.Number)
	}

	now = t0.Add(19 * time.Hour)
	if err := svc.EnsureCurrentNodeCRL(t.Context(), 6*time.Hour); err != nil {
		t.Fatalf("renew before expiry: %v", err)
	}
	if got := parsePublishedCRL(t, <-published, issuer.Cert, now); got.Number.Cmp(big.NewInt(2)) != 0 {
		t.Fatalf("renewal number = %s, want 2", got.Number)
	}

	now = t0.Add(50 * time.Hour)
	restarted := newService()
	if err := restarted.EnsureCurrentNodeCRL(t.Context(), 6*time.Hour); err != nil {
		t.Fatalf("restart after expired checkpoint: %v", err)
	}
	if got := parsePublishedCRL(t, <-published, issuer.Cert, now); got.Number.Cmp(big.NewInt(3)) != 0 {
		t.Fatalf("restart recovery number = %s, want 3", got.Number)
	}
}

func TestEnsureCurrentNodeCRLPublicationRetryDoesNotRenumber(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	root, _ := pki.NewRootCA("root")
	issuer, _ := root.NewIntermediate(pki.IssuerSelfHostedNode, "prod.spawnery.internal")
	st := store.NewTestStore(t)
	var attempts atomic.Int32
	var published []byte
	svc := authsvc.New(root.Cert, issuer,
		authsvc.WithClock(func() time.Time { return now }),
		authsvc.WithNodeRevocationStore(st, func(pem []byte) error {
			if attempts.Add(1) == 1 {
				return errors.New("sink unavailable")
			}
			published = append([]byte(nil), pem...)
			return nil
		}),
	)
	if err := svc.EnsureCurrentNodeCRL(t.Context(), 6*time.Hour); err == nil {
		t.Fatal("bootstrap sink failure did not surface")
	}
	checkpoint, err := st.NodeRevocations().GetCRL(t.Context(), issuer.Cert.SerialNumber.Text(16))
	if err != nil || checkpoint.Number != "1" {
		t.Fatalf("durable bootstrap checkpoint = %+v, %v", checkpoint, err)
	}
	if err := svc.EnsureCurrentNodeCRL(t.Context(), 6*time.Hour); err != nil {
		t.Fatalf("publication retry: %v", err)
	}
	if got := parsePublishedCRL(t, published, issuer.Cert, now); got.Number.Cmp(big.NewInt(1)) != 0 {
		t.Fatalf("retry renumbered CRL to %s", got.Number)
	}
}

func TestConcurrentNodeCRLRenewalAndRevocationPreserveEntries(t *testing.T) {
	t0 := time.Now().UTC().Truncate(time.Second)
	now := t0
	root, _ := pki.NewRootCA("root")
	issuer, _ := root.NewIntermediate(pki.IssuerSelfHostedNode, "prod.spawnery.internal")
	st := store.NewTestStore(t)
	var publishedMu sync.Mutex
	var published []*x509.RevocationList
	svc := authsvc.New(root.Cert, issuer,
		authsvc.WithClock(func() time.Time { return now }),
		authsvc.WithNodeRevocationStore(st, func(pem []byte) error {
			list, err := pki.ParseCRLPEM(pem)
			if err != nil {
				return err
			}
			publishedMu.Lock()
			published = append(published, list)
			publishedMu.Unlock()
			return nil
		}),
	)
	if err := svc.EnsureCurrentNodeCRL(t.Context(), 6*time.Hour); err != nil {
		t.Fatal(err)
	}
	leaf, _ := svc.IssueSelfHostedNode("node-a", "acct", t0.Add(72*time.Hour))
	now = t0.Add(19 * time.Hour)
	start := make(chan struct{})
	errs := make(chan error, 2)
	go func() {
		<-start
		errs <- svc.EnsureCurrentNodeCRL(t.Context(), 6*time.Hour)
	}()
	go func() {
		<-start
		errs <- svc.RevokeNodeCertificate(t.Context(), "node-a", issuer.Cert.SerialNumber, leaf.Cert.SerialNumber, "lost")
	}()
	close(start)
	for range 2 {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}
	checkpoint, err := st.NodeRevocations().GetCRL(t.Context(), issuer.Cert.SerialNumber.Text(16))
	if err != nil {
		t.Fatal(err)
	}
	latest, err := pki.ParseCRLPEM(checkpoint.PEM)
	if err != nil {
		t.Fatal(err)
	}
	if !containsCRLSerial(latest, leaf.Cert.SerialNumber) {
		t.Fatal("concurrent renewal lost revoked leaf")
	}
	publishedMu.Lock()
	defer publishedMu.Unlock()
	for i := 1; i < len(published); i++ {
		if published[i].Number.Cmp(published[i-1].Number) < 0 {
			t.Fatalf("publication regressed: %s then %s", published[i-1].Number, published[i].Number)
		}
	}
}

func TestRunNodeCRLRenewalRetriesFailureAndStopsWithContext(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	root, _ := pki.NewRootCA("root")
	issuer, _ := root.NewIntermediate(pki.IssuerSelfHostedNode, "prod.spawnery.internal")
	st := store.NewTestStore(t)
	var attempts atomic.Int32
	published := make(chan []byte, 1)
	svc := authsvc.New(root.Cert, issuer,
		authsvc.WithClock(func() time.Time { return now }),
		authsvc.WithNodeRevocationStore(st, func(pem []byte) error {
			if attempts.Add(1) == 1 {
				return errors.New("temporary sink failure")
			}
			published <- append([]byte(nil), pem...)
			return nil
		}),
	)
	ctx, cancel := context.WithCancel(t.Context())
	reported := make(chan error, 1)
	done := make(chan struct{})
	go func() {
		defer close(done)
		svc.RunNodeCRLRenewal(ctx, time.Millisecond, 6*time.Hour, func(err error) { reported <- err })
	}()
	select {
	case err := <-reported:
		if !strings.Contains(err.Error(), "temporary sink failure") {
			t.Fatalf("reported error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("renewal failure was not reported")
	}
	select {
	case pem := <-published:
		if got := parsePublishedCRL(t, pem, issuer.Cert, now); got.Number.Cmp(big.NewInt(1)) != 0 {
			t.Fatalf("retry renumbered CRL to %s", got.Number)
		}
	case <-time.After(time.Second):
		t.Fatal("renewal loop did not retry")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("renewal loop did not stop after cancellation")
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
