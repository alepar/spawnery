package pki

import "testing"

func TestPolicyOIDs(t *testing.T) {
	tests := []struct {
		name string
		got  string
		want string
	}{
		{name: "auth signing intermediate", got: AuthSigningIntermediatePolicyOID.String(), want: "2.25.220686076016167383886617156996082505779"},
		{name: "auth artifact signer", got: AuthArtifactSignerPolicyOID.String(), want: "2.25.252950571309652121262556377772788377018"},
		{name: "service issuer", got: ServiceIssuerPolicyOID.String(), want: "2.25.252512432928806341888652597142698706330"},
		{name: "cloud node issuer", got: CloudNodeIssuerPolicyOID.String(), want: "2.25.272377079450377973232136459441396509550"},
		{name: "self-hosted node issuer", got: SelfHostedNodeIssuerPolicyOID.String(), want: "2.25.13905568351287903487917266049020976148"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.got != tt.want {
				t.Fatalf("OID = %q, want %q", tt.got, tt.want)
			}
		})
	}
}
