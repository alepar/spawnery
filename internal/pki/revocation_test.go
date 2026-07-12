package pki

import (
	"bytes"
	"crypto/x509"
	"errors"
	"math/big"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestRevocationStatePersistsReloadsAndScopesSerialsByIssuer(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	root, _ := NewRootCA("root")
	service, _ := root.NewIntermediate(IssuerService, "prod.spawnery.internal")
	cloud, _ := root.NewIntermediate(IssuerCloudNode, "prod.spawnery.internal")
	path := filepath.Join(t.TempDir(), "revocations", "state.json")
	state, err := OpenRevocationState(path, []*x509.Certificate{service.Cert, cloud.Cert}, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	serial1 := big.NewInt(101)
	serial2 := big.NewInt(202)
	if err := state.ApplyPEM(mustCRLPEM(t, service, 1, now, serial1)); err != nil {
		t.Fatal(err)
	}
	if !state.IsRevoked(service.Cert.SerialNumber, serial1) || state.IsRevoked(cloud.Cert.SerialNumber, serial1) {
		t.Fatal("revocation was not scoped to its issuer")
	}
	if err := state.ApplyPEM(mustCRLPEM(t, service, 2, now, serial2)); err != nil {
		t.Fatal(err)
	}
	if state.IsRevoked(service.Cert.SerialNumber, serial1) || !state.IsRevoked(service.Cert.SerialNumber, serial2) {
		t.Fatal("new CRL did not replace the issuer snapshot")
	}
	if err := state.ApplyPEM(mustCRLPEM(t, cloud, 4, now, serial1)); err != nil {
		t.Fatal(err)
	}
	if got, ok := state.HighestNumber(service.Cert.SerialNumber); !ok || got.Cmp(big.NewInt(2)) != 0 {
		t.Fatalf("service highest number = %v, %v", got, ok)
	}
	if got, ok := state.HighestNumber(cloud.Cert.SerialNumber); !ok || got.Cmp(big.NewInt(4)) != 0 {
		t.Fatalf("cloud highest number = %v, %v", got, ok)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("state mode = %o", info.Mode().Perm())
	}
	if err := state.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := OpenRevocationState(path, []*x509.Certificate{cloud.Cert, service.Cert}, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	if reopened.IsRevoked(service.Cert.SerialNumber, serial1) || !reopened.IsRevoked(service.Cert.SerialNumber, serial2) || !reopened.IsRevoked(cloud.Cert.SerialNumber, serial1) {
		t.Fatal("reloaded revoked serial set is incorrect")
	}
}

func TestRevocationStateRejectsRollbackAndEquivocationWithoutMutation(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	root, _ := NewRootCA("root")
	issuer, _ := root.NewIntermediate(IssuerSelfHostedNode, "prod.spawnery.internal")
	path := filepath.Join(t.TempDir(), "revocations", "state.json")
	state, err := OpenRevocationState(path, []*x509.Certificate{issuer.Cert}, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = state.Close() })
	accepted := mustCRLPEM(t, issuer, 2, now, big.NewInt(2))
	if err := state.ApplyPEM(accepted); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(path)
	if err := state.ApplyPEM(accepted); err != nil {
		t.Fatalf("identical CRL was not idempotent: %v", err)
	}
	if err := state.ApplyPEM(mustCRLPEM(t, issuer, 1, now, big.NewInt(1))); !errors.Is(err, ErrCRLRollback) {
		t.Fatalf("rollback error = %v", err)
	}
	if err := state.ApplyPEM(mustCRLPEM(t, issuer, 2, now, big.NewInt(3))); !errors.Is(err, ErrCRLConflict) {
		t.Fatalf("equivocation error = %v", err)
	}
	after, _ := os.ReadFile(path)
	if !bytes.Equal(before, after) || !state.IsRevoked(issuer.Cert.SerialNumber, big.NewInt(2)) || state.IsRevoked(issuer.Cert.SerialNumber, big.NewInt(3)) {
		t.Fatal("rejected update changed memory or disk")
	}
}

func TestRevocationStateRejectsUnknownOrWrongSigner(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	root, _ := NewRootCA("root")
	issuer, _ := root.NewIntermediate(IssuerCloudNode, "prod.spawnery.internal")
	unknown, _ := root.NewIntermediate(IssuerCloudNode, "prod.spawnery.internal")
	state, err := OpenRevocationState(filepath.Join(t.TempDir(), "revocations", "state.json"), []*x509.Certificate{issuer.Cert}, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = state.Close() })
	if err := state.ApplyPEM(mustCRLPEM(t, unknown, 1, now, big.NewInt(1))); err == nil {
		t.Fatal("unknown issuer CRL accepted")
	}
	list, _ := issuer.CreateCRL(big.NewInt(1), nil, now, now.Add(time.Hour))
	list.Signature[0] ^= 0xff
	if err := state.ApplyPEM(MarshalCRLPEM(list)); err == nil {
		t.Fatal("forged CRL accepted")
	}
	if _, ok := state.HighestNumber(issuer.Cert.SerialNumber); ok {
		t.Fatal("rejected CRL created issuer state")
	}
}

func TestRevocationStateConcurrentReadersObserveImmutableSnapshots(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	root, _ := NewRootCA("root")
	issuer, _ := root.NewIntermediate(IssuerService, "prod.spawnery.internal")
	state, err := OpenRevocationState(filepath.Join(t.TempDir(), "revocations", "state.json"), []*x509.Certificate{issuer.Cert}, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = state.Close() })
	var wg sync.WaitGroup
	stop := make(chan struct{})
	for range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
					_ = state.IsRevoked(issuer.Cert.SerialNumber, big.NewInt(1))
					_, _ = state.HighestNumber(issuer.Cert.SerialNumber)
				}
			}
		}()
	}
	for number := int64(1); number <= 20; number++ {
		if err := state.ApplyPEM(mustCRLPEM(t, issuer, number, now, big.NewInt(number))); err != nil {
			t.Fatal(err)
		}
	}
	close(stop)
	wg.Wait()
	if !state.IsRevoked(issuer.Cert.SerialNumber, big.NewInt(20)) {
		t.Fatal("final snapshot missing")
	}
}

