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

	"github.com/urfave/cli/v3"
)

// terminalFlags are shared by attach/exec/shell: -addr is the node terminal endpoint (the mosh data
// plane goes straight there); -cp/-token/-config-dir are used only to list+pick a spawn when -spawn is omitted.
func terminalFlags() []cli.Flag {
	return []cli.Flag{
		configDirFlag(),
		&cli.StringFlag{Name: "addr", Value: "http://127.0.0.1:9092", Usage: "node terminal endpoint"},
		&cli.StringFlag{Name: "spawn", Usage: "spawn id (omit to pick interactively)"},
		&cli.StringFlag{Name: "cp", Value: "http://127.0.0.1:8080", Usage: "control-plane (for listing/picking spawns)"},
		&cli.StringFlag{Name: "token", Value: "dev-token", Usage: "dev auth token (CP); superseded by stored login credentials"},
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
	src := buildTokenSource(dir, c.String("token"), h2cClient())
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
		Name:      "exec",
		Usage:     "run a command in the spawn's container over a terminal",
		ArgsUsage: "[-it] -- <command> [args...]",
		Flags: append(terminalFlags(),
			// -it accepted for docker-like familiarity; the mosh path is always an interactive TTY.
			&cli.BoolFlag{Name: "it", Aliases: []string{"i", "t"}, Usage: "interactive tty (accepted; always on)"}),
		Action: func(_ context.Context, c *cli.Command) error {
			cmd := c.Args().Slice()
			if len(cmd) == 0 {
				return cli.Exit("usage: spawnctl exec [-it] [-spawn <id>] -- <command> [args...]", 2)
			}
			attachToSpawn(c.String("addr"), resolveSpawn(c), cmd)
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
