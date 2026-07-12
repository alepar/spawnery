package mtls

import (
	"bytes"
	"crypto"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"math/big"
	"time"

	"spawnery/internal/pki"
)

// ClientOptions configures an internal Spawnery TLS client.
type ClientOptions struct {
	Root                *x509.Certificate
	TrustDomain         string
	Identity            tls.Certificate
	ServerName          string
	ExpectedServiceRole string
	CurrentTime         func() time.Time
	IsRevoked           func(issuer, serial *big.Int) bool
}

// PeerVerifierOptions configures shared server-side peer verification.
type PeerVerifierOptions struct {
	Root        *x509.Certificate
	TrustDomain string
	CurrentTime func() time.Time
	IsRevoked   func(issuer, serial *big.Int) bool
}

// PeerVerifier is immutable verification policy shared by TLS handshake and request middleware.
// The callbacks may consult concurrency-safe live clock and revocation state.
type PeerVerifier struct {
	root        *x509.Certificate
	trustDomain string
	currentTime func() time.Time
	isRevoked   func(issuer, serial *big.Int) bool
}

// NewPeerVerifier validates and copies server-side peer verification policy.
func NewPeerVerifier(opts PeerVerifierOptions) (*PeerVerifier, error) {
	if opts.Root == nil {
		return nil, errors.New("mtls: nil root certificate")
	}
	if err := pki.ValidateTrustDomain(opts.TrustDomain); err != nil {
		return nil, fmt.Errorf("mtls: invalid trust domain: %w", err)
	}
	if opts.CurrentTime == nil {
		return nil, errors.New("mtls: current time callback is required")
	}
	if opts.IsRevoked == nil {
		return nil, errors.New("mtls: revocation callback is required")
	}
	root, err := x509.ParseCertificate(opts.Root.Raw)
	if err != nil {
		return nil, fmt.Errorf("mtls: parse root certificate: %w", err)
	}
	return &PeerVerifier{
		root:        root,
		trustDomain: opts.TrustDomain,
		currentTime: opts.CurrentTime,
		isRevoked:   opts.IsRevoked,
	}, nil
}

// ClientCertificateMode controls whether an internal server requires a client identity.
type ClientCertificateMode int

const (
	RequireClientCertificate ClientCertificateMode = iota
	VerifyClientCertificateIfGiven
)

// ServerOptions configures an internal Spawnery TLS server.
type ServerOptions struct {
	Verifier   *PeerVerifier
	Identity   tls.Certificate
	ClientMode ClientCertificateMode
}

// ClientConfig builds a TLS client configuration that verifies both the endpoint name and the
// exact Spawnery service role.
func ClientConfig(opts ClientOptions) (*tls.Config, error) {
	if opts.Root == nil {
		return nil, errors.New("mtls: nil root certificate")
	}
	if opts.ServerName == "" {
		return nil, errors.New("mtls: server name is required")
	}
	if opts.ExpectedServiceRole != pki.RoleCP && opts.ExpectedServiceRole != pki.RoleAuthService {
		return nil, fmt.Errorf("mtls: unsupported expected service role %q", opts.ExpectedServiceRole)
	}
	if err := pki.ValidateTrustDomain(opts.TrustDomain); err != nil {
		return nil, fmt.Errorf("mtls: invalid trust domain: %w", err)
	}

	roots := x509.NewCertPool()
	roots.AddCert(opts.Root)
	config := &tls.Config{
		RootCAs:    roots,
		ServerName: opts.ServerName,
		MinVersion: tls.VersionTLS13,
		Time:       opts.CurrentTime,
	}
	if len(opts.Identity.Certificate) != 0 {
		config.Certificates = []tls.Certificate{opts.Identity}
	}
	config.VerifyConnection = func(state tls.ConnectionState) error {
		principal, err := verifyConnectionPrincipal(state, opts.Root, opts.TrustDomain, opts.CurrentTime, opts.IsRevoked, x509.ExtKeyUsageServerAuth)
		if err != nil {
			return err
		}
		if principal.Kind != pki.KindService || principal.Role != opts.ExpectedServiceRole {
			return fmt.Errorf("mtls: peer principal is %s:%s, want service:%s", principal.Kind, principal.Role, opts.ExpectedServiceRole)
		}
		return nil
	}
	return config, nil
}

