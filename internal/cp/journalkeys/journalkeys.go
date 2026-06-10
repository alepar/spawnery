// Package journalkeys is the CP-side custody of owner-sealed JOURNAL-PASSWORD
// CIPHERTEXT (transient-tier design §4, sp-u53.5.4). The CP is never a journal-key
// custodian in plaintext: it stores ONLY the opaque owner-sealed Envelope bytes
// per (spawn, mount), relays them to a verified owner client on a resume, and
// forwards the owner's re-sealed delivery to the hosting node — exactly the
// ciphertext-only relay role the merged secret-delivery path already plays
// (internal/cp/secrets.go) for tmpfs secrets.
//
// Two pieces live here:
//
//   - Store: the per-(spawn,mount) ciphertext store. Put on the node-local ->
//     owner-sealed upgrade (the node seals the repo password to the owner and
//     uploads the Envelope); Get on a resume (the owner client fetches it to
//     unseal + re-seal to the target node). An in-memory implementation is
//     provided; a durable (Bun) backing rides the migrate slice (sp-u53.5.3).
//
//   - OwnerDeviceRegistry: the SEAM for fetching an owner's enrolled device HPKE
//     pubkeys (the set the password is sealed to). A server-side owner device
//     registry is NOT yet wired (the device-set log primitive exists in
//     internal/secrets/seal, but no CP store fronts it), so the default
//     implementation returns ErrRegistryNotWired; tests inject a static set and
//     the migrate slice wires the real registry. This is the documented
//     wired-vs-seam boundary for sp-u53.5.4.
//
// NOTE: this slice deliberately does NOT add Put/Get RPCs or touch the lifecycle
// state machine — those belong to MigrateSpawn (sp-u53.5.3, the consumer). This
// package is the storage + seam it will build on, kept independently testable.
package journalkeys

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"spawnery/internal/secrets/seal"
)

// ErrNotFound is returned by Store.Get when no ciphertext exists for the key.
var ErrNotFound = errors.New("journalkeys: no owner-sealed ciphertext for (spawn, secret)")

// ErrRegistryNotWired is returned by the default OwnerDeviceRegistry: a
// server-side owner device registry is not yet wired (see package doc). Callers
// that need owner device pubkeys must inject a real (or, in tests, a static)
// registry.
var ErrRegistryNotWired = errors.New("journalkeys: owner device registry not wired (provide an OwnerDeviceRegistry)")

// Store custodies owner-sealed journal-password ciphertext per (spawn, secret).
// secretID uses the journalkey.SecretID(mount) convention. The stored bytes are
// an opaque, CP-unreadable seal.Envelope (JSON); the CP holds no key to open it.
type Store interface {
	// Put stores (overwrites) the ciphertext for (spawnID, secretID).
	Put(ctx context.Context, spawnID, secretID string, ciphertext []byte) error
	// Get returns the stored ciphertext, or ErrNotFound.
	Get(ctx context.Context, spawnID, secretID string) ([]byte, error)
	// Delete drops ALL journal-key ciphertext for spawnID (spawn delete /
	// migrate-away cleanup). Absent is success.
	Delete(ctx context.Context, spawnID string) error
}

// MemStore is an in-memory, concurrency-safe Store for hermetic tests and the
// pre-durable phase. Ciphertext only; never plaintext.
type MemStore struct {
	mu sync.Mutex
	m  map[string]map[string][]byte // spawnID -> secretID -> ciphertext
}

// NewMemStore builds an empty in-memory store.
func NewMemStore() *MemStore { return &MemStore{m: map[string]map[string][]byte{}} }

var _ Store = (*MemStore)(nil)

// Put implements Store.
func (s *MemStore) Put(_ context.Context, spawnID, secretID string, ciphertext []byte) error {
	if spawnID == "" || secretID == "" {
		return fmt.Errorf("journalkeys: empty spawnID or secretID")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	byMount, ok := s.m[spawnID]
	if !ok {
		byMount = map[string][]byte{}
		s.m[spawnID] = byMount
	}
	byMount[secretID] = append([]byte(nil), ciphertext...)
	return nil
}

// Get implements Store.
func (s *MemStore) Get(_ context.Context, spawnID, secretID string) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ct, ok := s.m[spawnID][secretID]
	if !ok {
		return nil, ErrNotFound
	}
	return append([]byte(nil), ct...), nil
}

// Delete implements Store.
func (s *MemStore) Delete(_ context.Context, spawnID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.m, spawnID)
	return nil
}

// OwnerDeviceRegistry resolves an owner's enrolled device HPKE (X25519) pubkeys —
// the recipient set a journal password is sealed to. It is the seam between this
// custody and the owner device-set registry (internal/secrets/seal device-set
// log); see package doc for the wired-vs-seam status.
type OwnerDeviceRegistry interface {
	// Devices returns the X25519 pubkeys of ownerID's currently enrolled devices.
	Devices(ctx context.Context, ownerID string) ([]seal.X25519PubKey, error)
}

// UnwiredRegistry is the default OwnerDeviceRegistry: it returns
// ErrRegistryNotWired for every owner. Production / the migrate slice injects a
// registry backed by the verified owner device-set; tests inject StaticRegistry.
type UnwiredRegistry struct{}

// Devices always returns ErrRegistryNotWired.
func (UnwiredRegistry) Devices(context.Context, string) ([]seal.X25519PubKey, error) {
	return nil, ErrRegistryNotWired
}

// StaticRegistry is a fixed owner-> devices map for tests and single-owner
// deployments.
type StaticRegistry map[string][]seal.X25519PubKey

// Devices returns the configured pubkeys for ownerID (ErrRegistryNotWired if the
// owner is unknown, so an unconfigured owner fails closed rather than sealing to
// an empty recipient set).
func (r StaticRegistry) Devices(_ context.Context, ownerID string) ([]seal.X25519PubKey, error) {
	devs, ok := r[ownerID]
	if !ok || len(devs) == 0 {
		return nil, ErrRegistryNotWired
	}
	return devs, nil
}
