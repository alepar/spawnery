// Package authsvc implements Spawnery's Auth Service (AS): the identity root of trust, run in its own
// container apart from the CP. It holds the self-hosted intermediate CA key (which never leaves the
// service) and issues self-hosted node certificates; it publishes the Root CA for clients/CP/nodes to
// pin. It CANNOT issue cloud certs — the cloud intermediate is offline (see node-auth design §1/§4).
// Enrollment-token authentication (sp-0qc) and AS-signed sessions (sp-3ca) build on this skeleton.
package authsvc

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"sync"
	"time"

	"spawnery/internal/authsvc/store"
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

	sessionKey ed25519.PrivateKey // signs AS session tokens (sp-3ca); CP never holds it

	idp *IdP // identity core (A1: OAuth, refresh, device grant); nil until WithIdP is called

	deviceSet *deviceSetHandler // device-set registry; nil until WithDeviceSet is called

	nodeRevocations store.NodeRevocationRepo

	mu     sync.Mutex
	tokens map[string]enrollToken // pending one-time enrollment tokens
}

type enrollToken struct {
	accountID   string
	class       string // class to sign (only self-hosted; the AS has no cloud intermediate)
	fingerprint string // SPKI fingerprint the redeeming CSR key must match; "" = legacy unbound
	exp         time.Time
	used        bool
}

// Option configures a Service.
type Option func(*Service)

// WithClock overrides the time source (tests).
func WithClock(now func() time.Time) Option { return func(s *Service) { s.now = now } }

// WithEnrollTokenTTL overrides the enrollment-token lifetime.
func WithEnrollTokenTTL(d time.Duration) Option { return func(s *Service) { s.enrollTTL = d } }

// WithSessionKey sets the session-signing key (production loads a persisted key; default generates one).
func WithSessionKey(k ed25519.PrivateKey) Option { return func(s *Service) { s.sessionKey = k } }

// WithIdP attaches the identity core (OAuth, refresh, device grant) to the Service. Call after
// constructing a *IdP with NewIdP; the IdP's routes are registered in Handler().
func WithIdP(idp *IdP) Option { return func(s *Service) { s.idp = idp } }

// WithDeviceSet attaches the device-set registry to the Service.
//
//   - st is a DeviceSetRepo (the AS store's DeviceSets() method).
//   - spaOrigin is the exact Origin the browser SPA is served from (e.g. "https://app.example.com").
//     Pass "" to disable origin enforcement (tests only).
//   - accountFromReq extracts the authenticated account ID from a request.
func WithDeviceSet(st store.DeviceSetRepo, spaOrigin string, accountFromReq AccountFromRequest) Option {
	return func(s *Service) {
		s.deviceSet = &deviceSetHandler{
			st:             st,
			spaOrigin:      spaOrigin,
			accountFromReq: accountFromReq,
		}
	}
}

// WithNodeRevocations attaches the AS-published node deny-list store.
func WithNodeRevocations(st store.NodeRevocationRepo) Option {
	return func(s *Service) { s.nodeRevocations = st }
}

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
	if s.sessionKey == nil {
		_, s.sessionKey, _ = ed25519.GenerateKey(rand.Reader)
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
