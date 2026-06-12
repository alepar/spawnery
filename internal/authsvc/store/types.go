package store

import "github.com/uptrace/bun"

// User statuses.
const (
	UserActive   = "active"
	UserDisabled = "disabled"
)

// Client kinds for refresh families.
const (
	ClientWeb = "web"
	ClientCLI = "cli"
)

// Device-grant statuses (RFC 8628 lifecycle).
const (
	GrantPending  = "pending"
	GrantApproved = "approved"
	GrantDenied   = "denied"
	GrantRedeemed = "redeemed"
	GrantExpired  = "expired"
)

type User struct {
	bun.BaseModel `bun:"table:users,alias:u"`
	AccountID     string `bun:"account_id,pk"`
	GithubSub     int64  `bun:"github_sub,notnull"` // GitHub's immutable NUMERIC user id, never login [AM9]
	Handle        string `bun:"handle,notnull"`     // display-only
	Status        string `bun:"status,notnull"`
	CreatedAt     int64  `bun:"created_at,notnull"`
}

type RefreshSession struct {
	bun.BaseModel     `bun:"table:refresh_sessions,alias:rs"`
	TokenHash         string `bun:"token_hash,pk"` // sha256(token) hex — the raw token is never stored
	AccountID         string `bun:"account_id,notnull"`
	FamilyID          string `bun:"family_id,notnull"`
	ClientKind        string `bun:"client_kind,notnull"`
	SessionPubkeySPKI []byte `bun:"session_pubkey_spki,notnull"` // [AM5] PoP material, raw DER SPKI
	AccessTokenID     string `bun:"access_token_id,notnull"`     // token_id minted alongside (revocation feed payload)
	CreatedAt         int64  `bun:"created_at,notnull"`
	LastUsedAt        int64  `bun:"last_used_at,notnull"`
	ExpiresAt         int64  `bun:"expires_at,notnull"`        // 30d sliding
	FamilyCreatedAt   int64  `bun:"family_created_at,notnull"` // [AM6] 90d absolute
	SupersededBy      string `bun:"superseded_by,nullzero"`    // successor token_hash
	SupersededAt      int64  `bun:"superseded_at,nullzero"`    // grace-window anchor [AM3]
	SuccessorCache    string `bun:"successor_cache,nullzero"`  // cached successor pair JSON [AM3]
	Revoked           bool   `bun:"revoked,notnull"`
}

type OAuthState struct {
	bun.BaseModel     `bun:"table:oauth_states,alias:os"`
	State             string `bun:"state,pk"`
	FlowCookieHash    string `bun:"flow_cookie_hash,notnull"` // [AM8] binds callback to initiating browser session
	ClientChallenge   string `bun:"client_challenge,notnull"`
	ClientRedirectURI string `bun:"client_redirect_uri,notnull"`
	ClientState       string `bun:"client_state,notnull"`
	GhVerifier        string `bun:"gh_verifier,notnull"` // AS<->GitHub leg PKCE verifier
	CreatedAt         int64  `bun:"created_at,notnull"`
	ExpiresAt         int64  `bun:"expires_at,notnull"`
	Used              bool   `bun:"used,notnull"`
}

type DeviceGrant struct {
	bun.BaseModel     `bun:"table:device_grants,alias:dg"`
	DeviceCodeHash    string `bun:"device_code_hash,pk"`
	UserCode          string `bun:"user_code,notnull"`
	SessionPubkeySPKI []byte `bun:"session_pubkey_spki,notnull"` // [AM7] pubkey posted at device-authorization
	ClientKind        string `bun:"client_kind,notnull"`
	Status            string `bun:"status,notnull"`
	AccountID         string `bun:"account_id,nullzero"`
	AttemptCount      int    `bun:"attempt_count,notnull"`
	CreatedAt         int64  `bun:"created_at,notnull"`
	ExpiresAt         int64  `bun:"expires_at,notnull"`
	LastPolledAt      int64  `bun:"last_polled_at,nullzero"`
}

type RevocationEvent struct {
	bun.BaseModel `bun:"table:revocation_events,alias:re"`
	Seq           int64  `bun:"seq,pk,autoincrement"`
	AccountID     string `bun:"account_id,notnull"`
	FamilyID      string `bun:"family_id,notnull"`
	TokenIDs      string `bun:"token_ids,notnull"` // JSON array of access-token token_ids
	RevokedAt     int64  `bun:"revoked_at,notnull"`
}

// DeviceSetEntry is one append-only device-set log entry as stored in the AS.
// The AS stores raw entry bytes and a pre-computed head hash; it performs pure
// head comparison for the CAS gate and never validates signatures (WM1).
type DeviceSetEntry struct {
	bun.BaseModel `bun:"table:device_set_entries,alias:dse"`
	AccountID     string `bun:"account_id,pk,notnull"`
	Version       uint64 `bun:"version,pk,notnull"` // monotonic; genesis = 1
	PrevHash      []byte `bun:"prev_hash"`          // NULL for genesis
	HeadHash      []byte `bun:"head_hash,notnull"`  // encodeFields(Body, sigs...) chain hash
	EntryBytes    []byte `bun:"entry_bytes,notnull"` // json.Marshal(StoredEntry)
	CreatedAt     int64  `bun:"created_at,notnull"`
}
