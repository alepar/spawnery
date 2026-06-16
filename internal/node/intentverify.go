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
// In verify-and-log mode (AuthModeVerifyLog) failures are logged but execution proceeds
// [AM12]. A nil/empty AuthEnvelope is treated as "missing-intent" and also
// logged-not-enforced in that mode.

import (
	"fmt"
	"log"
	"time"

	authv1 "spawnery/gen/auth/v1"
	"spawnery/internal/authsvc/token"
	"spawnery/internal/intent"
)

// AuthMode controls whether intent-verification failures block execution or only log [AM12].
type AuthMode int

const (
	// AuthModeEnforced is the production mode: verification failures return a NACK code and
	// block execution.
	AuthModeEnforced AuthMode = iota
	// AuthModeVerifyLog is the dev/insecure mode: verification is run in full but failures only
	// produce a log line; execution proceeds regardless. Missing/nil envelopes are logged as
	// "missing-intent".
	AuthModeVerifyLog
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
	keySet     token.KeySet
	nodeOwner  string // for self-hosted owner enforcement
	nodeID     string // the node's own id; target_node_id must match this
	selfHosted bool
	mode       AuthMode
	now        func() time.Time
	jtiCache   *intent.JTICache
}

// NewIntentVerifier constructs a verifier. keySet is the pinned AS session pubkey set
// (operator-provisioned, never TOFU). nodeOwner is the declared node owner; selfHosted
// enables the extra owner==NodeOwner enforcement. nodeID is this node's own id.
func NewIntentVerifier(keySet token.KeySet, nodeOwner, nodeID string, selfHosted bool, mode AuthMode, now func() time.Time) *IntentVerifier {
	if now == nil {
		now = time.Now
	}
	return &IntentVerifier{
		keySet:     keySet,
		nodeOwner:  nodeOwner,
		nodeID:     nodeID,
		selfHosted: selfHosted,
		mode:       mode,
		now:        now,
		jtiCache:   intent.NewJTICache(now),
	}
}

// StartFields is the subset of a StartSpawn's execution fields the verifier compares
// against the signed IntentBody for field-by-field correspondence [AC1].
type StartFields struct {
	SpawnID       string
	Generation    uint64
	AppRef        string
	Image         string
	Model         string
	DataRef       string
	Mounts        []*authv1.MountRef
	AssertedOwner string
}

// OpenFields is the subset of a SessionOpen the verifier compares for correspondence.
type OpenFields struct {
	SpawnID       string
	Generation    uint64
	SessionID     string
	AssertedOwner string
}

// VerifyStart runs the full A4 verification chain for a StartSpawn.  Returns ("", "") on
// success.  On failure in enforced mode, returns (nackCode, detail); in verify-and-log mode
// always returns ("", "").
func (v *IntentVerifier) VerifyStart(env *authv1.AuthEnvelope, fields StartFields) (NACKCode, string) {
	return v.verify(env, fields.AssertedOwner, func(body *authv1.IntentBody) (NACKCode, string) {
		return v.checkStartCorrespondence(body, fields)
	})
}

// VerifyOpen runs the full A4 verification chain for a SessionOpen.
func (v *IntentVerifier) VerifyOpen(env *authv1.AuthEnvelope, fields OpenFields) (NACKCode, string) {
	return v.verify(env, fields.AssertedOwner, func(body *authv1.IntentBody) (NACKCode, string) {
		return v.checkOpenCorrespondence(body, fields)
	})
}

// verify runs the 8-step chain (steps 4–8 run only if the envelope is non-nil).
func (v *IntentVerifier) verify(
	env *authv1.AuthEnvelope,
	assertedOwner string,
	correspondenceFn func(*authv1.IntentBody) (NACKCode, string),
) (NACKCode, string) {
	nack, detail := v.doVerify(env, assertedOwner, correspondenceFn)
	if nack == "" {
		return "", ""
	}
	if v.mode == AuthModeVerifyLog {
		log.Printf("node intent NACK %s: %s (verify-and-log mode; proceeding)", nack, detail)
		return "", ""
	}
	return nack, detail
}

