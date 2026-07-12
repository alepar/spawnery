package pki

import (
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/asn1"
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
			count := 0
			for _, extension := range intermediate.Cert.Extensions {
				if extension.Id.Equal(issuerRoleOID) {
					count++
				}
			}
			if count != 1 {
				t.Fatalf("issuer role extension count = %d, want 1", count)
			}
		})
	}
}

func TestIssuerRoleFromCertificateRejectsInvalidPolicies(t *testing.T) {
	t.Parallel()

	valid, err := asn1.Marshal(string(IssuerService))
	if err != nil {
		t.Fatal(err)
	}
	unknown, err := asn1.Marshal("unknown-issuer")
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name       string
		extensions []pkix.Extension
	}{
		{name: "missing"},
		{name: "duplicate", extensions: []pkix.Extension{{Id: issuerRoleOID, Value: valid}, {Id: issuerRoleOID, Value: valid}}},
		{name: "malformed", extensions: []pkix.Extension{{Id: issuerRoleOID, Value: []byte("not DER")}}},
		{name: "unknown", extensions: []pkix.Extension{{Id: issuerRoleOID, Value: unknown}}},
		{name: "legacy cloud", extensions: []pkix.Extension{{Id: issuerRoleOID, Value: mustMarshalASN1String(t, ClassCloud)}}},
		{name: "legacy self-hosted", extensions: []pkix.Extension{{Id: issuerRoleOID, Value: mustMarshalASN1String(t, ClassSelfHosted)}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := IssuerRoleFromCertificate(&x509.Certificate{Extensions: tt.extensions}); err == nil {
				t.Fatal("invalid issuer policy accepted")
			}
		})
	}
}

func mustMarshalASN1String(t *testing.T, value string) []byte {
	t.Helper()
	encoded, err := asn1.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return encoded
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
