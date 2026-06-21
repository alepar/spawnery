package config

import "testing"

func TestValidateOrigin(t *testing.T) {
	good := []string{
		"https://app.example.com",
		"http://localhost:5173",
		"https://blacky.dayton:5173",
		"https://host:8443",
	}
	for _, s := range good {
		if err := validateOrigin(s); err != nil {
			t.Errorf("validateOrigin(%q) = %v, want nil", s, err)
		}
	}
	bad := map[string]string{
		"app.example.com":       "no scheme",
		"https://host/path":     "has path",
		"https://host/":         "trailing slash",
		"ftp://host":            "bad scheme",
		"https://":              "no host",
		"https://*.example.com": "wildcard host",
		"https://host?x=1":      "query",
		"https://host#f":        "fragment",
		"https://user:pw@host":  "userinfo",
		"":                      "empty",
	}
	for s, why := range bad {
		if err := validateOrigin(s); err == nil {
			t.Errorf("validateOrigin(%q) = nil, want error (%s)", s, why)
		}
	}
}

func TestCommonValidate_PublicURL(t *testing.T) {
	if err := (Common{}).Validate(); err != nil {
		t.Errorf("empty PublicURL should validate: %v", err)
	}
	if err := (Common{PublicURL: "https://app.example.com"}).Validate(); err != nil {
		t.Errorf("valid PublicURL: %v", err)
	}
	if err := (Common{PublicURL: "https://app.example.com/path"}).Validate(); err == nil {
		t.Error("PublicURL with a path should be rejected")
	}
}
