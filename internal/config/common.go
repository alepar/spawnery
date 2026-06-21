package config

import "fmt"

// Common holds configuration shared across binaries. Each binary embeds it with `koanf:",squash"`,
// and the cross-process common.yaml layer gives shared knobs a home.
type Common struct {
	// PublicURL is the canonical origin the app is served at (scheme://host[:port], no path).
	// When set, binaries DERIVE their CORS origins and OAuth/redirect callback URLs from it (any
	// field left empty is filled from PublicURL; an explicit field value always wins). Empty
	// PublicURL leaves every derived field at its own default (e.g. dev-permissive CORS).
	PublicURL string `koanf:"public_url"`
}

// Validate is Common's cross-field hook. Because Go method promotion lets an outer type's Validate
// shadow this one, an embedding type's Validate MUST call c.Common.Validate() explicitly.
func (c Common) Validate() error {
	if c.PublicURL != "" {
		if err := validateOrigin(c.PublicURL); err != nil {
			return fmt.Errorf("public_url %q: %w", c.PublicURL, err)
		}
	}
	return nil
}
