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

// CatalogEntry is one browse row: an app plus its newest version's tier/version.
type CatalogEntry struct {
	App           App
	LatestVersion string
	LatestTier    Tier
}

// CatalogFilter narrows a catalog browse. Query is a case-insensitive substring over
// display_name + summary + tags; empty Query browses all listed+public apps.
type CatalogFilter struct {
	Query string
}

type AppRepo interface {
	Get(ctx context.Context, id string) (App, error)
	Creator(ctx context.Context, appID string) (string, error)
	List(ctx context.Context) ([]App, error)
	Upsert(ctx context.Context, a App) error
	UpsertVersion(ctx context.Context, v AppVersion, mounts []MountDecl) error
	GetVersion(ctx context.Context, appID, version string) (AppVersion, error)
	LatestReviewed(ctx context.Context, appID string) (AppVersion, error)
	DeclaredMounts(ctx context.Context, appID, version string) ([]MountDecl, error)
	Catalog(ctx context.Context, f CatalogFilter) ([]CatalogEntry, error)
	ListByCreator(ctx context.Context, creatorID string) ([]CatalogEntry, error)
	AppDetail(ctx context.Context, id string) (App, []AppVersion, error)
	SetListed(ctx context.Context, appID string, listed bool) error
}

type SpawnRepo interface {
	Create(ctx context.Context, s Spawn, mounts []Mount) error
	Get(ctx context.Context, id string) (Spawn, error) // ErrNotFound on missing OR deleted
	LiveContainer(ctx context.Context, id string) (Container, bool, error)
	GetMounts(ctx context.Context, id string) ([]Mount, error)
	ListByOwner(ctx context.Context, ownerID string) ([]Spawn, error)
	Rename(ctx context.Context, id, name string) error    // ErrNotFound on missing OR deleted
	SetModel(ctx context.Context, id, model string) error // ErrNotFound on missing OR deleted
	MarkModelApplied(ctx context.Context, id string) error
	MarkModelApplyFailed(ctx context.Context, id, detail string) error
	ListUnappliedModel(ctx context.Context) ([]Spawn, error)

	ClaimStarting(ctx context.Context, id string, from []Status) (newGen int64, err error)
	SetActive(ctx context.Context, id, nodeID string, gen int64) error
	SetSuspending(ctx context.Context, id string, gen int64) error
	SetMountMarker(ctx context.Context, id, mount, marker string) error
	SetSuspended(ctx context.Context, id string, gen int64) error
	SetError(ctx context.Context, id string) error
	EndContainer(ctx context.Context, id string, gen int64, p Phase) error
	MarkUnreachable(ctx context.Context, ids []string) (int, error)
	MarkBootUnreachable(ctx context.Context) (int, error)
	MarkReachable(ctx context.Context, id string, gen int64) error // unreachable->active only, gen-fenced
	MarkRecovered(ctx context.Context, id string) error
	Touch(ctx context.Context, id string, ts int64) error
	MarkDeleted(ctx context.Context, id string, ts int64) error

	LiveContainersByNode(ctx context.Context, nodeID string) ([]Container, error)
	Adopt(ctx context.Context, id, nodeID string, gen int64) error
}

type AgentImageRepo interface {
	// Upsert inserts (or keeps, on conflict) the image row and replaces its binary set.
	// Caller supplies img.CreatedAt; existing created_at is preserved on conflict.
	Upsert(ctx context.Context, img AgentImage, binaries []string) error
	Get(ctx context.Context, image string) (AgentImage, error) // ErrNotFound on missing
	Binaries(ctx context.Context, image string) ([]string, error)
	List(ctx context.Context) ([]AgentImage, error)
}

type Store interface {
	Owners() OwnerRepo
	Apps() AppRepo
	Spawns() SpawnRepo
	AgentImages() AgentImageRepo
	// WithTx runs fn in a transaction. If called inside an existing WithTx, fn runs in the
	// SAME transaction (flat composition — no savepoints; an inner error rolls back the whole tx).
	WithTx(ctx context.Context, fn func(tx Store) error) error
	Close() error
}
