package main

import (
	"fmt"
	"strings"

	"spawnery/internal/config"
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

	CA struct {
		RootPEM          string `koanf:"root_pem"`
		IntermediateCert string `koanf:"intermediate_cert"`
		IntermediateKey  string `koanf:"intermediate_key"`
	} `koanf:"ca"`

	Session struct {
		KeyPEM     string `koanf:"key_pem"`
		KeyNextPEM string `koanf:"key_next_pem"`
	} `koanf:"session"`

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

	CP struct {
		URL       string        `koanf:"url"`
		RPCSecret config.Secret `koanf:"rpc_secret"`
		Secret    config.Secret `koanf:"secret"`
	} `koanf:"cp"`

	DevRelaxNodeAuth bool `koanf:"dev_relax_node_auth"`
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
	if c.GitHub.ClientID != "" && c.GitHub.RedirectURI == "" {
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
	if !c.Dev {
		if c.Session.KeyPEM == "" {
			return fmt.Errorf("session.key_pem is required in production (set dev=true for development)")
		}
		if string(c.GitHub.TokenEncKey) == "" && c.GitHub.TokenEncKeyFile == "" {
			return fmt.Errorf("github.token_enc_key (or github.token_enc_key_file) is required for at-rest github token encryption")
		}
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

// asEnvAliases maps existing AS_* and bare GITHUB_* environment variable names to dotted config
// keys, so current deployments keep working unchanged (the env layer sits above the files).
var asEnvAliases = map[string]string{
	"AS_PUBLIC_URL":                  "public_url",
	"AS_DEV":                         "dev",
	"AS_FAKE_GITHUB":                 "fake_github",
	"AS_LISTEN":                      "listen",
	"AS_ALLOWED_ORIGINS":             "allowed_origins",
	"AS_ROOT_CA_PEM":                 "ca.root_pem",
	"AS_INTERMEDIATE_CERT_PEM":       "ca.intermediate_cert",
	"AS_INTERMEDIATE_KEY_PEM":        "ca.intermediate_key",
	"AS_SESSION_KEY_PEM":             "session.key_pem",
	"AS_SESSION_KEY_NEXT_PEM":        "session.key_next_pem",
	"AS_DB_DSN":                      "db.dsn",
	"AS_DB_DRIVER":                   "db.driver",
	"AS_GITHUB_TOKEN_ENC_KEY":        "github.token_enc_key",
	"AS_GITHUB_TOKEN_ENC_KEY_FILE":   "github.token_enc_key_file",
	"GITHUB_CLIENT_ID":               "github.client_id",
	"GITHUB_CLIENT_SECRET":           "github.client_secret",
	"GITHUB_WEB_URL":                 "github.web_url",
	"GITHUB_API_URL":                 "github.api_url",
	"AS_GITHUB_REDIRECT_URI":         "github.redirect_uri",
	"AS_GITHUB_LINK_REDIRECT_URI":    "github.link_redirect_uri",
	"AS_GITHUB_POST_REDEEM_REDIRECT": "github.post_redeem_redirect",
	"GITHUB_DEFAULT_HOST":            "github.default_host",
	"AS_SPA_ORIGINS":                 "spa_origins",
	"AS_REDIRECT_URIS":               "redirect_uris",
	"AS_VERIFICATION_URI":            "verification_uri",
	"REGISTRATION_ENABLED":           "registration_enabled",
	"AS_MAX_FAMILIES":                "max_families",
	"AS_CP_URL":                      "cp.url",
	"AS_CP_RPC_SECRET":               "cp.rpc_secret",
	"AS_CP_SECRET":                   "cp.secret",
	"AS_DEV_RELAX_NODE_AUTH":         "dev_relax_node_auth",
}

// multiFlag is a repeatable string flag (used for --set key=value).
type multiFlag []string

func (m *multiFlag) String() string { return strings.Join(*m, ",") }
func (m *multiFlag) Set(v string) error {
	*m = append(*m, v)
	return nil
}
