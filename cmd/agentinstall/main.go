// Command agentinstall is a standalone CLI for installing agent artifacts
// (skills, MCP servers, config) into per-agent native config files.
// It has zero spawnery-internal imports and is go-install-able.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/urfave/cli/v3"

	"spawnery/internal/agentinstall"
)

func main() {
	app := &cli.Command{
		Name:  "agentinstall",
		Usage: "Install agent artifacts (skills, MCPs, configs) into per-agent native config files",
		Commands: []*cli.Command{
			listAgentsCmd(),
			applyCmd(),
			installCmd(),
		},
	}
	if err := app.Run(context.Background(), os.Args); err != nil {
		fmt.Fprintf(os.Stderr, "agentinstall: %v\n", err)
		os.Exit(1)
	}
}

func osEnviron() agentinstall.Environ {
	return agentinstall.OSEnviron{}
}

func listAgentsCmd() *cli.Command {
	return &cli.Command{
		Name:  "list-agents",
		Usage: "List registered agent emitters and which are currently detected",
		Flags: []cli.Flag{
			&cli.BoolFlag{
				Name:  "capabilities",
				Usage: "Emit the (kind,agent)->supported|no-op|best-effort matrix as JSON",
			},
		},
		Action: func(ctx context.Context, cmd *cli.Command) error {
			env := osEnviron()
			reg := agentinstall.NewRegistry(env)

			if cmd.Bool("capabilities") {
				data, err := json.Marshal(agentinstall.Capabilities(reg))
				if err != nil {
					return fmt.Errorf("marshal capabilities: %w", err)
				}
				fmt.Println(string(data))
				return nil
			}

			detected := agentinstall.Detect(env)

			detectedSet := make(map[string]bool)
			for _, name := range detected {
				detectedSet[name] = true
			}

			fmt.Println("Registered agents:")
			for _, name := range reg.Names() {
				status := "not detected"
				if detectedSet[name] {
					status = "detected"
				}
				fmt.Printf("  %-12s  %s\n", name, status)
			}
			return nil
		},
	}
}

func applyCmd() *cli.Command {
	return &cli.Command{
		Name:  "apply",
		Usage: "Apply all artifacts from a staging directory",
		Flags: []cli.Flag{
			&cli.StringFlag{
				Name:     "artifacts",
				Aliases:  []string{"a"},
				Usage:    "Path to the staging artifacts directory (must contain manifest.json)",
				Required: true,
			},
			&cli.StringFlag{
				Name:    "secrets",
				Aliases: []string{"s"},
				Usage:   "Path to the secrets directory",
			},
			&cli.StringFlag{
				Name:  "agent",
				Usage: "Apply only artifacts targeting this agent (claude|codex|opencode|hermes|goose); if omitted, applies to all targets",
			},
			&cli.DurationFlag{
				Name:  "secret-wait-timeout",
				Usage: "Maximum duration to wait for async-delivered secret files before declaring them missing (0 disables the wait)",
			},
			&cli.StringFlag{
				Name:  "profile-id",
				Usage: "Profile ID to stamp into managed.json provenance entries",
			},
			&cli.StringFlag{
				Name:  "profile-version",
				Usage: "Profile version to stamp into managed.json provenance entries",
			},
		},
		Action: func(ctx context.Context, cmd *cli.Command) error {
			artifactsDir := cmd.String("artifacts")
			secretsDir := cmd.String("secrets")
			agentFilter := cmd.String("agent")

			m, err := agentinstall.LoadManifest(artifactsDir)
			if err != nil {
				return fmt.Errorf("load manifest: %w", err)
			}

			env := osEnviron()
			reg := agentinstall.NewRegistry(env)
			opts := agentinstall.Options{
				HomeDir:          env.Home(),
				SecretsDir:       secretsDir,
				ArtifactsDir:     artifactsDir,
				SecretWaitTimeout: cmd.Duration("secret-wait-timeout"),
				ProfileID:        cmd.String("profile-id"),
				ProfileVersion:   cmd.String("profile-version"),
				ManagedIndexPath: filepath.Join(env.Home(), ".spawnery", "managed.json"),
			}

			result := agentinstall.ApplyFiltered(reg, m, opts, env, agentFilter)

			data, err := json.Marshal(result)
			if err != nil {
				return fmt.Errorf("marshal result: %w", err)
			}
			fmt.Println(string(data))
			return nil
		},
	}
}

func installCmd() *cli.Command {
	return &cli.Command{
		Name:  "install",
		Usage: "Install a single artifact",
		Flags: []cli.Flag{
			&cli.StringFlag{
				Name:    "agent",
				Aliases: []string{"a"},
				Usage:   "Target agent name (claude|codex|opencode|hermes|goose); mutually exclusive with --all-detected",
			},
			&cli.BoolFlag{
				Name:  "all-detected",
				Usage: "Install into all detected agents (config root must exist)",
			},
			&cli.StringFlag{
				Name:    "secrets",
				Aliases: []string{"s"},
				Usage:   "Path to the secrets directory",
			},
		},
		Commands: []*cli.Command{
			installSubCmd("skill", "Install a skill directory"),
			installSubCmd("mcp", "Install an MCP server entry"),
			installConfigSubCmd(),
		},
	}
}

