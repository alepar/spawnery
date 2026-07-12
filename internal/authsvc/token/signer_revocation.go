package token

import (
	"bytes"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/x509"
	"encoding/base64"
	"errors"
	"fmt"
	"math/big"
	"sync"
	"time"

	"google.golang.org/protobuf/proto"

	authv1 "spawnery/gen/auth/v1"
)

const maxSignerRevocationFutureSkew = 60 * time.Second

var (
	ErrSignerRevoked          = errors.New("token: signer revoked")
	ErrRevocationRollback     = errors.New("token: signer revocation rollback")
	ErrRevocationEquivocation = errors.New("token: signer revocation equivocation")
)

// SignerRevocationStatement is an authenticated, normalized offline revocation statement.
type SignerRevocationStatement struct {
	Environment            string
	Generation             uint64
	IssuedAt               time.Time
	RevokedSerials         []string
	RevokedSPKISHA256      [][sha256.Size]byte
	MinimumSignerNotBefore time.Time

	wire   string
	digest [sha256.Size]byte
}

// SignSignerRevocationStatement signs exact protobuf payload bytes with the offline auth-signing
// intermediate and returns a SignedAuthArtifact envelope.
func SignSignerRevocationStatement(intermediate *x509.Certificate, key crypto.Signer, payload []byte) (string, error) {
	if err := validateRevocationIntermediateProfile(intermediate); err != nil {
		return "", err
	}
	if key == nil || !publicKeysEqual(intermediate.PublicKey, key.Public()) {
		return "", errors.New("token: signer-revocation key does not match intermediate")
	}
	if len(payload) == 0 || len(payload) > maxArtifactPayloadSize {
		return "", errors.New("token: signer-revocation payload size is invalid")
	}
	domain, _ := artifactDomain(ArtifactTypeSignerRevocation)
	digest := sha256.Sum256(appendDomainPayload(domain, payload))
	signature, err := key.Sign(rand.Reader, digest[:], crypto.SHA256)
	if err != nil {
		return "", fmt.Errorf("token: sign signer-revocation statement: %w", err)
	}
	spki, err := x509.MarshalPKIXPublicKey(intermediate.PublicKey)
	if err != nil {
		return "", fmt.Errorf("token: marshal signer-revocation intermediate SPKI: %w", err)
	}
	keyID := sha256.Sum256(spki)
	envelope := &authv1.SignedAuthArtifact{
		ArtifactType: ArtifactTypeSignerRevocation,
		Payload:      append([]byte(nil), payload...),
		Signature:    signature,
		SignerChain:  [][]byte{append([]byte(nil), intermediate.Raw...)},
		KeyId:        append([]byte(nil), keyID[:]...),
	}
	raw, err := proto.Marshal(envelope)
	if err != nil {
		return "", fmt.Errorf("token: marshal signer-revocation artifact: %w", err)
	}
	wire := base64.RawURLEncoding.EncodeToString(raw)
	if len(wire) > maxEncodedArtifactSize {
		return "", errors.New("token: encoded signer-revocation artifact is too large")
	}
	return wire, nil
}

// ParseSignerRevocationStatement verifies offline authority and the exact payload bytes before
// decoding and normalizing the statement.
func ParseSignerRevocationStatement(wire string, root *x509.Certificate, environment string, now time.Time) (*SignerRevocationStatement, error) {
	if root == nil || len(root.Raw) == 0 || !root.IsCA || environment == "" || len(wire) == 0 || len(wire) > maxEncodedArtifactSize {
		return nil, ErrMalformed
	}
	raw, err := base64.RawURLEncoding.DecodeString(wire)
	if err != nil || base64.RawURLEncoding.EncodeToString(raw) != wire {
		return nil, ErrMalformed
	}
	var envelope authv1.SignedAuthArtifact
	if err := (proto.UnmarshalOptions{DiscardUnknown: false}).Unmarshal(raw, &envelope); err != nil || len(envelope.ProtoReflect().GetUnknown()) != 0 {
		return nil, ErrMalformed
	}
	if envelope.ArtifactType != ArtifactTypeSignerRevocation || len(envelope.Payload) == 0 || len(envelope.Payload) > maxArtifactPayloadSize ||
		len(envelope.Signature) == 0 || len(envelope.SignerChain) != 1 || len(envelope.KeyId) != sha256.Size {
		return nil, ErrMalformed
	}
	intermediateDER := envelope.SignerChain[0]
	if len(intermediateDER) == 0 || len(intermediateDER) > maxArtifactCertificateSize || bytes.Equal(intermediateDER, root.Raw) {
		return nil, ErrMalformed
	}
	intermediate, err := x509.ParseCertificate(intermediateDER)
	if err != nil {
		return nil, ErrMalformed
	}
	if err := validateRevocationIntermediate(intermediate, root, now); err != nil {
		return nil, fmt.Errorf("token: reject signer-revocation authority: %w", err)
	}
	spki, err := x509.MarshalPKIXPublicKey(intermediate.PublicKey)
	if err != nil {
		return nil, ErrMalformed
	}
	keyID := sha256.Sum256(spki)
	if subtle.ConstantTimeCompare(envelope.KeyId, keyID[:]) != 1 {
		return nil, ErrUnknownKey
	}
	domain, _ := artifactDomain(ArtifactTypeSignerRevocation)
	signedDigest := sha256.Sum256(appendDomainPayload(domain, envelope.Payload))
	publicKey, ok := intermediate.PublicKey.(*ecdsa.PublicKey)
	if !ok || !ecdsa.VerifyASN1(publicKey, signedDigest[:], envelope.Signature) {
		return nil, ErrSignature
	}

	var payload authv1.SignerRevocationStatement
	if err := (proto.UnmarshalOptions{DiscardUnknown: false}).Unmarshal(envelope.Payload, &payload); err != nil || len(payload.ProtoReflect().GetUnknown()) != 0 {
		return nil, ErrMalformed
	}
	statement, err := normalizeRevocationPayload(&payload, environment, now)
	if err != nil {
		return nil, err
	}
	statement.wire = wire
	statement.digest = sha256.Sum256(raw)
	return statement, nil
}

