package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"strconv"
)

// terminalFlags are shared by attach/exec/shell: -addr is the node terminal endpoint (mosh data
// plane goes straight there); -cp/-token are used only to list+pick a spawn when -spawn is omitted.
type terminalFlags struct {
	addr  *string
	spawn *string
	cp    *string
	token *string
}

func bindTerminalFlags(fs *flag.FlagSet) terminalFlags {
	return terminalFlags{
		addr:  fs.String("addr", "http://127.0.0.1:9092", "node terminal endpoint"),
		spawn: fs.String("spawn", "", "spawn id (omit to pick interactively)"),
		cp:    fs.String("cp", "http://127.0.0.1:8080", "control-plane address (for listing/picking spawns)"),
		token: fs.String("token", "dev-token", "dev auth token (CP)"),
	}
}

// resolveSpawn returns the chosen spawn id: the -spawn flag if set, else an interactive pick.
func (t terminalFlags) resolveSpawn() string {
	if *t.spawn != "" {
		return *t.spawn
	}
	id := chooseSpawn(*t.cp, *t.token)
	if id == "" {
		log.Fatal("no spawn selected")
	}
	return id
}

func runAttach(args []string) {
	fs := flag.NewFlagSet("attach", flag.ExitOnError)
	tf := bindTerminalFlags(fs)
	_ = fs.Parse(args)
	attachToSpawn(*tf.addr, tf.resolveSpawn(), nil) // nil cmd => opencode TUI
}

func runExec(args []string) {
	fs := flag.NewFlagSet("exec", flag.ExitOnError)
	tf := bindTerminalFlags(fs)
	// -it is accepted for docker-like familiarity; the mosh path is always an interactive TTY.
	_ = fs.Bool("it", false, "interactive tty (accepted for familiarity; always on)")
	_ = fs.Bool("i", false, "interactive (accepted; always on)")
	_ = fs.Bool("t", false, "tty (accepted; always on)")
	_ = fs.Parse(args)
	cmd := fs.Args() // everything after the flags (supports `-- /bin/bash -lc ...`)
	if len(cmd) == 0 {
		log.Fatal("usage: spawnctl exec [-it] [-spawn <id>] -- <command> [args...]")
	}
	attachToSpawn(*tf.addr, tf.resolveSpawn(), cmd)
}

func runShell(args []string) {
	fs := flag.NewFlagSet("shell", flag.ExitOnError)
	tf := bindTerminalFlags(fs)
	_ = fs.Parse(args)
	// A login-ish bash, falling back to sh if bash is absent in the image.
	attachToSpawn(*tf.addr, tf.resolveSpawn(), []string{"/bin/sh", "-c", "exec bash -l 2>/dev/null || exec sh"})
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
