package main

import (
	"fmt"
	"os"
	"time"

	configfiles "spawnery/config"
	"spawnery/internal/config"
)

// Spawnlet is the node-agent configuration schema. Documented defaults live in config/spawnlet.yaml;
// per-environment deltas in config/spawnlet.<env>.yaml. Every field is overridable via the
// spawnletEnvAliases env vars and CLI --set.
type Spawnlet struct {
	config.Common `koanf:",squash"`

	AgentImage       string        `koanf:"agent_image"        validate:"required"`
	SidecarImage     string        `koanf:"sidecar_image"      validate:"required"`
	OpenRouterKey    config.Secret `koanf:"openrouter_api_key"`
	DataRoot         string        `koanf:"data_root"          validate:"required"`
	SpawnletAddr     string        `koanf:"spawnlet_addr"`
	AgentBinaries    []string      `koanf:"agent_binaries"`
	ContainerRuntime string        `koanf:"container_runtime"`
	UsernsMode       string        `koanf:"userns_mode"`
	ASURL            string        `koanf:"as_url"`
	ASServerName     string        `koanf:"as_server_name"`
	EnrollToken      config.Secret `koanf:"enroll_token"`
	PodDNS           []string      `koanf:"pod_dns"`
	// SidecarCABundleFile is a DEV/TEST-ONLY knob (sp-wwtc.3): a host path to a merged CA bundle
	// (system roots + an extra trusted CA) bind-mounted into the sidecar with SSL_CERT_FILE pointed
	// at it. See spawnlet.ManagerConfig.SidecarCABundleFile for the full rationale. Empty (default,
	// production) = no mount, no override.
	SidecarCABundleFile string `koanf:"sidecar_ca_bundle_file"`

	Node struct {
		ID                           string        `koanf:"id"`
		Class                        string        `koanf:"class"`
		Owner                        string        `koanf:"owner"`
		AdvertiseIP                  string        `koanf:"advertise_ip"`
		TerminalAddr                 string        `koanf:"terminal_addr"`
		AuthMode                     string        `koanf:"auth_mode"`
		IDDir                        string        `koanf:"id_dir"`
		RootCA                       string        `koanf:"root_ca"`
		TrustDomain                  string        `koanf:"trust_domain"`
		Environment                  string        `koanf:"environment"`
		SignerRevocationStatement    string        `koanf:"signer_revocation_statement"`
		SignerRevocationState        string        `koanf:"signer_revocation_state"`
		UserRevocationState          string        `koanf:"user_revocation_state"`
		UserRevocationPollInterval   time.Duration `koanf:"user_revocation_poll_interval"`
		UserRevocationRequestTimeout time.Duration `koanf:"user_revocation_request_timeout"`
		UserRevocationMaxBackoff     time.Duration `koanf:"user_revocation_max_backoff"`
		CertificateRevocationState   string        `koanf:"certificate_revocation_state"`
		CertificateRevocationIssuers string        `koanf:"certificate_revocation_issuers"`
		CertificateRevocationCRLs    string        `koanf:"certificate_revocation_crls"`
		CertificateRevocationURLs    string        `koanf:"certificate_revocation_urls"`
		CertificateRevocationRefresh time.Duration `koanf:"certificate_revocation_refresh_interval"`
	} `koanf:"node"`

	CP struct {
		Addr       string `koanf:"addr"`
		NodeAddr   string `koanf:"node_addr"`
		ServerName string `koanf:"server_name"`
	} `koanf:"cp"`

	// GitHub points github: storage mounts at a non-github.com git host (e.g. a local Gitea) with a
	// static access token, bypassing the AS mint. All fields empty/false = production default
	// (github.com, secure, AS-minted). This is a DEV/TEST knob for the e2e-vm Gitea lane — see
	// scripts/e2e-vm/provision (gitea.env). A StaticToken (or StaticTokenFile) enables the lane.
	GitHub struct {
		APIBaseURL        string        `koanf:"api_base_url"`        // defaultGitHubRepoService.BaseURL, e.g. http://127.0.0.1:3000/api/v1
		Host              string        `koanf:"host"`                // mount host, must match the clone_url host (e.g. 127.0.0.1:3000)
		AllowInsecureHost bool          `koanf:"allow_insecure_host"` // permit http + non-github.com host
		StaticToken       config.Secret `koanf:"static_token"`        // Gitea PAT (basic-auth password for clone)
		StaticTokenFile   string        `koanf:"static_token_file"`   // alternative: read the PAT from this file
	} `koanf:"github"`

	Egress struct {
		Enforce       bool     `koanf:"enforce"`
		AllowCIDRs    []string `koanf:"allow_cidrs"`
		FloorForceOff bool     `koanf:"floor_force_off"`
	} `koanf:"egress"`

	Limits struct {
		MemMB int64   `koanf:"mem_mb"`
		CPU   float64 `koanf:"cpu"`
		Pids  int64   `koanf:"pids"`
	} `koanf:"limits"`

	Delta struct {
		Capture     bool     `koanf:"capture"`
		SquashDepth int      `koanf:"squash_depth"`
		ScrubPaths  []string `koanf:"scrub_paths"`
	} `koanf:"delta"`

	CRI struct {
		Endpoint       string `koanf:"endpoint"`
		RuntimeHandler string `koanf:"runtime_handler"`
	} `koanf:"cri"`

	Journal struct {
		Backend string `koanf:"backend"`
		Root    string `koanf:"root"`
		FSRoot  string `koanf:"fs_root"`
		NodeKey string `koanf:"node_key"`
		S3      struct {
			Endpoint            string        `koanf:"endpoint"`
			GarageAdminEndpoint string        `koanf:"garage_admin_endpoint"`
			GarageAdminToken    config.Secret `koanf:"garage_admin_token"`
			Region              string        `koanf:"region"`
			DisableTLS          bool          `koanf:"disable_tls"`
		} `koanf:"s3"`
	} `koanf:"journal"`
}

