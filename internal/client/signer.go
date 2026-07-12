package client

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"errors"
	"fmt"
	"time"

	"google.golang.org/protobuf/proto"

	authv1 "spawnery/gen/auth/v1"
	"spawnery/internal/intent"
	"spawnery/internal/pki"
)

// SessionSigner signs node-authorized intents with the key bound to the login session.
type SessionSigner interface {
	PublicSPKIDER() ([]byte, error)
	SignP1363(domain string, exactBody []byte) ([]byte, error)
}

// NodeCredentials contains only the credential material a verified node may receive.
type NodeCredentials struct {
	AccessToken string
	Signer      SessionSigner
}

type NodeCredentialSource interface {
	NodeCredentials(context.Context) (NodeCredentials, error)
}

type fixedNodeCredentialSource struct{ credentials NodeCredentials }

func (s fixedNodeCredentialSource) NodeCredentials(context.Context) (NodeCredentials, error) {
	return s.credentials, nil
}

// TargetTrust pins the environment and account policy used to verify a resolved node.
type TargetTrust struct {
	RootPEM                []byte
	TrustDomain            string
	AccountID              string
	CloudAccountID         string
	CertificateRevocations pki.CertificateRevocationChecker
	Now                    func() time.Time
}

func prepareNodeAuthorization(ctx context.Context, source NodeCredentialSource, trust TargetTrust) (NodeCredentialSource, error) {
	if source == nil {
		return nil, errors.New("client: node authorization requires login credentials")
	}
	if len(trust.RootPEM) == 0 || trust.TrustDomain == "" || trust.AccountID == "" || trust.CertificateRevocations == nil {
		return nil, errors.New("client: node authorization requires pinned root, trust domain, accounts, and revocation state")
	}
	credentials, err := source.NodeCredentials(ctx)
	if err != nil {
		return nil, fmt.Errorf("client: node credentials: %w", err)
	}
	if credentials.AccessToken == "" || credentials.Signer == nil {
		return nil, errors.New("client: incomplete node credentials")
	}
	if _, err := credentials.Signer.PublicSPKIDER(); err != nil {
		return nil, fmt.Errorf("client: session signer: %w", err)
	}
	return fixedNodeCredentialSource{credentials: credentials}, nil
}

// ECDSASessionSigner adapts a persistent P-256 private key to SessionSigner.
type ECDSASessionSigner struct {
	key *ecdsa.PrivateKey
}

func NewECDSASessionSigner(key *ecdsa.PrivateKey) (*ECDSASessionSigner, error) {
	if key == nil || key.Curve != elliptic.P256() || key.D == nil {
		return nil, errors.New("client: session signer requires a P-256 private key")
	}
	return &ECDSASessionSigner{key: key}, nil
}

func (s *ECDSASessionSigner) PublicSPKIDER() ([]byte, error) {
	return x509.MarshalPKIXPublicKey(&s.key.PublicKey)
}

func (s *ECDSASessionSigner) SignP1363(domain string, exactBody []byte) ([]byte, error) {
	msg := make([]byte, 0, len(domain)+len(exactBody))
	msg = append(msg, domain...)
	msg = append(msg, exactBody...)
	digest := sha256.Sum256(msg)
	r, ss, err := ecdsa.Sign(rand.Reader, s.key, digest[:])
	if err != nil {
		return nil, err
	}
	sig := make([]byte, 64)
	rb, sb := r.Bytes(), ss.Bytes()
	copy(sig[32-len(rb):32], rb)
	copy(sig[64-len(sb):], sb)
	return sig, nil
}

func buildSignedIntent(op intent.Op, body *authv1.IntentBody, signer SessionSigner) (*authv1.SignedIntent, error) {
	if signer == nil {
		return nil, errors.New("client: session signer is required")
	}
	bodyBytes, err := proto.Marshal(body)
	if err != nil {
		return nil, err
	}
	domain := intent.DomainFor(op)
	sig, err := signer.SignP1363(domain, bodyBytes)
	if err != nil {
		return nil, err
	}
	spki, err := signer.PublicSPKIDER()
	if err != nil {
		return nil, err
	}
	return &authv1.SignedIntent{Domain: domain, Body: bodyBytes, Sig: sig, SpkiDer: spki}, nil
}
