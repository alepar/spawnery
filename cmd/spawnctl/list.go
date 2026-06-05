package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"text/tabwriter"

	"connectrpc.com/connect"
	cpv1 "spawnery/gen/cp/v1"
	"spawnery/gen/cp/v1/cpv1connect"
)

// listSpawns fetches the caller's spawns from the CP (the auth token scopes them to the owner).
func listSpawns(cpAddr, token string) []*cpv1.SpawnSummary {
	client := cpv1connect.NewSpawnServiceClient(h2cClient(), cpAddr,
		connect.WithGRPC(), connect.WithInterceptors(cpBearer(token)))
	resp, err := client.ListSpawns(context.Background(), connect.NewRequest(&cpv1.ListSpawnsRequest{}))
	if err != nil {
		log.Fatalf("list spawns: %v", err)
	}
	return resp.Msg.GetSpawns()
}

func spawnStatus(s *cpv1.SpawnSummary) string {
	return strings.TrimPrefix(s.GetStatus().String(), "SPAWN_STATUS_")
}

func spawnName(s *cpv1.SpawnSummary) string {
	if n := s.GetName(); n != "" {
		return n
	}
	return "-"
}

// runList prints the caller's spawns as a table (id, status, name, app).
func runList(args []string) {
	fs := flag.NewFlagSet("list", flag.ExitOnError)
	cp := fs.String("cp", "http://127.0.0.1:8080", "control-plane address")
	token := fs.String("token", "dev-token", "dev auth token (CP)")
	_ = fs.Parse(args)

	sums := listSpawns(*cp, *token)
	if len(sums) == 0 {
		fmt.Fprintln(os.Stderr, "no spawns")
		return
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
	fmt.Fprintln(w, "SPAWN ID\tSTATUS\tNAME\tAPP")
	for _, s := range sums {
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", s.GetSpawnId(), spawnStatus(s), spawnName(s), s.GetAppId())
	}
	_ = w.Flush()
}

// chooseSpawn lists the caller's spawns and lets them pick one (fzf if present, else a numbered
// menu), returning the chosen spawn id.
func chooseSpawn(cpAddr, token string) string {
	sums := listSpawns(cpAddr, token)
	if len(sums) == 0 {
		log.Fatal("no spawns to choose from")
	}
	// Tab-delimited rows: id is column 1 (returned but hidden from the picker display).
	rows := make([]string, len(sums))
	for i, s := range sums {
		rows[i] = fmt.Sprintf("%s\t%s\t%s\t%s", s.GetSpawnId(), spawnStatus(s), spawnName(s), s.GetAppId())
	}
	if _, err := exec.LookPath("fzf"); err == nil {
		return fzfPick(rows)
	}
	return menuPick(rows)
}

// fzfPick pipes rows to fzf (hiding the id column) and returns the id of the selected row.
func fzfPick(rows []string) string {
	cmd := exec.Command("fzf",
		"--delimiter=\t", "--with-nth=2,3,4",
		"--header=select a spawn (status / name / app)",
		"--height=40%", "--reverse")
	cmd.Stdin = strings.NewReader(strings.Join(rows, "\n") + "\n")
	cmd.Stderr = os.Stderr
	out, err := cmd.Output()
	if err != nil {
		log.Fatalf("fzf: %v", err)
	}
	line := strings.TrimRight(string(out), "\n")
	if line == "" {
		return ""
	}
	return strings.SplitN(line, "\t", 2)[0]
}

// menuPick prints a numbered menu to stderr and reads the choice from stdin.
func menuPick(rows []string) string {
	for i, r := range rows {
		f := strings.SplitN(r, "\t", 4)
		fmt.Fprintf(os.Stderr, "  [%d] %-10s %-20s %s\n", i+1, f[1], f[2], f[3])
	}
	fmt.Fprint(os.Stderr, "select a spawn [1]: ")
	line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
	line = strings.TrimSpace(line)
	n := 1
	if line != "" {
		if v, err := strconv.Atoi(line); err == nil {
			n = v
		}
	}
	if n < 1 || n > len(rows) {
		log.Fatalf("invalid selection %d", n)
	}
	return strings.SplitN(rows[n-1], "\t", 2)[0]
}
