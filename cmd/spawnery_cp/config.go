package main

import (
	"fmt"
	"net"
	"net/url"
	"strings"
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
		Mode                      string `koanf:"mode" validate:"oneof=dev prod"`
		DevTokens                 string `koanf:"dev_tokens"`
		Environment               string `koanf:"environment"`
		RootCA                    string `koanf:"root_ca"`
		SignerRevocationStatement string `koanf:"signer_revocation_statement"`
		SignerRevocationState     string `koanf:"signer_revocation_state"`
		DevIntentEnabled          bool   `koanf:"dev_intent_enabled"`
		// GitHubLinkPreflightDisabled turns off the CreateSpawn owner-github-link preflight even when
		// an AS URL is configured. For the static-token git-host dev lane (local Gitea): mounts there
		// use a single node-static token, not a per-owner AS mint, so an owner "GitHub link" is
		// meaningless and the preflight would wrongly reject. DEV/TEST only; production leaves it off.
		GitHubLinkPreflightDisabled bool          `koanf:"github_link_preflight_disabled"`
		SessionReauthInterval       time.Duration `koanf:"session_reauth_interval"`
		ASURL                       string        `koanf:"as_url"`
		ASRevocationURL             string        `koanf:"as_revocation_url"`
		RevocationPollInterval      time.Duration `koanf:"revocation_poll_interval"`
	} `koanf:"auth"`

	Internal struct {
		InsecureDevNodeOnPublic   bool          `koanf:"insecure_dev_node_on_public"`
		Listen                    string        `koanf:"listen"`
		TrustDomain               string        `koanf:"trust_domain"`
		RootCA                    string        `koanf:"root_ca"`
		Cert                      string        `koanf:"cert"`
		Chain                     string        `koanf:"chain"`
		Key                       string        `koanf:"key"`
		ServerName                string        `koanf:"server_name"`
		RevocationState           string        `koanf:"revocation_state"`
		RevocationIssuers         []string      `koanf:"revocation_issuers"`
		RevocationCRLs            []string      `koanf:"revocation_crls"`
		RevocationRefreshInterval time.Duration `koanf:"revocation_refresh_interval"`
	} `koanf:"internal"`

	Telemetry         string        `koanf:"telemetry"`
	MaxSpawnsPerOwner int           `koanf:"max_spawns_per_owner" validate:"min=0"`
	ShutdownGrace     time.Duration `koanf:"shutdown_grace"`

	Evaluator struct {
		QuotaSuspendMB int64         `koanf:"quota_suspend_mb"`
		IdleEnabled    bool          `koanf:"idle_enabled"`
		IdleDetached   time.Duration `koanf:"idle_detached"`
		IdleAttached   time.Duration `koanf:"idle_attached"`
	} `koanf:"evaluator"`

	// Skills configures the Garage-backed skill object store for IngestSkillFromURL (sp-nrzf.3.14.4).
	// When endpoint is empty, URL skill ingest returns FailedPrecondition (no Garage configured).
	// The S3 key needs PutObject/GetObject/presign on the existing bucket — NOT CreateBucket
	// (the dev journal key is Forbidden for MakeBucket; provision the bucket out-of-band).
	Skills struct {
		// Endpoint is the S3 host:port for CP-internal access (PutObject/StatObject).
		// Reuses JOURNAL_S3_ENDPOINT when empty in practice, but is a separate config knob
		// to allow an explicit override.
		Endpoint config.Secret `koanf:"endpoint"`
		// NodeEndpoint is the S3 host:port for presigned GET URLs served to nodes.
		// Defaults to endpoint when empty (dev; S2 cross-netns is task .7).
		NodeEndpoint string `koanf:"node_endpoint"`
		// AccessKeyID and SecretAccessKey are the S3 credentials.
		AccessKeyID     config.Secret `koanf:"access_key_id"`
		SecretAccessKey config.Secret `koanf:"secret_access_key"`
		// Region is the S3 region label (Garage default "garage").
		Region string `koanf:"region"`
		// DisableTLS uses plain HTTP (dev Garage). Never set in production.
		DisableTLS bool `koanf:"disable_tls"`
		// Bucket is the pre-provisioned skills bucket name (default "spawnery-skills").
		Bucket string `koanf:"bucket"`
		// GitHubToken is an optional Bearer token for authenticated GitHub API access.
		GitHubToken config.Secret `koanf:"github_token"`
		// ZstdLevel is the zstd compression level (1–19; 0 = default ~3).
		ZstdLevel int `koanf:"zstd_level"`
	} `koanf:"skills"`
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
	if c.Internal.InsecureDevNodeOnPublic {
		if c.Auth.Mode != "dev" {
			return fmt.Errorf("internal.insecure_dev_node_on_public requires auth.mode=dev")
		}
		if c.Internal.Listen != "" {
			return fmt.Errorf("internal.insecure_dev_node_on_public cannot be combined with internal mTLS")
		}
		host, _, err := net.SplitHostPort(c.Listen)
		if err != nil || !loopbackHost(host) {
			return fmt.Errorf("internal.insecure_dev_node_on_public requires a loopback-only public listen address")
		}
	}
	for _, endpoint := range []struct{ name, value string }{
		{"auth.as_url", c.Auth.ASURL},
		{"auth.as_revocation_url", c.Auth.ASRevocationURL},
	} {
		if err := validateInternalHTTPSURL(endpoint.name, endpoint.value); err != nil {
			return err
		}
	}
	if c.Auth.Mode == "prod" {
		for _, required := range []struct{ name, value string }{
			{"auth.environment", c.Auth.Environment},
			{"auth.root_ca", c.Auth.RootCA},
			{"auth.signer_revocation_state", c.Auth.SignerRevocationState},
		} {
			if required.value == "" {
				return fmt.Errorf("auth.mode=prod requires %s", required.name)
			}
		}
		for _, required := range []struct{ name, value string }{
			{"internal.listen", c.Internal.Listen},
			{"internal.trust_domain", c.Internal.TrustDomain},
			{"internal.root_ca", c.Internal.RootCA},
			{"internal.cert", c.Internal.Cert},
			{"internal.chain", c.Internal.Chain},
			{"internal.key", c.Internal.Key},
			{"internal.revocation_state", c.Internal.RevocationState},
		} {
			if required.value == "" {
				return fmt.Errorf("auth.mode=prod requires %s", required.name)
			}
		}
		if len(c.Internal.RevocationIssuers) == 0 {
			return fmt.Errorf("auth.mode=prod requires internal.revocation_issuers")
		}
		if len(c.Internal.RevocationCRLs) == 0 {
			return fmt.Errorf("auth.mode=prod requires internal.revocation_crls")
		}
	}
	if (c.Auth.ASURL != "" || c.Auth.ASRevocationURL != "") && c.Internal.ServerName == "" {
		return fmt.Errorf("AS internal URLs require internal.server_name")
	}
	internalConfigured := c.Auth.Mode == "prod" || c.Auth.ASURL != "" || c.Auth.ASRevocationURL != "" ||
		c.Internal.Listen != "" || c.Internal.TrustDomain != "" || c.Internal.RootCA != "" ||
		c.Internal.Cert != "" || c.Internal.Chain != "" || c.Internal.Key != "" ||
		c.Internal.RevocationState != "" || len(c.Internal.RevocationIssuers) != 0 || len(c.Internal.RevocationCRLs) != 0
	if internalConfigured && c.Auth.Mode != "prod" {
		for _, required := range []struct{ name, value string }{
			{"internal.listen", c.Internal.Listen},
			{"internal.trust_domain", c.Internal.TrustDomain},
			{"internal.root_ca", c.Internal.RootCA},
			{"internal.cert", c.Internal.Cert},
			{"internal.chain", c.Internal.Chain},
			{"internal.key", c.Internal.Key},
			{"internal.revocation_state", c.Internal.RevocationState},
		} {
			if required.value == "" {
				return fmt.Errorf("internal mTLS configuration requires %s", required.name)
			}
		}
		if len(c.Internal.RevocationIssuers) == 0 || len(c.Internal.RevocationCRLs) == 0 {
			return fmt.Errorf("internal mTLS configuration requires internal.revocation_issuers and internal.revocation_crls")
		}
	}
	if c.Store.Driver == "postgres" && (c.Store.DSN == "" || string(c.Store.DSN) == sqliteDefaultDSN) {
		return fmt.Errorf("store.driver=postgres requires store.dsn (a postgres DSN)")
	}
	return nil
}