// ServerConfig builds a TLS server configuration that validates every presented client identity as
// a typed Spawnery principal. Optional mode permits only certificate absence.
func ServerConfig(opts ServerOptions) (*tls.Config, error) {
	if opts.Verifier == nil {
		return nil, errors.New("mtls: peer verifier is required")
	}
	if err := validateServerIdentity(opts.Identity); err != nil {
		return nil, err
	}

	var clientAuth tls.ClientAuthType
	switch opts.ClientMode {
	case RequireClientCertificate:
		clientAuth = tls.RequireAnyClientCert
	case VerifyClientCertificateIfGiven:
		clientAuth = tls.RequestClientCert
	default:
		return nil, fmt.Errorf("mtls: unsupported client certificate mode %d", opts.ClientMode)
	}

	clientCAs := x509.NewCertPool()
	clientCAs.AddCert(opts.Verifier.root)
	config := &tls.Config{
		Certificates: []tls.Certificate{opts.Identity},
		ClientAuth:   clientAuth,
		ClientCAs:    clientCAs,
		MinVersion:   tls.VersionTLS13,
		Time:         opts.Verifier.currentTime,
	}
	config.VerifyConnection = func(state tls.ConnectionState) error {
		if len(state.PeerCertificates) == 0 {
			if opts.ClientMode == VerifyClientCertificateIfGiven {
				return nil
			}
			return errors.New("mtls: client certificate is required")
		}
		_, err := opts.Verifier.verifyPresented(state.PeerCertificates, x509.ExtKeyUsageClientAuth)
		return err
	}
	return config, nil
}

func validateServerIdentity(identity tls.Certificate) error {
	if len(identity.Certificate) == 0 {
		return errors.New("mtls: server identity is required")
	}
	signer, ok := identity.PrivateKey.(crypto.Signer)
	if !ok || signer == nil {
		return errors.New("mtls: server identity requires a usable private key")
	}
	leaf := identity.Leaf
	if leaf == nil {
		var err error
		leaf, err = x509.ParseCertificate(identity.Certificate[0])
		if err != nil {
			return fmt.Errorf("mtls: parse server identity leaf: %w", err)
		}
	}
	leafPublic, err := x509.MarshalPKIXPublicKey(leaf.PublicKey)
	if err != nil {
		return fmt.Errorf("mtls: marshal server identity public key: %w", err)
	}
	signerPublic, err := x509.MarshalPKIXPublicKey(signer.Public())
	if err != nil {
		return fmt.Errorf("mtls: marshal server private key public key: %w", err)
	}
	if !bytes.Equal(leafPublic, signerPublic) {
		return errors.New("mtls: server private key does not match leaf certificate")
	}
	return nil
}

func (v *PeerVerifier) verifyPresented(chain []*x509.Certificate, usage x509.ExtKeyUsage) (pki.Principal, error) {
	return verifyPresentedPrincipal(chain, v.root, v.trustDomain, v.currentTime, v.isRevoked, usage)
}

func verifyConnectionPrincipal(state tls.ConnectionState, root *x509.Certificate, trustDomain string, currentTime func() time.Time, isRevoked func(issuer, serial *big.Int) bool, usage x509.ExtKeyUsage) (pki.Principal, error) {
	if len(state.VerifiedChains) == 0 || len(state.VerifiedChains[0]) < 2 {
		return pki.Principal{}, errors.New("mtls: peer certificate has no verified chain")
	}
	chain := state.VerifiedChains[0]
	return verifyPresentedPrincipal(chain[:len(chain)-1], root, trustDomain, currentTime, isRevoked, usage)
}

func verifyPresentedPrincipal(chain []*x509.Certificate, root *x509.Certificate, trustDomain string, currentTime func() time.Time, isRevoked func(issuer, serial *big.Int) bool, usage x509.ExtKeyUsage) (pki.Principal, error) {
	if len(chain) == 0 {
		return pki.Principal{}, errors.New("mtls: peer certificate is required")
	}
	now := time.Now()
	if currentTime != nil {
		now = currentTime()
	}
	principal, err := pki.VerifyPrincipal(chain[0], chain[1:], pki.VerifyOptions{
		Root:        root,
		TrustDomain: trustDomain,
		CurrentTime: now,
		KeyUsages:   []x509.ExtKeyUsage{usage},
		IsRevoked:   isRevoked,
	})
	if err != nil {
		return pki.Principal{}, fmt.Errorf("mtls: verify peer principal: %w", err)
	}
	return principal, nil
}
