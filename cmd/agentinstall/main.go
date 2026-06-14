// Command agentinstall is a standalone CLI for installing agent artifacts
// (skills, MCP servers, config) into per-agent native config files.
// It has zero spawnery-internal imports and is go-install-able.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
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
		Action: func(ctx context.Context, cmd *cli.Command) error {
			env := osEnviron()
			reg := agentinstall.NewRegistry(env)
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
		},
		Action: func(ctx context.Context, cmd *cli.Command) error {
			artifactsDir := cmd.String("artifacts")
			secretsDir := cmd.String("secrets")

			m, err := agentinstall.LoadManifest(artifactsDir)
			if err != nil {
				return fmt.Errorf("load manifest: %w", err)
			}

			env := osEnviron()
			reg := agentinstall.NewRegistry(env)
			opts := agentinstall.Options{
				HomeDir:      env.Home(),
				SecretsDir:   secretsDir,
				ArtifactsDir: artifactsDir,
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
			installSubCmd("config", "Apply config keys"),
		},
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

			// Resolve targets
			// NOTE: use cmd (the subcommand), not cmd.Root(); urfave/cli v3 walks
			// the lineage upward, so cmd.Bool/String finds flags declared on the
			// parent "install" command without crossing to the root.
			var targets []string
			parentCmd := cmd
			allDetected := parentCmd.Bool("all-detected")
			agentName := parentCmd.String("agent")

			switch {
			case allDetected:
				targets = []string{"all-detected"}
			case agentName != "":
				targets = strings.Split(agentName, ",")
			default:
				return fmt.Errorf("either --agent or --all-detected must be specified")
			}

			name := cmd.String("name")
			secretsDir := parentCmd.String("secrets")

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
			case agentinstall.KindConfig:
				artifact.Config = &agentinstall.ConfigPayload{
					Normalized: map[string]interface{}{},
				}
			}

			m := agentinstall.Manifest{Artifacts: []agentinstall.Artifact{artifact}}
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
