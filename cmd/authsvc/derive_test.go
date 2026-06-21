package main

import "testing"

func TestASDerive(t *testing.T) {
	const o = "https://app.example.com"

	// public_url set, GitHub NOT configured: non-github fields derived, github callbacks NOT.
	c := &AS{}
	c.PublicURL = o
	c.derive()
	if c.AllowedOrigins != o {
		t.Errorf("AllowedOrigins = %q, want %q", c.AllowedOrigins, o)
	}
	if c.SPAOrigins != o {
		t.Errorf("SPAOrigins = %q, want %q", c.SPAOrigins, o)
	}
	if c.RedirectURIs != o+"/callback,http://127.0.0.1/cb" {
		t.Errorf("RedirectURIs = %q, want SPA callback + CLI loopback", c.RedirectURIs)
	}
	if c.VerificationURI != o+"/device/verify" {
		t.Errorf("VerificationURI = %q, want %q", c.VerificationURI, o+"/device/verify")
	}
	if c.GitHub.RedirectURI != "" || c.GitHub.LinkRedirectURI != "" {
		t.Errorf("github callbacks should NOT derive without client_id: %q / %q", c.GitHub.RedirectURI, c.GitHub.LinkRedirectURI)
	}

	// public_url + GitHub configured: callbacks derive too.
	c2 := &AS{}
	c2.PublicURL = o
	c2.GitHub.ClientID = "cid"
	c2.derive()
	if c2.GitHub.RedirectURI != o+"/oauth/callback" {
		t.Errorf("github.redirect_uri = %q, want %q", c2.GitHub.RedirectURI, o+"/oauth/callback")
	}
	if c2.GitHub.LinkRedirectURI != o+"/github/link/callback" {
		t.Errorf("github.link_redirect_uri = %q, want %q", c2.GitHub.LinkRedirectURI, o+"/github/link/callback")
	}

	// explicit values win over derivation.
	c3 := &AS{SPAOrigins: "https://explicit.example.com", RedirectURIs: "https://x/cb"}
	c3.PublicURL = o
	c3.derive()
	if c3.SPAOrigins != "https://explicit.example.com" || c3.RedirectURIs != "https://x/cb" {
		t.Errorf("explicit values must be kept: %q / %q", c3.SPAOrigins, c3.RedirectURIs)
	}

	// no public_url: everything unchanged.
	c4 := &AS{}
	c4.derive()
	if c4.AllowedOrigins != "" || c4.RedirectURIs != "" || c4.VerificationURI != "" {
		t.Error("nothing should derive when public_url is empty")
	}
}