func validateRevocationIntermediate(intermediate, root *x509.Certificate, now time.Time) error {
	if err := validateRevocationIntermediateProfile(intermediate); err != nil {
		return err
	}
	if err := intermediate.CheckSignatureFrom(root); err != nil {
		return fmt.Errorf("intermediate does not chain directly to root: %w", err)
	}
	roots := x509.NewCertPool()
	roots.AddCert(root)
	if _, err := intermediate.Verify(x509.VerifyOptions{Roots: roots, CurrentTime: now, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageAny}}); err != nil {
		return fmt.Errorf("invalid auth-signing intermediate: %w", err)
	}
	return nil
}

func validateRevocationIntermediateProfile(intermediate *x509.Certificate) error {
	if intermediate == nil || len(intermediate.Raw) == 0 || !intermediate.IsCA || !intermediate.BasicConstraintsValid {
		return errors.New("token: signer-revocation authority must be a CA")
	}
	if !hasPolicy(intermediate, AuthSigningIntermediatePolicyOID) {
		return errors.New("token: signer-revocation authority lacks auth-signing intermediate policy")
	}
	if intermediate.KeyUsage&x509.KeyUsageCertSign == 0 || len(intermediate.ExtKeyUsage) != 0 || len(intermediate.UnknownExtKeyUsage) != 0 {
		return errors.New("token: signer-revocation authority has invalid key usage")
	}
	publicKey, ok := intermediate.PublicKey.(*ecdsa.PublicKey)
	if !ok || publicKey.Curve != elliptic.P256() {
		return errors.New("token: signer-revocation authority key is not ECDSA P-256")
	}
	return nil
}

func normalizeRevocationPayload(payload *authv1.SignerRevocationStatement, environment string, now time.Time) (*SignerRevocationStatement, error) {
	if payload.Environment == "" || payload.Environment != environment || payload.Generation == 0 || payload.IssuedAt == 0 {
		return nil, ErrMalformed
	}
	issuedAt := time.Unix(payload.IssuedAt, 0)
	if issuedAt.After(now.Add(maxSignerRevocationFutureSkew)) {
		return nil, errors.New("token: signer-revocation statement issued in the future")
	}
	serials := make([]string, len(payload.RevokedSerials))
	seenSerials := make(map[string]struct{}, len(serials))
	for i, encoded := range payload.RevokedSerials {
		serial := new(big.Int).SetBytes(encoded)
		if len(encoded) == 0 || serial.Sign() <= 0 {
			return nil, ErrMalformed
		}
		canonical := serial.Text(16)
		if _, exists := seenSerials[canonical]; exists {
			return nil, ErrMalformed
		}
		seenSerials[canonical] = struct{}{}
		serials[i] = canonical
	}
	spkiHashes := make([][sha256.Size]byte, len(payload.RevokedSpkiSha256))
	seenSPKI := make(map[[sha256.Size]byte]struct{}, len(spkiHashes))
	for i, encoded := range payload.RevokedSpkiSha256 {
		if len(encoded) != sha256.Size {
			return nil, ErrMalformed
		}
		copy(spkiHashes[i][:], encoded)
		if _, exists := seenSPKI[spkiHashes[i]]; exists {
			return nil, ErrMalformed
		}
		seenSPKI[spkiHashes[i]] = struct{}{}
	}
	minimum := time.Time{}
	if payload.MinimumSignerNotBefore != 0 {
		minimum = time.Unix(payload.MinimumSignerNotBefore, 0)
	}
	return &SignerRevocationStatement{
		Environment: payload.Environment, Generation: payload.Generation, IssuedAt: issuedAt,
		RevokedSerials: serials, RevokedSPKISHA256: spkiHashes, MinimumSignerNotBefore: minimum,
	}, nil
}

