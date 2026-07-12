package main

import (
	"context"
	"crypto/x509"
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
	"spawnery/internal/pki"
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
			&cli.StringFlag{Name: "trust-domain", Usage: "expected SPIFFE trust domain for production node verification"},
			&cli.StringFlag{Name: "as", Usage: "Auth Service origin for node revocation checks; defaults to the stored login AS URL"},
			&cli.StringFlag{Name: "crl-state", Usage: "persistent certificate revocation checkpoint (required with --root-ca)"},
			&cli.StringSliceFlag{Name: "crl-issuer", Usage: "trusted issuing-intermediate PEM (repeatable; required with --root-ca)"},
			&cli.StringSliceFlag{Name: "crl", Usage: "current signed CRL PEM to apply before verification (repeatable)"},
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
			rootCAPath := strings.TrimSpace(c.String("root-ca"))
			trustDomain := strings.TrimSpace(c.String("trust-domain"))
			crlStatePath := strings.TrimSpace(c.String("crl-state"))
			issuerPaths := c.StringSlice("crl-issuer")
			crlPaths := c.StringSlice("crl")
			if err := validateMovePKIFlags(rootCAPath, trustDomain, crlStatePath, issuerPaths, crlPaths); err != nil {
				return cli.Exit(err.Error(), 2)
			}
			dir, err := resolveDir(c)
			if err != nil {
				return cli.Exit(err.Error(), 1)
			}
			opts, err := loadMoveOptions(dir, c.String("token"), strings.TrimSpace(c.String("as")), rootCAPath, trustDomain, crlStatePath, issuerPaths, crlPaths, time.Now)
			if err != nil {
				return cli.Exit(err.Error(), 1)
			}
			if opts.CloseCertificateRevocations != nil {
				defer opts.CloseCertificateRevocations()
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

func loadMoveOptions(dir, tokenFlag, asFlag, rootCAPath, trustDomain, crlStatePath string, issuerPaths, crlPaths []string, clock func() time.Time) (client.MoveOptions, error) {
	if err := validateMovePKIFlags(rootCAPath, trustDomain, crlStatePath, issuerPaths, crlPaths); err != nil {
		return client.MoveOptions{}, err
	}
	if clock == nil {
		return client.MoveOptions{}, errors.New("move options require a clock")
	}
	now := clock()
	opts := client.MoveOptions{
		AccountID:   resolveMoveAccountID(dir, tokenFlag),
		TrustDomain: trustDomain,
	}
	if rootCAPath != "" {
		rootPEM, err := os.ReadFile(rootCAPath)
		if err != nil {
			return client.MoveOptions{}, fmt.Errorf("read root CA PEM: %w", err)
		}
		opts.RootPEM = rootPEM
		root, err := pki.ParseCertPEM(rootPEM)
		if err != nil {
			return client.MoveOptions{}, fmt.Errorf("parse root CA PEM: %w", err)
		}
		roots := x509.NewCertPool()
		roots.AddCert(root)
		issuers := make([]*x509.Certificate, 0, len(issuerPaths))
		for _, path := range issuerPaths {
			raw, err := os.ReadFile(path)
			if err != nil {
				return client.MoveOptions{}, fmt.Errorf("read CRL issuer PEM: %w", err)
			}
			issuer, err := pki.ParseCertPEM(raw)
			if err != nil {
				return client.MoveOptions{}, fmt.Errorf("parse CRL issuer PEM: %w", err)
			}
			if _, err := issuer.Verify(x509.VerifyOptions{Roots: roots, CurrentTime: now, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageAny}}); err != nil {
				return client.MoveOptions{}, fmt.Errorf("verify CRL issuer: %w", err)
			}
			issuers = append(issuers, issuer)
		}
		state, err := pki.OpenRevocationState(crlStatePath, issuers, clock)
		if err != nil {
			return client.MoveOptions{}, fmt.Errorf("open certificate revocation state: %w", err)
		}
		for _, path := range crlPaths {
			raw, err := os.ReadFile(path)
			if err != nil {
				_ = state.Close()
				return client.MoveOptions{}, fmt.Errorf("read CRL PEM: %w", err)
			}
			if err := state.ApplyPEM(raw); err != nil {
				_ = state.Close()
				return client.MoveOptions{}, fmt.Errorf("apply CRL PEM: %w", err)
			}
		}
		for _, issuer := range issuers {
			if _, ok := state.HighestNumber(issuer.SerialNumber); !ok {
				_ = state.Close()
				return client.MoveOptions{}, fmt.Errorf("certificate revocation state has no current CRL for issuer %s", issuer.SerialNumber.Text(16))
			}
		}
		opts.CertificateRevocations = state.IsRevoked
		opts.CloseCertificateRevocations = state.Close
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

func validateMovePKIFlags(rootCAPath, trustDomain, crlStatePath string, issuerPaths, crlPaths []string) error {
	if (rootCAPath == "") != (trustDomain == "") {
		return errors.New("--root-ca and --trust-domain must be provided together")
	}
	if rootCAPath == "" {
		if crlStatePath != "" || len(issuerPaths) != 0 || len(crlPaths) != 0 {
			return errors.New("certificate revocation flags require --root-ca and --trust-domain")
		}
		return nil
	}
	if crlStatePath == "" || len(issuerPaths) == 0 {
		return errors.New("production node verification requires --crl-state and at least one --crl-issuer")
	}
	return nil
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
