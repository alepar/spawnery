// Package authsvc implements Spawnery's Auth Service (AS): the identity root of trust, run in its own
// container apart from the CP. It holds the self-hosted intermediate CA key (which never leaves the
// service) and issues self-hosted node certificates; it publishes the Root CA for clients/CP/nodes to
// pin. It CANNOT issue cloud certs — the cloud intermediate is offline (see node-auth design §1/§4).
// Enrollment-token authentication (sp-0qc) and AS-signed sessions (sp-3ca) build on this skeleton.
package authsvc

import (
	"context"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"fmt"
	"math/big"
	"net/url"
	"sync"
	"time"

	"spawnery/internal/authsvc/store"
	"spawnery/internal/authsvc/token"
	"spawnery/internal/pki"
)

// NewDevelopmentSigningCredential creates the ephemeral auth-signing hierarchy used only by
// explicit development and hermetic-test bootstraps. Production loads pre-certified leaves and
// never places the auth-signing intermediate key online.
func NewDevelopmentSigningCredential(root *pki.CA, environment string, now time.Time) (*token.SigningCredential, error) {
	if root == nil || root.Cert == nil || root.Key == nil || environment == "" {
		return nil, errors.New("authsvc: invalid development signing root")
	}
	serial := func() (*big.Int, error) {
		value, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
		if err != nil {
			return nil, err
		}
		return value.SetBit(value, 127, 1), nil
	}
	issue := func(template, parent *x509.Certificate, publicKey, signer any) (*x509.Certificate, error) {
		der, err := x509.CreateCertificate(rand.Reader, template, parent, publicKey, signer)
		if err != nil {
			return nil, err
		}
		return x509.ParseCertificate(der)
	}
	intermediateSerial, err := serial()
	if err != nil {
		return nil, err
	}
	intermediateKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	intermediateTemplate := &x509.Certificate{
		SerialNumber: intermediateSerial, Subject: pkix.Name{CommonName: "Spawnery development auth signing intermediate"},
		NotBefore: now.Add(-time.Minute), NotAfter: now.Add(180 * 24 * time.Hour),
		KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageCRLSign, BasicConstraintsValid: true, IsCA: true, MaxPathLen: 0,
		Policies: []x509.OID{pki.AuthSigningIntermediatePolicyOID},
	}
	intermediate, err := issue(intermediateTemplate, root.Cert, &intermediateKey.PublicKey, root.Key)
	if err != nil {
		return nil, err
	}
	leafSerial, err := serial()
	if err != nil {
		return nil, err
	}
	_, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	identity, err := url.Parse("spiffe://" + environment + ".spawnery.internal/signer/auth-artifact/dev")
	if err != nil {
		return nil, err
	}
	leafTemplate := &x509.Certificate{
		SerialNumber: leafSerial, Subject: pkix.Name{CommonName: "Spawnery development auth artifact signer"},
		NotBefore: now.Add(-time.Minute), NotAfter: now.Add(90 * 24 * time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, Policies: []x509.OID{pki.AuthArtifactSignerPolicyOID}, URIs: []*url.URL{identity},
	}
	leaf, err := issue(leafTemplate, intermediate, privateKey.Public(), intermediateKey)
	if err != nil {
		return nil, err
	}
	return token.NewSigningCredential(privateKey, []*x509.Certificate{leaf, intermediate}, root.Cert, environment, now)
}

const (
	defaultEnrollTTL = 10 * time.Minute    // one-time enrollment tokens are short-lived
	nodeCertTTL      = 90 * 24 * time.Hour // issued node-leaf validity
)

// Service is the Auth Service. It holds the self-hosted intermediate (cert + key) and the Root CA cert
// it publishes for pinning. By construction it holds ONLY the self-hosted intermediate, so it can issue
// self-hosted identities only.
type Service struct {
	root                   *x509.Certificate
	intermediate           *pki.CA // self-hosted intermediate (holds the signing key)
	trustDomain            string
	certificateRevocations pki.CertificateRevocationChecker

	now       func() time.Time
	enrollTTL time.Duration

	idp                      *IdP // identity core (A1: OAuth, refresh, device grant); nil until WithIdP is called
	enrollmentAccountFromReq AccountFromRequest

	deviceSet *deviceSetHandler // device-set registry; nil until WithDeviceSet is called

	nodeRevocationStore store.Store
	nodeCRLSink         func([]byte) error
	nodeCRLPublishMu    sync.Mutex
	nodeCRLCommitted    func(*big.Int)

	githubMintStore       store.Store
	githubMintProvider    GitHubProvider
	nodeIdentityExtractor NodeIdentityExtractor
	githubMintAuthorizer  GitHubMintAuthorizer
	githubTokenSignal     GitHubTokenRotatedNotifier
	githubMintLocksMu     sync.Mutex
	githubMintLocks       map[string]*sync.Mutex

	githubLinkExchanger      GitHubLinkExchanger
	githubLinkStore          store.Store
	githubLinkAppClientID    string
	githubLinkRedirectURI    string // AS's own /github/link/callback URL registered at the App
	githubLinkPostRedeem     string // SPA page to land on after callback (no nonce in URL)
	githubLinkDefaultHost    string // e.g. "github.com"
	githubLinkAccountFromReq AccountFromRequest
	githubLinkSPAOrigin      string // exact Origin the SPA is served from; "" disables enforcement
	githubLinkMu             sync.Mutex
	githubLinkStates         map[string]githubLinkState // keyed by OAuth state param
	githubLinkFlows          map[string]*githubLinkFlow // keyed by flow_id

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

type NodeIdentityExtractor func(context.Context) (nodeID string, ok bool)

type GitHubMintAuthorization struct {
	NodeID       string
	SpawnID      string
	Generation   uint64
	SecretID     string
	Version      uint64
	DeliveryID   string
	RepositoryID string
}

type GitHubMintAuthorizer interface {
	AuthorizeGitHubMint(context.Context, GitHubMintAuthorization) error
}

type GitHubMintAuthorizerFunc func(context.Context, GitHubMintAuthorization) error

func (f GitHubMintAuthorizerFunc) AuthorizeGitHubMint(ctx context.Context, req GitHubMintAuthorization) error {
	return f(ctx, req)
}

type GitHubTokenRotatedSignal struct {
	SecretID            string
	Version             uint64
	DeliveryID          string
	AccessExpiresAtUnix int64
}

type GitHubTokenRotatedNotifier interface {
	SignalGitHubTokenRotated(context.Context, GitHubTokenRotatedSignal) error
}

type GitHubTokenRotatedNotifierFunc func(context.Context, GitHubTokenRotatedSignal) error

func (f GitHubTokenRotatedNotifierFunc) SignalGitHubTokenRotated(ctx context.Context, sig GitHubTokenRotatedSignal) error {
	return f(ctx, sig)
}

// WithClock overrides the time source (tests).
func WithClock(now func() time.Time) Option { return func(s *Service) { s.now = now } }

// WithEnrollTokenTTL overrides the enrollment-token lifetime.
func WithEnrollTokenTTL(d time.Duration) Option { return func(s *Service) { s.enrollTTL = d } }

// WithTrustDomain selects the environment SPIFFE trust domain for issuance and peer verification.
func WithTrustDomain(trustDomain string) Option {
	return func(s *Service) { s.trustDomain = trustDomain }
}

// WithCertificateRevocations installs the mandatory fail-closed certificate revocation view.
func WithCertificateRevocations(checker pki.CertificateRevocationChecker) Option {
	return func(s *Service) { s.certificateRevocations = checker }
}

// Validate checks construction-time settings required by production issuance and verification.
func (s *Service) Validate() error {
	if s == nil {
		return fmt.Errorf("authsvc: nil service")
	}
	if err := pki.ValidateTrustDomain(s.trustDomain); err != nil {
		return fmt.Errorf("authsvc: trust domain: %w", err)
	}
	if s.certificateRevocations == nil {
		return errors.New("authsvc: certificate revocation state is required")
	}
	return nil
}

// WithIdP attaches the identity core (OAuth, refresh, device grant) to the Service. Call after
// constructing a *IdP with NewIdP; the IdP's routes are registered in Handler().
func WithIdP(idp *IdP) Option { return func(s *Service) { s.idp = idp } }

// WithEnrollmentTokenIssuance enables authenticated public issuance of fingerprint-bound node
// enrollment tokens. The extractor must verify the caller's AS session and return its account ID.
func WithEnrollmentTokenIssuance(accountFromReq AccountFromRequest) Option {
	return func(s *Service) { s.enrollmentAccountFromReq = accountFromReq }
}

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

func WithNodeRevocationStore(st store.Store, sink func([]byte) error) Option {
	return func(s *Service) {
		s.nodeRevocationStore = st
		s.nodeCRLSink = sink
	}
}

func WithGitHubMinting(st store.Store, provider GitHubProvider) Option {
	return func(s *Service) {
		s.githubMintStore = st
		s.githubMintProvider = provider
	}
}

func WithNodeIdentityExtractor(extract NodeIdentityExtractor) Option {
	return func(s *Service) { s.nodeIdentityExtractor = extract }
}

func WithGitHubMintAuthorizer(authz GitHubMintAuthorizer) Option {
	return func(s *Service) { s.githubMintAuthorizer = authz }
}

func WithGitHubTokenRotatedNotifier(notifier GitHubTokenRotatedNotifier) Option {
	return func(s *Service) { s.githubTokenSignal = notifier }
}

// GitHubLinkConfig configures the owner-driven GitHub App link-bootstrap flow (spec r2 §5-§6).
// Store is where the durable AS-custodial refresh chain is persisted via RedeemUpsert.
type GitHubLinkConfig struct {
	Exchanger          GitHubLinkExchanger
	Store              store.Store
	AppClientID        string
	RedirectURI        string
	PostRedeemRedirect string
	DefaultHost        string
	AccountFromReq     AccountFromRequest
	// SPAOrigin is the exact Origin the browser SPA is served from (e.g. "https://app.example.com").
	// Required for credentialed CORS on /github/link/redeem (spike S1). Pass "" in tests that
	// don't test CORS (all link endpoints will accept any Origin).
	SPAOrigin string
}

func WithGitHubLink(cfg GitHubLinkConfig) Option {
	return func(s *Service) {
		s.githubLinkExchanger = cfg.Exchanger
		s.githubLinkStore = cfg.Store
		s.githubLinkAppClientID = cfg.AppClientID
		s.githubLinkRedirectURI = cfg.RedirectURI
		s.githubLinkPostRedeem = cfg.PostRedeemRedirect
		s.githubLinkDefaultHost = cfg.DefaultHost
		s.githubLinkAccountFromReq = cfg.AccountFromReq
		s.githubLinkSPAOrigin = cfg.SPAOrigin
		s.githubLinkStates = map[string]githubLinkState{}
		s.githubLinkFlows = map[string]*githubLinkFlow{}
	}
}

// New builds a Service from an in-memory root cert + self-hosted intermediate CA.
func New(root *x509.Certificate, selfHostedIntermediate *pki.CA, opts ...Option) *Service {
	s := &Service{
		root:                  root,
		intermediate:          selfHostedIntermediate,
		trustDomain:           pki.DefaultTrustDomain,
		now:                   time.Now,
		enrollTTL:             defaultEnrollTTL,
		tokens:                map[string]enrollToken{},
		githubMintLocks:       map[string]*sync.Mutex{},
		nodeIdentityExtractor: nodeIDFromContext,
	}
	for _, o := range opts {
		o(s)
	}
	if s.idp != nil {
		if s.githubMintStore == nil {
			s.githubMintStore = s.idp.store
		}
		if s.githubMintProvider == nil {
			s.githubMintProvider = s.idp.github
		}
	}
	return s
}

// Load builds a Service from PEM material as it would be provisioned in production: the Root CA cert
// (published), and the self-hosted intermediate cert + private key (held secret).
func Load(rootPEM, interCertPEM, interKeyPEM []byte, trustDomain string, revoked pki.CertificateRevocationChecker) (*Service, error) {
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
	service := New(root, &pki.CA{Cert: interCert, Key: interKey}, WithTrustDomain(trustDomain), WithCertificateRevocations(revoked))
	if err := service.Validate(); err != nil {
		return nil, err
	}
	return service, nil
}

// IssueSelfHostedNode issues a self-hosted node certificate bound to accountID. The class is always
// self-hosted — the AS has no cloud intermediate to sign anything else.
func (s *Service) IssueSelfHostedNode(nodeID, accountID string, notAfter time.Time) (*pki.Node, error) {
	return s.intermediate.IssueNode(nodeID, accountID, pki.ClassSelfHosted, s.trustDomain, notAfter)
}

// RootCAPEM returns the Root CA certificate clients/CP/nodes pin as their trust anchor.
func (s *Service) RootCAPEM() []byte {
	return pki.MarshalCertPEM(s.root)
}
