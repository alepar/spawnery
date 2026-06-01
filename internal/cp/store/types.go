package store

import "github.com/uptrace/bun"

type Status string // durable spawn lifecycle
const (
	Starting    Status = "starting"
	Active      Status = "active"
	Suspending  Status = "suspending"
	Suspended   Status = "suspended"
	Unreachable Status = "unreachable"
	Errored     Status = "error"
	Deleted     Status = "deleted"
)

type Phase string // running-container episode phase
const (
	PhaseStarting   Phase = "starting"
	PhaseActive     Phase = "active"
	PhaseSuspending Phase = "suspending"
	PhaseStopped    Phase = "stopped"
	PhaseLost       Phase = "lost"
)

type Owner struct {
	bun.BaseModel `bun:"table:owners,alias:o"`
	ID            string `bun:"id,pk"`
	Email         string `bun:"email"`
	CreatedAt     int64  `bun:"created_at,notnull"`
}

type Tier string // marketplace trust tier (E5 §5)
const (
	TierUnverified Tier = "unverified"
	TierScanned    Tier = "scanned"
	TierReviewed   Tier = "reviewed"
)

type App struct {
	bun.BaseModel `bun:"table:apps,alias:a"`
	ID            string `bun:"id,pk"`
	DisplayName   string `bun:"display_name"`
	Summary       string `bun:"summary,notnull"`
	Tags          string `bun:"tags,notnull"`
	Visibility    string `bun:"visibility,notnull"`
	Listed        bool   `bun:"listed,notnull"`
	CreatedAt     int64  `bun:"created_at,notnull"`
}

type AppVersion struct {
	bun.BaseModel `bun:"table:app_versions,alias:av"`
	AppID         string `bun:"app_id,pk"`
	Version       string `bun:"version,pk"`
	Ref           string `bun:"ref,notnull"`
	Tier          Tier   `bun:"tier,notnull"`
	CreatedAt     int64  `bun:"created_at,notnull"`
}

type MountDecl struct {
	bun.BaseModel `bun:"table:app_version_mounts,alias:avm"`
	AppID         string `bun:"app_id,pk"`
	Version       string `bun:"version,pk"`
	Name          string `bun:"name,pk"`
	Required      bool   `bun:"required,notnull"`
}

type Spawn struct {
	bun.BaseModel `bun:"table:spawns,alias:s"`
	ID            string `bun:"id,pk"`
	OwnerID       string `bun:"owner_id,notnull"`
	AppID         string `bun:"app_id,notnull"`
	AppVersion    string `bun:"app_version,notnull"`
	AppRef        string `bun:"app_ref,notnull"`
	Pinned        bool   `bun:"pinned,notnull"`
	Model         string `bun:"model,notnull"`
	Status        Status `bun:"status,notnull"`
	Recovered     bool   `bun:"recovered,notnull"`
	CreatedAt     int64  `bun:"created_at,notnull"`
	LastUsedAt    int64  `bun:"last_used_at,notnull"`
	SuspendedAt   *int64 `bun:"suspended_at"`
	DeletedAt     *int64 `bun:"deleted_at"`
}

// Container is the running episode. spawn:container = 1-to-0..1 (uniq_live_container on ended_at IS NULL).
type Container struct {
	bun.BaseModel `bun:"table:spawn_containers,alias:c"`
	SpawnID       string `bun:"spawn_id,pk"`
	Generation    int64  `bun:"generation,pk"`
	NodeID        string `bun:"node_id,notnull"`
	Phase         Phase  `bun:"phase,notnull"`
	StartedAt     int64  `bun:"started_at,notnull"`
	EndedAt       *int64 `bun:"ended_at"`
}

type Mount struct {
	bun.BaseModel `bun:"table:spawn_mounts,alias:m"`
	SpawnID       string `bun:"spawn_id,pk"`
	Name          string `bun:"name,pk"`
	BackendURI    string `bun:"backend_uri,notnull"`
	PersistMarker string `bun:"persist_marker"`
}
