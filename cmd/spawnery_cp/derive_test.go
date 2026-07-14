package main

import "testing"

func TestCPDerive(t *testing.T) {
	// public_url set + allowed_origins empty -> derived from public_url.
	c := &CP{AllowedOrigins: ""}
	c.PublicURL = "https://app.example.com"
	c.derive()
	if c.AllowedOrigins != "https://app.example.com" {
		t.Errorf("AllowedOrigins = %q, want derived from public_url", c.AllowedOrigins)
	}

	// explicit allowed_origins wins over derivation.
	c2 := &CP{AllowedOrigins: "https://explicit.example.com"}
	c2.PublicURL = "https://app.example.com"
	c2.derive()
	if c2.AllowedOrigins != "https://explicit.example.com" {
		t.Errorf("AllowedOrigins = %q, want explicit value kept", c2.AllowedOrigins)
	}

	// no public_url -> unchanged (stays empty -> dev-permissive downstream).
	c3 := &CP{AllowedOrigins: ""}
	c3.derive()
	if c3.AllowedOrigins != "" {
		t.Errorf("AllowedOrigins = %q, want empty when public_url unset", c3.AllowedOrigins)
	}
}

func TestCPDeriveSplitsInternalRevocationPathLists(t *testing.T) {
	c := &CP{}
	c.Internal.RevocationIssuers = []string{"/pki/service.pem,/pki/cloud.pem,/pki/self.pem"}
	c.Internal.RevocationCRLs = []string{"/pki/service.crl,/pki/cloud.crl,/pki/self.crl"}
	c.derive()
	if len(c.Internal.RevocationIssuers) != 3 || c.Internal.RevocationIssuers[1] != "/pki/cloud.pem" {
		t.Fatalf("issuers = %#v", c.Internal.RevocationIssuers)
	}
	if len(c.Internal.RevocationCRLs) != 3 || c.Internal.RevocationCRLs[2] != "/pki/self.crl" {
		t.Fatalf("CRLs = %#v", c.Internal.RevocationCRLs)
	}
}
