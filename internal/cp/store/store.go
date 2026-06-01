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

type OwnerRepo interface {
	Get(ctx context.Context, id string) (Owner, error)
	Upsert(ctx context.Context, o Owner) error
}

type AppRepo interface {
	Get(ctx context.Context, id string) (App, error)
	List(ctx context.Context) ([]App, error)
	Upsert(ctx context.Context, a App) error
	UpsertVersion(ctx context.Context, v AppVersion, mounts []MountDecl) error
	GetVersion(ctx context.Context, appID, version string) (AppVersion, error)
	LatestReviewed(ctx context.Context, appID string) (AppVersion, error)
	DeclaredMounts(ctx context.Context, appID, version string) ([]MountDecl, error)
}

type SpawnRepo interface {
	Create(ctx context.Context, s Spawn, mounts []Mount) error
	Get(ctx context.Context, id string) (Spawn, error) // ErrNotFound on missing OR deleted
	LiveContainer(ctx context.Context, id string) (Container, bool, error)
	GetMounts(ctx context.Context, id string) ([]Mount, error)
	ListByOwner(ctx context.Context, ownerID string) ([]Spawn, error)

	ClaimStarting(ctx context.Context, id string, from []Status) (newGen int64, err error)
	SetActive(ctx context.Context, id, nodeID string, gen int64) error
	SetSuspending(ctx context.Context, id string, gen int64) error
	SetMountMarker(ctx context.Context, id, mount, marker string) error
	SetSuspended(ctx context.Context, id string, gen int64) error
	SetError(ctx context.Context, id string) error
	EndContainer(ctx context.Context, id string, gen int64, p Phase) error
	MarkUnreachable(ctx context.Context, ids []string) (int, error)
	MarkBootUnreachable(ctx context.Context) (int, error)
	MarkRecovered(ctx context.Context, id string) error
	Touch(ctx context.Context, id string, ts int64) error
	MarkDeleted(ctx context.Context, id string, ts int64) error

	LiveContainersByNode(ctx context.Context, nodeID string) ([]Container, error)
	Adopt(ctx context.Context, id, nodeID string, gen int64) error
}

type Store interface {
	Owners() OwnerRepo
	Apps() AppRepo
	Spawns() SpawnRepo
	// WithTx runs fn in a transaction. If called inside an existing WithTx, fn runs in the
	// SAME transaction (flat composition — no savepoints; an inner error rolls back the whole tx).
	WithTx(ctx context.Context, fn func(tx Store) error) error
	Close() error
}
