// Package store is the CP's durable state layer: owners, apps/versions, and the spawn lifecycle
// index (spawns + the running-container episode entity), over Bun (SQLite embedded / Postgres).
package store

import (
	"context"
	"errors"
)

// ErrConflict is returned when a guarded transition's precondition (status set) is not met.
// ErrNotFound is returned for a missing or soft-deleted entity on a lifecycle lookup.
var (
	ErrConflict = errors.New("store: transition conflict")
	ErrNotFound = errors.New("store: not found")
)

// Config selects the backend. Driver is "sqlite" or "postgres".
type Config struct {
	Driver string
	DSN    string
}

// Store is the durable CP state layer. WithTx composes repos in one transaction.
// (Repo accessors Owners()/Apps()/Spawns() are added in later tasks.)
type Store interface {
	// WithTx runs fn in a transaction. If called inside an existing WithTx, fn runs in the
	// SAME transaction (flat composition — no savepoints; an inner error rolls back the whole tx).
	WithTx(ctx context.Context, fn func(tx Store) error) error
	Close() error
}
