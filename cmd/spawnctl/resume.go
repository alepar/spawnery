// resume.go — `spawnctl resume <spawn-id>`: resume a suspended spawn in place.
//
// Resume is async + intent-gated exactly like create: the CP blocks ResumeSpawn until the client
// submits a signed intent (A4 two-phase sign-after-resolve [AC1][AM12]). provisionWithIntent runs
// pollAndSign concurrently with the blocking RPC. Unlike `move`, this stays on the same node and
// requires no owner-sealed keys — it restores the spawn's mounts from the node-local journal.
package main

import (
	"context"
	"fmt"

	"connectrpc.com/connect"

	cpv1 "spawnery/gen/cp/v1"
	"spawnery/gen/cp/v1/cpv1connect"

	"github.com/urfave/cli/v3"
)

func resumeCmd() *cli.Command {
	return &cli.Command{
		Name:      "resume",
		Usage:     "resume a suspended spawn in place (restores its mounts from the journal)",
		ArgsUsage: "<spawn-id>",
		Flags: []cli.Flag{
			configDirFlag(),
			&cli.StringFlag{Name: "cp", Value: "http://127.0.0.1:8080", Usage: "control-plane address"},
			&cli.StringFlag{Name: "token", Value: "dev-token", Usage: "dev auth token (CP); superseded by stored login credentials"},
		},
		Action: func(ctx context.Context, c *cli.Command) error {
			if c.Args().Len() != 1 {
				return cli.Exit("usage: spawnctl resume <spawn-id>", 2)
			}
			spawnID := c.Args().Get(0)
			dir, err := resolveDir(c)
			if err != nil {
				return cli.Exit(err.Error(), 1)
			}
			src := buildTokenSource(dir, c.String("token"), h2cClient())
			client := cpv1connect.NewSpawnServiceClient(h2cClient(), c.String("cp"),
				connect.WithGRPC(), connect.WithInterceptors(tokenSourceInterceptor(src)))
			// ResumeSpawn blocks at the CP awaiting the signed intent; provisionWithIntent drives
			// pollAndSign concurrently and retries once on a retryable NACK.
			if err := provisionWithIntent(ctx, client, spawnID, intentParams{}, func(rpcCtx context.Context) error {
				_, rpcErr := client.ResumeSpawn(rpcCtx, connect.NewRequest(&cpv1.ResumeSpawnRequest{SpawnId: spawnID}))
				return rpcErr
			}); err != nil {
				return cli.Exit("resume failed: "+err.Error(), 1)
			}
			fmt.Fprintf(c.Writer, "resumed %s\n", spawnID)
			return nil
		},
	}
}
