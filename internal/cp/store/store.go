// Package store is the CP's durable state layer: owners, apps/versions, and the spawn lifecycle
// index (spawns + the running-container episode entity), over Bun (SQLite embedded / Postgres).
package store

import (
	"context"
	"errors"
)

// ErrConflict is returned when a guarded transition's precondition (status set or CAS) is not met.
// ErrNotFound is returned for a missing or soft-deleted entity on a lifecycle lookup.
// ErrClaimLost is returned when a claim-fenced operation finds the lease is gone (expired,
// preempted, or released); the driver MUST bail out and commit no further transitions.
var (
	ErrConflict  = errors.New("store: transition conflict")
	ErrNotFound  = errors.New("store: not found")
	ErrClaimLost = errors.New("store: claim lost")
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
	LatestContainer(ctx context.Context, id string) (Container, bool, error)
	GetMounts(ctx context.Context, id string) ([]Mount, error)
	GetArtifacts(ctx context.Context, id string) ([]Artifact, error)
	AddArtifacts(ctx context.Context, id string, artifacts []Artifact) error
	ListByOwner(ctx context.Context, ownerID string) ([]Spawn, error)
	Rename(ctx context.Context, id, name string) error    // ErrNotFound on missing OR deleted
	SetModel(ctx context.Context, id, model string) error // ErrNotFound on missing OR deleted
	MarkModelApplied(ctx context.Context, id string) error
	MarkModelApplyFailed(ctx context.Context, id, detail string) error
	ListUnappliedModel(ctx context.Context) ([]Spawn, error)

	// SetBaseImageDigest records the content-addressable base-image digest resolved by the node
	// at create time (spec §4 / sp-ei4.1.10). ErrNotFound when the spawn is missing or deleted.
	SetBaseImageDigest(ctx context.Context, id, digest string) error

	ClaimStarting(ctx context.Context, id string, from []Status) (newGen int64, err error)
	SetActive(ctx context.Context, id, nodeID string, gen int64) error
	SetSuspending(ctx context.Context, id string, gen int64) error
	SetForking(ctx context.Context, id string, gen int64, captureDeadlineTS int64) error
	SetMountMarker(ctx context.Context, id, mount, marker string) error
	SetSuspended(ctx context.Context, id string, gen int64) error
	// RevertSuspended is the migration "defined failure" transition (sp-u53.5.3): a starting episode
	// whose resume-on-target failed rolls BACK to suspended (gen-fenced), ending the target container
	// row so the prior suspend's markers stay the recoverable state. starting -> suspended.
	RevertSuspended(ctx context.Context, id string, gen int64) error
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

	// Acquire atomically claims the spawn row for the given holder+leaseID.
	// It CAS-es on expectedSeq (the status_seq the caller just read) and requires that the spawn
	// has no active claim (claim_holder IS NULL OR claim_deadline < nowTS). On success the claim
	// columns are set, status_seq is bumped, and newSeq = expectedSeq+1 is returned so the caller
	// can chain TransitionClaimed without a re-read. rowcount 0 → ErrConflict (stale seq or
	// active claim); the caller must re-read and re-decide.
	Acquire(ctx context.Context, id, holder, leaseID string, nowTS, deadlineTS, expectedSeq int64) (newSeq int64, err error)

	// AcquireForkingRecovery is the recovery-sweeper variant of Acquire for forking sources. It can
	// take over when the claim is absent/expired OR when fork_capture_deadline has expired, even if
	// the driver is still heartbeating the normal claim.
	AcquireForkingRecovery(ctx context.Context, id, holder, leaseID string, nowTS, deadlineTS, expectedSeq int64) (newSeq int64, err error)

	// Heartbeat extends the claim's deadline. It is fenced only by leaseID (NOT by status_seq)
	// so it does NOT bump status_seq. rowcount 0 → ErrClaimLost (the claim was expired, preempted,
	// or released); the driver MUST stop driving immediately and commit no further transitions.
	Heartbeat(ctx context.Context, id, leaseID string, newDeadlineTS int64) error

	// Release clears the claim columns and bumps status_seq (returning authority to CP sweepers).
	// It is lease-fenced: only the holder with the matching leaseID can release.
	// rowcount 0 → ErrClaimLost (the claim was already gone — preempted or expired).
	Release(ctx context.Context, id, leaseID string) error

	// TransitionClaimed performs a fully-fenced status transition on a claimed spawn. It CAS-es on
	// expectedSeq (must match) AND leaseID (must match) AND expectedGen (must be the current live
	// container's generation — fences against a recreated episode). On success status_seq is bumped
	// and newSeq = expectedSeq+1 is returned. The caller should chain Acquire's newSeq here.
	// rowcount 0 → ErrConflict; the status predicate is intentionally absent (if status_seq matched,
	// status cannot have changed concurrently).
	TransitionClaimed(ctx context.Context, id, leaseID string, expectedSeq, expectedGen int64, to Status) (newSeq int64, err error)

	// TransitionForkingRecovered returns a recovered source from Forking to Active, clearing its
	// durable fork capture deadline while leaving claim release to the caller's follow-up Release.
	TransitionForkingRecovered(ctx context.Context, id, leaseID string, expectedSeq, expectedGen int64) (newSeq int64, err error)

	// MarkForkingLost resolves a forking source whose backing pod cannot be recovered. It CAS-es on
	// status_seq while status is Forking, clears fork/claim metadata, and ends any live container as
	// lost.
	MarkForkingLost(ctx context.Context, id string, expectedSeq int64) (newSeq int64, err error)

	// ListStranded returns spawns in a transient status (Suspending, and Resuming once 7.5 adds it)
	// whose claim is absent or expired (claim_holder IS NULL OR claim_deadline < nowTS). These are
	// candidates for recovery: the driving CP goroutine crashed and the lease was not renewed.
	// Results are ordered by id ASC for deterministic test output.
	// Decision and revert logic (reconcile against node ground truth) lives in the CP layer (7.6/7.7).
	ListStranded(ctx context.Context, nowTS int64) ([]Spawn, error)

	// ListRecoverableForking returns forking sources whose claim is absent/expired OR whose
	// fork_capture_deadline has expired. Results are ordered by id ASC for deterministic recovery.
	ListRecoverableForking(ctx context.Context, nowTS int64) ([]Spawn, error)

	// ReconcileSuspendedAfterError is the late-reply reconcile transition for sp-u53.7.2: when a
	// node genuinely completed a suspend AFTER the CP's stall window fired (leaving the spawn in
	// Errored with its container already ended by SetError), a late SuspendComplete can arrive with
	// no live waiter. This method transitions Errored→Suspended without touching the container row
	// (it was already ended). Unlike SetSuspended, it does NOT guard on a live container or try to
	// end one. Accepts only Errored to prevent misuse on active/suspending rows.
	// The generation fence must be applied by the caller (e.g. via LatestContainer check) before
	// calling here — this method guards only on status to keep the store layer simple.
	ReconcileSuspendedAfterError(ctx context.Context, id string) error

	// MarkDeletedClaimed is the claim/lease/gen-fenced deleted transition for claimed cleanup flows.
	// It hides the row from Get/ListByOwner, clears claim/forking metadata, and ends the live
	// container as lost after the durable row update succeeds.
	MarkDeletedClaimed(ctx context.Context, id, leaseID string, expectedSeq, expectedGen int64, ts int64) (newSeq int64, err error)
}