// Validate runs cross-field checks beyond the struct tags.
func (s Spawnlet) Validate() error {
	if err := s.Common.Validate(); err != nil {
		return err
	}
	switch s.Node.AuthMode {
	case "insecure":
	case "enforced":
	default:
		return fmt.Errorf("node.auth_mode must be one of insecure or enforced")
	}
	if s.CP.Addr != "" && s.Node.AuthMode == "enforced" && s.Node.TerminalAddr != "" {
		return fmt.Errorf("node.terminal_addr is forbidden for enforced CP-attached nodes")
	}
	if s.Node.AuthMode == "enforced" || s.CP.Addr != "" {
		if s.Node.Environment == "" {
			return fmt.Errorf("node.environment is required for client authorization")
		}
		if s.Node.RootCA == "" && s.Node.IDDir == "" {
			return fmt.Errorf("node.root_ca or node.id_dir is required for client authorization")
		}
		if s.Node.SignerRevocationState == "" {
			return fmt.Errorf("node.signer_revocation_state is required for client authorization")
		}
	}
	if s.Node.AuthMode == "insecure" {
		return nil
	}
	if s.ASURL == "" || s.ASServerName == "" {
		return fmt.Errorf("as_url and as_server_name are required in enforced mode")
	}
	if s.Node.UserRevocationState == "" {
		return fmt.Errorf("node user revocation state is required in enforced mode")
	}
	if s.Node.UserRevocationPollInterval <= 0 {
		return fmt.Errorf("node user revocation positive poll interval is required in enforced mode")
	}
	if s.Node.UserRevocationRequestTimeout <= 0 {
		return fmt.Errorf("node user revocation positive request timeout is required in enforced mode")
	}
	if s.Node.UserRevocationMaxBackoff <= 0 {
		return fmt.Errorf("node user revocation positive max backoff is required in enforced mode")
	}
	if s.Node.UserRevocationMaxBackoff < s.Node.UserRevocationPollInterval {
		return fmt.Errorf("node user revocation max backoff must be at least the poll interval")
	}
	if s.CP.ServerName == "" {
		return fmt.Errorf("cp.server_name is required in enforced mode")
	}
	if s.ASURL != "" && s.ASServerName == "" {
		return fmt.Errorf("as_server_name is required when as_url is configured in enforced mode")
	}
	if s.Node.CertificateRevocationState == "" || s.Node.CertificateRevocationIssuers == "" || s.Node.CertificateRevocationCRLs == "" && s.Node.CertificateRevocationURLs == "" {
		return fmt.Errorf("node certificate revocation state, issuers, and CRL sources are required in enforced mode")
	}
	if s.Node.CertificateRevocationCRLs != "" && s.Node.CertificateRevocationURLs != "" {
		return fmt.Errorf("configure exactly one node certificate CRL source channel")
	}
	return nil
}

