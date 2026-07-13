package node

// intentverify.go implements the A4 node-side SignedIntent verification chain
// (auth-identity design §5 [AC1][AM1][AM11][AM12]). The full chain:
//  1. AS Ed25519 sig on token + expiry (authsvc/token.Verify)
//  2. aud == "node"                             [MC2]
//  3. Owner match (CP-asserted owner; self-hosted also enforces == NodeOwner)
//  4. SPKI hashes to token.session_key_hash     [AM11]
//  5. Intent sig over exact received bytes      [WM9]
//  6. Field-by-field correspondence             [AC1]
//  7. Freshness: past ≤ FreshnessWindow+SkewBudget; future ≤ SkewBudget  [AC1]
//  8. JTI uniqueness + process-start floor      [AC1]
//
// Every failure is returned to the caller; there is no permissive runtime mode.

import (
	"bytes"
	"fmt"
	"slices"
	"time"

	"google.golang.org/protobuf/proto"

	authv1 "spawnery/gen/auth/v1"
	"spawnery/internal/authsvc/token"
	"spawnery/internal/intent"
)

// NACKCode is a machine-readable reason for an intent rejection, threaded back through
// Connect errors / WS close reasons [AC1 minor note].
// Type-aliased to intent.NACKCode so spawnctl (and any package that imports intent but not
// node) can use the same constants without importing this package.
type NACKCode = intent.NACKCode

// Re-export the canonical constants from intent so existing code in this package compiles
// unchanged. Callers outside this package should reference intent.NACK* directly.
const (
	NACKMissingIntent  = intent.NACKMissingIntent
	NACKTokenInvalid   = intent.NACKTokenInvalid
	NACKWrongAudience  = intent.NACKWrongAudience
	NACKOwnerMismatch  = intent.NACKOwnerMismatch
	NACKCNFMismatch    = intent.NACKCNFMismatch
	NACKBadSig         = intent.NACKBadSig
	NACKCorrespondence = intent.NACKCorrespondence
	NACKStale          = intent.NACKStale
	NACKSkew           = intent.NACKSkew
	NACKReplay         = intent.NACKReplay
)

// IntentVerifier implements the A4 node-side verification chain.
type IntentVerifier struct {
	artifacts   *token.Verifier
	nodeOwner   string // for self-hosted owner enforcement
	nodeID      string // the node's own id; target_node_id must match this
	selfHosted  bool
	now         func() time.Time
	jtiCache    *intent.JTICache
	revocations UserRevocationLookup
}

type UserRevocationLookup interface {
	IsRevoked(tokenID, accountID string) bool
}

// NewIntentVerifier constructs a verifier. artifacts is rooted in the environment CA and validates
// certified session-token artifacts. nodeOwner is the declared node owner; selfHosted
// enables the extra owner==NodeOwner enforcement. nodeID is this node's own id.
func NewIntentVerifier(artifacts *token.Verifier, nodeOwner, nodeID string, selfHosted bool, now func() time.Time, revocations ...UserRevocationLookup) *IntentVerifier {
	if now == nil {
		now = time.Now
	}
	var lookup UserRevocationLookup
	if len(revocations) > 0 {
		lookup = revocations[0]
	}
	return &IntentVerifier{
		artifacts:   artifacts,
		nodeOwner:   nodeOwner,
		nodeID:      nodeID,
		selfHosted:  selfHosted,
		now:         now,
		jtiCache:    intent.NewJTICache(now),
		revocations: lookup,
	}
}

// StartFields is the subset of a StartSpawn's execution fields the verifier compares
// against the signed IntentBody for field-by-field correspondence [AC1].
type StartFields struct {
	Op                intent.Op
	SpawnID           string
	Generation        uint64
	AppRef            string
	Image             string
	Model             string
	DataRef           string
	Mounts            []*authv1.MountRef
	AttachedSecretIDs []string
	AssertedOwner     string
}