type AgentImageRepo interface {
	// Upsert inserts (or keeps, on conflict) the image row and replaces its binary set.
	// Caller supplies img.CreatedAt; existing created_at is preserved on conflict.
	Upsert(ctx context.Context, img AgentImage, binaries []string) error
	Get(ctx context.Context, image string) (AgentImage, error) // ErrNotFound on missing
	Binaries(ctx context.Context, image string) ([]string, error)
	List(ctx context.Context) ([]AgentImage, error)
}

type TransferSetRepo interface {
	Create(ctx context.Context, ts TransferSet) error
	Get(ctx context.Context, id string) (TransferSet, error)
	ListFailedForks(ctx context.Context) ([]TransferSet, error)
	ListReclaimableForks(ctx context.Context, staleRestoringBefore int64) ([]TransferSet, error)
	SetPins(ctx context.Context, id string, sourceGeneration uint64, mountPins map[string]string, rootfsPins []RootfsArtifactPin, updatedAt int64) error
	SetTargetNode(ctx context.Context, id string, targetNodeID string, updatedAt int64) error
	SetStatus(ctx context.Context, id string, status TransferSetStatus, updatedAt int64) error
	SetTransferKeyStatus(ctx context.Context, id string, status TransferKeyStatus, updatedAt int64) error
}

