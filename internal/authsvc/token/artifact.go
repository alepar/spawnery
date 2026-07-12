package token

import (
	"bytes"
	"container/list"
	"crypto/ed25519"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/x509"
	"encoding/asn1"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"google.golang.org/protobuf/proto"

	authv1 "spawnery/gen/auth/v1"
)

const (
	ArtifactTypeSession          = "session-token"
	ArtifactTypeRevocation       = "revocation-entry"
	ArtifactTypeSignerRevocation = "signer-revocation"

	maxArtifactPayloadSize     = 64 * 1024
	maxArtifactCertificateSize = 16 * 1024
	maxEncodedArtifactSize     = 128 * 1024
	maxChainCacheEntries       = 256
)

var (
	AuthSigningIntermediatePolicyOID = asn1.ObjectIdentifier{1, 3, 6, 1, 4, 1, 57264, 1, 1}
	AuthArtifactSignerPolicyOID      = asn1.ObjectIdentifier{1, 3, 6, 1, 4, 1, 57264, 1, 2}
)

func artifactDomain(artifactType string) (string, error) {
	switch artifactType {
	case ArtifactTypeSession:
		return DomainPrefix, nil
	case ArtifactTypeRevocation:
		return RevocationDomainPrefix, nil
	case ArtifactTypeSignerRevocation:
		return "spawnery/signer-revocation/v1", nil
	default:
		return "", fmt.Errorf("token: unknown artifact type %q", artifactType)
	}
}

func onlineArtifactDomain(artifactType string) (string, error) {
	if artifactType == ArtifactTypeSignerRevocation {
		return "", errors.New("token: signer-revocation artifacts require offline intermediate authority")
	}
	return artifactDomain(artifactType)
}

// SigningCredential binds one Ed25519 private key to its purpose-constrained, leaf-first chain.
type SigningCredential struct {
	PrivateKey ed25519.PrivateKey
	Chain      []*x509.Certificate
	KeyID      [32]byte
}

// SignerRevocationView is the verifier's independently authorized signer-revocation state.
type SignerRevocationView interface {
	Generation() uint64
	RejectSigner(*x509.Certificate) error
}

// Verifier validates self-describing artifacts against one environment root.
type Verifier struct {
	root          *x509.Certificate
	rootHash      [32]byte
	environment   string
	revocations   SignerRevocationView
	chainCache    chainValidationCache
	validateChain func([]*x509.Certificate, *x509.Certificate, string, time.Time) (*validatedSigner, error)
}

type chainCacheKey struct {
	root       [32]byte
	leaf       [32]byte
	generation uint64
}

type chainCacheEntry struct {
	key               chainCacheKey
	validated         *validatedSigner
	chainFingerprints [][32]byte
}

type chainValidationCache struct {
	mu      sync.Mutex
	lru     *list.List
	entries map[chainCacheKey]*list.Element
}

type validatedSigner struct {
	leaf      *x509.Certificate
	publicKey ed25519.PublicKey
	keyID     [32]byte
	validFrom time.Time
	expires   time.Time
}

// NewSigningCredential validates an artifact-signing key and chain before they can be used.
func NewSigningCredential(priv ed25519.PrivateKey, chain []*x509.Certificate, root *x509.Certificate, environment string, now time.Time) (*SigningCredential, error) {
	validated, err := validateSignerChain(chain, root, environment, now)
	if err != nil {
		return nil, err
	}
	if len(priv) != ed25519.PrivateKeySize {
		return nil, errors.New("token: private key does not match signer certificate")
	}
	canonical := ed25519.NewKeyFromSeed(priv[:ed25519.SeedSize])
	if subtle.ConstantTimeCompare(priv, canonical) != 1 || subtle.ConstantTimeCompare(canonical[ed25519.SeedSize:], validated.publicKey) != 1 {
		return nil, errors.New("token: private key does not match signer certificate")
	}
	return &SigningCredential{
		PrivateKey: canonical,
		Chain:      append([]*x509.Certificate(nil), chain...),
		KeyID:      validated.keyID,
	}, nil
}

