// suspend.go — `spawnctl suspend <spawn-id>`: suspend a running spawn in place.
//
// Unlike resume, suspend needs no signed intent: SuspendSpawn is a plain RPC — the CP snapshots
// the spawn's mounts to the journal and tears down the pod. `spawnctl resume` restores it later.
package main

import (
	"context"
	"fmt"
	"log"

	"spawnery/internal/client"

	"github.com/urfave/cli/v3"
)

func suspendCmd() *cli.Command {
	return &cli.Command{
		Name:      "suspend",
		Usage:     "suspend a running spawn in place (snapshots its mounts to the journal and tears down the pod)",
		ArgsUsage: "<spawn-id>",
		Flags: []cli.Flag{
			configDirFlag(),
			&cli.StringFlag{Name: "cp", Value: "http://127.0.0.1:8080", Usage: "control-plane address"},
			&cli.StringFlag{Name: "token", Value: "dev-token", Usage: "dev auth token (CP); superseded by stored login credentials"},
		},
		Action: func(ctx context.Context, c *cli.Command) error {
			if c.Args().Len() != 1 {
				return cli.Exit("usage: spawnctl suspend <spawn-id>", 2)
			}
			spawnID := c.Args().Get(0)
			dir, err := resolveDir(c)
			if err != nil {
				return cli.Exit(err.Error(), 1)
			}
			src := buildTokenSource(dir, c.String("token"), connectClient())
			sdk := client.New(c.String("cp"), src, nil, client.WithWarnHandler(func(err error) {
				log.Printf("%v", err)
			}))
			if err := sdk.Suspend(ctx, spawnID); err != nil {
				return cli.Exit("suspend failed: "+err.Error(), 1)
			}
			fmt.Fprintf(c.Writer, "suspended %s\n", spawnID)
			return nil
		},
	}
}
