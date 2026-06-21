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