// NewVerifier constructs a strict root-anchored artifact verifier.
func NewVerifier(root *x509.Certificate, environment string, revocations SignerRevocationView) (*Verifier, error) {
	if root == nil || len(root.Raw) == 0 || !root.IsCA {
		return nil, errors.New("token: invalid verification root")
	}
	if environment == "" {
		return nil, errors.New("token: missing environment")
	}
	return &Verifier{
		root:          root,
		rootHash:      sha256.Sum256(root.Raw),
		environment:   environment,
		revocations:   revocations,
		chainCache:    chainValidationCache{lru: list.New(), entries: make(map[chainCacheKey]*list.Element)},
		validateChain: validateSignerChain,
	}, nil
}

// Sign signs domain || exact payload bytes and returns one unpadded base64url protobuf envelope.
func (credential *SigningCredential) Sign(artifactType string, payload []byte) (string, error) {
	if credential == nil || len(credential.PrivateKey) != ed25519.PrivateKeySize || len(credential.Chain) == 0 {
		return "", errors.New("token: invalid signing credential")
	}
	domain, err := onlineArtifactDomain(artifactType)
	if err != nil {
		return "", err
	}
	if len(payload) == 0 || len(payload) > maxArtifactPayloadSize {
		return "", errors.New("token: artifact payload size is invalid")
	}
	message := make([]byte, 0, len(domain)+len(payload))
	message = append(message, domain...)
	message = append(message, payload...)
	chain := make([][]byte, len(credential.Chain))
	for i, cert := range credential.Chain {
		if cert == nil || len(cert.Raw) == 0 || len(cert.Raw) > maxArtifactCertificateSize {
			return "", errors.New("token: signer certificate size is invalid")
		}
		chain[i] = append([]byte(nil), cert.Raw...)
	}
	envelope := &authv1.SignedAuthArtifact{
		ArtifactType: artifactType,
		Payload:      append([]byte(nil), payload...),
		Signature:    ed25519.Sign(credential.PrivateKey, message),
		SignerChain:  chain,
		KeyId:        append([]byte(nil), credential.KeyID[:]...),
	}
	raw, err := proto.Marshal(envelope)
	if err != nil {
		return "", fmt.Errorf("token: marshal artifact: %w", err)
	}
	wire := base64.RawURLEncoding.EncodeToString(raw)
	if len(wire) > maxEncodedArtifactSize {
		return "", errors.New("token: encoded artifact is too large")
	}
	return wire, nil
}

// Verify returns exact payload bytes only after chain, purpose, revocation, key ID, and signature
// validation succeeds. Payload semantics are deliberately enforced by callers after this step.
func (verifier *Verifier) Verify(wire, expectedType string, now time.Time) ([]byte, error) {
	if verifier == nil || verifier.root == nil || len(wire) == 0 || len(wire) > maxEncodedArtifactSize {
		return nil, ErrMalformed
	}
	if _, err := onlineArtifactDomain(expectedType); err != nil {
		return nil, ErrMalformed
	}
	raw, err := base64.RawURLEncoding.DecodeString(wire)
	if err != nil {
		return nil, ErrMalformed
	}
	var envelope authv1.SignedAuthArtifact
	if err := (proto.UnmarshalOptions{DiscardUnknown: false}).Unmarshal(raw, &envelope); err != nil || len(envelope.ProtoReflect().GetUnknown()) != 0 {
		return nil, ErrMalformed
	}
	domain, err := onlineArtifactDomain(envelope.ArtifactType)
	if err != nil || envelope.ArtifactType != expectedType || len(envelope.Payload) == 0 || len(envelope.Payload) > maxArtifactPayloadSize ||
		len(envelope.Signature) != ed25519.SignatureSize || len(envelope.SignerChain) < 1 || len(envelope.SignerChain) > 4 || len(envelope.KeyId) != sha256.Size {
		return nil, ErrMalformed
	}

	chain := make([]*x509.Certificate, len(envelope.SignerChain))
	for i, der := range envelope.SignerChain {
		if len(der) == 0 || len(der) > maxArtifactCertificateSize {
			return nil, ErrMalformed
		}
		cert, err := x509.ParseCertificate(der)
		if err != nil {
			return nil, ErrMalformed
		}
		chain[i] = cert
	}
	generation := uint64(0)
	if verifier.revocations != nil {
		generation = verifier.revocations.Generation()
	}
	cacheKey := chainCacheKey{
		root:       verifier.rootHash,
		leaf:       sha256.Sum256(chain[0].Raw),
		generation: generation,
	}
	validated, cacheHit := verifier.chainCache.get(cacheKey, chain, now)
	if !cacheHit {
		validated, err = verifier.validateChain(chain, verifier.root, verifier.environment, now)
		if err != nil {
			return nil, fmt.Errorf("token: reject signer chain: %w", err)
		}
	}
	if verifier.revocations != nil {
		if err := verifier.revocations.RejectSigner(validated.leaf); err != nil {
			return nil, fmt.Errorf("token: signer revoked: %w", err)
		}
	}
	if !cacheHit {
		verifier.chainCache.put(cacheKey, chain, validated)
	}
	if subtle.ConstantTimeCompare(envelope.KeyId, validated.keyID[:]) != 1 {
		return nil, ErrUnknownKey
	}
	message := make([]byte, 0, len(domain)+len(envelope.Payload))
	message = append(message, domain...)
	message = append(message, envelope.Payload...)
	if !ed25519.Verify(validated.publicKey, message, envelope.Signature) {
		return nil, ErrSignature
	}
	return append([]byte(nil), envelope.Payload...), nil
}

