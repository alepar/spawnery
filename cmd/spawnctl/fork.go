package main

import (
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	"connectrpc.com/connect"
	"github.com/urfave/cli/v3"

	cpv1 "spawnery/gen/cp/v1"
	"spawnery/gen/cp/v1/cpv1connect"
	"spawnery/internal/secrets/seal"
)

type forkClient interface {
	ForkSpawn(context.Context, *connect.Request[cpv1.ForkSpawnRequest]) (*connect.Response[cpv1.ForkSpawnResponse], error)
	ownerSealedDeliveryClient
}

var _ forkClient = (cpv1connect.SpawnServiceClient)(nil)

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

func runFork(ctx context.Context, client forkClient, dev *seal.Device, spawnID, nodeID, class, name string, out io.Writer, now time.Time) error {
	req, err := forkTarget(spawnID, nodeID, class, name)
	if err != nil {
		return err
	}

	fmt.Fprintf(out, "fork %s\n", spawnID)
	switch {
	case req.TargetNodeId != "":
		fmt.Fprintf(out, "  target node %s\n", req.TargetNodeId)
	case req.TargetClass != "":
		fmt.Fprintf(out, "  target class %s\n", req.TargetClass)
	default:
		fmt.Fprintln(out, "  target same node")
	}

	resp, err := client.ForkSpawn(ctx, connect.NewRequest(req))
	if err != nil {
		return fmt.Errorf("fork: %w", err)
	}
	forkID := resp.Msg.ForkSpawnId
	fmt.Fprintf(out, "  fork %s active on node %s\n", forkID, resp.Msg.NodeId)
	if resp.Msg.TransferSetId != "" {
		fmt.Fprintf(out, "  transfer set %s\n", resp.Msg.TransferSetId)
	}

	delivered, err := deliverOwnerSealedJournalKeys(ctx, client, dev, forkID, out, now)
	if err != nil {
		return fmt.Errorf("fork created as %s; delivery pending: %w", forkID, err)
	}
	if delivered == 0 {
		fmt.Fprintln(out, "  done.")
		return nil
	}
	fmt.Fprintln(out, "  done.")
	return nil
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
		},
		Action: func(ctx context.Context, c *cli.Command) error {
			if c.Args().Len() != 1 {
				return cli.Exit("usage: spawnctl fork <spawn-id> [--node <id>|--class <class>] [--name <name>]", 2)
			}
			spawnID := strings.TrimSpace(c.Args().Get(0))
			if spawnID == "" {
				return cli.Exit("spawn id is required", 2)
			}
			dir, err := resolveDir(c)
			if err != nil {
				return cli.Exit(err.Error(), 1)
			}
			dev, err := loadDevice(dir)
			if err != nil {
				return cli.Exit(err.Error(), 1)
			}
			src := buildTokenSource(dir, c.String("token"), h2cClient())
			client := cpv1connect.NewSpawnServiceClient(h2cClient(), c.String("cp"),
				connect.WithGRPC(), connect.WithInterceptors(tokenSourceInterceptor(src)))
			if err := runFork(ctx, client, dev, spawnID, c.String("node"), c.String("class"), c.String("name"), c.Writer, time.Now()); err != nil {
				return cli.Exit("fork failed: "+err.Error(), 1)
			}
			return nil
		},
	}
}
