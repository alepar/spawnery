package mtls

import (
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

// ClientCertificateMode controls whether an internal server requires a client identity.
type ClientCertificateMode int

const (
	RequireClientCertificate ClientCertificateMode = iota
	VerifyClientCertificateIfGiven
)

// ServerOptions configures an internal Spawnery TLS server.
type ServerOptions struct {
	Root        *x509.Certificate
	TrustDomain string
	Identity    tls.Certificate
	ClientMode  ClientCertificateMode
	CurrentTime func() time.Time
	IsRevoked   func(issuer, serial *big.Int) bool
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
	if opts.Root == nil {
		return nil, errors.New("mtls: nil root certificate")
	}
	if len(opts.Identity.Certificate) == 0 {
		return nil, errors.New("mtls: server identity is required")
	}
	if err := pki.ValidateTrustDomain(opts.TrustDomain); err != nil {
		return nil, fmt.Errorf("mtls: invalid trust domain: %w", err)
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
	clientCAs.AddCert(opts.Root)
	config := &tls.Config{
		Certificates: []tls.Certificate{opts.Identity},
		ClientAuth:   clientAuth,
		ClientCAs:    clientCAs,
		MinVersion:   tls.VersionTLS13,
		Time:         opts.CurrentTime,
	}
	config.VerifyConnection = func(state tls.ConnectionState) error {
		if len(state.PeerCertificates) == 0 {
			if opts.ClientMode == VerifyClientCertificateIfGiven {
				return nil
			}
			return errors.New("mtls: client certificate is required")
		}
		_, err := verifyPresentedPrincipal(state.PeerCertificates, opts.Root, opts.TrustDomain, opts.CurrentTime, opts.IsRevoked, x509.ExtKeyUsageClientAuth)
		return err
	}
	return config, nil
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
