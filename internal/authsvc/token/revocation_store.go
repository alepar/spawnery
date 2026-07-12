package token

import (
	"bytes"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const signerRevocationStoreVersion = 1

type persistedSignerRevocation struct {
	Version  int    `json:"version"`
	Envelope string `json:"envelope"`
	SHA256   string `json:"sha256"`
}

// SignerRevocationStore durably owns one monotonic signer-revocation verifier view.
type SignerRevocationStore struct {
	mu           sync.Mutex
	path         string
	root         *x509.Certificate
	environment  string
	state        *SignerRevocationState
	beforeRename func() error
}

// OpenSignerRevocationStore opens and cryptographically revalidates persisted state. A missing file
// starts at generation zero; malformed, untrusted, or permissively-mode files fail closed.
func OpenSignerRevocationStore(path string, root *x509.Certificate, environment string, now time.Time) (*SignerRevocationStore, error) {
	if path == "" || root == nil || len(root.Raw) == 0 || !root.IsCA || environment == "" {
		return nil, errors.New("token: invalid signer-revocation store configuration")
	}
	dir := filepath.Dir(path)
	if err := ensurePrivateDirectory(dir); err != nil {
		return nil, err
	}
	state, err := NewSignerRevocationState(root, environment)
	if err != nil {
		return nil, err
	}
	store := &SignerRevocationStore{path: path, root: root, environment: environment, state: state}
	record, err := readPersistedSignerRevocation(path)
	if errors.Is(err, os.ErrNotExist) {
		return store, nil
	}
	if err != nil {
		return nil, err
	}
	statement, err := ParseSignerRevocationStatement(record.Envelope, root, environment, now)
	if err != nil {
		return nil, fmt.Errorf("token: revalidate persisted signer revocation: %w", err)
	}
	if err := state.Apply(statement); err != nil {
		return nil, fmt.Errorf("token: restore signer revocation state: %w", err)
	}
	return store, nil
}

func (store *SignerRevocationStore) Generation() uint64 {
	if store == nil {
		return 0
	}
	return store.state.Generation()
}

func (store *SignerRevocationStore) RejectSigner(leaf *x509.Certificate) error {
	if store == nil {
		return nil
	}
	return store.state.RejectSigner(leaf)
}

// Apply durably replaces the accepted statement before publishing it to artifact verifiers.
func (store *SignerRevocationStore) Apply(statement *SignerRevocationStatement) error {
	if store == nil || statement == nil {
		return ErrMalformed
	}
	store.mu.Lock()
	defer store.mu.Unlock()

	candidate, changed, err := store.state.prepare(statement)
	if err != nil || !changed {
		return err
	}
	if err := store.persist(statement); err != nil {
		return err
	}
	store.state.publish(candidate)
	return nil
}

// LoadAndApply reads a deployment-provided envelope and applies it to the persisted store. A
// missing configured statement is permitted only before any generation has been accepted.
func (store *SignerRevocationStore) LoadAndApply(path string, now time.Time) error {
	if store == nil || path == "" {
		return errors.New("token: invalid signer-revocation statement path")
	}
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		if store.Generation() == 0 {
			return nil
		}
		return errors.New("token: configured signer-revocation statement disappeared")
	}
	if err != nil {
		return fmt.Errorf("token: read configured signer-revocation statement: %w", err)
	}
	wire := strings.TrimSpace(string(raw))
	statement, err := ParseSignerRevocationStatement(wire, store.root, store.environment, now)
	if err != nil {
		return fmt.Errorf("token: parse configured signer-revocation statement: %w", err)
	}
	return store.Apply(statement)
}

func (store *SignerRevocationStore) persist(statement *SignerRevocationStatement) error {
	record := persistedSignerRevocation{
		Version: signerRevocationStoreVersion, Envelope: statement.canonical.wire, SHA256: hex.EncodeToString(statement.canonical.digest[:]),
	}
	encoded, err := json.Marshal(record)
	if err != nil {
		return fmt.Errorf("token: marshal signer-revocation state: %w", err)
	}
	encoded = append(encoded, '\n')
	dir := filepath.Dir(store.path)
	temporary, err := os.CreateTemp(dir, ".signer-revocation-*")
	if err != nil {
		return fmt.Errorf("token: create signer-revocation temporary file: %w", err)
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
		return fmt.Errorf("token: set signer-revocation temporary permissions: %w", err)
	}
	if _, err := temporary.Write(encoded); err != nil {
		return fmt.Errorf("token: write signer-revocation temporary file: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		return fmt.Errorf("token: sync signer-revocation temporary file: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("token: close signer-revocation temporary file: %w", err)
	}
	if store.beforeRename != nil {
		if err := store.beforeRename(); err != nil {
			return fmt.Errorf("token: replace signer-revocation state: %w", err)
		}
	}
	if err := os.Rename(temporaryPath, store.path); err != nil {
		return fmt.Errorf("token: replace signer-revocation state: %w", err)
	}
	removeTemporary = false
	directory, err := os.Open(dir)
	if err != nil {
		return fmt.Errorf("token: open signer-revocation directory for sync: %w", err)
	}
	defer directory.Close()
	if err := directory.Sync(); err != nil {
		return fmt.Errorf("token: sync signer-revocation directory: %w", err)
	}
	return nil
}

func ensurePrivateDirectory(path string) error {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		if err := os.MkdirAll(path, 0o700); err != nil {
			return fmt.Errorf("token: create signer-revocation directory: %w", err)
		}
		if err := os.Chmod(path, 0o700); err != nil {
			return fmt.Errorf("token: secure signer-revocation directory: %w", err)
		}
		info, err = os.Lstat(path)
	}
	if err != nil {
		return fmt.Errorf("token: stat signer-revocation directory: %w", err)
	}
	if !info.IsDir() || info.Mode().Perm() != 0o700 {
		return errors.New("token: signer-revocation directory is not private")
	}
	return nil
}

func readPersistedSignerRevocation(path string) (*persistedSignerRevocation, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 {
		return nil, errors.New("token: persisted signer-revocation state must be a regular 0600 file")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("token: read persisted signer-revocation state: %w", err)
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	var record persistedSignerRevocation
	if err := decoder.Decode(&record); err != nil {
		return nil, fmt.Errorf("token: decode persisted signer-revocation state: %w", err)
	}
	if err := requireJSONEOF(decoder); err != nil {
		return nil, err
	}
	if record.Version != signerRevocationStoreVersion || record.Envelope == "" || len(record.SHA256) != sha256.Size*2 {
		return nil, errors.New("token: invalid persisted signer-revocation state")
	}
	envelopeRaw, err := base64.RawURLEncoding.DecodeString(record.Envelope)
	if err != nil || base64.RawURLEncoding.EncodeToString(envelopeRaw) != record.Envelope {
		return nil, errors.New("token: invalid persisted signer-revocation envelope")
	}
	wantDigest, err := hex.DecodeString(record.SHA256)
	if err != nil || len(wantDigest) != sha256.Size {
		return nil, errors.New("token: invalid persisted signer-revocation checksum")
	}
	gotDigest := sha256.Sum256(envelopeRaw)
	if !bytes.Equal(wantDigest, gotDigest[:]) {
		return nil, errors.New("token: persisted signer-revocation checksum mismatch")
	}
	return &record, nil
}

func requireJSONEOF(decoder *json.Decoder) error {
	var extra any
	if err := decoder.Decode(&extra); errors.Is(err, io.EOF) {
		return nil
	} else if err != nil {
		return fmt.Errorf("token: decode persisted signer-revocation trailing data: %w", err)
	}
	return errors.New("token: persisted signer-revocation state has trailing data")
}
