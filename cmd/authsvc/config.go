package main

import (
	"fmt"
	"time"

	"spawnery/internal/config"
)

const (
	defaultNodeCRLRenewInterval = time.Hour
	defaultNodeCRLRenewBefore   = 6 * time.Hour
	selfHostedNodeCRLValidity   = 24 * time.Hour
)

// AS is the auth-service configuration schema. Documented defaults live in config/authsvc.yaml;
// per-environment deltas in config/authsvc.<env>.yaml. Every field is overridable via the
// asEnvAliases env vars and CLI --set.
type AS struct {
	config.Common `koanf:",squash"`

	Dev            bool   `koanf:"dev"`
	FakeGithub     bool   `koanf:"fake_github"`
	Listen         string `koanf:"listen" validate:"required,hostname_port"`
	AllowedOrigins string `koanf:"allowed_origins"`

	Internal ASInternalTLS `koanf:"internal"`

	// Reachable, multi-user fake GitHub (T2, sp-tq0t.13): opt-in fields for a black-box acceptance
	// suite that needs to reach the fake's browser-facing authorize redirect from another host and
	// obtain N distinct OAuth owners. All empty (the default) = today's behavior exactly: loopback
	// bind, single default user.
	FakeGitHubAddr    string `koanf:"fake_github_addr"`     // bind addr for reachable mode, e.g. "0.0.0.0:9099"
	FakeGitHubBaseURL string `koanf:"fake_github_base_url"` // advertised base URL; required when Addr is set
	FakeGitHubUsers   string `koanf:"fake_github_users"`    // "alice:2000001,bob" or "alice,bob" (id derived when omitted)
	// FakeGitHubToken, when set, makes the fake issue this EXACT string as every minted access
	// token instead of a random one (githubfake.Options.FixedToken). DEV/TEST-ONLY: the e2e-vm
	// lane (sp-wwtc.4) sets this to a Gitea PAT it minted at provision time, so the sidecar's
	// GitHub MITM proxy injection (Authorization: Basic base64("x-access-token:"+token)) — the real,
	// non-optional production injection path — lands a credential Gitea actually accepts. Empty
	// (the default) preserves the historical random-token behavior everywhere else.
	FakeGitHubToken string `koanf:"fake_github_token"`

	CA struct {
		TrustDomain                  string        `koanf:"trust_domain" validate:"required"`
		RootPEM                      string        `koanf:"root_pem"`
		IntermediateCert             string        `koanf:"intermediate_cert"`
		IntermediateKey              string        `koanf:"intermediate_key"`
		RevocationCRL                string        `koanf:"revocation_crl"`
		RevocationRenewInterval      time.Duration `koanf:"revocation_renew_interval"`
		RevocationRenewBefore        time.Duration `koanf:"revocation_renew_before"`
		LegacyRevocationCertificates string        `koanf:"legacy_revocation_certificates"`
	} `koanf:"ca"`

	Signing ASAuthSigning `koanf:"signing"`

	DB struct {
		Driver string `koanf:"driver"`
		DSN    string `koanf:"dsn"`
	} `koanf:"db"`

	GitHub struct {
		TokenEncKey        config.Secret `koanf:"token_enc_key"`
		TokenEncKeyFile    string        `koanf:"token_enc_key_file"`
		ClientID           string        `koanf:"client_id"`
		ClientSecret       config.Secret `koanf:"client_secret"`
		WebURL             string        `koanf:"web_url"`
		APIURL             string        `koanf:"api_url"`
		RedirectURI        string        `koanf:"redirect_uri"`
		LinkRedirectURI    string        `koanf:"link_redirect_uri"`
		PostRedeemRedirect string        `koanf:"post_redeem_redirect"`
		DefaultHost        string        `koanf:"default_host"`
	} `koanf:"github"`

	SPAOrigins      string `koanf:"spa_origins"`
	RedirectURIs    string `koanf:"redirect_uris"`
	VerificationURI string `koanf:"verification_uri"`

	RegistrationEnabled bool `koanf:"registration_enabled"`
	MaxFamilies         int  `koanf:"max_families" validate:"min=1"`
	RateLimits          struct {
		DevicePerMin int `koanf:"device_per_min" validate:"min=1"`
	} `koanf:"rate_limits"`

	CP struct {
		URL        string `koanf:"url"`
		ServerName string `koanf:"server_name"`
	} `koanf:"cp"`
}