func appendDomainPayload(domain string, payload []byte) []byte {
	message := make([]byte, 0, len(domain)+len(payload))
	message = append(message, domain...)
	return append(message, payload...)
}

func publicKeysEqual(left, right any) bool {
	leftDER, leftErr := x509.MarshalPKIXPublicKey(left)
	rightDER, rightErr := x509.MarshalPKIXPublicKey(right)
	return leftErr == nil && rightErr == nil && bytes.Equal(leftDER, rightDER)
}

type signerRevocationSnapshot struct {
	generation             uint64
	digest                 [sha256.Size]byte
	wire                   string
	revokedSerials         map[string]struct{}
	revokedSPKISHA256      map[[sha256.Size]byte]struct{}
	minimumSignerNotBefore time.Time
}

// SignerRevocationState is a monotonic verifier view of offline signer revocations.
type SignerRevocationState struct {
	mu          sync.RWMutex
	environment string
	snapshot    signerRevocationSnapshot
}

func NewSignerRevocationState(environment string) (*SignerRevocationState, error) {
	if environment == "" {
		return nil, errors.New("token: missing signer-revocation environment")
	}
	return &SignerRevocationState{environment: environment}, nil
}

func (state *SignerRevocationState) Generation() uint64 {
	if state == nil {
		return 0
	}
	state.mu.RLock()
	defer state.mu.RUnlock()
	return state.snapshot.generation
}

func (state *SignerRevocationState) Apply(statement *SignerRevocationStatement) error {
	if state == nil || statement == nil || statement.Environment != state.environment || statement.Generation == 0 || statement.wire == "" {
		return ErrMalformed
	}
	candidate := snapshotFromStatement(statement)
	state.mu.Lock()
	defer state.mu.Unlock()
	if statement.Generation < state.snapshot.generation {
		return ErrRevocationRollback
	}
	if statement.Generation == state.snapshot.generation {
		if statement.digest == state.snapshot.digest {
			return nil
		}
		return ErrRevocationEquivocation
	}
	state.snapshot = candidate
	return nil
}

func (state *SignerRevocationState) prepare(statement *SignerRevocationStatement) (signerRevocationSnapshot, bool, error) {
	if state == nil || statement == nil || statement.Environment != state.environment || statement.Generation == 0 || statement.wire == "" {
		return signerRevocationSnapshot{}, false, ErrMalformed
	}
	state.mu.RLock()
	defer state.mu.RUnlock()
	if statement.Generation < state.snapshot.generation {
		return signerRevocationSnapshot{}, false, ErrRevocationRollback
	}
	if statement.Generation == state.snapshot.generation {
		if statement.digest == state.snapshot.digest {
			return signerRevocationSnapshot{}, false, nil
		}
		return signerRevocationSnapshot{}, false, ErrRevocationEquivocation
	}
	return snapshotFromStatement(statement), true, nil
}

func (state *SignerRevocationState) publish(snapshot signerRevocationSnapshot) {
	state.mu.Lock()
	state.snapshot = snapshot
	state.mu.Unlock()
}

func (state *SignerRevocationState) RejectSigner(leaf *x509.Certificate) error {
	if state == nil || leaf == nil || leaf.SerialNumber == nil {
		return nil
	}
	state.mu.RLock()
	if state.snapshot.generation == 0 {
		state.mu.RUnlock()
		return nil
	}
	state.mu.RUnlock()
	spki, err := x509.MarshalPKIXPublicKey(leaf.PublicKey)
	if err != nil {
		return fmt.Errorf("token: marshal signer SPKI for revocation: %w", err)
	}
	serial := leaf.SerialNumber.Text(16)
	spkiHash := sha256.Sum256(spki)
	state.mu.RLock()
	defer state.mu.RUnlock()
	_, serialRevoked := state.snapshot.revokedSerials[serial]
	_, spkiRevoked := state.snapshot.revokedSPKISHA256[spkiHash]
	tooOld := !state.snapshot.minimumSignerNotBefore.IsZero() && leaf.NotBefore.Before(state.snapshot.minimumSignerNotBefore)
	if serialRevoked || spkiRevoked || tooOld {
		return ErrSignerRevoked
	}
	return nil
}

func snapshotFromStatement(statement *SignerRevocationStatement) signerRevocationSnapshot {
	serials := make(map[string]struct{}, len(statement.RevokedSerials))
	for _, serial := range statement.RevokedSerials {
		serials[serial] = struct{}{}
	}
	spkiHashes := make(map[[sha256.Size]byte]struct{}, len(statement.RevokedSPKISHA256))
	for _, hash := range statement.RevokedSPKISHA256 {
		spkiHashes[hash] = struct{}{}
	}
	return signerRevocationSnapshot{
		generation: statement.Generation, digest: statement.digest, wire: statement.wire,
		revokedSerials: serials, revokedSPKISHA256: spkiHashes, minimumSignerNotBefore: statement.MinimumSignerNotBefore,
	}
}
