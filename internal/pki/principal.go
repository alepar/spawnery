package pki

import (
	"errors"
	"fmt"
	"net/url"
	"strings"
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

func validateTrustDomain(trustDomain string) error {
	if trustDomain == "" || strings.ToLower(trustDomain) != trustDomain || strings.ContainsAny(trustDomain, "/:@%") || strings.HasPrefix(trustDomain, ".") || strings.HasSuffix(trustDomain, ".") {
		return fmt.Errorf("pki: invalid trust domain %q", trustDomain)
	}
	for _, r := range trustDomain {
		if !isAlphaNumeric(r) && r != '.' && r != '-' {
			return fmt.Errorf("pki: invalid trust domain %q", trustDomain)
		}
	}
	return nil
}

func validatePathSegment(segment string) error {
	if segment == "" {
		return errors.New("pki: SPIFFE path segments must be non-empty")
	}
	for _, r := range segment {
		if !isAlphaNumeric(r) && r != '.' && r != '_' && r != '~' && r != '-' {
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
