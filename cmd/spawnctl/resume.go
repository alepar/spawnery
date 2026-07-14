// resume.go — `spawnctl resume <spawn-id>`: resume a suspended spawn in place.
//
// Resume is async + intent-gated exactly like create: the CP blocks ResumeSpawn until the client
// submits a signed intent (A4 two-phase sign-after-resolve [AC1][AM12]). internal/client's Resume
// runs pollAndSign concurrently with the blocking RPC. Unlike `move`, this stays on the same node
// and requires no owner-sealed keys — it restores the spawn's mounts from the node-local journal.
package main

import (
	"context"
	"fmt"
	"log"
	"strings"
	"time"

	"spawnery/internal/client"

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
			&cli.StringFlag{Name: "root-ca", Usage: "path to the pinned Root CA PEM"},
			&cli.StringFlag{Name: "trust-domain", Usage: "expected SPIFFE trust domain"},
			&cli.StringFlag{Name: "crl-state", Usage: "persistent certificate revocation checkpoint"},
			&cli.StringSliceFlag{Name: "crl-issuer", Usage: "trusted issuing-intermediate PEM (repeatable)"},
			&cli.StringSliceFlag{Name: "crl", Usage: "current signed CRL PEM (repeatable)"},
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
			src := buildTokenSource(dir, c.String("token"), connectClient())
			opts, err := loadMoveOptions(dir, c.String("token"), strings.TrimSpace(c.String("root-ca")), strings.TrimSpace(c.String("trust-domain")), strings.TrimSpace(c.String("crl-state")), c.StringSlice("crl-issuer"), c.StringSlice("crl"), time.Now)
			if err != nil {
				return cli.Exit(err.Error(), 1)
			}
			if opts.CloseCertificateRevocations != nil {
				defer func() { _ = opts.CloseCertificateRevocations() }()
			}
			trust, err := targetTrustFromMoveOptions(opts)
			if err != nil {
				return cli.Exit(err.Error(), 1)
			}
			sdk := client.New(c.String("cp"), src, nil, client.WithNodeAuthorization(src, trust), client.WithWarnHandler(func(err error) {
				log.Printf("%v", err)
			}))
			// ResumeSpawn blocks at the CP awaiting the signed intent; Resume drives pollAndSign
			// concurrently and retries once on a retryable NACK.
			if err := sdk.Resume(ctx, spawnID); err != nil {
				return cli.Exit("resume failed: "+err.Error(), 1)
			}
			fmt.Fprintf(c.Writer, "resumed %s\n", spawnID)
			return nil
		},
	}
}