// ProfileRepo manages the owner-scoped profile spine (Profile + ProfileEntry + ProfileSecret
// references). All mutations are CAS-fenced via the profile's version column.
type ProfileRepo interface {
	// Create inserts a new Profile (version=1). Returns an error if profile_id conflicts.
	Create(ctx context.Context, p Profile) error
	// Get loads a profile by profile_id along with its entries (ordered by entry_id ASC) and
	// secret refs (ordered by secret_id ASC). Returns ErrNotFound when absent.
	Get(ctx context.Context, profileID string) (Profile, []ProfileEntry, []ProfileSecret, error)
	// ListByOwner returns all profiles for the given owner (unordered).
	ListByOwner(ctx context.Context, ownerID string) ([]Profile, error)
	// Rename CAS-renames the profile. Returns ErrNotFound (missing) or ErrConflict (stale version).
	Rename(ctx context.Context, profileID string, expectedVersion uint64, name string, now int64) (newVersion uint64, err error)
	// Delete removes the profile and its children. Not CAS-fenced (caller already owns the profile).
	Delete(ctx context.Context, profileID string) error
	// AddEntry CAS-adds an entry to the profile. Returns ErrNotFound or ErrConflict.
	AddEntry(ctx context.Context, profileID string, expectedVersion uint64, e ProfileEntry, now int64) (newVersion uint64, err error)
	// RemoveEntry CAS-removes an entry. Returns ErrNotFound or ErrConflict.
	RemoveEntry(ctx context.Context, profileID string, expectedVersion uint64, entryID string, now int64) (newVersion uint64, err error)
	// AddSecretRef CAS-adds a secret reference. Returns ErrNotFound or ErrConflict.
	AddSecretRef(ctx context.Context, profileID string, expectedVersion uint64, secretID string, now int64) (newVersion uint64, err error)
	// RemoveSecretRef CAS-removes a secret reference. Returns ErrNotFound or ErrConflict.
	RemoveSecretRef(ctx context.Context, profileID string, expectedVersion uint64, secretID string, now int64) (newVersion uint64, err error)
}

// CustomizationCatalogRepo manages curated catalog entries.
// Any authenticated owner may Create an entry they own; List returns only listed=true entries
// (globally readable). Get is owner-readable (any authenticated caller). Update/Delete/SetListed
// are creator-only (enforced at the handler layer, not here).
type CustomizationCatalogRepo interface {
	// Create inserts a new catalog entry. Returns an error on duplicate catalog_id.
	Create(ctx context.Context, e CustomizationCatalogEntry) error
	// Get returns the entry for catalogID. ErrNotFound when absent.
	Get(ctx context.Context, catalogID string) (CustomizationCatalogEntry, error)
	// List returns only listed=true entries, ordered by name ASC.
	List(ctx context.Context) ([]CustomizationCatalogEntry, error)
	// ListByCreator returns all entries for the given creator (including unlisted), ordered by name ASC.
	ListByCreator(ctx context.Context, creatorID string) ([]CustomizationCatalogEntry, error)
	// Update replaces name, description, and content for an entry. ErrNotFound when absent.
	Update(ctx context.Context, catalogID string, name, description string, content []byte, now int64) error
	// SetListed sets the listing visibility of an entry. ErrNotFound when absent.
	SetListed(ctx context.Context, catalogID string, listed bool) error
	// Delete removes an entry. ErrNotFound when absent.
	Delete(ctx context.Context, catalogID string) error
}

type Store interface {
	Owners() OwnerRepo
	Apps() AppRepo
	Spawns() SpawnRepo
	AgentImages() AgentImageRepo
	TransferSets() TransferSetRepo
	Profiles() ProfileRepo
	CustomizationCatalog() CustomizationCatalogRepo
	// WithTx runs fn in a transaction. If called inside an existing WithTx, fn runs in the
	// SAME transaction (flat composition — no savepoints; an inner error rolls back the whole tx).
	WithTx(ctx context.Context, fn func(tx Store) error) error
	Close() error
}
