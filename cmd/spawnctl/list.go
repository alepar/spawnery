package main

import (
	"bufio"
	"context"
	"fmt"
	"log"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"text/tabwriter"

	"connectrpc.com/connect"
	"github.com/urfave/cli/v3"
	cpv1 "spawnery/gen/cp/v1"
	"spawnery/gen/cp/v1/cpv1connect"
)

// listSpawns fetches the caller's spawns from the CP (the token source scopes them to the owner).
func listSpawns(cpAddr string, src *cpTokenSource) []*cpv1.SpawnSummary {
	client := cpv1connect.NewSpawnServiceClient(h2cClient(), cpAddr,
		connect.WithGRPC(), connect.WithInterceptors(tokenSourceInterceptor(src)))
	resp, err := client.ListSpawns(context.Background(), connect.NewRequest(&cpv1.ListSpawnsRequest{}))
	if err != nil {
		log.Fatalf("list spawns: %v", err)
	}
	return resp.Msg.GetSpawns()
}

func spawnStatus(s *cpv1.SpawnSummary) string {
	base := strings.TrimPrefix(s.GetStatus().String(), "SPAWN_STATUS_")
	// Append the current transition phase for in-flight transitions so the user sees real
	// progress rather than a frozen SUSPENDING/RESUMING label (sp-u53.7.2).
	if phase := s.GetTransitionPhase(); phase != "" {
		return base + ":" + phase
	}
	// Append the failed step for ERROR spawns so the table column carries triage context.
	if s.GetStatus() == cpv1.SpawnStatus_SPAWN_STATUS_ERROR && s.GetErrorStep() != "" {
		return base + ":" + s.GetErrorStep()
	}
	return base
}

// provisionProgress returns "[step/total] label" when provisioning is in progress, or "" when
// ProvisionTotal is 0 (no live progress data — CP restart cleared it, or not yet populated).
func provisionProgress(s *cpv1.SpawnSummary) string {
	if s.GetProvisionTotal() == 0 {
		return ""
	}
	return fmt.Sprintf("[%d/%d] %s", s.GetProvisionStep(), s.GetProvisionTotal(), s.GetProvisionStepLabel())
}

// provisionFailure returns a human-readable failure headline for a terminal error status.
// Full detail is included inline; multi-line detail is preserved verbatim.
func provisionFailure(s *cpv1.SpawnSummary) string {
	step := s.GetErrorStep()
	detail := s.GetErrorDetail()
	switch {
	case step != "" && detail != "":
		return "✗ failed at " + step + ": " + detail
	case step != "":
		return "✗ failed at " + step
	case detail != "":
		return "✗ failed: " + detail
	default:
		return "✗ failed"
	}
}

// nextProgressLine computes the next progress line from s and whether it changed from prev.
// Returns ("", false) when ProvisionTotal is 0 (no live progress). Callers use this to dedupe
// poll-loop output: identical consecutive steps are printed only once.
func nextProgressLine(prev string, s *cpv1.SpawnSummary) (line string, changed bool) {
	line = provisionProgress(s)
	return line, line != "" && line != prev
}

func spawnName(s *cpv1.SpawnSummary) string {
	if n := s.GetName(); n != "" {
		return n
	}
	return "-"
}

// listCmd lists the caller's spawns as a table (id, status, name, app).
func listCmd() *cli.Command {
	return &cli.Command{
		Name:    "list",
		Aliases: []string{"ls"},
		Usage:   "list your spawns (id, status, name, app)",
		Flags: []cli.Flag{
			configDirFlag(),
			&cli.StringFlag{Name: "cp", Value: "http://127.0.0.1:8080", Usage: "control-plane address"},
			&cli.StringFlag{Name: "token", Value: "dev-token", Usage: "dev auth token (CP); superseded by stored login credentials"},
		},
		Action: func(_ context.Context, c *cli.Command) error {
			dir, err := resolveDir(c)
			if err != nil {
				return cli.Exit(err.Error(), 1)
			}
			src := buildTokenSource(dir, c.String("token"), h2cClient())
			sums := listSpawns(c.String("cp"), src)
			if len(sums) == 0 {
				fmt.Fprintln(os.Stderr, "no spawns")
				return nil
			}
			w := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
			fmt.Fprintln(w, "SPAWN ID\tSTATUS\tNAME\tAPP")
			for _, s := range sums {
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", s.GetSpawnId(), spawnStatus(s), spawnName(s), s.GetAppId())
			}
			return w.Flush()
		},
	}
}

// chooseSpawn lists the caller's spawns and lets them pick one (fzf if present, else a numbered
// menu), returning the chosen spawn id.
func chooseSpawn(cpAddr string, src *cpTokenSource) string {
	sums := listSpawns(cpAddr, src)
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