type ASInternalTLS struct {
	Listen                    string        `koanf:"listen"`
	TrustDomain               string        `koanf:"trust_domain"`
	RootCA                    string        `koanf:"root_ca"`
	Cert                      string        `koanf:"cert"`
	Chain                     string        `koanf:"chain"`
	Key                       string        `koanf:"key"`
	ServerName                string        `koanf:"server_name"`
	RevocationState           string        `koanf:"revocation_state"`
	RevocationIssuers         string        `koanf:"revocation_issuers"`
	RevocationCRLs            string        `koanf:"revocation_crls"`
	RevocationURLs            string        `koanf:"revocation_urls"`
	RevocationRefreshInterval time.Duration `koanf:"revocation_refresh_interval"`
}

// ASAuthSigning names the purpose-constrained online leaf credentials used for authorization
// artifacts. The auth-signing intermediate private key is deliberately absent: it stays offline.
type ASAuthSigning struct {
	Environment     string `koanf:"environment"`
	RootPEM         string `koanf:"root_pem"`
	CurrentKeyPEM   string `koanf:"current_key_pem"`
	CurrentChainPEM string `koanf:"current_chain_pem"`
	NextKeyPEM      string `koanf:"next_key_pem"`
	NextChainPEM    string `koanf:"next_chain_pem"`
}

// derive fills origin/callback/redirect fields from Common.PublicURL when they are left empty. An
// explicit value always wins; the GitHub callback URLs derive only when GitHub is configured
// (client_id set), so a deployment without GitHub never silently activates the link flow.
func (c *AS) derive() {
	o := c.PublicURL
	if o == "" {
		return
	}
	if c.AllowedOrigins == "" {
		c.AllowedOrigins = o
	}
	if c.SPAOrigins == "" {
		c.SPAOrigins = o
	}
	if c.RedirectURIs == "" {
		// The SPA post-login callback (derived) plus the spawnctl loopback login redirect, which
		// the AS port-relaxes (RFC 8252 §7.3) — without it, CLI login would have no registered
		// loopback and break. Path /cb matches cmd/spawnctl/login.go.
		c.RedirectURIs = o + "/callback,http://127.0.0.1/cb"
	}
	if c.VerificationURI == "" {
		c.VerificationURI = o + "/device/verify"
	}
	if (c.GitHub.ClientID != "" || c.FakeGithub) && c.GitHub.RedirectURI == "" {
		c.GitHub.RedirectURI = o + "/oauth/callback"
	}
	// github.link_redirect_uri is intentionally NOT derived: a non-empty value ACTIVATES the
	// /github/link/* bootstrap flow, so it must stay an explicit operator opt-in (set
	// AS_GITHUB_LINK_REDIRECT_URI). Deriving it would silently enable that surface on any prod AS
	// that configured GitHub but never asked for the link flow.
}

