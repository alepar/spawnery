package pki

import (
	"crypto/x509"
	"testing"
)

func TestIntermediateIssuerRoles(t *testing.T) {
	t.Parallel()

	root, err := NewRootCA("root")
	if err != nil {
		t.Fatal(err)
	}
	for _, role := range []IssuerRole{IssuerService, IssuerCloudNode, IssuerSelfHostedNode} {
		t.Run(string(role), func(t *testing.T) {
			intermediate, err := root.NewIntermediate(role, "prod.spawnery.internal")
			if err != nil {
				t.Fatalf("NewIntermediate: %v", err)
			}
			if err := intermediate.Cert.CheckSignatureFrom(root.Cert); err != nil {
				t.Fatalf("intermediate not signed by root: %v", err)
			}
			if !intermediate.Cert.IsCA || intermediate.Cert.MaxPathLen != 0 || !intermediate.Cert.MaxPathLenZero {
				t.Fatalf("intermediate constraints = IsCA %v MaxPathLen %d zero %v", intermediate.Cert.IsCA, intermediate.Cert.MaxPathLen, intermediate.Cert.MaxPathLenZero)
			}
			got, err := IssuerRoleFromCertificate(intermediate.Cert)
			if err != nil {
				t.Fatalf("IssuerRoleFromCertificate: %v", err)
			}
			if got != role {
				t.Fatalf("role = %q, want %q", got, role)
			}
			if len(intermediate.Cert.Policies) != 1 {
				t.Fatalf("issuer policies = %v, want exactly one", intermediate.Cert.Policies)
			}
		})
	}
}

func TestIssuerRoleFromCertificateRejectsInvalidPolicies(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		policies []x509.OID
	}{
		{name: "missing"},
		{name: "duplicate", policies: []x509.OID{ServiceIssuerPolicyOID, ServiceIssuerPolicyOID}},
		{name: "unknown", policies: []x509.OID{mustParseOID(t, "1.2.3")}},
		{name: "multiple roles", policies: []x509.OID{ServiceIssuerPolicyOID, CloudNodeIssuerPolicyOID}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := IssuerRoleFromCertificate(&x509.Certificate{Policies: tt.policies}); err == nil {
				t.Fatal("invalid issuer policy accepted")
			}
		})
	}
}

func mustParseOID(t *testing.T, value string) x509.OID {
	t.Helper()
	oid, err := x509.ParseOID(value)
	if err != nil {
		t.Fatal(err)
	}
	return oid
}

func TestNewIntermediateRejectsUnknownRole(t *testing.T) {
	root, err := NewRootCA("root")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := root.NewIntermediate(IssuerRole("unknown"), "prod.spawnery.internal"); err == nil {
		t.Fatal("unknown issuer role accepted")
	}
}