// resolveInstallTargets parses the --agent / --all-detected flags from cmd (a subcommand whose
// parent carries those flags) and returns the targets slice.
func resolveInstallTargets(cmd *cli.Command) ([]string, error) {
	// urfave/cli v3 walks the lineage upward, so cmd.Bool/String finds flags on parent "install".
	allDetected := cmd.Bool("all-detected")
	agentName := cmd.String("agent")
	switch {
	case allDetected:
		return []string{"all-detected"}, nil
	case agentName != "":
		return strings.Split(agentName, ","), nil
	default:
		return nil, fmt.Errorf("either --agent or --all-detected must be specified")
	}
}

func installSubCmd(kindStr, usage string) *cli.Command {
	kind := agentinstall.Kind(kindStr)
	return &cli.Command{
		Name:  kindStr,
		Usage: usage,
		Flags: []cli.Flag{
			&cli.StringFlag{
				Name:     "name",
				Aliases:  []string{"n"},
				Usage:    "Artifact name",
				Required: true,
			},
			&cli.StringFlag{
				Name:  "dir",
				Usage: "Skill directory path (for skill kind)",
			},
			&cli.StringFlag{
				Name:  "command",
				Usage: "MCP stdio command (for mcp kind)",
			},
			&cli.StringSliceFlag{
				Name:  "args",
				Usage: "MCP stdio command arguments (for mcp kind)",
			},
			&cli.StringFlag{
				Name:  "url",
				Usage: "MCP HTTP URL (for mcp kind)",
			},
		},
		Action: func(ctx context.Context, cmd *cli.Command) error {
			env := osEnviron()

			targets, err := resolveInstallTargets(cmd)
			if err != nil {
				return err
			}

			name := cmd.String("name")
			secretsDir := cmd.String("secrets")

			artifact := agentinstall.Artifact{
				Kind:    kind,
				Name:    name,
				Targets: targets,
			}

			switch kind {
			case agentinstall.KindSkill:
				dir := cmd.String("dir")
				if dir == "" {
					return fmt.Errorf("--dir is required for skill kind")
				}
				artifact.Skill = &agentinstall.SkillPayload{Dir: dir}
			case agentinstall.KindMCP:
				mcpCmd := cmd.String("command")
				mcpURL := cmd.String("url")
				if mcpCmd != "" {
					artifact.MCP = &agentinstall.MCPPayload{
						Stdio: &agentinstall.MCPTransportStdio{
							Command: mcpCmd,
							Args:    cmd.StringSlice("args"),
						},
					}
				} else if mcpURL != "" {
					artifact.MCP = &agentinstall.MCPPayload{
						HTTP: &agentinstall.MCPTransportHTTP{URL: mcpURL},
					}
				} else {
					return fmt.Errorf("either --command or --url is required for mcp kind")
				}
			}

			m := agentinstall.Manifest{
				SchemaVersion: agentinstall.CurrentSchemaVersion,
				Artifacts:     []agentinstall.Artifact{artifact},
			}
			reg := agentinstall.NewRegistry(env)
			opts := agentinstall.Options{
				HomeDir:    env.Home(),
				SecretsDir: secretsDir,
			}

			result := agentinstall.Apply(reg, m, opts, env)

			data, err := json.Marshal(result)
			if err != nil {
				return fmt.Errorf("marshal result: %w", err)
			}
			fmt.Println(string(data))
			return nil
		},
	}
}

// installConfigSubCmd returns the `install config` subcommand with --set key=value support.
func installConfigSubCmd() *cli.Command {
	return &cli.Command{
		Name:  "config",
		Usage: "Apply config keys to agent config files",
		Flags: []cli.Flag{
			&cli.StringFlag{
				Name:     "name",
				Aliases:  []string{"n"},
				Usage:    "Artifact name",
				Required: true,
			},
			&cli.StringSliceFlag{
				Name:  "set",
				Usage: "Set a normalized config key (format: key=value, repeatable; e.g. --set approvalPosture=yolo)",
			},
		},
		Action: func(ctx context.Context, cmd *cli.Command) error {
			env := osEnviron()

			targets, err := resolveInstallTargets(cmd)
			if err != nil {
				return err
			}

			name := cmd.String("name")
			secretsDir := cmd.String("secrets")

			// Parse --set key=value pairs into the normalized map.
			normalized := make(map[string]interface{})
			for _, kv := range cmd.StringSlice("set") {
				idx := strings.IndexByte(kv, '=')
				if idx < 0 {
					return fmt.Errorf("--set %q: expected key=value format", kv)
				}
				key := kv[:idx]
				val := kv[idx+1:]
				if key == "" {
					return fmt.Errorf("--set %q: key must not be empty", kv)
				}
				normalized[key] = val
			}

			artifact := agentinstall.Artifact{
				Kind:    agentinstall.KindConfig,
				Name:    name,
				Targets: targets,
				Config: &agentinstall.ConfigPayload{
					Normalized: normalized,
				},
			}

			m := agentinstall.Manifest{
				SchemaVersion: agentinstall.CurrentSchemaVersion,
				Artifacts:     []agentinstall.Artifact{artifact},
			}
			reg := agentinstall.NewRegistry(env)
			opts := agentinstall.Options{
				HomeDir:    env.Home(),
				SecretsDir: secretsDir,
			}

			result := agentinstall.Apply(reg, m, opts, env)

			data, err := json.Marshal(result)
			if err != nil {
				return fmt.Errorf("marshal result: %w", err)
			}
			fmt.Println(string(data))
			return nil
		},
	}
}
