package main

import (
	"fmt"
	"time"

	"spawnery/internal/config"
)

// CP is the control-plane configuration schema. Documented defaults live in config/cp.yaml;
// per-environment deltas in config/cp.<env>.yaml. Every field is overridable via the cpEnvAliases
// env vars and CLI --set.
type CP struct {
	config.Common `koanf:",squash"`

	Listen         string `koanf:"listen" validate:"required,hostname_port"`
	AllowedOrigins string `koanf:"allowed_origins"`

	Store struct {
		Driver string        `koanf:"driver" validate:"oneof=sqlite postgres"`
		DSN    config.Secret `koanf:"dsn"`
	} `koanf:"store"`

	Auth struct {
		Mode                   string        `koanf:"mode" validate:"oneof=dev prod"`
		DevTokens              string        `koanf:"dev_tokens"`
		ASSessionPubkeys       string        `koanf:"as_session_pubkeys"`
		DevASKey               string        `koanf:"dev_as_key"`
		DevIntentEnabled       bool          `koanf:"dev_intent_enabled"`
		SessionReauthInterval  time.Duration `koanf:"session_reauth_interval"`
		ASRPCSecret            config.Secret `koanf:"as_rpc_secret"`
		ASURL                  string        `koanf:"as_url"`
		ASRevocationURL        string        `koanf:"as_revocation_url"`
		ASCPSecret             config.Secret `koanf:"as_cp_secret"`
		RevocationPollInterval time.Duration `koanf:"revocation_poll_interval"`
	} `koanf:"auth"`

	Telemetry         string        `koanf:"telemetry"`
	MaxSpawnsPerOwner int           `koanf:"max_spawns_per_owner" validate:"min=0"`
	ShutdownGrace     time.Duration `koanf:"shutdown_grace"`

	Evaluator struct {
		QuotaSuspendMB int64         `koanf:"quota_suspend_mb"`
		IdleEnabled    bool          `koanf:"idle_enabled"`
		IdleDetached   time.Duration `koanf:"idle_detached"`
		IdleAttached   time.Duration `koanf:"idle_attached"`
	} `koanf:"evaluator"`

	Node struct {
		AuthMode string `koanf:"auth_mode" validate:"oneof=insecure enforced"`
		Listen   string `koanf:"listen" validate:"required,hostname_port"`
		RootCA   string `koanf:"root_ca"`
		TLSCert  string `koanf:"tls_cert"`
		TLSKey   string `koanf:"tls_key"`
	} `koanf:"node"`
}

// DevMode reports whether the CP runs in dev (permissive) auth mode.
func (c CP) DevMode() bool { return c.Auth.Mode != "prod" }

// derive fills origin/callback fields from Common.PublicURL when they are left empty. An explicit
// value always wins; an empty PublicURL leaves the field at its own default (dev-permissive CORS).
func (c *CP) derive() {
	if c.PublicURL != "" && c.AllowedOrigins == "" {
		c.AllowedOrigins = c.PublicURL
	}
}

// Validate runs cross-field checks beyond the struct tags.
func (c CP) Validate() error {
	if err := c.Common.Validate(); err != nil { // explicit: method promotion would shadow it
		return err
	}
	if c.Auth.Mode == "prod" && c.Auth.ASSessionPubkeys == "" {
		return fmt.Errorf("auth.mode=prod requires auth.as_session_pubkeys (no keys configured)")
	}
	if c.Store.Driver == "postgres" && (c.Store.DSN == "" || string(c.Store.DSN) == sqliteDefaultDSN) {
		return fmt.Errorf("store.driver=postgres requires store.dsn (a postgres DSN)")
	}
	return nil
}

// cpEnvAliases maps existing CP environment variable names to dotted config keys, so current
// deployments keep working unchanged (the env layer sits above the files). New knobs are reached
// via these names or CLI --set.
var cpEnvAliases = map[string]string{
	"CP_PUBLIC_URL":               "public_url",
	"CP_LISTEN":                   "listen",
	"CP_ALLOWED_ORIGINS":          "allowed_origins",
	"CP_STORE_DRIVER":             "store.driver",
	"CP_STORE_DSN":                "store.dsn",
	"CP_AUTH_MODE":                "auth.mode",
	"CP_DEV_TOKENS":               "auth.dev_tokens",
	"CP_AS_SESSION_PUBKEYS":       "auth.as_session_pubkeys",
	"CP_DEV_AS_KEY":               "auth.dev_as_key",
	"CP_DEV_INTENT_ENABLED":       "auth.dev_intent_enabled",
	"CP_SESSION_REAUTH_INTERVAL":  "auth.session_reauth_interval",
	"CP_AS_RPC_SECRET":            "auth.as_rpc_secret",
	"CP_AS_URL":                   "auth.as_url",
	"CP_AS_REVOCATION_URL":        "auth.as_revocation_url",
	"CP_SHUTDOWN_GRACE":           "shutdown_grace",
	"CP_AS_CP_SECRET":             "auth.as_cp_secret",
	"CP_REVOCATION_POLL_INTERVAL": "auth.revocation_poll_interval",
	"CP_TELEMETRY":                "telemetry",
	"CP_MAX_SPAWNS_PER_OWNER":     "max_spawns_per_owner",
	"EVALUATOR_QUOTA_SUSPEND_MB":  "evaluator.quota_suspend_mb",
	"EVALUATOR_IDLE_ENABLED":      "evaluator.idle_enabled",
	"EVALUATOR_IDLE_DETACHED":     "evaluator.idle_detached",
	"EVALUATOR_IDLE_ATTACHED":     "evaluator.idle_attached",
	"NODE_AUTH_MODE":              "node.auth_mode",
	"CP_NODE_LISTEN":              "node.listen",
	"CP_NODE_ROOT_CA":             "node.root_ca",
	"CP_NODE_TLS_CERT":            "node.tls_cert",
	"CP_NODE_TLS_KEY":             "node.tls_key",
}