func TestRevocationStateExclusiveOwnershipRejectsStaleWriter(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	root, _ := NewRootCA("root")
	issuer, _ := root.NewIntermediate(IssuerService, "prod.spawnery.internal")
	path := filepath.Join(t.TempDir(), "revocations", "state.json")
	old, err := OpenRevocationState(path, []*x509.Certificate{issuer.Cert}, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	if _, err := OpenRevocationState(path, []*x509.Certificate{issuer.Cert}, func() time.Time { return now }); !errors.Is(err, ErrRevocationStateLocked) {
		t.Fatalf("second owner error = %v", err)
	}
	if err := old.ApplyPEM(mustCRLPEM(t, issuer, 1, now, big.NewInt(1))); err != nil {
		t.Fatal(err)
	}
	if err := old.Close(); err != nil {
		t.Fatal(err)
	}
	next, err := OpenRevocationState(path, []*x509.Certificate{issuer.Cert}, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = next.Close() })
	if err := next.ApplyPEM(mustCRLPEM(t, issuer, 2, now, big.NewInt(2))); err != nil {
		t.Fatal(err)
	}
	if err := old.ApplyPEM(mustCRLPEM(t, issuer, 3, now, big.NewInt(3))); !errors.Is(err, ErrRevocationStateClosed) {
		t.Fatalf("stale writer error = %v", err)
	}
	if !old.IsRevoked(issuer.Cert.SerialNumber, big.NewInt(999)) {
		t.Fatal("closed verifier did not fail closed")
	}
	if got, _ := next.HighestNumber(issuer.Cert.SerialNumber); got.Cmp(big.NewInt(2)) != 0 {
		t.Fatalf("active owner floor = %v", got)
	}
}

