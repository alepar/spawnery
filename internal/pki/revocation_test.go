package pki

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"crypto/x509"
	"errors"
	"fmt"
	"math/big"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
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
	if !state.IsRevoked(service.Cert.SerialNumber, serial1) || !state.IsRevoked(cloud.Cert.SerialNumber, serial1) {
		t.Fatal("revocation was not scoped or missing issuer snapshot did not fail closed")
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

func TestOpenRevocationStateRejectsIssuersOutsideAggregateBound(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	root, _ := NewRootCA("root")
	service, _ := root.NewIntermediate(IssuerService, "prod.spawnery.internal")
	secondService, _ := root.NewIntermediate(IssuerService, "prod.spawnery.internal")
	cloud, _ := root.NewIntermediate(IssuerCloudNode, "prod.spawnery.internal")
	selfHosted, _ := root.NewIntermediate(IssuerSelfHostedNode, "prod.spawnery.internal")
	oversizedSerial := *service.Cert
	oversizedSerial.SerialNumber = new(big.Int).Lsh(big.NewInt(1), 160)

	for _, test := range []struct {
		name    string
		issuers []*x509.Certificate
	}{
		{name: "fourth issuer", issuers: []*x509.Certificate{service.Cert, cloud.Cert, selfHosted.Cert, secondService.Cert}},
		{name: "duplicate role", issuers: []*x509.Certificate{service.Cert, secondService.Cert}},
		{name: "oversized serial", issuers: []*x509.Certificate{&oversizedSerial}},
	} {
		t.Run(test.name, func(t *testing.T) {
			state, err := OpenRevocationState(filepath.Join(t.TempDir(), "revocations", "state.json"), test.issuers, func() time.Time { return now })
			if err == nil {
				_ = state.Close()
				t.Fatal("issuer set outside aggregate bound accepted")
			}
		})
	}
}

func TestRevocationStateFailsClosedWhenSnapshotExpiresAndRecoversOnRefresh(t *testing.T) {
	base := time.Now().UTC().Truncate(time.Second)
	var clock atomic.Int64
	clock.Store(base.Unix())
	now := func() time.Time { return time.Unix(clock.Load(), 0).UTC() }
	root, _ := NewRootCA("root")
	issuer, _ := root.NewIntermediate(IssuerService, "prod.spawnery.internal")
	state, err := OpenRevocationState(filepath.Join(t.TempDir(), "revocations", "state.json"), []*x509.Certificate{issuer.Cert}, now)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = state.Close() })
	unlisted := big.NewInt(900)
	if !state.IsRevoked(issuer.Cert.SerialNumber, unlisted) {
		t.Fatal("missing CRL snapshot did not fail closed")
	}
	first, err := issuer.CreateCRL(big.NewInt(1), nil, base, base.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if err := state.ApplyPEM(MarshalCRLPEM(first)); err != nil {
		t.Fatal(err)
	}
	if state.IsRevoked(issuer.Cert.SerialNumber, unlisted) {
		t.Fatal("current CRL rejected an unlisted serial")
	}
	clock.Store(base.Add(time.Minute).Unix())
	if !state.IsRevoked(issuer.Cert.SerialNumber, unlisted) {
		t.Fatal("expired CRL snapshot did not fail closed at NextUpdate")
	}
	fresh, err := issuer.CreateCRL(big.NewInt(2), nil, base.Add(time.Minute), base.Add(2*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if err := state.ApplyPEM(MarshalCRLPEM(fresh)); err != nil {
		t.Fatal(err)
	}
	if state.IsRevoked(issuer.Cert.SerialNumber, unlisted) {
		t.Fatal("fresh higher CRL did not restore normal lookup")
	}
	clock.Store(base.Add(30 * time.Second).Unix())
	if !state.IsRevoked(issuer.Cert.SerialNumber, unlisted) {
		t.Fatal("clock rollback before ThisUpdate did not fail closed")
	}
	clock.Store(base.Add(time.Minute).Unix())
	if state.IsRevoked(issuer.Cert.SerialNumber, unlisted) {
		t.Fatal("restoring a time within the CRL window did not restore lookup")
	}
}

func TestRevocationStateReopensExpiredCheckpointAndPreservesRollbackFloor(t *testing.T) {
	base := time.Now().UTC().Truncate(time.Second)
	root, _ := NewRootCA("root")
	issuer, _ := root.NewIntermediate(IssuerService, "prod.spawnery.internal")
	path := filepath.Join(t.TempDir(), "revocations", "state.json")
	state, err := OpenRevocationState(path, []*x509.Certificate{issuer.Cert}, func() time.Time { return base })
	if err != nil {
		t.Fatal(err)
	}
	expiring, err := issuer.CreateCRL(big.NewInt(5), nil, base, base.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if err := state.ApplyPEM(MarshalCRLPEM(expiring)); err != nil {
		t.Fatal(err)
	}
	if err := state.Close(); err != nil {
		t.Fatal(err)
	}

	recoveryTime := base.Add(2 * time.Minute)
	reopened, err := OpenRevocationState(path, []*x509.Certificate{issuer.Cert}, func() time.Time { return recoveryTime })
	if err != nil {
		t.Fatalf("reopen expired checkpoint: %v", err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	if got, ok := reopened.HighestNumber(issuer.Cert.SerialNumber); !ok || got.Cmp(big.NewInt(5)) != 0 {
		t.Fatalf("expired checkpoint floor = %v, %v", got, ok)
	}
	if !reopened.IsRevoked(issuer.Cert.SerialNumber, big.NewInt(99)) {
		t.Fatal("expired recovery checkpoint did not fail closed")
	}
	rollback, err := issuer.CreateCRL(big.NewInt(4), nil, recoveryTime, recoveryTime.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if err := reopened.ApplyPEM(MarshalCRLPEM(rollback)); !errors.Is(err, ErrCRLRollback) {
		t.Fatalf("recovery rollback error = %v", err)
	}
	fresh, err := issuer.CreateCRL(big.NewInt(6), nil, recoveryTime, recoveryTime.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if err := reopened.ApplyPEM(MarshalCRLPEM(fresh)); err != nil {
		t.Fatal(err)
	}
	if reopened.IsRevoked(issuer.Cert.SerialNumber, big.NewInt(99)) {
		t.Fatal("higher current CRL did not leave recovery mode")
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

func TestRevocationStatePublishesOnlyNewlyRevokedAfterDurableApply(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	root, _ := NewRootCA("root")
	issuer, _ := root.NewIntermediate(IssuerService, "prod.spawnery.internal")
	state, err := OpenRevocationState(filepath.Join(t.TempDir(), "revocations", "state.json"), []*x509.Certificate{issuer.Cert}, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = state.Close() })
	updates := make(chan RevocationUpdate, 4)
	unsubscribe := state.SubscribeRevocations(func(update RevocationUpdate) { updates <- update })
	t.Cleanup(unsubscribe)

	state.beforeRename = func() error { return errors.New("disk unavailable") }
	accepted := mustCRLPEM(t, issuer, 1, now, big.NewInt(1))
	if err := state.ApplyPEM(accepted); err == nil {
		t.Fatal("persistence failure accepted")
	}
	select {
	case update := <-updates:
		t.Fatalf("published before durable apply: %+v", update)
	case <-time.After(20 * time.Millisecond):
	}
	state.beforeRename = nil
	if err := state.ApplyPEM(accepted); err != nil {
		t.Fatal(err)
	}
	assertRevocationUpdate(t, updates, issuer.Cert.SerialNumber, big.NewInt(1))
	if err := state.ApplyPEM(accepted); err != nil {
		t.Fatal(err)
	}
	select {
	case update := <-updates:
		t.Fatalf("idempotent replay published: %+v", update)
	case <-time.After(20 * time.Millisecond):
	}
	if err := state.ApplyPEM(mustCRLPEM(t, issuer, 2, now, big.NewInt(1), big.NewInt(2))); err != nil {
		t.Fatal(err)
	}
	assertRevocationUpdate(t, updates, issuer.Cert.SerialNumber, big.NewInt(2))
}

func TestRevocationStateSubscriptionUnsubscribeCloseAndPanic(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	root, _ := NewRootCA("root")
	issuer, _ := root.NewIntermediate(IssuerService, "prod.spawnery.internal")
	state, err := OpenRevocationState(filepath.Join(t.TempDir(), "revocations", "state.json"), []*x509.Certificate{issuer.Cert}, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	updates := make(chan RevocationUpdate, 2)
	state.SubscribeRevocations(func(RevocationUpdate) { panic("contained") })
	unsubscribe := state.SubscribeRevocations(func(update RevocationUpdate) { updates <- update })
	unsubscribe()
	unsubscribe()
	if err := state.ApplyPEM(mustCRLPEM(t, issuer, 1, now, big.NewInt(1))); err != nil {
		t.Fatal(err)
	}
	select {
	case update := <-updates:
		t.Fatalf("unsubscribed callback published: %+v", update)
	case <-time.After(20 * time.Millisecond):
	}
	if err := state.Close(); err != nil {
		t.Fatal(err)
	}
	if err := state.ApplyPEM(mustCRLPEM(t, issuer, 2, now, big.NewInt(2))); !errors.Is(err, ErrRevocationStateClosed) {
		t.Fatalf("apply after close error = %v", err)
	}
}

func TestRevocationStateApplyDoesNotWaitForSubscriber(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	root, _ := NewRootCA("root")
	issuer, _ := root.NewIntermediate(IssuerService, "prod.spawnery.internal")
	state, err := OpenRevocationState(filepath.Join(t.TempDir(), "revocations", "state.json"), []*x509.Certificate{issuer.Cert}, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = state.Close() })
	started := make(chan struct{})
	unblock := make(chan struct{})
	state.SubscribeRevocations(func(RevocationUpdate) {
		close(started)
		<-unblock
	})
	crl := mustCRLPEM(t, issuer, 1, now, big.NewInt(1))
	applyDone := make(chan error, 1)
	go func() {
		applyDone <- state.ApplyPEM(crl)
	}()
	select {
	case err := <-applyDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("ApplyPEM blocked on subscriber")
	}
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("subscriber was not invoked")
	}
	close(unblock)
}

func assertRevocationUpdate(t *testing.T, updates <-chan RevocationUpdate, issuer, serial *big.Int) {
	t.Helper()
	select {
	case update := <-updates:
		if update.IssuerSerial.Cmp(issuer) != 0 || len(update.NewlyRevoked) != 1 || update.NewlyRevoked[0].Cmp(serial) != 0 {
			t.Fatalf("update = %+v, want issuer %s serial %s", update, issuer, serial)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for revocation update")
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

func TestRevocationStateSubprocessLockReleasedAfterCrash(t *testing.T) {
	const helperEnv = "SPAWNERY_TEST_REVOCATION_LOCK_HELPER"
	if path := os.Getenv(helperEnv); path != "" {
		lock, err := acquireRevocationStateLock(path)
		if err != nil {
			fmt.Printf("error:%v\n", err)
			return
		}
		_ = lock
		fmt.Println("locked")
		select {}
	}

	path := filepath.Join(t.TempDir(), "state.lock")
	cmd := exec.Command(os.Args[0], "-test.run=^TestRevocationStateSubprocessLockReleasedAfterCrash$")
	cmd.Env = append(os.Environ(), helperEnv+"="+path)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
		_ = cmd.Wait()
	})
	ready := make(chan string, 1)
	go func() {
		scanner := bufio.NewScanner(stdout)
		if scanner.Scan() {
			ready <- scanner.Text()
			return
		}
		ready <- ""
	}()
	select {
	case line := <-ready:
		if line != "locked" {
			t.Fatalf("lock helper output = %q, stderr = %q", line, stderr.String())
		}
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for lock helper")
	}
	if _, err := acquireRevocationStateLock(path); !errors.Is(err, ErrRevocationStateLocked) {
		t.Fatalf("parent lock contention error = %v", err)
	}
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Wait(); err == nil {
		t.Fatal("crashed lock helper exited successfully")
	}
	cmd.Process = nil
	lock, err := acquireRevocationStateLock(path)
	if err != nil {
		t.Fatalf("lock not released after helper crash: %v", err)
	}
	if err := releaseRevocationStateLock(lock); err != nil {
		t.Fatal(err)
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
	recovery, err := OpenRevocationState(path, []*x509.Certificate{issuer.Cert}, func() time.Time { return now.Add(2 * time.Hour) })
	if err != nil {
		t.Fatalf("expired persisted CRL did not reopen for recovery: %v", err)
	}
	if !recovery.IsRevoked(issuer.Cert.SerialNumber, big.NewInt(999)) {
		t.Fatal("expired persisted CRL recovery did not fail closed")
	}
	if err := recovery.Close(); err != nil {
		t.Fatal(err)
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

func TestRevocationStateSizeLimitBeforeReadAndRename(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	root, _ := NewRootCA("root")
	issuer, _ := root.NewIntermediate(IssuerService, "prod.spawnery.internal")
	dir := filepath.Join(t.TempDir(), "revocations")
	state, err := OpenRevocationState(filepath.Join(dir, "state.json"), []*x509.Certificate{issuer.Cert}, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = state.Close() })
	oversized := &revocationSnapshot{issuers: map[string]issuerRevocationSnapshot{
		issuer.Cert.SerialNumber.Text(16): {number: big.NewInt(1), pem: strings.Repeat("x", maxRevocationStateSize)},
	}}
	renameReached := false
	state.beforeRename = func() error { renameReached = true; return nil }
	if err := state.persist(oversized, func() {}); !errors.Is(err, ErrRevocationStateTooLarge) {
		t.Fatalf("oversized candidate error = %v", err)
	}
	if renameReached {
		t.Fatal("oversized candidate reached rename")
	}

	path := filepath.Join(dir, "oversized.json")
	if err := os.WriteFile(path, bytes.Repeat([]byte{'x'}, maxRevocationStateSize), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := readPersistedRevocationState(path); errors.Is(err, ErrRevocationStateTooLarge) {
		t.Fatal("persisted state at size boundary reported too large")
	}
	if err := os.WriteFile(path, bytes.Repeat([]byte{'x'}, maxRevocationStateSize+1), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := readPersistedRevocationState(path); !errors.Is(err, ErrRevocationStateTooLarge) {
		t.Fatalf("oversized persisted state error = %v", err)
	}
}

func TestRevocationStatePersistsMaximumCRLsForEveryIssuerRole(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	root, _ := NewRootCA("root")
	roles := []IssuerRole{IssuerService, IssuerCloudNode, IssuerSelfHostedNode}
	issuers := make([]*CA, 0, len(roles))
	certificates := make([]*x509.Certificate, 0, len(roles))
	for _, role := range roles {
		issuer, err := root.NewIntermediate(role, "prod.spawnery.internal")
		if err != nil {
			t.Fatal(err)
		}
		issuers = append(issuers, issuer)
		certificates = append(certificates, issuer.Cert)
	}
	path := filepath.Join(t.TempDir(), "revocations", "state.json")
	state, err := OpenRevocationState(path, certificates, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	for index, issuer := range issuers {
		if err := state.ApplyPEM(mustNearMaximumCRLPEM(t, issuer, int64(index+1), now)); err != nil {
			_ = state.Close()
			t.Fatalf("apply %s CRL: %v", roles[index], err)
		}
	}
	if err := state.Close(); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() <= 16<<20 {
		t.Fatalf("three-role boundary fixture is only %d bytes", info.Size())
	}
	reopened, err := OpenRevocationState(path, certificates, func() time.Time { return now })
	if err != nil {
		t.Fatalf("reopen three-role boundary state: %v", err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	for index, issuer := range issuers {
		if number, ok := reopened.HighestNumber(issuer.Cert.SerialNumber); !ok || number.Cmp(big.NewInt(int64(index+1))) != 0 {
			t.Fatalf("reopened %s CRL number = %v, %v", roles[index], number, ok)
		}
	}
}

func mustNearMaximumCRLPEM(t *testing.T, issuer *CA, number int64, now time.Time) []byte {
	t.Helper()
	const maximumEntries = 110_000
	entries := make([]x509.RevocationListEntry, maximumEntries)
	serialBase := new(big.Int).Lsh(big.NewInt(1), 152)
	for index := range entries {
		entries[index] = x509.RevocationListEntry{
			SerialNumber:   new(big.Int).Add(serialBase, big.NewInt(int64(index+1))),
			RevocationTime: now.Add(-time.Minute),
		}
	}
	var der []byte
	low, high := 0, len(entries)+1
	for low+1 < high {
		count := low + (high-low)/2
		candidate, err := x509.CreateRevocationList(rand.Reader, &x509.RevocationList{
			Number:                    big.NewInt(number),
			ThisUpdate:                now,
			NextUpdate:                now.Add(time.Hour),
			RevokedCertificateEntries: entries[:count],
		}, issuer.Cert, issuer.Key)
		if err != nil {
			t.Fatal(err)
		}
		if len(candidate) <= maxCRLDERSize {
			low, der = count, candidate
		} else {
			high = count
		}
	}
	if len(der) <= maxCRLDERSize-128 || len(der) > maxCRLDERSize {
		t.Fatalf("near-maximum CRL DER length = %d", len(der))
	}
	list, err := x509.ParseRevocationList(der)
	if err != nil {
		t.Fatal(err)
	}
	return MarshalCRLPEM(list)
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
