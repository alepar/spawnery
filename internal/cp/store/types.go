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
	BaseImageDigest string `bun:"base_image_digest,notnull"`
	// ProfileID records which profile was applied at create time (sp-nrzf.3.8/3.9).
	// Empty string for spawns created without a profile.
	ProfileID string `bun:"profile_id,notnull"`
	// ProfileVersion is the profile's CAS version at create time — the snapshot pin (sp-nrzf.3.8 §9).
	// 0 for spawns created without a profile.
	ProfileVersion uint64 `bun:"profile_version,notnull"`
	CreatedAt   int64   `bun:"created_at,notnull"`
	LastUsedAt  int64   `bun:"last_used_at,notnull"`
	SuspendedAt *int64  `bun:"suspended_at"`
	DeletedAt   *int64  `bun:"deleted_at"`
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

// Artifact is one content-agnostic delivery unit persisted per spawn (mirrors the wire ArtifactSpec).
// Sensitive artifacts are stored metadata-only (Inline is nil); their values ride the existing
// DeliverSecrets/SealedSecret path keyed by EnvVarName.
type Artifact struct {
	bun.BaseModel   `bun:"table:spawn_artifacts,alias:art"`
	SpawnID         string `bun:"spawn_id,pk"`
	ArtifactID      string `bun:"artifact_id,pk"`
	Inline          []byte `bun:"inline"`
	ContentType     int32  `bun:"content_type,notnull"`
	TargetContainer int32  `bun:"target_container,notnull"`
	DestPath        string `bun:"dest_path,notnull"`
	Mode            uint32 `bun:"mode,notnull"`
	Sensitive       bool   `bun:"sensitive,notnull"`
	EnvVarName      string `bun:"env_var_name,notnull"`
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

// --- Profiles (sp-nrzf.3.5) ------------------------------------------------

// ProfileEntryKind discriminates what a ProfileEntry installs into a spawn.
type ProfileEntryKind string

const (
	ProfileEntrySkill  ProfileEntryKind = "skill"
	ProfileEntryMCP    ProfileEntryKind = "mcp"
	ProfileEntryConfig ProfileEntryKind = "config"
	ProfileEntryPlugin ProfileEntryKind = "plugin"
)

// ProfileSourceKind discriminates where a ProfileEntry's content comes from.
type ProfileSourceKind string

const (
	ProfileSourceCatalog ProfileSourceKind = "catalog_ref"
	ProfileSourceCustom  ProfileSourceKind = "custom"
)

// Profile is the owner-scoped customization container with a version-CAS column.
type Profile struct {
	bun.BaseModel `bun:"table:profiles,alias:pf"`
	ProfileID     string `bun:"profile_id,pk"`
	OwnerID       string `bun:"owner_id,notnull"`
	Name          string `bun:"name,notnull"`
	Version       uint64 `bun:"version,notnull"` // CAS token
	UpdatedAt     int64  `bun:"updated_at,notnull"`
}

// ProfileEntry is one item (skill/mcp/config/plugin) inside a Profile.
// Targets and MCPSecretRefs are decoded from JSON text columns on read.
type ProfileEntry struct {
	bun.BaseModel  `bun:"table:profile_entries,alias:pe"`
	ProfileID      string            `bun:"profile_id,pk"`
	EntryID        string            `bun:"entry_id,pk"`
	Kind           ProfileEntryKind  `bun:"kind,notnull"`
	Name           string            `bun:"name,notnull"`
	SourceKind     ProfileSourceKind `bun:"source_kind,notnull"`
	CatalogID      string            `bun:"catalog_id,notnull"`
	CustomInline   []byte            `bun:"custom_inline"`
	TargetsJSON    string            `bun:"targets,notnull"`         // JSON []string, default ["all"]
	SecretRefsJSON string            `bun:"mcp_secret_refs,notnull"` // JSON []string env-var names
	Targets        []string          `bun:"-"`                       // decoded in repo
	MCPSecretRefs  []string          `bun:"-"`                       // decoded in repo
}

// ProfileSecret holds a reference from a Profile to a secret (schema-only;
// attach/delivery is deferred to the secrets session sp-nrzf.3.7).
type ProfileSecret struct {
	bun.BaseModel `bun:"table:profile_secrets,alias:ps"`
	ProfileID     string `bun:"profile_id,pk"`
	SecretID      string `bun:"secret_id,pk"`
}

// --- Customization Catalog (sp-nrzf.3.6) -----------------------------------

// CustomizationCatalogEntry is one curated customization item in the catalog.
// Any authenticated owner may create entries they own (creator_id); writes (Update/Delete/SetListed)
// are creator-only; List returns only listed=true entries (globally readable).
type CustomizationCatalogEntry struct {
	bun.BaseModel `bun:"table:customization_catalog,alias:cc"`
	CatalogID     string `bun:"catalog_id,pk"`
	CreatorID     string `bun:"creator_id,notnull"`
	Kind          string `bun:"kind,notnull"`        // skill|mcp|config|plugin (ProfileEntryKind string)
	Name          string `bun:"name,notnull"`
	Description   string `bun:"description,notnull"`
	Content       []byte `bun:"content"`             // curated inline content (BLOB/bytea)
	Listed        bool   `bun:"listed,notnull"`
	CreatedAt     int64  `bun:"created_at,notnull"`
	UpdatedAt     int64  `bun:"updated_at,notnull"`
}