func TestRevocationStatePersistenceFailureSemantics(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	root, _ := NewRootCA("root")
	issuer, _ := root.NewIntermediate(IssuerService, "prod.spawnery.internal")
	t.Run("before rename preserves old state", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "revocations", "state.json")
		state, err := OpenRevocationState(path, []*x509.Certificate{issuer.Cert}, func() time.Time { return now })
		if err != nil {
			t.Fatal(err)
		}
		if err := state.ApplyPEM(mustCRLPEM(t, issuer, 1, now, big.NewInt(1))); err != nil {
			t.Fatal(err)
		}
		state.beforeRename = func() error { return errors.New("injected") }
		if err := state.ApplyPEM(mustCRLPEM(t, issuer, 2, now, big.NewInt(2))); err == nil {
			t.Fatal("pre-rename failure ignored")
		}
		if got, _ := state.HighestNumber(issuer.Cert.SerialNumber); got.Cmp(big.NewInt(1)) != 0 {
			t.Fatalf("memory floor = %v", got)
		}
		_ = state.Close()
		reopened, err := OpenRevocationState(path, []*x509.Certificate{issuer.Cert}, func() time.Time { return now })
		if err != nil {
			t.Fatal(err)
		}
		defer reopened.Close()
		if got, _ := reopened.HighestNumber(issuer.Cert.SerialNumber); got.Cmp(big.NewInt(1)) != 0 {
			t.Fatalf("disk floor = %v", got)
		}
	})

	t.Run("after rename advances floor and poisons", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "revocations", "state.json")
		state, err := OpenRevocationState(path, []*x509.Certificate{issuer.Cert}, func() time.Time { return now })
		if err != nil {
			t.Fatal(err)
		}
		defer state.Close()
		if err := state.ApplyPEM(mustCRLPEM(t, issuer, 1, now, big.NewInt(1))); err != nil {
			t.Fatal(err)
		}
		state.afterRename = func() error { return errors.New("injected") }
		if err := state.ApplyPEM(mustCRLPEM(t, issuer, 3, now, big.NewInt(3))); !errors.Is(err, ErrRevocationStatePoisoned) {
			t.Fatalf("post-rename error = %v", err)
		}
		if got, _ := state.HighestNumber(issuer.Cert.SerialNumber); got.Cmp(big.NewInt(3)) != 0 {
			t.Fatalf("advanced floor = %v", got)
		}
		if !state.IsRevoked(issuer.Cert.SerialNumber, big.NewInt(999)) {
			t.Fatal("poisoned verifier did not fail closed")
		}
		if err := state.ApplyPEM(mustCRLPEM(t, issuer, 4, now, big.NewInt(4))); !errors.Is(err, ErrRevocationStatePoisoned) {
			t.Fatalf("poisoned mutation error = %v", err)
		}
		state.afterRename = nil
		if err := state.Close(); err != nil {
			t.Fatal(err)
		}
		reopened, err := OpenRevocationState(path, []*x509.Certificate{issuer.Cert}, func() time.Time { return now })
		if err != nil {
			t.Fatal(err)
		}
		defer reopened.Close()
		if got, _ := reopened.HighestNumber(issuer.Cert.SerialNumber); got.Cmp(big.NewInt(3)) != 0 {
			t.Fatalf("renamed disk floor = %v", got)
		}
	})
}

func TestRevocationStateRevalidatesPersistedState(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	root, _ := NewRootCA("root")
	issuer, _ := root.NewIntermediate(IssuerService, "prod.spawnery.internal")
	path := filepath.Join(t.TempDir(), "revocations", "state.json")
	state, err := OpenRevocationState(path, []*x509.Certificate{issuer.Cert}, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	if err := state.ApplyPEM(mustCRLPEM(t, issuer, 1, now, big.NewInt(1))); err != nil {
		t.Fatal(err)
	}
	_ = state.Close()
	otherRoot, _ := NewRootCA("other")
	otherIssuer, _ := otherRoot.NewIntermediate(IssuerService, "prod.spawnery.internal")
	if _, err := OpenRevocationState(path, []*x509.Certificate{otherIssuer.Cert}, func() time.Time { return now }); err == nil {
		t.Fatal("persisted CRL accepted under different issuer")
	}
	if _, err := OpenRevocationState(path, []*x509.Certificate{issuer.Cert}, func() time.Time { return now.Add(2 * time.Hour) }); err == nil {
		t.Fatal("expired persisted CRL accepted")
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenRevocationState(path, []*x509.Certificate{issuer.Cert}, func() time.Time { return now }); err == nil {
		t.Fatal("permissive persisted file accepted")
	}
}

func TestRevocationStateRejectsMalformedPersistenceAndSharedDirectory(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	root, _ := NewRootCA("root")
	issuer, _ := root.NewIntermediate(IssuerService, "prod.spawnery.internal")
	for name, content := range map[string]string{
		"truncated":     `{"version":1`,
		"wrong version": `{"version":2,"issuers":[]}`,
		"unknown field": `{"version":1,"issuers":[],"extra":true}`,
		"null issuers":  `{"version":1,"issuers":null}`,
		"trailing data": `{"version":1,"issuers":[]} {}`,
	} {
		t.Run(name, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "revocations")
			if err := os.Mkdir(dir, 0o700); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(dir, "state.json")
			if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := OpenRevocationState(path, []*x509.Certificate{issuer.Cert}, func() time.Time { return now }); err == nil {
				t.Fatal("malformed persisted state accepted")
			}
		})
	}

	dir := filepath.Join(t.TempDir(), "shared")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenRevocationState(filepath.Join(dir, "state.json"), []*x509.Certificate{issuer.Cert}, func() time.Time { return now }); err == nil {
		t.Fatal("shared revocation state directory accepted")
	}
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o755 {
		t.Fatalf("opening store changed shared directory mode to %o", info.Mode().Perm())
	}
}

func mustCRLPEM(t *testing.T, issuer *CA, number int64, now time.Time, serials ...*big.Int) []byte {
	t.Helper()
	entries := make([]x509.RevocationListEntry, 0, len(serials))
	for _, serial := range serials {
		entries = append(entries, x509.RevocationListEntry{SerialNumber: serial, RevocationTime: now.Add(-time.Minute)})
	}
	list, err := issuer.CreateCRL(big.NewInt(number), entries, now, now.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	return MarshalCRLPEM(list)
}
