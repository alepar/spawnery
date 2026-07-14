package pki

import (
	"crypto/x509"
	"encoding/asn1"
	"errors"
	"fmt"
	"math/big"
	"net/url"
	"strings"
	"time"
)

var subjectAltNameOID = asn1.ObjectIdentifier{2, 5, 29, 17}

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

// CertificateRevocationChecker reports whether serial is revoked by issuer. Callers must provide a
// current fail-closed implementation; certificate verification never silently disables revocation.
type CertificateRevocationChecker func(issuer, serial *big.Int) bool

// VerifyOptions configures strict Spawnery X.509-SVID verification.
type VerifyOptions struct {
	Root        *x509.Certificate
	TrustDomain string
	CurrentTime time.Time
	KeyUsages   []x509.ExtKeyUsage
	IsRevoked   CertificateRevocationChecker
}

// VerifyPrincipal validates leaf to Root and returns its typed Spawnery principal.
func VerifyPrincipal(leaf *x509.Certificate, intermediates []*x509.Certificate, opts VerifyOptions) (Principal, error) {
	if leaf == nil {
		return Principal{}, errors.New("pki: nil leaf certificate")
	}
	if opts.Root == nil {
		return Principal{}, errors.New("pki: nil root certificate")
	}
	if opts.IsRevoked == nil {
		return Principal{}, errors.New("pki: revocation checker is required")
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
	if err := validateRawSubjectAltName(leaf, principal); err != nil {
		return Principal{}, err
	}
	for _, chain := range chains {
		if len(chain) != 3 || !chain[len(chain)-1].Equal(opts.Root) {
			continue
		}
		issuer := chain[1]
		if err := validateIntermediateSPIFFEID(issuer, opts.TrustDomain); err != nil {
			continue
		}
		issuerRole, err := IssuerRoleFromCertificate(issuer)
		if err != nil || !issuerPermitsPrincipal(issuerRole, principal) {
			continue
		}
		if opts.IsRevoked(issuer.SerialNumber, leaf.SerialNumber) {
			return Principal{}, errors.New("pki: certificate is revoked")
		}
		return principal, nil
	}
	return Principal{}, errors.New("pki: no verified chain has a permitted issuer role")
}

func validateIntermediateSPIFFEID(issuer *x509.Certificate, trustDomain string) error {
	if issuer == nil || len(issuer.URIs) != 1 {
		return errors.New("pki: signing intermediate must contain exactly one URI SAN")
	}
	id := issuer.URIs[0]
	if id == nil || id.Scheme != "spiffe" || id.Host != trustDomain || id.Path != "" || id.RawPath != "" || id.User != nil || id.RawQuery != "" || id.Fragment != "" || id.ForceQuery || id.Opaque != "" || id.String() != "spiffe://"+trustDomain {
		return errors.New("pki: signing intermediate URI SAN does not match the configured trust domain")
	}
	return nil
}

func validateRawSubjectAltName(leaf *x509.Certificate, principal Principal) error {
	var value []byte
	count := 0
	for _, extension := range leaf.Extensions {
		if extension.Id.Equal(subjectAltNameOID) {
			count++
			value = extension.Value
		}
	}
	if count != 1 {
		return fmt.Errorf("pki: subjectAltName extension count is %d, want 1", count)
	}
	var sequence asn1.RawValue
	rest, err := asn1.Unmarshal(value, &sequence)
	if err != nil || len(rest) != 0 || sequence.Class != 0 || sequence.Tag != asn1.TagSequence || !sequence.IsCompound {
		return errors.New("pki: malformed subjectAltName extension")
	}
	uriCount := 0
	dnsNames := make(map[string]struct{})
	ipAddresses := make(map[string]struct{})
	for names := sequence.Bytes; len(names) > 0; {
		var name asn1.RawValue
		names, err = asn1.Unmarshal(names, &name)
		if err != nil || name.Class != 2 || name.IsCompound {
			return errors.New("pki: malformed GeneralName")
		}
		switch name.Tag {
		case 6: // uniformResourceIdentifier
			uriCount++
			if uriCount > 1 || len(leaf.URIs) != 1 || string(name.Bytes) != leaf.URIs[0].String() {
				return errors.New("pki: raw URI SAN does not match the parsed SPIFFE ID")
			}
		case 2: // dNSName
			if principal.Kind != KindService || len(name.Bytes) == 0 || !isASCII(name.Bytes) {
				return errors.New("pki: DNS SAN is permitted only on service leaves")
			}
			dnsName := strings.ToLower(string(name.Bytes))
			if _, duplicate := dnsNames[dnsName]; duplicate {
				return fmt.Errorf("pki: duplicate DNS GeneralName %q", dnsName)
			}
			dnsNames[dnsName] = struct{}{}
		case 7: // iPAddress
			if principal.Kind != KindService || len(name.Bytes) != 4 && len(name.Bytes) != 16 {
				return errors.New("pki: IP SAN is permitted only on service leaves")
			}
			ipAddress := string(name.Bytes)
			if _, duplicate := ipAddresses[ipAddress]; duplicate {
				return errors.New("pki: duplicate IP GeneralName")
			}
			ipAddresses[ipAddress] = struct{}{}
		default:
			return fmt.Errorf("pki: unsupported GeneralName tag %d", name.Tag)
		}
	}
	if uriCount != 1 {
		return fmt.Errorf("pki: raw URI SAN count is %d, want 1", uriCount)
	}
	return nil
}

func isASCII(value []byte) bool {
	for _, b := range value {
		if b > 0x7f {
			return false
		}
	}
	return true
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
	if len(leaf.EmailAddresses) != 0 {
		return errors.New("pki: leaf must not contain email SANs")
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

// ValidateTrustDomain validates a canonical SPIFFE trust-domain authority.
func ValidateTrustDomain(trustDomain string) error { return validateTrustDomain(trustDomain) }

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
