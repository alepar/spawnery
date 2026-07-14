package pki

import "crypto/x509"

var (
	AuthSigningIntermediatePolicyOID = mustPolicyOID("2.25.220686076016167383886617156996082505779")
	AuthArtifactSignerPolicyOID      = mustPolicyOID("2.25.252950571309652121262556377772788377018")
	ServiceIssuerPolicyOID           = mustPolicyOID("2.25.252512432928806341888652597142698706330")
	CloudNodeIssuerPolicyOID         = mustPolicyOID("2.25.272377079450377973232136459441396509550")
	SelfHostedNodeIssuerPolicyOID    = mustPolicyOID("2.25.13905568351287903487917266049020976148")
)

func mustPolicyOID(value string) x509.OID {
	oid, err := x509.ParseOID(value)
	if err != nil {
		panic(err)
	}
	return oid
}

// HasPolicy reports whether cert contains want in its RFC 5280 certificate policies extension.
func HasPolicy(cert *x509.Certificate, want x509.OID) bool {
	if cert == nil {
		return false
	}
	for _, policy := range cert.Policies {
		if policy.Equal(want) {
			return true
		}
	}
	return false
}