// Validate runs cross-field checks beyond the struct tags.
func (c AS) Validate() error {
	if err := c.Common.Validate(); err != nil {
		return err
	}
	if (c.Signing.NextKeyPEM == "") != (c.Signing.NextChainPEM == "") {
		return fmt.Errorf("signing.next_key_pem and signing.next_chain_pem must be configured together")
	}
	if !c.Dev {
		required := []struct {
			name  string
			value string
		}{
			{"signing.environment", c.Signing.Environment},
			{"signing.root_pem", c.Signing.RootPEM},
			{"signing.current_key_pem", c.Signing.CurrentKeyPEM},
			{"signing.current_chain_pem", c.Signing.CurrentChainPEM},
		}
		for _, field := range required {
			if field.value == "" {
				return fmt.Errorf("%s is required in production (set dev=true for development)", field.name)
			}
		}
		if string(c.GitHub.TokenEncKey) == "" && c.GitHub.TokenEncKeyFile == "" {
			return fmt.Errorf("github.token_enc_key (or github.token_enc_key_file) is required for at-rest github token encryption")
		}
		internalRequired := []struct {
			name  string
			value string
		}{
			{"internal.listen", c.Internal.Listen},
			{"ca.revocation_crl", c.CA.RevocationCRL},
			{"internal.trust_domain", c.Internal.TrustDomain},
			{"internal.root_ca", c.Internal.RootCA},
			{"internal.cert", c.Internal.Cert},
			{"internal.chain", c.Internal.Chain},
			{"internal.key", c.Internal.Key},
			{"internal.server_name", c.Internal.ServerName},
			{"internal.revocation_state", c.Internal.RevocationState},
			{"internal.revocation_issuers", c.Internal.RevocationIssuers},
			{"internal.revocation_crls or internal.revocation_urls", c.Internal.RevocationCRLs + c.Internal.RevocationURLs},
			{"cp.url", c.CP.URL},
			{"cp.server_name", c.CP.ServerName},
		}
		for _, field := range internalRequired {
			if field.value == "" {
				return fmt.Errorf("%s is required in production (set dev=true for development)", field.name)
			}
		}
	}
	if c.Internal.TrustDomain != "" && c.CA.TrustDomain != "" && c.Internal.TrustDomain != c.CA.TrustDomain {
		return fmt.Errorf("internal.trust_domain must match ca.trust_domain")
	}
	if (c.CP.URL == "") != (c.CP.ServerName == "") {
		return fmt.Errorf("cp.url and cp.server_name must be configured together")
	}
	if c.Internal.RevocationCRLs != "" && c.Internal.RevocationURLs != "" {
		return fmt.Errorf("configure exactly one of internal.revocation_crls or internal.revocation_urls")
	}
	renewInterval, renewBefore := c.nodeCRLRenewalSettings()
	if renewBefore <= 0 || renewBefore >= selfHostedNodeCRLValidity {
		return fmt.Errorf("ca.revocation_renew_before must be between 0 and %s", selfHostedNodeCRLValidity)
	}
	if renewInterval <= 0 || renewInterval > renewBefore {
		return fmt.Errorf("ca.revocation_renew_interval must be positive and no greater than ca.revocation_renew_before")
	}
	// Real GitHub requires client credentials unless fake_github=true or dev mode with no client_id
	// (dev fallback to in-process fake).
	useFakeGitHub := c.FakeGithub || (c.Dev && c.GitHub.ClientID == "")
	if !useFakeGitHub {
		if c.GitHub.ClientID == "" {
			return fmt.Errorf("github.client_id is required when not using fake_github")
		}
		if string(c.GitHub.ClientSecret) == "" {
			return fmt.Errorf("github.client_secret is required when not using fake_github")
		}
	}
	return nil
}

func (c AS) nodeCRLRenewalSettings() (time.Duration, time.Duration) {
	interval := c.CA.RevocationRenewInterval
	if interval == 0 {
		interval = defaultNodeCRLRenewInterval
	}
	renewBefore := c.CA.RevocationRenewBefore
	if renewBefore == 0 {
		renewBefore = defaultNodeCRLRenewBefore
	}
	return interval, renewBefore
}

