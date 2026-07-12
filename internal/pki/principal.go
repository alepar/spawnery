package pki

import (
	"crypto/x509"
	"errors"
	"fmt"
	"math/big"
	"net/url"
	"strings"
	"time"
)

const (
	KindService = "service"
	KindNode    = "node"

	RoleCP          = "cp"
	RoleAuthService = "authsvc"
	RoleCloud       = "cloud"
	RoleSelfHosted  = "self-hosted"
)

// Principal is the typed identity carried by a Spawnery X.509-SVID.
type Principal struct {
	TrustDomain string
	Kind        string
	Role        string
	InstanceID  string
	AccountID   string
	NodeID      string
}

// VerifyOptions configures strict Spawnery X.509-SVID verification.
type VerifyOptions struct {
	Root        *x509.Certificate
	TrustDomain string
	CurrentTime time.Time
	KeyUsages   []x509.ExtKeyUsage
	IsRevoked   func(issuer, serial *big.Int) bool
}

// VerifyPrincipal validates leaf to Root and returns its typed Spawnery principal.
func VerifyPrincipal(leaf *x509.Certificate, intermediates []*x509.Certificate, opts VerifyOptions) (Principal, error) {
	if leaf == nil {
		return Principal{}, errors.New("pki: nil leaf certificate")
	}
	if opts.Root == nil {
		return Principal{}, errors.New("pki: nil root certificate")
	}
	if err := validateTrustDomain(opts.TrustDomain); err != nil {
		return Principal{}, err
	}
	roots := x509.NewCertPool()
	roots.AddCert(opts.Root)
	intermediatePool := x509.NewCertPool()
	for _, cert := range intermediates {
		if cert == nil {
			return Principal{}, errors.New("pki: nil intermediate certificate")
		}
		intermediatePool.AddCert(cert)
	}
	keyUsages := opts.KeyUsages
	if len(keyUsages) == 0 {
		keyUsages = []x509.ExtKeyUsage{x509.ExtKeyUsageAny}
	}
	chains, err := leaf.Verify(x509.VerifyOptions{
		Roots:         roots,
		Intermediates: intermediatePool,
		CurrentTime:   opts.CurrentTime,
		KeyUsages:     keyUsages,
	})
	if err != nil {
		return Principal{}, fmt.Errorf("pki: certificate path validation: %w", err)
	}
	if err := validateLeafProfile(leaf); err != nil {
		return Principal{}, err
	}
	principal, err := ParsePrincipal(leaf.URIs[0], opts.TrustDomain)
	if err != nil {
		return Principal{}, err
	}
	for _, chain := range chains {
		if len(chain) != 3 || !chain[len(chain)-1].Equal(opts.Root) {
			continue
		}
		issuer := chain[1]
		issuerRole, err := IssuerRoleFromCertificate(issuer)
		if err != nil || !issuerPermitsPrincipal(issuerRole, principal) {
			continue
		}
		if opts.IsRevoked != nil && opts.IsRevoked(issuer.SerialNumber, leaf.SerialNumber) {
			return Principal{}, errors.New("pki: certificate is revoked")
		}
		return principal, nil
	}
	return Principal{}, errors.New("pki: no verified chain has a permitted issuer role")
}

func validateLeafProfile(leaf *x509.Certificate) error {
	if leaf.IsCA || !leaf.BasicConstraintsValid {
		return errors.New("pki: leaf must carry a CA=false basic constraint")
	}
	if leaf.KeyUsage != x509.KeyUsageDigitalSignature {
		return fmt.Errorf("pki: leaf key usage is %v, want DigitalSignature only", leaf.KeyUsage)
	}
	if len(leaf.ExtKeyUsage) != 2 || !hasExtKeyUsage(leaf.ExtKeyUsage, x509.ExtKeyUsageClientAuth) || !hasExtKeyUsage(leaf.ExtKeyUsage, x509.ExtKeyUsageServerAuth) || len(leaf.UnknownExtKeyUsage) != 0 {
		return errors.New("pki: leaf must carry exactly ClientAuth and ServerAuth EKUs")
	}
	if len(leaf.URIs) != 1 {
		return fmt.Errorf("pki: leaf URI SAN count is %d, want 1", len(leaf.URIs))
	}
	return nil
}

func hasExtKeyUsage(usages []x509.ExtKeyUsage, want x509.ExtKeyUsage) bool {
	for _, usage := range usages {
		if usage == want {
			return true
		}
	}
	return false
}

func issuerPermitsPrincipal(role IssuerRole, principal Principal) bool {
	switch role {
	case IssuerService:
		return principal.Kind == KindService && isServiceRole(principal.Role)
	case IssuerCloudNode:
		return principal.Kind == KindNode && principal.Role == RoleCloud
	case IssuerSelfHostedNode:
		return principal.Kind == KindNode && principal.Role == RoleSelfHosted
	default:
		return false
	}
}

