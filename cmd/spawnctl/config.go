package main

import "spawnery/internal/config"

// SpawnctlCfg is the spawnctl configuration schema. spawnctl is primarily flag-driven; this
// schema covers the handful of root-level values that benefit from YAML-layer defaulting and
// per-environment overrides. Documented defaults live in config/spawnctl.yaml; per-environment
// deltas in config/spawnctl.<env>.yaml.
type SpawnctlCfg struct {
	config.Common `koanf:",squash"`

	Addr string `koanf:"addr"` // spawnlet address (standalone mode; --addr flag)
	CP   string `koanf:"cp"`   // control-plane address (--cp flag)
}

// Validate runs cross-field checks beyond the struct tags.
func (c SpawnctlCfg) Validate() error {
	return c.Common.Validate()
}

// spawnctlEnvAliases maps spawnctl environment variable names to dotted config keys, so current
// deployments keep working unchanged. This is the migration ledger for all env vars the binary
// reads.
//
// Env vars read by the binary but excluded from this table (kept as inline os.Getenv):
//
//	SPAWNERY_TOKEN / CP_DEV_TOKEN — two-variable priority fallback in buildTokenSource and
//	resolveMoveAccountID: SPAWNERY_TOKEN > CP_DEV_TOKEN > -token flag > auth.json > "dev-token".
//	This ordered dual-source precedence cannot be expressed as a single alias entry (Go map
//	iteration is non-deterministic; both can't map to "token" reliably). Left inline in
//	authstate.go and move.go.
//
//	SPAWNCTL_LOGIN_PORT — narrow SSH-tunnel OAuth callback port, single call site in login.go.
//	Threading the config struct to that one point adds more churn than value for a single knob.
//
//	DISPLAY / WAYLAND_DISPLAY — runtime display-server detection in gh.go; out of framework
//	scope per §1.1 (incidental OS/runtime env reads).
var spawnctlEnvAliases = map[string]string{
	// No env vars map cleanly to SpawnctlCfg fields; see above for all exclusions.
}
