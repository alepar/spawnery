// Package authsvc implements Spawnery's Auth Service (AS): the identity root of trust, run in its own
// container apart from the CP. It holds the self-hosted intermediate CA key (which never leaves the
// service) and issues self-hosted node certificates; it publishes the Root CA for clients/CP/nodes to
// pin. It CANNOT issue cloud certs — the cloud intermediate is offline (see node-auth design §1/§4).
// Enrollment-token authentication (sp-0qc) and AS-signed sessions (sp-3ca) build on this skeleton.
package authsvc

import (
	"crypto/x509"
	"sync"
	"time"

	"spawnery/internal/pki"
)

const (
	defaultEnrollTTL = 10 * time.Minute    // one-time enrollment tokens are short-lived
	nodeCertTTL      = 90 * 24 * time.Hour // issued node-leaf validity
)

// Service is the Auth Service. It holds the self-hosted intermediate (cert + key) and the Root CA cert
// it publishes for pinning. By construction it holds ONLY the self-hosted intermediate, so it can issue
// self-hosted identities only.
type Service struct {
	root         *x509.Certificate
	intermediate *pki.CA // self-hosted intermediate (holds the signing key)

	now       func() time.Time
	enrollTTL time.Duration

	mu     sync.Mutex
	tokens map[string]enrollToken // pending one-time enrollment tokens
}

type enrollToken struct {
	accountID string
	exp       time.Time
	used      bool
}

// Option configures a Service.
type Option func(*Service)

// WithClock overrides the time source (tests).
func WithClock(now func() time.Time) Option { return func(s *Service) { s.now = now } }

// WithEnrollTokenTTL overrides the enrollment-token lifetime.
func WithEnrollTokenTTL(d time.Duration) Option { return func(s *Service) { s.enrollTTL = d } }

// New builds a Service from an in-memory root cert + self-hosted intermediate CA.
func New(root *x509.Certificate, selfHostedIntermediate *pki.CA, opts ...Option) *Service {
	s := &Service{
		root:         root,
		intermediate: selfHostedIntermediate,
		now:          time.Now,
		enrollTTL:    defaultEnrollTTL,
		tokens:       map[string]enrollToken{},
	}
	for _, o := range opts {
		o(s)
	}
	return s
}

// Load builds a Service from PEM material as it would be provisioned in production: the Root CA cert
// (published), and the self-hosted intermediate cert + private key (held secret).
func Load(rootPEM, interCertPEM, interKeyPEM []byte) (*Service, error) {
	root, err := pki.ParseCertPEM(rootPEM)
	if err != nil {
		return nil, err
	}
	interCert, err := pki.ParseCertPEM(interCertPEM)
	if err != nil {
		return nil, err
	}
	interKey, err := pki.ParseKeyPEM(interKeyPEM)
	if err != nil {
		return nil, err
	}
	return New(root, &pki.CA{Cert: interCert, Key: interKey}), nil
}

// IssueSelfHostedNode issues a self-hosted node certificate bound to accountID. The class is always
// self-hosted — the AS has no cloud intermediate to sign anything else.
func (s *Service) IssueSelfHostedNode(nodeID, accountID string, notAfter time.Time) (*pki.Node, error) {
	return s.intermediate.IssueNode(nodeID, accountID, pki.ClassSelfHosted, notAfter)
}

// RootCAPEM returns the Root CA certificate clients/CP/nodes pin as their trust anchor.
func (s *Service) RootCAPEM() []byte {
	return pki.MarshalCertPEM(s.root)
}
