// Package intent implements the per-operation SignedIntent artifact for A4 node verification
// (auth-identity design §5 [AC1][AM11]). It is a deliberate leaf package — usable by the
// node verifier, the CP-adjacent helpers, and spawnctl — so it depends only on gen/auth/v1,
// stdlib crypto, and protobuf.
package intent

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"errors"
	"math/big"
	"strings"
	"time"

	"google.golang.org/protobuf/proto"

	authv1 "spawnery/gen/auth/v1"
)

// NACKCode is a machine-readable reason for a node intent rejection, threaded back through
// Connect errors so the client can classify retryable vs. non-retryable failures [AC1].
// The node intentverify.go type-aliases this so both packages share the same string constants.
type NACKCode string

// Canonical NACK codes. The node intentverify package re-exports these via a type alias.
const (
	NACKMissingIntent  NACKCode = "MISSING_INTENT"
	NACKTokenInvalid   NACKCode = "TOKEN_INVALID"
	NACKWrongAudience  NACKCode = "WRONG_AUDIENCE"
	NACKOwnerMismatch  NACKCode = "OWNER_MISMATCH"
	NACKCNFMismatch    NACKCode = "CNF_MISMATCH"
	NACKBadSig         NACKCode = "BAD_SIG"
	NACKCorrespondence NACKCode = "CORRESPONDENCE"
	NACKStale          NACKCode = "STALE"
	NACKSkew           NACKCode = "SKEW"
	NACKReplay         NACKCode = "REPLAY"
)

// RetryableNACK reports whether a Connect error detail string from a failed provision
// contains a retryable NACK code. Retryable codes are transient freshness races (STALE,
// SKEW, REPLAY); all others are non-retryable structural mismatches that a fresh key will
// not fix. The detail format is "NACK_CODE: human detail..." as emitted by the node.
func RetryableNACK(detail string) bool {
	for _, code := range []NACKCode{NACKStale, NACKSkew, NACKReplay} {
		if strings.HasPrefix(detail, string(code)+":") || detail == string(code) {
			return true
		}
	}
	return false
}

// FreshnessWindow is the maximum age of an intent (|now - issued_at| ≤ FreshnessWindow +
// SkewBudget). Both are named constants so tests and the node verifier share the same values.
const (
	FreshnessWindow = 90 * time.Second
	SkewBudget      = 30 * time.Second
)

// Domain tag constants [AC1][AM11]. The op field inside the body mirrors the suffix so a
// body cannot be replayed under a different domain even if a verifier mis-wires the tag.
const (
	DomainCreateSpawn   = "spawnery/intent/create-spawn/v1"
	DomainResumeSpawn   = "spawnery/intent/resume-spawn/v1"
	DomainRecreateSpawn = "spawnery/intent/recreate-spawn/v1"
	DomainMigrateSpawn  = "spawnery/intent/migrate-spawn/v1"
	DomainSessionOpen   = "spawnery/intent/session-open/v1"
)

// Op identifies which lifecycle operation an intent covers.
type Op string

const (
	OpCreateSpawn   Op = "create-spawn"
	OpResumeSpawn   Op = "resume-spawn"
	OpRecreateSpawn Op = "recreate-spawn"
	OpMigrateSpawn  Op = "migrate-spawn"
	OpSessionOpen   Op = "session-open"
)

// DomainFor returns the domain-separation tag for op.
func DomainFor(op Op) string {
	switch op {
	case OpCreateSpawn:
		return DomainCreateSpawn
	case OpResumeSpawn:
		return DomainResumeSpawn
	case OpRecreateSpawn:
		return DomainRecreateSpawn
	case OpMigrateSpawn:
		return DomainMigrateSpawn
	case OpSessionOpen:
		return DomainSessionOpen
	default:
		return "spawnery/intent/" + string(op) + "/v1"
	}
}

// Build marshals body, signs domain || body_bytes with priv (P-256 P1363), and returns a
// SignedIntent carrying the full DER SPKI [AM11]. The body must have a unique jti and a
// current issued_at set by the caller; Build does NOT generate jti/timestamp.
func Build(op Op, body *authv1.IntentBody, priv *ecdsa.PrivateKey) (*authv1.SignedIntent, error) {
	bodyBytes, err := proto.Marshal(body)
	if err != nil {
		return nil, err
	}
	domain := DomainFor(op)
	sig, err := signP1363(domain, bodyBytes, priv)
	if err != nil {
		return nil, err
	}
	spkiDER, err := x509.MarshalPKIXPublicKey(&priv.PublicKey)
	if err != nil {
		return nil, err
	}
	return &authv1.SignedIntent{
		Domain:  domain,
		Body:    bodyBytes,
		Sig:     sig,
		SpkiDer: spkiDER,
	}, nil
}