// spawnletEnvAliases maps legacy environment variable names to dotted config keys so existing
// deployments keep working unchanged (the env layer sits above the files).
var spawnletEnvAliases = map[string]string{
	"AGENT_IMAGE":                                  "agent_image",
	"SIDECAR_IMAGE":                                "sidecar_image",
	"OPENROUTER_API_KEY":                           "openrouter_api_key",
	"DATA_ROOT":                                    "data_root",
	"SPAWNLET_ADDR":                                "spawnlet_addr",
	"AGENT_BINARIES":                               "agent_binaries",
	"CONTAINER_RUNTIME":                            "container_runtime",
	"USERNS_MODE":                                  "userns_mode",
	"AS_URL":                                       "as_url",
	"AS_SERVER_NAME":                               "as_server_name",
	"ENROLL_TOKEN":                                 "enroll_token",
	"POD_DNS":                                      "pod_dns",
	"SIDECAR_CA_BUNDLE_FILE":                       "sidecar_ca_bundle_file",
	"NODE_ID":                                      "node.id",
	"NODE_CLASS":                                   "node.class",
	"NODE_OWNER":                                   "node.owner",
	"NODE_ADVERTISE_IP":                            "node.advertise_ip",
	"NODE_TERMINAL_ADDR":                           "node.terminal_addr",
	"NODE_AUTH_MODE":                               "node.auth_mode",
	"NODE_ID_DIR":                                  "node.id_dir",
	"NODE_ROOT_CA":                                 "node.root_ca",
	"NODE_TRUST_DOMAIN":                            "node.trust_domain",
	"NODE_AUTH_ENVIRONMENT":                        "node.environment",
	"NODE_SIGNER_REVOCATION_STATEMENT":             "node.signer_revocation_statement",
	"NODE_SIGNER_REVOCATION_STATE":                 "node.signer_revocation_state",
	"NODE_USER_REVOCATION_STATE":                   "node.user_revocation_state",
	"NODE_USER_REVOCATION_POLL_INTERVAL":           "node.user_revocation_poll_interval",
	"NODE_USER_REVOCATION_REQUEST_TIMEOUT":         "node.user_revocation_request_timeout",
	"NODE_USER_REVOCATION_MAX_BACKOFF":             "node.user_revocation_max_backoff",
	"NODE_CERTIFICATE_REVOCATION_STATE":            "node.certificate_revocation_state",
	"NODE_CERTIFICATE_REVOCATION_ISSUERS":          "node.certificate_revocation_issuers",
	"NODE_CERTIFICATE_REVOCATION_CRLS":             "node.certificate_revocation_crls",
	"NODE_CERTIFICATE_REVOCATION_URLS":             "node.certificate_revocation_urls",
	"NODE_CERTIFICATE_REVOCATION_REFRESH_INTERVAL": "node.certificate_revocation_refresh_interval",
	"CP_ADDR":                                      "cp.addr",
	"CP_NODE_ADDR":                                 "cp.node_addr",
	"CP_SERVER_NAME":                               "cp.server_name",
	"GITHUB_API_BASE_URL":                          "github.api_base_url",
	"GITHUB_HOST":                                  "github.host",
	"GITHUB_ALLOW_INSECURE_HOST":                   "github.allow_insecure_host",
	"GITHUB_STATIC_TOKEN":                          "github.static_token",
	"GITHUB_STATIC_TOKEN_FILE":                     "github.static_token_file",
	"EGRESS_ENFORCE":                               "egress.enforce",
	"EGRESS_ALLOW_CIDRS":                           "egress.allow_cidrs",
	"EGRESS_FLOOR_FORCE_OFF":                       "egress.floor_force_off",
	"MEM_LIMIT_MB":                                 "limits.mem_mb",
	"CPU_LIMIT":                                    "limits.cpu",
	"PIDS_LIMIT":                                   "limits.pids",
	"DELTA_CAPTURE":                                "delta.capture",
	"DELTA_SQUASH_DEPTH":                           "delta.squash_depth",
	"DELTA_SCRUB_PATHS":                            "delta.scrub_paths",
	"CRI_ENDPOINT":                                 "cri.endpoint",
	"CRI_RUNTIME_HANDLER":                          "cri.runtime_handler",
	"JOURNAL_BACKEND":                              "journal.backend",
	"JOURNAL_ROOT":                                 "journal.root",
	"JOURNAL_FS_ROOT":                              "journal.fs_root",
	"JOURNAL_NODE_KEY":                             "journal.node_key",
	"JOURNAL_S3_ENDPOINT":                          "journal.s3.endpoint",
	"JOURNAL_GARAGE_ADMIN_ENDPOINT":                "journal.s3.garage_admin_endpoint",
	"JOURNAL_GARAGE_ADMIN_TOKEN":                   "journal.s3.garage_admin_token",
	"JOURNAL_S3_REGION":                            "journal.s3.region",
	"JOURNAL_S3_DISABLE_TLS":                       "journal.s3.disable_tls",
}

func loadConfig() (*Spawnlet, error) {
	configDir, sets := config.StdFlags("spawnlet", os.Args[1:])
	return config.Load[Spawnlet]("spawnlet", config.Options{
		Args:        os.Args[1:],
		Embedded:    configfiles.FS,
		SecretsFS:   configfiles.FS,
		ExternalDir: configDir,
		EnvAliases:  spawnletEnvAliases,
		Sets:        sets,
	})
}