// Authorization is the identity established by a successful root-anchored node-token and intent verification.
type Authorization struct {
	AccountID      string
	TokenID        string
	ExpiresAt      time.Time
	SessionKeyHash []byte
}

// OpenFields is the subset of a SessionOpen the verifier compares for correspondence.
type OpenFields struct {
	SpawnID       string
	Generation    uint64
	SessionID     string
	AssertedOwner string
}

type ReauthFields struct {
	SpawnID       string
	Generation    uint64
	SessionID     string
	AssertedOwner string
}

// VerifyStart runs the full A4 verification chain for a StartSpawn.
func (v *IntentVerifier) VerifyStart(env *authv1.AuthEnvelope, fields StartFields) (Authorization, NACKCode, string) {
	return v.verify(env, fields.AssertedOwner, fields.Op, func(body *authv1.IntentBody, auth Authorization) (NACKCode, string) {
		return v.checkStartCorrespondence(body, fields)
	})
}

// VerifyOpen runs the full A4 verification chain for a SessionOpen.
func (v *IntentVerifier) VerifyOpen(env *authv1.AuthEnvelope, fields OpenFields) (Authorization, NACKCode, string) {
	return v.verify(env, fields.AssertedOwner, intent.OpSessionOpen, func(body *authv1.IntentBody, auth Authorization) (NACKCode, string) {
		return v.checkOpenCorrespondence(body, fields)
	})
}

func (v *IntentVerifier) VerifyReauth(env *authv1.AuthEnvelope, fields ReauthFields) (Authorization, NACKCode, string) {
	return v.verify(env, fields.AssertedOwner, intent.OpSessionReauth, func(body *authv1.IntentBody, auth Authorization) (NACKCode, string) {
		if nack, detail := v.checkOpenCorrespondence(body, OpenFields{SpawnID: fields.SpawnID, Generation: fields.Generation, SessionID: fields.SessionID}); nack != "" {
			return nack, detail
		}
		if body.GetNewTokenId() != auth.TokenID {
			return NACKCorrespondence, fmt.Sprintf("new_token_id: intent=%q token=%q", body.GetNewTokenId(), auth.TokenID)
		}
		return "", ""
	})
}

