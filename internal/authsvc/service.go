// Package authsvc implements Spawnery's Auth Service (AS): the identity root of trust, run in its own
// container apart from the CP. It holds the self-hosted intermediate CA key (which never leaves the
// service) and issues self-hosted node certificates; it publishes the Root CA for clients/CP/nodes to
// pin. It CANNOT issue cloud certs — the cloud intermediate is offline (see node-auth design §1/§4).
// Enrollment-token authentication (sp-0qc) and AS-signed sessions (sp-3ca) build on this skeleton.
package authsvc

import (
	"crypto/x509"
	"time"

	"spawnery/internal/pki"
)

// Service is the Auth Service. It holds the self-hosted intermediate (cert + key) and the Root CA cert
// it publishes for pinning. By construction it holds ONLY the self-hosted intermediate, so it can issue
// self-hosted identities only.
type Service struct {
	root         *x509.Certificate
	intermediate *pki.CA // self-hosted intermediate (holds the signing key)
}

// New builds a Service from an in-memory root cert + self-hosted intermediate CA.
func New(root *x509.Certificate, selfHostedIntermediate *pki.CA) *Service {
	return &Service{root: root, intermediate: selfHostedIntermediate}
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