func (cache *chainValidationCache) get(key chainCacheKey, chain []*x509.Certificate, now time.Time) (*validatedSigner, bool) {
	cache.mu.Lock()
	defer cache.mu.Unlock()
	element, ok := cache.entries[key]
	if !ok {
		return nil, false
	}
	entry := element.Value.(*chainCacheEntry)
	if now.Before(entry.validated.validFrom) || !now.Before(entry.validated.expires) {
		cache.lru.Remove(element)
		delete(cache.entries, key)
		return nil, false
	}
	if len(entry.chainFingerprints) != len(chain) {
		return nil, false
	}
	for i, cert := range chain {
		if sha256.Sum256(cert.Raw) != entry.chainFingerprints[i] {
			return nil, false
		}
	}
	cache.lru.MoveToFront(element)
	return entry.validated, true
}

func (cache *chainValidationCache) put(key chainCacheKey, chain []*x509.Certificate, validated *validatedSigner) {
	cache.mu.Lock()
	defer cache.mu.Unlock()
	if existing, ok := cache.entries[key]; ok {
		cache.lru.Remove(existing)
		delete(cache.entries, key)
	}
	fingerprints := make([][32]byte, len(chain))
	for i, cert := range chain {
		fingerprints[i] = sha256.Sum256(cert.Raw)
	}
	element := cache.lru.PushFront(&chainCacheEntry{key: key, validated: validated, chainFingerprints: fingerprints})
	cache.entries[key] = element
	if cache.lru.Len() <= maxChainCacheEntries {
		return
	}
	oldest := cache.lru.Back()
	entry := oldest.Value.(*chainCacheEntry)
	delete(cache.entries, entry.key)
	cache.lru.Remove(oldest)
}