// verify runs the 8-step chain (steps 4–8 run only if the envelope is non-nil).
func (v *IntentVerifier) verify(
	env *authv1.AuthEnvelope,
	assertedOwner string,
	expectedOp intent.Op,
	correspondenceFn func(*authv1.IntentBody, Authorization) (NACKCode, string),
) (Authorization, NACKCode, string) {
	var zero Authorization
	// Step 0: nil envelope.
	if env == nil || (env.AccessToken == "" && env.Intent == nil) {
		return zero, NACKMissingIntent, "no auth envelope"
	}

	// Step 1: AS Ed25519 sig on token + expiry.
	now := v.now()
	payload, err := v.artifacts.Verify(env.AccessToken, token.ArtifactTypeSession, now)
	if err != nil {
		return zero, NACKTokenInvalid, err.Error()
	}
	var body authv1.SessionTokenBody
	if err := proto.Unmarshal(payload, &body); err != nil {
		return zero, NACKTokenInvalid, "session token unmarshal: " + err.Error()
	}
	if err := token.ValidateSessionBody(&body, now); err != nil {
		return zero, NACKTokenInvalid, err.Error()
	}

	// Step 2: aud == "node" [MC2].
	if body.Audience != "node" {
		return zero, NACKWrongAudience, fmt.Sprintf("aud=%q want node", body.Audience)
	}
	if v.revocations != nil && v.revocations.IsRevoked(body.TokenId, body.AccountId) {
		return zero, NACKTokenInvalid, "node authorization is revoked"
	}

	// Step 3: owner match. CP-asserted owner must match the token's account_id.
	// In enforced cloud mode (not self-hosted) asserted_owner must not be empty: an empty
	// value would silently skip this cross-check, which is the only per-request CP→owner
	// binding in cloud deployments. Self-hosted nodes rely on the NodeOwner check below.
	if !v.selfHosted && assertedOwner == "" {
		return zero, NACKOwnerMismatch, "asserted_owner must not be empty in cloud mode"
	}
	if assertedOwner != "" && body.AccountId != assertedOwner {
		return zero, NACKOwnerMismatch, fmt.Sprintf("token account_id=%q != asserted_owner=%q", body.AccountId, assertedOwner)
	}
	// Self-hosted: also enforce account_id == NodeOwner.
	if v.selfHosted && v.nodeOwner != "" && body.AccountId != v.nodeOwner {
		return zero, NACKOwnerMismatch, fmt.Sprintf("token account_id=%q != nodeOwner=%q (self-hosted)", body.AccountId, v.nodeOwner)
	}

	si := env.Intent
	if si == nil {
		return zero, NACKMissingIntent, "no signed intent"
	}

	// Step 4: SPKI hashes to session_key_hash [AM11].
	if !intent.SPKIMatchesHash(si.SpkiDer, body.SessionKeyHash) {
		return zero, NACKCNFMismatch, "SPKI SHA-256 does not match session_key_hash in token"
	}
	if si.Domain != intent.DomainFor(expectedOp) {
		return zero, NACKCorrespondence, fmt.Sprintf("domain: intent=%q expected=%q", si.Domain, intent.DomainFor(expectedOp))
	}

	// Step 5: intent sig over exact received bytes [WM9].
	if err := intent.VerifySig(si.Domain, si.Body, si.Sig, si.SpkiDer); err != nil {
		return zero, NACKBadSig, err.Error()
	}

	// Parse the body bytes.
	intentBody, err := intent.ParseBody(si.Body)
	if err != nil {
		return zero, NACKBadSig, "intent body unmarshal: " + err.Error()
	}
	if intentBody.GetOp() != string(expectedOp) {
		return zero, NACKCorrespondence, fmt.Sprintf("op: intent=%q expected=%q", intentBody.GetOp(), expectedOp)
	}
	auth := Authorization{
		AccountID: body.GetAccountId(), TokenID: body.GetTokenId(),
		ExpiresAt:      time.Unix(body.GetExpiresAt(), 0),
		SessionKeyHash: bytes.Clone(body.GetSessionKeyHash()),
	}

	// Step 6: field-by-field correspondence (caller-specific).
	if nack, detail := correspondenceFn(intentBody, auth); nack != "" {
		return zero, nack, detail
	}

	// Step 7: freshness [AC1].
	// Past direction: age ≤ FreshnessWindow + SkewBudget.
	// Future direction: only SkewBudget tolerance (spec §5 pins skew at ±30s; FreshnessWindow
	// does not extend in the future direction).
	issuedAt := time.Unix(intentBody.IssuedAt, 0)
	age := now.Sub(issuedAt)
	if age < 0 {
		if -age > intent.SkewBudget {
			return zero, NACKSkew, fmt.Sprintf("intent issued_at is %.0fs in the future (max skew %s); node time: %d", (-age).Seconds(), intent.SkewBudget, now.Unix())
		}
	} else if age > intent.FreshnessWindow+intent.SkewBudget {
		return zero, NACKStale, fmt.Sprintf("intent is %.0fs old (max %s+%s); node time: %d", age.Seconds(), intent.FreshnessWindow, intent.SkewBudget, now.Unix())
	}

	// Step 8: JTI uniqueness + process-start floor [AC1].
	if err := v.jtiCache.Admit(intentBody.Jti, issuedAt); err != nil {
		return zero, NACKReplay, err.Error()
	}

	return auth, "", ""
}

