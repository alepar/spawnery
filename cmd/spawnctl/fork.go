package main

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/urfave/cli/v3"

	cpv1 "spawnery/gen/cp/v1"
	"spawnery/internal/client"
)

func forkTarget(spawnID, nodeID, class, name string) (*cpv1.ForkSpawnRequest, error) {
	nodeID = strings.TrimSpace(nodeID)
	class = strings.TrimSpace(class)
	name = strings.TrimSpace(name)
	if nodeID != "" && class != "" {
		return nil, fmt.Errorf("specify --node or --class, not both")
	}
	return &cpv1.ForkSpawnRequest{
		SpawnId:      spawnID,
		TargetNodeId: nodeID,
		TargetClass:  class,
		Name:         name,
	}, nil
}

func forkCmd() *cli.Command {
	return &cli.Command{
		Name:      "fork",
		Usage:     "fork an active spawn to the same node, a node, or a node class",
		ArgsUsage: "<spawn-id>",
		Flags: []cli.Flag{
			configDirFlag(),
			&cli.StringFlag{Name: "cp", Value: "http://127.0.0.1:8080", Usage: "control-plane address"},
			&cli.StringFlag{Name: "token", Value: "dev-token", Usage: "dev auth token"},
			&cli.StringFlag{Name: "node", Usage: "target node id"},
			&cli.StringFlag{Name: "class", Usage: "target node class"},
			&cli.StringFlag{Name: "name", Usage: "optional fork display name"},
			&cli.StringFlag{Name: "root-ca", Usage: "path to the pinned Root CA PEM for production node verification"},
			&cli.StringFlag{Name: "trust-domain", Usage: "expected SPIFFE trust domain for production node verification"},
			&cli.StringFlag{Name: "as", Usage: "Auth Service origin for node revocation checks; defaults to the stored login AS URL"},
		},
		Action: func(ctx context.Context, c *cli.Command) error {
			if c.Args().Len() != 1 {
				return cli.Exit("usage: spawnctl fork <spawn-id> [--node <id>|--class <class>] [--name <name>]", 2)
			}
			spawnID := strings.TrimSpace(c.Args().Get(0))
			if spawnID == "" {
				return cli.Exit("spawn id is required", 2)
			}
			req, err := forkTarget(spawnID, c.String("node"), c.String("class"), c.String("name"))
			if err != nil {
				return cli.Exit(err.Error(), 2)
			}
			dir, err := resolveDir(c)
			if err != nil {
				return cli.Exit(err.Error(), 1)
			}
			dev, err := loadDevice(dir)
			if err != nil {
				return cli.Exit(err.Error(), 1)
			}
			src := buildTokenSource(dir, c.String("token"), connectClient())
			sdk := client.New(c.String("cp"), src, nil)

			fmt.Fprintf(c.Writer, "fork %s\n", spawnID)
			switch {
			case req.TargetNodeId != "":
				fmt.Fprintf(c.Writer, "  target node %s\n", req.TargetNodeId)
			case req.TargetClass != "":
				fmt.Fprintf(c.Writer, "  target class %s\n", req.TargetClass)
			default:
				fmt.Fprintln(c.Writer, "  target same node")
			}

			opts, err := loadMoveOptions(dir, c.String("token"), strings.TrimSpace(c.String("as")), strings.TrimSpace(c.String("root-ca")), strings.TrimSpace(c.String("trust-domain")))
			if err != nil {
				return cli.Exit(err.Error(), 1)
			}
			if _, err := sdk.Fork(ctx, dev, req, c.Writer, time.Now(), opts); err != nil {
				return cli.Exit("fork failed: "+err.Error(), 1)
			}
			fmt.Fprintln(c.Writer, "  done.")
			return nil
		},
	}
}
