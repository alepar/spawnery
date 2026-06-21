package config

// Common holds configuration shared across binaries. Fields are promoted here as they become
// genuinely shared during rollout; it is intentionally minimal today. Each binary embeds it with
// `koanf:",squash"`, and the cross-process common.yaml layer gives shared knobs a home.
type Common struct{}

// Validate is Common's cross-field hook. Because Go method promotion lets an outer type's Validate
// shadow this one, an embedding type's Validate MUST call c.Common.Validate() explicitly.
func (Common) Validate() error { return nil }
