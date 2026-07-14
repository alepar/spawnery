package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/urfave/cli/v3"
)

// terminalFlags are shared by the standalone/development attach and shell paths.
func terminalFlags() []cli.Flag {
	return []cli.Flag{
		configDirFlag(),
		&cli.StringFlag{Name: "addr", Value: "http://127.0.0.1:9092", Usage: "node terminal endpoint"},
		&cli.StringFlag{Name: "spawn", Usage: "spawn id (omit to pick interactively)"},
		&cli.StringFlag{Name: "cp", Value: "http://127.0.0.1:8080", Usage: "control-plane (for listing/picking spawns)"},
		&cli.StringFlag{Name: "token", Value: "dev-token", Usage: "dev auth token (CP); superseded by stored login credentials"},
	}
}

func execFlags() []cli.Flag {
	return []cli.Flag{
		configDirFlag(),
		&cli.StringFlag{Name: "spawn", Usage: "spawn id (omit to pick interactively)"},
		&cli.StringFlag{Name: "cp", Value: "http://127.0.0.1:8080", Usage: "control-plane address"},
		&cli.StringFlag{Name: "token", Value: "dev-token", Usage: "dev auth token (CP); production exec requires stored login credentials"},
		&cli.StringFlag{Name: "root-ca", Usage: "path to the pinned Root CA PEM for node verification"},
		&cli.StringFlag{Name: "trust-domain", Usage: "expected SPIFFE trust domain"},
		&cli.StringFlag{Name: "crl-state", Usage: "persistent certificate revocation checkpoint"},
		&cli.StringSliceFlag{Name: "crl-issuer", Usage: "trusted issuing-intermediate PEM (repeatable)"},
		&cli.StringSliceFlag{Name: "crl", Usage: "current signed CRL PEM (repeatable)"},
	}
}

// resolveSpawn returns the chosen spawn id: the -spawn flag if set, else an interactive pick via the CP.
func resolveSpawn(c *cli.Command) string {
	if s := c.String("spawn"); s != "" {
		return s
	}
	dir, err := resolveDir(c)
	if err != nil {
		log.Fatalf("resolveSpawn: config dir: %v", err)
	}
	src := buildTokenSource(dir, c.String("token"), connectClient())
	id := chooseSpawn(c.String("cp"), src)
	if id == "" {
		log.Fatal("no spawn selected")
	}
	return id
}

func attachCmd() *cli.Command {
	return &cli.Command{
		Name:  "attach",
		Usage: "attach the opencode TUI to a running spawn (via mosh)",
		Flags: terminalFlags(),
		Action: func(_ context.Context, c *cli.Command) error {
			attachToSpawn(c.String("addr"), resolveSpawn(c), nil) // nil cmd => opencode TUI
			return nil
		},
	}
}

func execCmd() *cli.Command {
	return &cli.Command{
		Name:  "exec",
		Usage: "run a non-interactive command in the spawn's container (streams output, propagates exit code)",
		Description: "Runs <command> non-interactively in the spawn's agent container — e.g. a test command — " +
			"streaming its stdout/stderr live and exiting with the command's own exit code. No TTY/mosh required, " +
			"so it is pipeable and scriptable. For an interactive shell use `spawnctl shell`; for the TUI use " +
			"`spawnctl attach`.",
		ArgsUsage: "-- <command> [args...]",
		Flags: append(execFlags(),
			// -it is a no-op kept for docker muscle-memory: exec is always non-interactive.
			&cli.BoolFlag{Name: "it", Aliases: []string{"i", "t"}, Usage: "no-op (exec is always non-interactive; use `spawnctl shell` for a TTY)"}),
		Action: func(ctx context.Context, c *cli.Command) error {
			cmd := c.Args().Slice()
			if len(cmd) == 0 {
				return cli.Exit("usage: spawnctl exec [-spawn <id>] -- <command> [args...]", 2)
			}
			dir, err := resolveDir(c)
			if err != nil {
				return cli.Exit(err.Error(), 1)
			}
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
			source := buildTokenSource(dir, c.String("token"), connectClient())
			sdk, err := buildAuthenticatedExecClient(c.String("cp"), source, trust)
			if err != nil {
				return cli.Exit(err.Error(), 1)
			}
			code, err := runExec(ctx, sdk, resolveSpawn(c), cmd, c.Writer, c.ErrWriter)
			if err != nil {
				return cli.Exit(fmt.Sprintf("spawnctl exec: %v", err), 1)
			}
			if code != 0 {
				return cli.Exit("", code)
			}
			return nil
		},
	}
}

func shellCmd() *cli.Command {
	return &cli.Command{
		Name:  "shell",
		Usage: "open a shell in the spawn's container (= exec bash)",
		Flags: terminalFlags(),
		Action: func(_ context.Context, c *cli.Command) error {
			// Interactive login bash, falling back to sh if bash is absent. NOTE: do NOT redirect the
			// exec'd shell's stderr — bash is only interactive when BOTH stdin and stderr are TTYs, so
			// `2>/dev/null` would make it non-interactive (no PS1/echo) and swallow errors. The redirect
			// here is only on `command -v` (its probe output), not on the shell we exec.
			attachToSpawn(c.String("addr"), resolveSpawn(c),
				[]string{"/bin/sh", "-c", "command -v bash >/dev/null 2>&1 && exec bash -il || exec sh -i"})
			return nil
		},
	}
}

// attachToSpawn asks the node to start a mosh-backed terminal session running cmd (nil => opencode
// TUI) in the spawn's container, then execs mosh-client straight to the node.
func attachToSpawn(addr, spawn string, cmd []string) {
	var body io.Reader
	if len(cmd) > 0 {
		b, _ := json.Marshal(map[string]any{"cmd": cmd})
		body = bytes.NewReader(b)
	}
	endpoint := addr + "/terminal?spawn=" + url.QueryEscape(spawn)
	resp, err := http.Post(endpoint, "application/json", body)
	if err != nil {
		log.Fatalf("contacting node: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		log.Fatalf("node returned %s: %s", resp.Status, b)
	}
	var ts struct {
		Host string `json:"Host"`
		Port int    `json:"Port"`
		Key  string `json:"Key"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&ts); err != nil {
		log.Fatalf("decoding connect info: %v", err)
	}
	host := ts.Host
	if host == "" {
		if pu, e := url.Parse(addr); e == nil {
			host = pu.Hostname()
		}
	}
	fmt.Fprintf(os.Stderr, "spawnctl: attaching mosh to %s:%d (spawn %s)\n", host, ts.Port, spawn)
	c := exec.Command("mosh-client", host, strconv.Itoa(ts.Port))
	c.Env = append(os.Environ(), "MOSH_KEY="+ts.Key)
	c.Stdin, c.Stdout, c.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := c.Run(); err != nil {
		log.Fatalf("mosh-client: %v", err)
	}
}
