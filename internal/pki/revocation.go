package pki

import (
	"bytes"
	"crypto/sha256"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

const revocationStateVersion = 1

var (
	ErrCRLRollback             = errors.New("pki: CRL rollback")
	ErrCRLConflict             = errors.New("pki: conflicting CRL number")
	ErrRevocationStateLocked   = errors.New("pki: revocation state is already open")
	ErrRevocationStateClosed   = errors.New("pki: revocation state is closed")
	ErrRevocationStatePoisoned = errors.New("pki: revocation state persistence is ambiguous")
)

type persistedRevocationState struct {
	Version int                         `json:"version"`
	Issuers []persistedIssuerRevocation `json:"issuers"`
}

type persistedIssuerRevocation struct {
	IssuerSerial string `json:"issuer_serial"`
	CRLPEM       string `json:"crl_pem"`
}

type issuerRevocationSnapshot struct {
	number  *big.Int
	digest  [sha256.Size]byte
	revoked map[string]struct{}
	pem     string
}

type revocationSnapshot struct {
	issuers    map[string]issuerRevocationSnapshot
	failClosed bool
}

// RevocationState owns a durable monotonic CRL checkpoint and publishes immutable reader snapshots.
type RevocationState struct {
	mu           sync.Mutex
	path         string
	issuers      map[string]*x509.Certificate
	now          func() time.Time
	snapshot     atomic.Pointer[revocationSnapshot]
	lockFile     *os.File
	closed       bool
	poisoned     error
	beforeRename func() error
	afterRename  func() error
}

// OpenRevocationState opens and revalidates the persisted CRLs for the configured trusted issuers.
func OpenRevocationState(path string, issuers []*x509.Certificate, now func() time.Time) (*RevocationState, error) {
	if path == "" || len(issuers) == 0 || now == nil {
		return nil, errors.New("pki: invalid revocation state configuration")
	}
	currentTime := now()
	if currentTime.IsZero() {
		return nil, errors.New("pki: revocation state clock returned zero time")
	}
	trusted := make(map[string]*x509.Certificate, len(issuers))
	for _, issuer := range issuers {
		if err := validateCRLIssuer(issuer); err != nil {
			return nil, fmt.Errorf("pki: reject revocation issuer: %w", err)
		}
		if currentTime.Before(issuer.NotBefore) || currentTime.After(issuer.NotAfter) {
			return nil, errors.New("pki: revocation issuer is not currently valid")
		}
		serial := issuer.SerialNumber.Text(16)
		if _, duplicate := trusted[serial]; duplicate {
			return nil, fmt.Errorf("pki: duplicate revocation issuer serial %s", serial)
		}
		trusted[serial] = issuer
	}
	dir := filepath.Dir(path)
	if err := ensurePrivateRevocationDirectory(dir); err != nil {
		return nil, err
	}
	lockFile, err := acquireRevocationStateLock(path + ".lock")
	if err != nil {
		return nil, err
	}
	releaseLock := true
	defer func() {
		if releaseLock {
			_ = releaseRevocationStateLock(lockFile)
		}
	}()
	state := &RevocationState{path: path, issuers: trusted, now: now, lockFile: lockFile}
	initial := &revocationSnapshot{issuers: make(map[string]issuerRevocationSnapshot)}
	record, err := readPersistedRevocationState(path)
	if errors.Is(err, os.ErrNotExist) {
		state.snapshot.Store(initial)
		releaseLock = false
		return state, nil
	}
	if err != nil {
		return nil, err
	}
	restored, err := state.restore(record, currentTime)
	if err != nil {
		return nil, err
	}
	state.snapshot.Store(restored)
	releaseLock = false
	return state, nil
}

// Close releases exclusive ownership. Readers retained after Close fail closed.
func (state *RevocationState) Close() error {
	if state == nil {
		return nil
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	if state.closed {
		return nil
	}
	state.closed = true
	state.publishFailClosed(state.snapshot.Load())
	err := releaseRevocationStateLock(state.lockFile)
	state.lockFile = nil
	return err
}

// ApplyPEM verifies and durably applies a newer CRL before publishing it to readers.
func (state *RevocationState) ApplyPEM(data []byte) error {
	if state == nil {
		return errors.New("pki: nil revocation state")
	}
	list, err := ParseCRLPEM(data)
	if err != nil {
		return err
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	if err := state.mutationErrorLocked(); err != nil {
		return err
	}
	currentTime := state.now()
	if currentTime.IsZero() {
		return errors.New("pki: revocation state clock returned zero time")
	}
	issuerSerial, issuer, err := state.issuerForCRL(list)
	if err != nil {
		return err
	}
	if err := VerifyCRL(list, issuer, currentTime); err != nil {
		return err
	}
	candidateEntry := snapshotFromCRL(list)
	current := state.snapshot.Load()
	if existing, ok := current.issuers[issuerSerial]; ok {
		switch list.Number.Cmp(existing.number) {
		case -1:
			return ErrCRLRollback
		case 0:
			if candidateEntry.digest == existing.digest {
				return nil
			}
			return ErrCRLConflict
		}
	}
	candidate := cloneRevocationSnapshot(current)
	candidate.issuers[issuerSerial] = candidateEntry
	renamed := false
	if err := state.persist(candidate, func() {
		state.snapshot.Store(candidate)
		renamed = true
	}); err != nil {
		if renamed {
			state.poisoned = fmt.Errorf("%w: %w", ErrRevocationStatePoisoned, err)
			state.publishFailClosed(candidate)
			return state.poisoned
		}
		return err
	}
	return nil
}

// IsRevoked reports membership in the latest CRL for issuer. A closed or poisoned view fails closed.
func (state *RevocationState) IsRevoked(issuer, serial *big.Int) bool {
	if state == nil || issuer == nil || serial == nil || issuer.Sign() <= 0 || serial.Sign() <= 0 {
		return false
	}
	snapshot := state.snapshot.Load()
	if snapshot == nil {
		return true
	}
	if snapshot.failClosed {
		return true
	}
	entry, ok := snapshot.issuers[issuer.Text(16)]
	if !ok {
		return false
	}
	_, revoked := entry.revoked[serial.Text(16)]
	return revoked
}

// HighestNumber returns a defensive copy of the highest accepted CRL number for issuer.
func (state *RevocationState) HighestNumber(issuer *big.Int) (*big.Int, bool) {
	if state == nil || issuer == nil || issuer.Sign() <= 0 {
		return nil, false
	}
	snapshot := state.snapshot.Load()
	if snapshot == nil {
		return nil, false
	}
	entry, ok := snapshot.issuers[issuer.Text(16)]
	if !ok {
		return nil, false
	}
	return new(big.Int).Set(entry.number), true
}

func (state *RevocationState) issuerForCRL(list *x509.RevocationList) (string, *x509.Certificate, error) {
	var matchedSerial string
	var matched *x509.Certificate
	for serial, issuer := range state.issuers {
		if !bytes.Equal(list.RawIssuer, issuer.RawSubject) || !bytes.Equal(list.AuthorityKeyId, issuer.SubjectKeyId) {
			continue
		}
		if err := list.CheckSignatureFrom(issuer); err != nil {
			continue
		}
		if matched != nil {
			return "", nil, errors.New("pki: CRL matches multiple configured issuers")
		}
		matchedSerial, matched = serial, issuer
	}
	if matched == nil {
		return "", nil, errors.New("pki: CRL was not signed by a configured issuer")
	}
	return matchedSerial, matched, nil
}

func (state *RevocationState) restore(record *persistedRevocationState, now time.Time) (*revocationSnapshot, error) {
	snapshot := &revocationSnapshot{issuers: make(map[string]issuerRevocationSnapshot, len(record.Issuers))}
	previousSerial := ""
	for _, persisted := range record.Issuers {
		if persisted.IssuerSerial == "" || persisted.CRLPEM == "" || persisted.IssuerSerial <= previousSerial {
			return nil, errors.New("pki: persisted revocation issuers are not canonical")
		}
		issuer, ok := state.issuers[persisted.IssuerSerial]
		if !ok {
			return nil, fmt.Errorf("pki: persisted CRL has unconfigured issuer %s", persisted.IssuerSerial)
		}
		list, err := ParseCRLPEM([]byte(persisted.CRLPEM))
		if err != nil {
			return nil, fmt.Errorf("pki: parse persisted CRL: %w", err)
		}
		serial, matched, err := state.issuerForCRL(list)
		if err != nil {
			return nil, fmt.Errorf("pki: identify persisted CRL issuer: %w", err)
		}
		if serial != persisted.IssuerSerial || matched != issuer {
			return nil, errors.New("pki: persisted CRL issuer serial mismatch")
		}
		if err := VerifyCRL(list, issuer, now); err != nil {
			return nil, fmt.Errorf("pki: verify persisted CRL: %w", err)
		}
		snapshot.issuers[serial] = snapshotFromCRL(list)
		previousSerial = persisted.IssuerSerial
	}
	return snapshot, nil
}

func snapshotFromCRL(list *x509.RevocationList) issuerRevocationSnapshot {
	revoked := make(map[string]struct{}, len(list.RevokedCertificateEntries))
	for _, entry := range list.RevokedCertificateEntries {
		revoked[entry.SerialNumber.Text(16)] = struct{}{}
	}
	return issuerRevocationSnapshot{
		number: new(big.Int).Set(list.Number), digest: sha256.Sum256(list.Raw), revoked: revoked, pem: string(MarshalCRLPEM(list)),
	}
}

func cloneRevocationSnapshot(current *revocationSnapshot) *revocationSnapshot {
	clone := &revocationSnapshot{issuers: make(map[string]issuerRevocationSnapshot, len(current.issuers)), failClosed: current.failClosed}
	for serial, entry := range current.issuers {
		clone.issuers[serial] = entry
	}
	return clone
}

func (state *RevocationState) publishFailClosed(current *revocationSnapshot) {
	if current == nil {
		current = &revocationSnapshot{issuers: make(map[string]issuerRevocationSnapshot)}
	}
	failed := cloneRevocationSnapshot(current)
	failed.failClosed = true
	state.snapshot.Store(failed)
}

func (state *RevocationState) mutationErrorLocked() error {
	if state.closed {
		return ErrRevocationStateClosed
	}
	if state.poisoned != nil {
		return state.poisoned
	}
	return nil
}

func (state *RevocationState) persist(snapshot *revocationSnapshot, renamed func()) error {
	serials := make([]string, 0, len(snapshot.issuers))
	for serial := range snapshot.issuers {
		serials = append(serials, serial)
	}
	sort.Strings(serials)
	record := persistedRevocationState{Version: revocationStateVersion, Issuers: make([]persistedIssuerRevocation, 0, len(serials))}
	for _, serial := range serials {
		record.Issuers = append(record.Issuers, persistedIssuerRevocation{IssuerSerial: serial, CRLPEM: snapshot.issuers[serial].pem})
	}
	encoded, err := json.Marshal(record)
	if err != nil {
		return fmt.Errorf("pki: marshal revocation state: %w", err)
	}
	encoded = append(encoded, '\n')
	dir := filepath.Dir(state.path)
	temporary, err := os.CreateTemp(dir, ".revocation-state-*")
	if err != nil {
		return fmt.Errorf("pki: create revocation state temporary file: %w", err)
	}
	temporaryPath := temporary.Name()
	removeTemporary := true
	defer func() {
		_ = temporary.Close()
		if removeTemporary {
			_ = os.Remove(temporaryPath)
		}
	}()
	if err := temporary.Chmod(0o600); err != nil {
		return fmt.Errorf("pki: set revocation state temporary permissions: %w", err)
	}
	if _, err := temporary.Write(encoded); err != nil {
		return fmt.Errorf("pki: write revocation state temporary file: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		return fmt.Errorf("pki: sync revocation state temporary file: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("pki: close revocation state temporary file: %w", err)
	}
	if state.beforeRename != nil {
		if err := state.beforeRename(); err != nil {
			return fmt.Errorf("pki: replace revocation state: %w", err)
		}
	}
	if err := os.Rename(temporaryPath, state.path); err != nil {
		return fmt.Errorf("pki: replace revocation state: %w", err)
	}
	removeTemporary = false
	renamed()
	if state.afterRename != nil {
		if err := state.afterRename(); err != nil {
			return fmt.Errorf("pki: sync revocation state directory: %w", err)
		}
	}
	directory, err := os.Open(dir)
	if err != nil {
		return fmt.Errorf("pki: open revocation state directory for sync: %w", err)
	}
	defer directory.Close()
	if err := directory.Sync(); err != nil {
		return fmt.Errorf("pki: sync revocation state directory: %w", err)
	}
	return nil
}

func ensurePrivateRevocationDirectory(path string) error {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		if err := os.MkdirAll(path, 0o700); err != nil {
			return fmt.Errorf("pki: create revocation state directory: %w", err)
		}
		if err := os.Chmod(path, 0o700); err != nil {
			return fmt.Errorf("pki: secure revocation state directory: %w", err)
		}
		info, err = os.Lstat(path)
	}
	if err != nil {
		return fmt.Errorf("pki: stat revocation state directory: %w", err)
	}
	if !info.IsDir() || info.Mode().Perm() != 0o700 {
		return errors.New("pki: revocation state directory is not private")
	}
	return nil
}

func acquireRevocationStateLock(path string) (*os.File, error) {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("pki: open revocation state lock: %w", err)
	}
	closeOnError := true
	defer func() {
		if closeOnError {
			_ = file.Close()
		}
	}()
	info, err := file.Stat()
	if err != nil {
		return nil, fmt.Errorf("pki: stat revocation state lock: %w", err)
	}
	if !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 {
		return nil, errors.New("pki: revocation state lock must be a regular 0600 file")
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
			return nil, ErrRevocationStateLocked
		}
		return nil, fmt.Errorf("pki: lock revocation state: %w", err)
	}
	closeOnError = false
	return file, nil
}

func releaseRevocationStateLock(file *os.File) error {
	if file == nil {
		return nil
	}
	unlockErr := syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
	closeErr := file.Close()
	if unlockErr != nil {
		return fmt.Errorf("pki: unlock revocation state: %w", unlockErr)
	}
	if closeErr != nil {
		return fmt.Errorf("pki: close revocation state lock: %w", closeErr)
	}
	return nil
}

func readPersistedRevocationState(path string) (*persistedRevocationState, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 {
		return nil, errors.New("pki: persisted revocation state must be a regular 0600 file")
	}
	if info.Size() > 16<<20 {
		return nil, errors.New("pki: persisted revocation state is too large")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("pki: read persisted revocation state: %w", err)
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	var record persistedRevocationState
	if err := decoder.Decode(&record); err != nil {
		return nil, fmt.Errorf("pki: decode persisted revocation state: %w", err)
	}
	if err := requireRevocationJSONEOF(decoder); err != nil {
		return nil, err
	}
	if record.Version != revocationStateVersion || record.Issuers == nil {
		return nil, errors.New("pki: invalid persisted revocation state")
	}
	return &record, nil
}

func requireRevocationJSONEOF(decoder *json.Decoder) error {
	var extra any
	if err := decoder.Decode(&extra); errors.Is(err, io.EOF) {
		return nil
	} else if err != nil {
		return fmt.Errorf("pki: decode persisted revocation state trailing data: %w", err)
	}
	return errors.New("pki: persisted revocation state has trailing data")
}
