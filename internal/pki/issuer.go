package pki

import (
	"crypto/x509"
	"errors"
	"fmt"
)

type IssuerRole string

const (
	IssuerService        IssuerRole = "service-issuer"
	IssuerCloudNode      IssuerRole = "cloud-node-issuer"
	IssuerSelfHostedNode IssuerRole = "self-hosted-node-issuer"
)

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

func issuerRolePolicy(role IssuerRole) (x509.OID, error) {
	normalized, err := normalizeIssuerRole(role)
	if err != nil {
		return x509.OID{}, err
	}
	switch normalized {
	case IssuerService:
		return ServiceIssuerPolicyOID, nil
	case IssuerCloudNode:
		return CloudNodeIssuerPolicyOID, nil
	case IssuerSelfHostedNode:
		return SelfHostedNodeIssuerPolicyOID, nil
	default:
		return x509.OID{}, fmt.Errorf("pki: unsupported issuer role %q", normalized)
	}
}

// IssuerRoleFromCertificate reads the certificate's single Spawnery issuer policy.
func IssuerRoleFromCertificate(cert *x509.Certificate) (IssuerRole, error) {
	if cert == nil {
		return "", errors.New("pki: nil issuer certificate")
	}
	if len(cert.Policies) != 1 {
		return "", fmt.Errorf("pki: issuer policy count is %d, want 1", len(cert.Policies))
	}
	policy := cert.Policies[0]
	switch {
	case policy.Equal(ServiceIssuerPolicyOID):
		return IssuerService, nil
	case policy.Equal(CloudNodeIssuerPolicyOID):
		return IssuerCloudNode, nil
	case policy.Equal(SelfHostedNodeIssuerPolicyOID):
		return IssuerSelfHostedNode, nil
	default:
		return "", fmt.Errorf("pki: unsupported issuer policy %q", policy.String())
	}
}
