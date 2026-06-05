package main

import (
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

// runTmux implements `spawnctl tmux -spawn <id>`: ask the spawnlet (node) to start a mosh-backed
// terminal session for the spawn (the opencode TUI in tmux, attached to the spawn's shared opencode
// session), then exec mosh-client straight to the node over UDP. Reattaches if a session exists.
func runTmux(args []string) {
	fs := flag.NewFlagSet("tmux", flag.ExitOnError)
	addr := fs.String("addr", "http://127.0.0.1:9090", "spawnlet (node) address")
	spawn := fs.String("spawn", "", "spawn id to attach")
	_ = fs.Parse(args)
	if *spawn == "" {
		log.Fatal("usage: spawnctl tmux -spawn <id> [-addr http://node:9090]")
	}

	endpoint := *addr + "/terminal?spawn=" + url.QueryEscape(*spawn)
	resp, err := http.Post(endpoint, "application/json", nil)
	if err != nil {
		log.Fatalf("tmux: contacting spawnlet: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		log.Fatalf("tmux: spawnlet returned %s: %s", resp.Status, b)
	}
	var ts struct {
		Host string `json:"Host"`
		Port int    `json:"Port"`
		Key  string `json:"Key"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&ts); err != nil {
		log.Fatalf("tmux: decoding connect info: %v", err)
	}

	host := ts.Host
	if host == "" { // mosh auto-detected; fall back to the spawnlet's host
		if pu, e := url.Parse(*addr); e == nil {
			host = pu.Hostname()
		}
	}
	fmt.Fprintf(os.Stderr, "spawnctl: attaching mosh to %s:%d (spawn %s)\n", host, ts.Port, *spawn)

	cmd := exec.Command("mosh-client", host, strconv.Itoa(ts.Port))
	cmd.Env = append(os.Environ(), "MOSH_KEY="+ts.Key)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		log.Fatalf("tmux: mosh-client: %v", err)
	}
}