// ParsePrincipal parses a canonical Spawnery SPIFFE ID in trustDomain.
func ParsePrincipal(id *url.URL, trustDomain string) (Principal, error) {
	if id == nil {
		return Principal{}, errors.New("pki: nil SPIFFE ID")
	}
	if err := validateTrustDomain(trustDomain); err != nil {
		return Principal{}, err
	}
	if id.Scheme != "spiffe" || id.Host != trustDomain || id.User != nil || id.RawQuery != "" || id.Fragment != "" || id.ForceQuery || id.Opaque != "" {
		return Principal{}, fmt.Errorf("pki: non-canonical SPIFFE ID %q", id.String())
	}
	if id.RawPath != "" || strings.Contains(id.EscapedPath(), "%") {
		return Principal{}, fmt.Errorf("pki: escaped SPIFFE path is not permitted: %q", id.String())
	}
	segments := strings.Split(strings.TrimPrefix(id.Path, "/"), "/")
	if !strings.HasPrefix(id.Path, "/") {
		return Principal{}, fmt.Errorf("pki: SPIFFE ID path must be absolute: %q", id.Path)
	}
	for _, segment := range segments {
		if err := validatePathSegment(segment); err != nil {
			return Principal{}, err
		}
	}

	principal := Principal{TrustDomain: trustDomain}
	switch {
	case len(segments) == 3 && segments[0] == KindService && isServiceRole(segments[1]):
		principal.Kind = KindService
		principal.Role = segments[1]
		principal.InstanceID = segments[2]
	case len(segments) == 4 && segments[0] == KindNode && isNodeRole(segments[1]):
		principal.Kind = KindNode
		principal.Role = segments[1]
		principal.AccountID = segments[2]
		principal.NodeID = segments[3]
	default:
		return Principal{}, fmt.Errorf("pki: unsupported SPIFFE path %q", id.Path)
	}
	return principal, nil
}

// ServiceID constructs a canonical service SPIFFE ID.
func ServiceID(trustDomain, role, instanceID string) (*url.URL, error) {
	if !isServiceRole(role) {
		return nil, fmt.Errorf("pki: unsupported service role %q", role)
	}
	return principalID(trustDomain, KindService, role, instanceID)
}

// NodeID constructs a canonical node SPIFFE ID.
func NodeID(trustDomain, role, accountID, nodeID string) (*url.URL, error) {
	if !isNodeRole(role) {
		return nil, fmt.Errorf("pki: unsupported node role %q", role)
	}
	return principalID(trustDomain, KindNode, role, accountID, nodeID)
}

func principalID(trustDomain string, segments ...string) (*url.URL, error) {
	if err := validateTrustDomain(trustDomain); err != nil {
		return nil, err
	}
	for _, segment := range segments {
		if err := validatePathSegment(segment); err != nil {
			return nil, err
		}
	}
	id := &url.URL{Scheme: "spiffe", Host: trustDomain, Path: "/" + strings.Join(segments, "/")}
	if _, err := ParsePrincipal(id, trustDomain); err != nil {
		return nil, err
	}
	return id, nil
}

func principalIDForTrustDomain(trustDomain string) (*url.URL, error) {
	if err := validateTrustDomain(trustDomain); err != nil {
		return nil, err
	}
	return &url.URL{Scheme: "spiffe", Host: trustDomain}, nil
}

func validateTrustDomain(trustDomain string) error {
	if trustDomain == "" || strings.ToLower(trustDomain) != trustDomain || strings.ContainsAny(trustDomain, "/:@%") {
		return fmt.Errorf("pki: invalid trust domain %q", trustDomain)
	}
	for _, r := range trustDomain {
		if !isAlphaNumeric(r) && r != '.' && r != '-' && r != '_' {
			return fmt.Errorf("pki: invalid trust domain %q", trustDomain)
		}
	}
	return nil
}

func validatePathSegment(segment string) error {
	if segment == "" || segment == "." || segment == ".." {
		return errors.New("pki: SPIFFE path segments must be non-empty and non-relative")
	}
	for _, r := range segment {
		if !isAlphaNumeric(r) && r != '.' && r != '_' && r != '-' {
			return fmt.Errorf("pki: invalid SPIFFE path segment %q", segment)
		}
	}
	return nil
}

func isAlphaNumeric(r rune) bool {
	return r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9'
}

func isServiceRole(role string) bool { return role == RoleCP || role == RoleAuthService }
func isNodeRole(role string) bool    { return role == RoleCloud || role == RoleSelfHosted }
