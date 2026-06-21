package main

import (
	"flag"
	"os"
	"strings"

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
	EnrollToken      config.Secret `koanf:"enroll_token"`
	PodDNS           []string      `koanf:"pod_dns"`

	Node struct {
		ID              string `koanf:"id"`
		Class           string `koanf:"class"`
		Owner           string `koanf:"owner"`
		AdvertiseIP     string `koanf:"advertise_ip"`
		TerminalAddr    string `koanf:"terminal_addr"`
		AuthMode        string `koanf:"auth_mode"`
		IDDir           string `koanf:"id_dir"`
		RootCA          string `koanf:"root_ca"`
		ASPubkeys       string `koanf:"as_pubkeys"`
		GitHubMintDevID string `koanf:"github_mint_dev_id"`
	} `koanf:"node"`

	CP struct {
		Addr     string `koanf:"addr"`
		NodeAddr string `koanf:"node_addr"`
	} `koanf:"cp"`

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
	return s.Common.Validate()
}

// spawnletEnvAliases maps legacy environment variable names to dotted config keys so existing
// deployments keep working unchanged (the env layer sits above the files).
var spawnletEnvAliases = map[string]string{
	"AGENT_IMAGE":                   "agent_image",
	"SIDECAR_IMAGE":                 "sidecar_image",
	"OPENROUTER_API_KEY":            "openrouter_api_key",
	"DATA_ROOT":                     "data_root",
	"SPAWNLET_ADDR":                 "spawnlet_addr",
	"AGENT_BINARIES":                "agent_binaries",
	"CONTAINER_RUNTIME":             "container_runtime",
	"USERNS_MODE":                   "userns_mode",
	"AS_URL":                        "as_url",
	"ENROLL_TOKEN":                  "enroll_token",
	"POD_DNS":                       "pod_dns",
	"NODE_ID":                       "node.id",
	"NODE_CLASS":                    "node.class",
	"NODE_OWNER":                    "node.owner",
	"NODE_ADVERTISE_IP":             "node.advertise_ip",
	"NODE_TERMINAL_ADDR":            "node.terminal_addr",
	"NODE_AUTH_MODE":                "node.auth_mode",
	"NODE_ID_DIR":                   "node.id_dir",
	"NODE_ROOT_CA":                  "node.root_ca",
	"NODE_AS_PUBKEYS":               "node.as_pubkeys",
	"NODE_GITHUB_MINT_DEV_NODE_ID":  "node.github_mint_dev_id",
	"CP_ADDR":                       "cp.addr",
	"CP_NODE_ADDR":                  "cp.node_addr",
	"EGRESS_ENFORCE":                "egress.enforce",
	"EGRESS_ALLOW_CIDRS":            "egress.allow_cidrs",
	"EGRESS_FLOOR_FORCE_OFF":        "egress.floor_force_off",
	"MEM_LIMIT_MB":                  "limits.mem_mb",
	"CPU_LIMIT":                     "limits.cpu",
	"PIDS_LIMIT":                    "limits.pids",
	"DELTA_CAPTURE":                 "delta.capture",
	"DELTA_SQUASH_DEPTH":            "delta.squash_depth",
	"DELTA_SCRUB_PATHS":             "delta.scrub_paths",
	"CRI_ENDPOINT":                  "cri.endpoint",
	"CRI_RUNTIME_HANDLER":           "cri.runtime_handler",
	"JOURNAL_BACKEND":               "journal.backend",
	"JOURNAL_ROOT":                  "journal.root",
	"JOURNAL_FS_ROOT":               "journal.fs_root",
	"JOURNAL_NODE_KEY":              "journal.node_key",
	"JOURNAL_S3_ENDPOINT":           "journal.s3.endpoint",
	"JOURNAL_GARAGE_ADMIN_ENDPOINT": "journal.s3.garage_admin_endpoint",
	"JOURNAL_GARAGE_ADMIN_TOKEN":    "journal.s3.garage_admin_token",
	"JOURNAL_S3_REGION":             "journal.s3.region",
	"JOURNAL_S3_DISABLE_TLS":        "journal.s3.disable_tls",
}

// multiFlag is a repeatable string flag (used for --set key=value).
type multiFlag []string

func (m *multiFlag) String() string { return strings.Join(*m, ",") }
func (m *multiFlag) Set(v string) error {
	*m = append(*m, v)
	return nil
}

func loadConfig() (*Spawnlet, error) {
	fs := flag.NewFlagSet("spawnlet", flag.ExitOnError)
	var sets multiFlag
	_ = fs.String("env", "", "environment dev|staging|prod (overrides SPAWNERY_ENV)")
	configDir := fs.String("config-dir", "", "external config override dir (overrides SPAWNERY_CONFIG_DIR)")
	fs.Var(&sets, "set", "override a config key: key.path=value (repeatable)")
	if err := fs.Parse(os.Args[1:]); err != nil {
		return nil, err
	}
	return config.Load[Spawnlet]("spawnlet", config.Options{
		Args:        os.Args[1:],
		Embedded:    configfiles.FS,
		SecretsFS:   configfiles.FS,
		ExternalDir: *configDir,
		EnvAliases:  spawnletEnvAliases,
		Sets:        []string(sets),
	})
}