// checkStartCorrespondence implements step 6 for StartSpawn [AC1].
func (v *IntentVerifier) checkStartCorrespondence(body *authv1.IntentBody, fields StartFields) (NACKCode, string) {
	if body.SpawnId != fields.SpawnID {
		return NACKCorrespondence, fmt.Sprintf("spawn_id: intent=%q exec=%q", body.SpawnId, fields.SpawnID)
	}
	if body.Generation != fields.Generation {
		return NACKCorrespondence, fmt.Sprintf("generation: intent=%d exec=%d", body.Generation, fields.Generation)
	}
	if v.nodeID != "" && body.TargetNodeId != v.nodeID {
		return NACKCorrespondence, fmt.Sprintf("target_node_id: intent=%q nodeID=%q", body.TargetNodeId, v.nodeID)
	}
	if body.AppRef != fields.AppRef {
		return NACKCorrespondence, fmt.Sprintf("app_ref: intent=%q exec=%q", body.AppRef, fields.AppRef)
	}
	if body.Image != fields.Image {
		return NACKCorrespondence, fmt.Sprintf("image: intent=%q exec=%q", body.Image, fields.Image)
	}
	if body.Model != fields.Model {
		return NACKCorrespondence, fmt.Sprintf("model: intent=%q exec=%q", body.Model, fields.Model)
	}
	if body.DataRef != fields.DataRef {
		return NACKCorrespondence, fmt.Sprintf("data_ref: intent=%q exec=%q", body.DataRef, fields.DataRef)
	}
	// Mounts: count and each binding field must match in order.
	if len(body.Mounts) != len(fields.Mounts) {
		return NACKCorrespondence, fmt.Sprintf("mounts count: intent=%d exec=%d", len(body.Mounts), len(fields.Mounts))
	}
	for i, m := range fields.Mounts {
		bm := body.Mounts[i]
		if bm.Name != m.Name || bm.BackendUri != m.BackendUri || bm.CredentialSecretId != m.CredentialSecretId || bm.CreateIfMissing != m.CreateIfMissing || bm.RepositoryId != m.RepositoryId {
			return NACKCorrespondence, fmt.Sprintf("mounts[%d]: intent={%q,%q,%q,%t,%q} exec={%q,%q,%q,%t,%q}",
				i,
				bm.Name, bm.BackendUri, bm.CredentialSecretId, bm.CreateIfMissing, bm.RepositoryId,
				m.Name, m.BackendUri, m.CredentialSecretId, m.CreateIfMissing, m.RepositoryId)
		}
	}
	if !slices.Equal(canonicalStringSet(body.GetAttachedSecretIds()), canonicalStringSet(fields.AttachedSecretIDs)) {
		return NACKCorrespondence, fmt.Sprintf("attached_secret_ids: intent=%v exec=%v", body.GetAttachedSecretIds(), fields.AttachedSecretIDs)
	}
	return "", ""
}

func canonicalStringSet(values []string) []string {
	set := make(map[string]struct{}, len(values))
	for _, value := range values {
		if value != "" {
			set[value] = struct{}{}
		}
	}
	result := make([]string, 0, len(set))
	for value := range set {
		result = append(result, value)
	}
	slices.Sort(result)
	return result
}

// checkOpenCorrespondence implements step 6 for SessionOpen [AC1][AM11].
func (v *IntentVerifier) checkOpenCorrespondence(body *authv1.IntentBody, fields OpenFields) (NACKCode, string) {
	if body.SpawnId != fields.SpawnID {
		return NACKCorrespondence, fmt.Sprintf("spawn_id: intent=%q exec=%q", body.SpawnId, fields.SpawnID)
	}
	if body.Generation != fields.Generation {
		return NACKCorrespondence, fmt.Sprintf("generation: intent=%d exec=%d", body.Generation, fields.Generation)
	}
	if body.SessionId != fields.SessionID {
		return NACKCorrespondence, fmt.Sprintf("session_id: intent=%q exec=%q", body.SessionId, fields.SessionID)
	}
	if body.TargetNodeId != v.nodeID {
		return NACKCorrespondence, fmt.Sprintf("target_node_id: intent=%q nodeID=%q", body.TargetNodeId, v.nodeID)
	}
	return "", ""
}
