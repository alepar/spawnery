package main

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	"github.com/urfave/cli/v3"
	"google.golang.org/protobuf/proto"

	authv1 "spawnery/gen/auth/v1"
	"spawnery/internal/client"
)

// `spawnctl move <spawn-id> <target>` drives the data-only local<->cloud migration (sp-u53.5.3). It
// orchestrates the owner-side leg of the journal-key travel that the CP cannot do (the CP holds no
// key): fetch the owner-sealed ciphertext, drive MigrateSpawn (suspend source -> resume on target),
// then unseal locally + reseal to the target node's sub-key + deliver, so the journaled mounts restore
// on the target. <target> is a node id, or the literal "cloud" for the cloud class.
//
// The orchestration itself (fetch/migrate/reseal/deliver, the mid-flight progress lines) lives in
// internal/client's Migrate; this file keeps only the CLI concerns: flag/arg handling, loading
// move options (account id, root CA, revocation URL) from auth.json/env/flags, and the
// header/footer lines around the SDK's output.

// moveCmd wires `spawnctl move <spawn-id> <target>` to the control plane.
func moveCmd() *cli.Command {
	return &cli.Command{
		Name:      "move",
		Usage:     "migrate a spawn to another node or the cloud (suspend here, resume there)",
		ArgsUsage: "<spawn-id> <target|cloud>",
		Flags: []cli.Flag{
			configDirFlag(),
			&cli.StringFlag{Name: "cp", Value: "http://127.0.0.1:8080", Usage: "control-plane address"},
			&cli.StringFlag{Name: "token", Value: "dev-token", Usage: "dev auth token"},
			&cli.StringFlag{Name: "root-ca", Usage: "path to the pinned Root CA PEM for production node verification"},
			&cli.StringFlag{Name: "as", Usage: "Auth Service origin for node revocation checks; defaults to the stored login AS URL"},
		},
		Action: func(ctx context.Context, c *cli.Command) error {
			if c.Args().Len() != 2 {
				return cli.Exit("usage: spawnctl move <spawn-id> <target|cloud>", 2)
			}
			spawnID := c.Args().Get(0)
			target := strings.TrimSpace(c.Args().Get(1))
			if target == "" {
				return cli.Exit("a target node id (or \"cloud\") is required", 2)
			}
			dir, err := resolveDir(c)
			if err != nil {
				return cli.Exit(err.Error(), 1)
			}
			opts, err := loadMoveOptions(dir, c.String("token"), strings.TrimSpace(c.String("as")), strings.TrimSpace(c.String("root-ca")))
			if err != nil {
				return cli.Exit(err.Error(), 1)
			}
			dev, err := loadDevice(dir)
			if err != nil {
				return cli.Exit(err.Error(), 1)
			}
			src := buildTokenSource(dir, c.String("token"), connectClient())
			sdk := client.New(c.String("cp"), src, nil, client.WithWarnHandler(func(err error) {
				log.Printf("%v", err)
			}))
			fmt.Fprintf(c.Writer, "move %s -> %s\n", spawnID, target)
			if err := sdk.Migrate(ctx, dev, spawnID, target, c.Writer, time.Now(), opts); err != nil {
				return cli.Exit("move failed: "+err.Error(), 1)
			}
			fmt.Fprintln(c.Writer, "  done.")
			return nil
		},
	}
}

func loadMoveOptions(dir, tokenFlag, asFlag, rootCAPath string) (client.MoveOptions, error) {
	opts := client.MoveOptions{
		AccountID: resolveMoveAccountID(dir, tokenFlag),
	}
	if rootCAPath != "" {
		rootPEM, err := os.ReadFile(rootCAPath)
		if err != nil {
			return client.MoveOptions{}, fmt.Errorf("read root CA PEM: %w", err)
		}
		opts.RootPEM = rootPEM
	}
	asURL := strings.TrimRight(asFlag, "/")
	if asURL == "" {
		state, err := loadState(dir)
		if err == nil && state != nil {
			asURL = strings.TrimRight(state.ASURL, "/")
		}
	}
	if asURL != "" {
		opts.RevocationURL = asURL + "/node-revocations"
	}
	return opts, nil
}

func resolveMoveAccountID(dir, tokenFlag string) string {
	for _, token := range []string{os.Getenv("SPAWNERY_TOKEN"), os.Getenv("CP_DEV_TOKEN")} {
		if accountID, err := accountIDFromAccessToken(token); err == nil && accountID != "" {
			return accountID
		}
	}
	if tokenFlag != "" && tokenFlag != "dev-token" {
		if accountID, err := accountIDFromAccessToken(tokenFlag); err == nil && accountID != "" {
			return accountID
		}
	}
	state, err := loadState(dir)
	if err != nil || state == nil {
		return ""
	}
	if state.AccountID != "" {
		return state.AccountID
	}
	accountID, _ := accountIDFromAccessToken(state.AccessToken)
	return accountID
}

func accountIDFromAccessToken(wire string) (string, error) {
	bodyB64, _, ok := strings.Cut(wire, ".")
	if !ok {
		return "", errors.New("token is not in session-token wire format")
	}
	bodyBytes, err := base64.RawURLEncoding.DecodeString(bodyB64)
	if err != nil {
		return "", err
	}
	var body authv1.SessionTokenBody
	if err := proto.Unmarshal(bodyBytes, &body); err != nil {
		return "", err
	}
	return body.AccountId, nil
}
