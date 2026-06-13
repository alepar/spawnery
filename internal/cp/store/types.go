package store

import "github.com/uptrace/bun"

type Status string // durable spawn lifecycle
const (
	Starting    Status = "starting"
	Active      Status = "active"
	Suspending  Status = "suspending"
	Suspended   Status = "suspended"
	Resuming    Status = "resuming"
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
	CreatorID     string `bun:"creator_id,notnull"`
	CreatedAt     int64  `bun:"created_at,notnull"`
}

type AppVersion struct {
	bun.BaseModel `bun:"table:app_versions,alias:av"`
	AppID         string `bun:"app_id,pk"`
	Version       string `bun:"version,pk"`
	Ref           string `bun:"ref,notnull"`
	Tier          Tier   `bun:"tier,notnull"`
	Manifest      string `bun:"manifest,notnull"`
	CreatedAt     int64  `bun:"created_at,notnull"`
}

type MountDecl struct {
	bun.BaseModel `bun:"table:app_version_mounts,alias:avm"`
	AppID         string `bun:"app_id,pk"`
	Version       string `bun:"version,pk"`
	Name          string `bun:"name,pk"`
	Path          string `bun:"path,notnull"`
	Seed          string `bun:"seed,notnull"`
	Required      bool   `bun:"required,notnull"`
}

// transientStatuses are spawn statuses that represent an in-progress transition. Every spawn in one
// of these states is expected to have an active claim; one without (NULL claim or expired
// claim_deadline) is considered stranded and eligible for recovery.
var transientStatuses = []Status{Suspending, Resuming}

type Spawn struct {
	bun.BaseModel    `bun:"table:spawns,alias:s"`
	ID               string `bun:"id,pk"`
	OwnerID          string `bun:"owner_id,notnull"`
	Name             string `bun:"name,notnull"`
	AppID            string `bun:"app_id,notnull"`
	AppVersion       string `bun:"app_version,notnull"`
	AppRef           string `bun:"app_ref,notnull"`
	Pinned           bool   `bun:"pinned,notnull"`
	Model            string `bun:"model,notnull"`
	Image            string `bun:"image,notnull"`
	RunnableID       string `bun:"runnable_id,notnull"`
	Mode             string `bun:"mode,notnull"`
	Status           Status `bun:"status,notnull"`
	Recovered        bool   `bun:"recovered,notnull"`
	ModelApplied     bool   `bun:"model_applied,notnull"`
	ModelApplyDetail string `bun:"model_apply_detail,notnull"`
	// BaseImageDigest is the content-addressable digest of the agent's base image, resolved at
	// create time by the node and stored here for cross-node resume (spec §4 / sp-ei4.1.10).
	// Empty for spawns created before this field was introduced.
	BaseImageDigest string  `bun:"base_image_digest,notnull"`
	CreatedAt       int64   `bun:"created_at,notnull"`
	LastUsedAt      int64   `bun:"last_used_at,notnull"`
	SuspendedAt     *int64  `bun:"suspended_at"`
	DeletedAt       *int64  `bun:"deleted_at"`
	// StatusSeq is an optimistic-concurrency version that increments on every status or activity
	// mutation. It is CP-store-internal — not surfaced on the wire. Every guarded write CAS-es on
	// the StatusSeq the caller read, closing the TOCTOU window between a sweeper's decision and
	// an operator's write. The claim/lease primitives (Acquire/Heartbeat/Release/TransitionClaimed)
	// fence on this column; Heartbeat is the sole mutation that does NOT bump it.
	StatusSeq    int64   `bun:"status_seq,notnull"`
	ClaimHolder  *string `bun:"claim_holder"`
	ClaimLeaseID *string `bun:"claim_lease_id"`
	ClaimDeadline *int64 `bun:"claim_deadline"`
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

type TransferSetStatus string

const (
	TransferSetPending            TransferSetStatus = "pending"
	TransferSetCapturing          TransferSetStatus = "capturing"
	TransferSetKeyDeliveryPending TransferSetStatus = "key_delivery_pending"
	TransferSetRestoring          TransferSetStatus = "restoring"
	TransferSetActive             TransferSetStatus = "active"
	TransferSetFailed             TransferSetStatus = "failed"
)

type TransferKeyStatus string

const (
	TransferKeyPending     TransferKeyStatus = "pending"
	TransferKeySourceReady TransferKeyStatus = "source_ready"
	TransferKeyTargetReady TransferKeyStatus = "target_ready"
)

type RootfsArtifactPin struct {
	ArtifactID       string `json:"artifact_id"`
	ArtifactType     string `json:"artifact_type"`
	Generation       uint64 `json:"generation"`
	Sequence         int    `json:"sequence"`
	BaseImageDigest  string `json:"base_image_digest"`
	Format           string `json:"format"`
	ContentDigest    string `json:"content_digest"`
	UncompressedSize int64  `json:"uncompressed_size"`
}

// TransferSet is the CP's durable migration restore authority. MountManifestPins and
// RootfsArtifactPins are decoded from JSON columns on read; callers must restore only these pins.
type TransferSet struct {
	bun.BaseModel                 `bun:"table:migration_transfer_sets,alias:mts"`
	ID                            string              `bun:"id,pk"`
	SpawnID                       string              `bun:"spawn_id,notnull"`
	SourceGeneration              uint64              `bun:"source_generation,notnull"`
	TargetGeneration              uint64              `bun:"target_generation,notnull"`
	SourceNodeID                  string              `bun:"source_node_id,notnull"`
	TargetNodeID                  string              `bun:"target_node_id,notnull"`
	BaseImageDigest               string              `bun:"base_image_digest,notnull"`
	MountManifestPinsJSON         string              `bun:"mount_manifest_pins,notnull"`
	RootfsArtifactPinsJSON        string              `bun:"rootfs_artifact_pins,notnull"`
	TransferKeyMetadataJSON       string              `bun:"transfer_key_ciphertext_metadata,notnull"`
	MountManifestPins             map[string]string   `bun:"-"`
	RootfsArtifactPins            []RootfsArtifactPin `bun:"-"`
	TransferKeyCiphertextMetadata map[string]string   `bun:"-"`
	TransferKeyStatus             TransferKeyStatus   `bun:"transfer_key_status,notnull"`
	Status                        TransferSetStatus   `bun:"status,notnull"`
	CreatedAt                     int64               `bun:"created_at,notnull"`
	UpdatedAt                     int64               `bun:"updated_at,notnull"`
}

type AgentImage struct {
	bun.BaseModel `bun:"table:agent_images,alias:ai"`
	Image         string `bun:"image,pk"`
	CreatedAt     int64  `bun:"created_at,notnull"`
}

type AgentImageBinary struct {
	bun.BaseModel `bun:"table:agent_image_binaries,alias:aib"`
	Image         string `bun:"image,pk"`
	Binary        string `bun:"binary_name,pk"`
}
