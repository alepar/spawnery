package pki

import (
	"crypto/x509"
	"encoding/asn1"
	"errors"
	"fmt"
)

type IssuerRole string

const (
	IssuerService        IssuerRole = "service-issuer"
	IssuerCloudNode      IssuerRole = "cloud-node-issuer"
	IssuerSelfHostedNode IssuerRole = "self-hosted-node-issuer"
)

// issuerRoleOID identifies Spawnery's root-authorized intermediate role policy.
var issuerRoleOID = asn1.ObjectIdentifier{1, 3, 6, 1, 4, 1, 57264, 1, 1}

func normalizeIssuerRole(role IssuerRole) (IssuerRole, error) {
	switch role {
	case IssuerService, IssuerCloudNode, IssuerSelfHostedNode:
		return role, nil
	case IssuerRole(ClassCloud):
		return IssuerCloudNode, nil
	case IssuerRole(ClassSelfHosted):
		return IssuerSelfHostedNode, nil
	default:
		return "", fmt.Errorf("pki: unsupported issuer role %q", role)
	}
}

func marshalIssuerRole(role IssuerRole) ([]byte, error) {
	normalized, err := normalizeIssuerRole(role)
	if err != nil {
		return nil, err
	}
	return asn1.Marshal(string(normalized))
}

// IssuerRoleFromCertificate reads the single Spawnery issuer policy extension.
func IssuerRoleFromCertificate(cert *x509.Certificate) (IssuerRole, error) {
	if cert == nil {
		return "", errors.New("pki: nil issuer certificate")
	}
	var value []byte
	count := 0
	for _, extension := range cert.Extensions {
		if extension.Id.Equal(issuerRoleOID) {
			count++
			value = extension.Value
		}
	}
	if count != 1 {
		return "", fmt.Errorf("pki: issuer role extension count is %d, want 1", count)
	}
	var encoded string
	rest, err := asn1.Unmarshal(value, &encoded)
	if err != nil || len(rest) != 0 {
		return "", errors.New("pki: malformed issuer role extension")
	}
	return normalizeIssuerRole(IssuerRole(encoded))
}
