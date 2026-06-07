//go:build e2e

package cp_test

import (
	"bytes"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/creack/pty"

	"spawnery/internal/spawnlet"
)

// TestCPGooseAcpNoriAttach is the DEFINITIVE end-to-end proof for sp-9xr.12.2:
// spawnctl attach to an acp-mode spawn launches nori (the Rust ACP TUI) inside
// the agent container, and nori JOINS THE SAME shared goose session the web
// ChatView is on, completing a real model turn.
//
// It mirrors the production attach path exactly:
//   - sidecar + agent (goose-acp) pod, acpmux on :7000 (the same harness as
//     TestAcpmuxTwoClientsShareSession). acpmux is the single goose client and
//     the shared-session server.
//   - The node's terminal attach runs `docker exec -it <agent> <inner>` where
//     <inner> = spawnlet.TerminalInnerCmd("acp") = the nori launch argv. We drive
//     that command under a PTY (creack/pty, as mosh's PTY would), answering the
//     terminal's cursor/DA queries so nori's ratatui initializes, and pass the
//     one-shot [PROMPT] positional.
//
// Asserts BOTH halves of the claim:
//  1. nori renders a REAL model response ("banana") — i.e. nori -> baked
//     "spawnery" agent -> acpdial -> acpmux -> goose -> model -> back.
//  2. SHARED SESSION: a separate raw ACP client late-joins acpmux AFTER nori's
//     turn and replays the SAME "banana" turn — proving nori and the web client
//     share one goose conversation (fanout/replay across clients).
//
// Requires Docker + spawnery/agent:dev + spawnery/sidecar:dev + OPENROUTER_API_KEY
// (env or repo-root .env). FAILS loudly (no skips).
func TestCPGooseAcpNoriAttach(t *testing.T) {
	key := loadOpenRouterKey(t)

	// Pod: sidecar (owns netns + injects the real key) + agent (goose-acp/acpmux),
	// identical to TestAcpmuxTwoClientsShareSession / the node's Manager spec.
	const sidecarAddr = "127.0.0.1:8080"
	sidecar := dockerRunD(t, []string{
		"-e", "OPENROUTER_API_KEY=" + key,
		"-e", "SIDECAR_ADDR=" + sidecarAddr,
		"spawnery/sidecar:dev",
	})
	t.Cleanup(func() { _ = exec.Command("docker", "rm", "-f", sidecar).Run() })

	sidecarIP := dockerIP(t, sidecar)
	if sidecarIP == "" {
		t.Fatal("sidecar has no bridge IP (rootless-without-bridge unsupported for this test)")
	}

	agent := dockerRunD(t, []string{
		"--network", "container:" + sidecar,
		"-e", "ACP_LISTEN=tcp://0.0.0.0:7000",
		"-e", "OPENAI_BASE_URL=http://" + sidecarAddr + "/v1",
		"-e", "SPAWN_MODEL=openai/gpt-4o-mini",
		"--entrypoint", "/usr/bin/tini",
		"spawnery/agent:dev", "--", "/entrypoint.sh", "goose-acp",
	})
	t.Cleanup(func() { _ = exec.Command("docker", "rm", "-f", agent).Run() })

	waitAcpmuxReady(t, agent, sidecarIP+":7000")

	// The EXACT inner command the node runs for an acp-mode attach (production
	// path; the tmux/acp/served mapping is unit-tested in spawnlet).
	inner := spawnlet.TerminalInnerCmd("acp")
	if len(inner) == 0 || inner[0] != "nori" {
		t.Fatalf("acp inner cmd is not nori: %v", inner)
	}
	t.Logf("acp-attach inner command: %v", inner)

	// Run it as the node would: docker exec -it <agent> <inner> "<prompt>", under
	// a PTY (mosh supplies the outer PTY in production; creack/pty here). Append
	// the one-shot prompt positional.
	const prompt = "Reply with exactly the single word: banana"
	argv := append([]string{"docker", "exec", "-it", "-e", "TERM=xterm-256color", agent}, inner...)
	argv = append(argv, prompt)

	out := drivePTY(t, argv, 60*time.Second)
	clean := stripANSI(out)
	t.Logf("nori TUI (stripped, tail): %q", tail(clean, 600))

	if !strings.Contains(strings.ToLower(clean), "banana") {
		t.Fatalf("nori did not render the model's 'banana' response.\n--- stripped TUI ---\n%s", tail(clean, 2000))
	}
	t.Log("nori rendered the real model response 'banana' via spawnery agent -> acpdial -> acpmux -> goose")

	// SHARED-SESSION proof: a fresh raw ACP client (stand-in for the web/node
	// pump) connects to acpmux AFTER nori's turn; session/new triggers replay of
	// the buffered history — which must include nori's "banana" turn. This is the
	// live counterpart to the late-join replay in TestAcpmuxTwoClientsShareSession,
	// now where the FIRST client was nori-via-acpdial.
	c := dialRawACP(t, sidecarIP+":7000")
	defer c.Close()
	rawHandshake(t, c) // initialize + session/new -> replay to this late joiner
	replay := rawCollectFanned(t, c, 1, 30*time.Second)
	t.Logf("late-join replay text: %q", replay)
	if !strings.Contains(strings.ToLower(replay), "banana") {
		t.Fatalf("late-joining client did NOT see nori's 'banana' turn replayed; shared session broken (got %q)", replay)
	}
	t.Log("SHARED SESSION verified: a 2nd ACP client late-joined acpmux and replayed nori's turn (nori + web share one goose conversation)")
}