func validateInternalHTTPSURL(name, raw string) error {
	if raw == "" {
		return nil
	}
	parsed, err := url.Parse(raw)
	if err != nil || !strings.EqualFold(parsed.Scheme, "https") || parsed.Host == "" || parsed.User != nil {
		return fmt.Errorf("%s must use https with an absolute host", name)
	}
	return nil
}

func loopbackHost(host string) bool {
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// cpEnvAliases maps existing CP environment variable names to dotted config keys, so current
// deployments keep working unchanged (the env layer sits above the files). New knobs are reached
// via these names or CLI --set.
var cpEnvAliases = map[string]string{
	"CP_PUBLIC_URL":                       "public_url",
	"CP_LISTEN":                           "listen",
	"CP_ALLOWED_ORIGINS":                  "allowed_origins",
	"CP_STORE_DRIVER":                     "store.driver",
	"CP_STORE_DSN":                        "store.dsn",
	"CP_AUTH_MODE":                        "auth.mode",
	"CP_DEV_TOKENS":                       "auth.dev_tokens",
	"CP_AUTH_ENVIRONMENT":                 "auth.environment",
	"CP_AUTH_ROOT_CA":                     "auth.root_ca",
	"CP_AUTH_SIGNER_REVOCATION_STATEMENT": "auth.signer_revocation_statement",
	"CP_AUTH_SIGNER_REVOCATION_STATE":     "auth.signer_revocation_state",
	"CP_DEV_INTENT_ENABLED":               "auth.dev_intent_enabled",
	"CP_GITHUB_LINK_PREFLIGHT_DISABLED":   "auth.github_link_preflight_disabled",
	"CP_SESSION_REAUTH_INTERVAL":          "auth.session_reauth_interval",
	"CP_AS_URL":                           "auth.as_url",
	"CP_AS_REVOCATION_URL":                "auth.as_revocation_url",
	"CP_SHUTDOWN_GRACE":                   "shutdown_grace",
	"CP_REVOCATION_POLL_INTERVAL":         "auth.revocation_poll_interval",
	"CP_INSECURE_DEV_NODE_ON_PUBLIC":      "internal.insecure_dev_node_on_public",
	"CP_INTERNAL_LISTEN":                  "internal.listen",
	"CP_INTERNAL_TRUST_DOMAIN":            "internal.trust_domain",
	"CP_INTERNAL_ROOT_CA":                 "internal.root_ca",
	"CP_INTERNAL_TLS_CERT":                "internal.cert",
	"CP_INTERNAL_TLS_CHAIN":               "internal.chain",
	"CP_INTERNAL_TLS_KEY":                 "internal.key",
	"CP_INTERNAL_SERVER_NAME":             "internal.server_name",
	"CP_INTERNAL_REVOCATION_STATE":        "internal.revocation_state",
	"CP_INTERNAL_REVOCATION_ISSUERS":      "internal.revocation_issuers",
	"CP_INTERNAL_REVOCATION_CRLS":         "internal.revocation_crls",
	"CP_INTERNAL_REVOCATION_REFRESH":      "internal.revocation_refresh_interval",
	"CP_TELEMETRY":                        "telemetry",
	"CP_MAX_SPAWNS_PER_OWNER":             "max_spawns_per_owner",
	"EVALUATOR_QUOTA_SUSPEND_MB":          "evaluator.quota_suspend_mb",
	"EVALUATOR_IDLE_ENABLED":              "evaluator.idle_enabled",
	"EVALUATOR_IDLE_DETACHED":             "evaluator.idle_detached",
	"EVALUATOR_IDLE_ATTACHED":             "evaluator.idle_attached",
	// Skills / Garage ingest (sp-nrzf.3.14.4)
	"SKILLS_S3_ENDPOINT":      "skills.endpoint",
	"SKILLS_S3_NODE_ENDPOINT": "skills.node_endpoint",
	"SKILLS_S3_ACCESS_KEY":    "skills.access_key_id",
	"SKILLS_S3_SECRET_KEY":    "skills.secret_access_key",
	"SKILLS_S3_REGION":        "skills.region",
	"SKILLS_S3_DISABLE_TLS":   "skills.disable_tls",
	"SKILLS_BUCKET":           "skills.bucket",
	"SKILLS_GITHUB_TOKEN":     "skills.github_token",
	"SKILLS_ZSTD_LEVEL":       "skills.zstd_level",
}
