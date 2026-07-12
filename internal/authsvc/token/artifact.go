package token

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/x509"
	"encoding/asn1"
	"errors"
	"fmt"
	"strings"
	"time"
)

const (
	ArtifactTypeSession          = "session-token"
	ArtifactTypeRevocation       = "revocation-entry"
	ArtifactTypeSignerRevocation = "signer-revocation"
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

// SigningCredential binds one Ed25519 private key to its purpose-constrained, leaf-first chain.
type SigningCredential struct {
	PrivateKey ed25519.PrivateKey
	Chain      []*x509.Certificate
	KeyID      [32]byte
}

type validatedSigner struct {
	leaf      *x509.Certificate
	publicKey ed25519.PublicKey
	keyID     [32]byte
	expires   time.Time
}

// NewSigningCredential validates an artifact-signing key and chain before they can be used.
func NewSigningCredential(priv ed25519.PrivateKey, chain []*x509.Certificate, root *x509.Certificate, environment string, now time.Time) (*SigningCredential, error) {
	validated, err := validateSignerChain(chain, root, environment, now)
	if err != nil {
		return nil, err
	}
	if len(priv) != ed25519.PrivateKeySize || subtle.ConstantTimeCompare(priv[ed25519.SeedSize:], validated.publicKey) != 1 {
		return nil, errors.New("token: private key does not match signer certificate")
	}
	return &SigningCredential{
		PrivateKey: append(ed25519.PrivateKey(nil), priv...),
		Chain:      append([]*x509.Certificate(nil), chain...),
		KeyID:      validated.keyID,
	}, nil
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
	expires := leaf.NotAfter
	for _, cert := range chain[1:] {
		if cert.NotAfter.Before(expires) {
			expires = cert.NotAfter
		}
	}
	return &validatedSigner{
		leaf:      leaf,
		publicKey: append(ed25519.PublicKey(nil), publicKey...),
		keyID:     sha256.Sum256(spki),
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