// drivePTY runs argv under a PTY, answering the terminal device-status/attribute
// queries a ratatui TUI emits at startup (ESC[6n -> cursor position; ESC[c ->
// primary device attributes) so nori initializes off a real-looking terminal.
// It returns all bytes captured until the command exits or timeout, then kills
// the command (a one-shot nori turn keeps the TUI open after answering).
func drivePTY(t *testing.T, argv []string, timeout time.Duration) string {
	t.Helper()
	cmd := exec.Command(argv[0], argv[1:]...)
	ptmx, err := pty.Start(cmd)
	if err != nil {
		t.Fatalf("pty start %v: %v", argv, err)
	}
	defer func() { _ = ptmx.Close() }()
	_ = pty.Setsize(ptmx, &pty.Winsize{Rows: 40, Cols: 120})

	var buf bytes.Buffer
	captured := make(chan struct{})
	go func() {
		defer close(captured)
		tmp := make([]byte, 4096)
		for {
			n, e := ptmx.Read(tmp)
			if n > 0 {
				chunk := tmp[:n]
				buf.Write(chunk)
				// Answer cursor-position report (DSR) and primary device attributes (DA).
				if bytes.Contains(chunk, []byte("\x1b[6n")) {
					_, _ = ptmx.Write([]byte("\x1b[1;1R"))
				}
				if bytes.Contains(chunk, []byte("\x1b[c")) {
					_, _ = ptmx.Write([]byte("\x1b[?1;2c"))
				}
			}
			if e != nil {
				return
			}
		}
	}()

	select {
	case <-captured: // command exited and pty drained
	case <-time.After(timeout):
	}
	_ = cmd.Process.Kill()
	_, _ = cmd.Process.Wait()
	_ = ptmx.Close()
	<-captured // let the reader finish draining
	return buf.String()
}

// stripANSI removes the ANSI/OSC escape sequences a TUI emits so plain-text
// assertions (e.g. "banana") work against the rendered frame.
func stripANSI(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c != 0x1b { // not ESC
			if c == '\r' {
				b.WriteByte('\n')
				continue
			}
			b.WriteByte(c)
			continue
		}
		// ESC sequence: skip CSI (ESC[ ... final 0x40-0x7e) and OSC (ESC] ... BEL/ST).
		if i+1 >= len(s) {
			break
		}
		switch s[i+1] {
		case '[':
			i += 2
			for i < len(s) && (s[i] < 0x40 || s[i] > 0x7e) {
				i++
			}
		case ']':
			i += 2
			for i < len(s) && s[i] != 0x07 {
				if s[i] == 0x1b && i+1 < len(s) && s[i+1] == '\\' {
					i++
					break
				}
				i++
			}
		case '(', ')':
			i += 2 // charset designators: ESC( X
		default:
			i++ // other 2-byte escapes
		}
	}
	return b.String()
}

func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[len(s)-n:]
}
