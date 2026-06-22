// status.go — `spawnctl status <id>`: show spawn status and full provisioning failure detail.
package main

import (
	"context"
	"fmt"
	"io"

	"github.com/urfave/cli/v3"
	cpv1 "spawnery/gen/cp/v1"
)

func statusCmd() *cli.Command {
	return &cli.Command{
		Name:      "status",
		Usage:     "show provisioning status and failure detail for a spawn",
		ArgsUsage: "<spawn-id>",
		Flags: []cli.Flag{
			configDirFlag(),
			&cli.StringFlag{Name: "cp", Value: "http://127.0.0.1:8080", Usage: "control-plane address"},
			&cli.StringFlag{Name: "token", Value: "dev-token", Usage: "dev auth token (CP); superseded by stored login credentials"},
		},
		Action: func(_ context.Context, c *cli.Command) error {
			if c.Args().Len() != 1 {
				return cli.Exit("usage: spawnctl status <spawn-id>", 2)
			}
			spawnID := c.Args().Get(0)
			dir, err := resolveDir(c)
			if err != nil {
				return cli.Exit(err.Error(), 1)
			}
			src := buildTokenSource(dir, c.String("token"), h2cClient())
			sums := listSpawns(c.String("cp"), src)
			for _, s := range sums {
				if s.GetSpawnId() == spawnID {
					renderStatus(s, c.Writer)
					return nil
				}
			}
			return cli.Exit("spawn "+spawnID+" not found", 1)
		},
	}
}

// renderStatus prints a detailed status block for a single spawn. On ERROR it also prints the
// provisionFailure headline and the full error_detail verbatim (multi-line safe, no truncation).
func renderStatus(s *cpv1.SpawnSummary, w io.Writer) {
	fmt.Fprintf(w, "status: %s\n", spawnStatus(s))
	if s.GetStatus() == cpv1.SpawnStatus_SPAWN_STATUS_ERROR {
		fmt.Fprintln(w, provisionFailure(s))
		if detail := s.GetErrorDetail(); detail != "" {
			fmt.Fprintln(w, detail)
		}
	}
}