func validateSignerChain(chain []*x509.Certificate, root *x509.Certificate, environment string, now time.Time) (*validatedSigner, error) {
	if root == nil || len(root.Raw) == 0 {
		return nil, errors.New("token: missing verification root")
	}
	if environment == "" {
		return nil, errors.New("token: missing environment")
	}
	if len(chain) < 1 || len(chain) > 4 {
		return nil, errors.New("token: signer chain must contain one to four certificates")
	}

	seen := make(map[[32]byte]struct{}, len(chain))
	for _, cert := range chain {
		if cert == nil || len(cert.Raw) == 0 {
			return nil, errors.New("token: signer chain contains an empty certificate")
		}
		fingerprint := sha256.Sum256(cert.Raw)
		if _, exists := seen[fingerprint]; exists {
			return nil, errors.New("token: signer chain contains a duplicate certificate")
		}
		seen[fingerprint] = struct{}{}
		if bytes.Equal(cert.Raw, root.Raw) {
			return nil, errors.New("token: signer chain must omit the root")
		}
	}
	if len(chain) < 2 {
		return nil, errors.New("token: signer chain is missing its auth-signing intermediate")
	}
	for i := 0; i+1 < len(chain); i++ {
		if err := chain[i].CheckSignatureFrom(chain[i+1]); err != nil {
			return nil, fmt.Errorf("token: signer chain is not leaf-first: %w", err)
		}
	}
	if err := chain[len(chain)-1].CheckSignatureFrom(root); err != nil {
		return nil, fmt.Errorf("token: signer chain does not terminate at root: %w", err)
	}

	roots := x509.NewCertPool()
	roots.AddCert(root)
	intermediates := x509.NewCertPool()
	for _, cert := range chain[1:] {
		intermediates.AddCert(cert)
	}
	if _, err := chain[0].Verify(x509.VerifyOptions{
		Roots:         roots,
		Intermediates: intermediates,
		CurrentTime:   now,
		KeyUsages:     []x509.ExtKeyUsage{x509.ExtKeyUsageAny},
	}); err != nil {
		return nil, fmt.Errorf("token: invalid signer chain: %w", err)
	}

	leaf := chain[0]
	issuer := chain[1]
	if !issuer.IsCA || !hasPolicy(issuer, AuthSigningIntermediatePolicyOID) {
		return nil, errors.New("token: signer issuer lacks auth-signing intermediate policy")
	}
	publicKey, ok := leaf.PublicKey.(ed25519.PublicKey)
	if !ok || leaf.PublicKeyAlgorithm != x509.Ed25519 || len(publicKey) != ed25519.PublicKeySize {
		return nil, errors.New("token: signer leaf key is not Ed25519")
	}
	if leaf.IsCA {
		return nil, errors.New("token: signer leaf must not be a CA")
	}
	if leaf.KeyUsage != x509.KeyUsageDigitalSignature {
		return nil, errors.New("token: signer leaf must have digital-signature key usage only")
	}
	if len(leaf.ExtKeyUsage) != 0 || len(leaf.UnknownExtKeyUsage) != 0 {
		return nil, errors.New("token: signer leaf must not have extended key usages")
	}
	if !hasPolicy(leaf, AuthArtifactSignerPolicyOID) {
		return nil, errors.New("token: signer leaf lacks auth-artifact policy")
	}
	if err := validateSignerURI(leaf, environment); err != nil {
		return nil, err
	}

	spki, err := x509.MarshalPKIXPublicKey(publicKey)
	if err != nil {
		return nil, fmt.Errorf("token: marshal signer SPKI: %w", err)
	}
	validFrom := root.NotBefore
	expires := root.NotAfter
	for _, cert := range chain {
		if cert.NotBefore.After(validFrom) {
			validFrom = cert.NotBefore
		}
		if cert.NotAfter.Before(expires) {
			expires = cert.NotAfter
		}
	}
	return &validatedSigner{
		leaf:      leaf,
		publicKey: append(ed25519.PublicKey(nil), publicKey...),
		keyID:     sha256.Sum256(spki),
		validFrom: validFrom,
		expires:   expires,
	}, nil
}

func hasPolicy(cert *x509.Certificate, want asn1.ObjectIdentifier) bool {
	for _, policy := range cert.PolicyIdentifiers {
		if policy.Equal(want) {
			return true
		}
	}
	return false
}

func validateSignerURI(leaf *x509.Certificate, environment string) error {
	if len(leaf.URIs) != 1 {
		return errors.New("token: signer leaf must contain exactly one URI SAN")
	}
	u := leaf.URIs[0]
	if u.Scheme != "spiffe" || u.Host != environment+".spawnery.internal" ||
		u.User != nil || u.Opaque != "" || u.RawQuery != "" || u.Fragment != "" {
		return errors.New("token: signer leaf has the wrong SPIFFE trust domain")
	}
	const pathPrefix = "/signer/auth-artifact/"
	if !strings.HasPrefix(u.Path, pathPrefix) || len(u.Path) == len(pathPrefix) ||
		strings.Contains(u.Path[len(pathPrefix):], "/") || u.EscapedPath() != u.Path {
		return errors.New("token: signer leaf has the wrong SPIFFE identity path")
	}
	return nil
}