// SPKIMatchesHash checks SHA-256(spkiDER) == sessionKeyHash [AM11]. Uses a constant-time
// comparison to avoid timing leaks on the hash bytes.
func SPKIMatchesHash(spkiDER, sessionKeyHash []byte) bool {
	h := sha256.Sum256(spkiDER)
	if len(sessionKeyHash) != 32 {
		return false
	}
	var diff byte
	for i := range h {
		diff |= h[i] ^ sessionKeyHash[i]
	}
	return diff == 0
}

// VerifySig verifies the P1363 ECDSA signature over domain || body using the SPKI DER
// public key [AM11]. Verification is over the EXACT received body bytes — never
// re-marshalled (WM9 discipline).
func VerifySig(domain string, body, sig, spkiDER []byte) error {
	pub, err := parseSPKIECDSA(spkiDER)
	if err != nil {
		return err
	}
	msg := make([]byte, 0, len(domain)+len(body))
	msg = append(msg, domain...)
	msg = append(msg, body...)
	hash := sha256.Sum256(msg)
	r, s, err := p1363ToRS(sig)
	if err != nil {
		return err
	}
	if !ecdsa.Verify(pub, hash[:], r, s) {
		return errors.New("intent: signature does not verify")
	}
	return nil
}

// ParseBody unmarshals body bytes into IntentBody without re-encoding.
func ParseBody(body []byte) (*authv1.IntentBody, error) {
	var ib authv1.IntentBody
	if err := proto.Unmarshal(body, &ib); err != nil {
		return nil, err
	}
	return &ib, nil
}

// GenerateSessionKey generates a fresh ECDSA P-256 session keypair.
func GenerateSessionKey() (*ecdsa.PrivateKey, error) {
	return ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
}

// SessionKeyHash returns SHA-256(DER SPKI) as used in the cnf claim [AM11].
// Mirrors token.SessionKeyHash but defined here to keep this package leaf.
func SessionKeyHash(spkiDER []byte) []byte {
	h := sha256.Sum256(spkiDER)
	return h[:]
}

// SPKIDER returns the DER SPKI for an ECDSA P-256 key. Helper for callers that only have the
// private key.
func SPKIDER(priv *ecdsa.PrivateKey) ([]byte, error) {
	return x509.MarshalPKIXPublicKey(&priv.PublicKey)
}

// signP1363 signs domain||body with priv and returns P1363 raw 64-byte r||s.
func signP1363(domain string, body []byte, priv *ecdsa.PrivateKey) ([]byte, error) {
	msg := make([]byte, 0, len(domain)+len(body))
	msg = append(msg, domain...)
	msg = append(msg, body...)
	hash := sha256.Sum256(msg)
	r, s, err := ecdsa.Sign(rand.Reader, priv, hash[:])
	if err != nil {
		return nil, err
	}
	// P1363: r || s, each padded to 32 bytes for P-256.
	rb, sb := r.Bytes(), s.Bytes()
	out := make([]byte, 64)
	copy(out[32-len(rb):32], rb)
	copy(out[64-len(sb):64], sb)
	return out, nil
}

// p1363ToRS splits a 64-byte P1363 signature into r, s *big.Int.
func p1363ToRS(sig []byte) (*big.Int, *big.Int, error) {
	if len(sig) != 64 {
		return nil, nil, errors.New("intent: invalid P1363 signature length")
	}
	r := new(big.Int).SetBytes(sig[:32])
	s := new(big.Int).SetBytes(sig[32:])
	return r, s, nil
}

func parseSPKIECDSA(spkiDER []byte) (*ecdsa.PublicKey, error) {
	k, err := x509.ParsePKIXPublicKey(spkiDER)
	if err != nil {
		return nil, err
	}
	pub, ok := k.(*ecdsa.PublicKey)
	if !ok {
		return nil, errors.New("intent: key is not ECDSA")
	}
	if pub.Curve != elliptic.P256() {
		return nil, errors.New("intent: key is not P-256")
	}
	return pub, nil
}
