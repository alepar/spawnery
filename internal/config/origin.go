package config

import (
	"fmt"
	"net/url"
	"strings"
)

// validateOrigin checks that s is a clean web origin: an http/https scheme, a non-wildcard host
// (with optional port), and NOTHING else — no path (not even a trailing slash), query, fragment,
// or userinfo. A served-at value must be a bare origin so the derived CORS origins and redirect
// URIs are exact and unambiguous.
func validateOrigin(s string) error {
	if s == "" {
		return fmt.Errorf("empty")
	}
	u, err := url.Parse(s)
	if err != nil {
		return fmt.Errorf("not a valid URL: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("scheme must be http or https")
	}
	if u.Host == "" {
		return fmt.Errorf("missing host")
	}
	if strings.Contains(u.Host, "*") {
		return fmt.Errorf("host must not contain a wildcard")
	}
	if u.Path != "" {
		return fmt.Errorf("must not include a path or trailing slash")
	}
	if u.RawQuery != "" {
		return fmt.Errorf("must not include a query")
	}
	if u.Fragment != "" {
		return fmt.Errorf("must not include a fragment")
	}
	if u.User != nil {
		return fmt.Errorf("must not include userinfo")
	}
	return nil
}