// doVerify runs the actual checks without considering the mode.
func (v *IntentVerifier) doVerify(
	env *authv1.AuthEnvelope,
	assertedOwner string,
	correspondenceFn func(*authv1.IntentBody) (NACKCode, string),
) (NACKCode, string) {
	// Step 0: nil envelope.
	if env == nil || (env.AccessToken == "" && env.Intent == nil) {
		return NACKMissingIntent, "no auth envelope"
	}

	// Step 1: AS Ed25519 sig on token + expiry.
	now := v.now()
	body, err := token.Verify(env.AccessToken, v.keySet, now)
	if err != nil {
		return NACKTokenInvalid, err.Error()
	}

	// Step 2: aud == "node" [MC2].
	if body.Audience != "node" {
		return NACKWrongAudience, fmt.Sprintf("aud=%q want node", body.Audience)
	}

	// Step 3: owner match. CP-asserted owner must match the token's account_id.
	// In enforced cloud mode (not self-hosted) asserted_owner must not be empty: an empty
	// value would silently skip this cross-check, which is the only per-request CP→owner
	// binding in cloud deployments. Self-hosted nodes rely on the NodeOwner check below.
	if v.mode == AuthModeEnforced && !v.selfHosted && assertedOwner == "" {
		return NACKOwnerMismatch, "asserted_owner must not be empty in enforced cloud mode"
	}
	if assertedOwner != "" && body.AccountId != assertedOwner {
		return NACKOwnerMismatch, fmt.Sprintf("token account_id=%q != asserted_owner=%q", body.AccountId, assertedOwner)
	}
	// Self-hosted: also enforce account_id == NodeOwner.
	if v.selfHosted && v.nodeOwner != "" && body.AccountId != v.nodeOwner {
		return NACKOwnerMismatch, fmt.Sprintf("token account_id=%q != nodeOwner=%q (self-hosted)", body.AccountId, v.nodeOwner)
	}

	si := env.Intent
	if si == nil {
		return NACKMissingIntent, "no signed intent"
	}

	// Step 4: SPKI hashes to session_key_hash [AM11].
	if !intent.SPKIMatchesHash(si.SpkiDer, body.SessionKeyHash) {
		return NACKCNFMismatch, "SPKI SHA-256 does not match session_key_hash in token"
	}

	// Step 5: intent sig over exact received bytes [WM9].
	if err := intent.VerifySig(si.Domain, si.Body, si.Sig, si.SpkiDer); err != nil {
		return NACKBadSig, err.Error()
	}

	// Parse the body bytes.
	intentBody, err := intent.ParseBody(si.Body)
	if err != nil {
		return NACKBadSig, "intent body unmarshal: " + err.Error()
	}

	// Step 6: field-by-field correspondence (caller-specific).
	if nack, detail := correspondenceFn(intentBody); nack != "" {
		return nack, detail
	}

	// Step 7: freshness [AC1].
	// Past direction: age ≤ FreshnessWindow + SkewBudget.
	// Future direction: only SkewBudget tolerance (spec §5 pins skew at ±30s; FreshnessWindow
	// does not extend in the future direction).
	issuedAt := time.Unix(intentBody.IssuedAt, 0)
	age := now.Sub(issuedAt)
	if age < 0 {
		if -age > intent.SkewBudget {
			return NACKSkew, fmt.Sprintf("intent issued_at is %.0fs in the future (max skew %s); node time: %d", (-age).Seconds(), intent.SkewBudget, now.Unix())
		}
	} else if age > intent.FreshnessWindow+intent.SkewBudget {
		return NACKStale, fmt.Sprintf("intent is %.0fs old (max %s+%s); node time: %d", age.Seconds(), intent.FreshnessWindow, intent.SkewBudget, now.Unix())
	}

	// Step 8: JTI uniqueness + process-start floor [AC1].
	if err := v.jtiCache.Admit(intentBody.Jti, issuedAt); err != nil {
		return NACKReplay, err.Error()
	}

	return "", ""
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
	// For create-spawn: app_ref, image, model must match.
	// Guard on the signed body value (not the exec field) so that a CP sending an empty
	// execution value for a field the client signed non-empty is caught [AM1].
	if body.AppRef != "" && body.AppRef != fields.AppRef {
		return NACKCorrespondence, fmt.Sprintf("app_ref: intent=%q exec=%q", body.AppRef, fields.AppRef)
	}
	if body.Image != "" && body.Image != fields.Image {
		return NACKCorrespondence, fmt.Sprintf("image: intent=%q exec=%q", body.Image, fields.Image)
	}
	if body.Model != "" && body.Model != fields.Model {
		return NACKCorrespondence, fmt.Sprintf("model: intent=%q exec=%q", body.Model, fields.Model)
	}
	// For resume/recreate/migrate: data_ref.
	if body.DataRef != "" && body.DataRef != fields.DataRef {
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
	return "", ""
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
	return "", ""
}