// asEnvAliases maps existing AS_* and bare GITHUB_* environment variable names to dotted config
// keys, so current deployments keep working unchanged (the env layer sits above the files).
var asEnvAliases = map[string]string{
	"AS_PUBLIC_URL":                           "public_url",
	"AS_DEV":                                  "dev",
	"AS_FAKE_GITHUB":                          "fake_github",
	"AS_FAKE_GITHUB_ADDR":                     "fake_github_addr",
	"AS_FAKE_GITHUB_BASE_URL":                 "fake_github_base_url",
	"AS_FAKE_GITHUB_USERS":                    "fake_github_users",
	"AS_FAKE_GITHUB_TOKEN":                    "fake_github_token",
	"AS_LISTEN":                               "listen",
	"AS_INTERNAL_LISTEN":                      "internal.listen",
	"AS_INTERNAL_TRUST_DOMAIN":                "internal.trust_domain",
	"AS_INTERNAL_ROOT_CA":                     "internal.root_ca",
	"AS_INTERNAL_CERT":                        "internal.cert",
	"AS_INTERNAL_CHAIN":                       "internal.chain",
	"AS_INTERNAL_KEY":                         "internal.key",
	"AS_INTERNAL_SERVER_NAME":                 "internal.server_name",
	"AS_INTERNAL_REVOCATION_STATE":            "internal.revocation_state",
	"AS_INTERNAL_REVOCATION_ISSUERS":          "internal.revocation_issuers",
	"AS_INTERNAL_REVOCATION_CRLS":             "internal.revocation_crls",
	"AS_INTERNAL_REVOCATION_URLS":             "internal.revocation_urls",
	"AS_INTERNAL_REVOCATION_REFRESH_INTERVAL": "internal.revocation_refresh_interval",
	"AS_NODE_CRL_RENEW_INTERVAL":              "ca.revocation_renew_interval",
	"AS_NODE_CRL_RENEW_BEFORE":                "ca.revocation_renew_before",
	"AS_ALLOWED_ORIGINS":                      "allowed_origins",
	"AS_ROOT_CA_PEM":                          "ca.root_pem",
	"AS_TRUST_DOMAIN":                         "ca.trust_domain",
	"AS_INTERMEDIATE_CERT_PEM":                "ca.intermediate_cert",
	"AS_INTERMEDIATE_KEY_PEM":                 "ca.intermediate_key",
	"AS_SELF_HOSTED_REVOCATION_CRL":           "ca.revocation_crl",
	"AS_LEGACY_NODE_REVOCATION_CERTIFICATES":  "ca.legacy_revocation_certificates",
	"AS_AUTH_SIGNING_ENVIRONMENT":             "signing.environment",
	"AS_AUTH_SIGNING_ROOT_PEM":                "signing.root_pem",
	"AS_AUTH_SIGNING_CURRENT_KEY_PEM":         "signing.current_key_pem",
	"AS_AUTH_SIGNING_CURRENT_CHAIN_PEM":       "signing.current_chain_pem",
	"AS_AUTH_SIGNING_NEXT_KEY_PEM":            "signing.next_key_pem",
	"AS_AUTH_SIGNING_NEXT_CHAIN_PEM":          "signing.next_chain_pem",
	"AS_DB_DSN":                               "db.dsn",
	"AS_DB_DRIVER":                            "db.driver",
	"AS_GITHUB_TOKEN_ENC_KEY":                 "github.token_enc_key",
	"AS_GITHUB_TOKEN_ENC_KEY_FILE":            "github.token_enc_key_file",
	"GITHUB_CLIENT_ID":                        "github.client_id",
	"GITHUB_CLIENT_SECRET":                    "github.client_secret",
	"GITHUB_WEB_URL":                          "github.web_url",
	"GITHUB_API_URL":                          "github.api_url",
	"AS_GITHUB_REDIRECT_URI":                  "github.redirect_uri",
	"AS_GITHUB_LINK_REDIRECT_URI":             "github.link_redirect_uri",
	"AS_GITHUB_POST_REDEEM_REDIRECT":          "github.post_redeem_redirect",
	"GITHUB_DEFAULT_HOST":                     "github.default_host",
	"AS_SPA_ORIGINS":                          "spa_origins",
	"AS_REDIRECT_URIS":                        "redirect_uris",
	"AS_VERIFICATION_URI":                     "verification_uri",
	"REGISTRATION_ENABLED":                    "registration_enabled",
	"AS_MAX_FAMILIES":                         "max_families",
	"AS_DEVICE_PER_MIN":                       "rate_limits.device_per_min",
	"AS_CP_URL":                               "cp.url",
	"AS_CP_SERVER_NAME":                       "cp.server_name",
}
