package spawnlet

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strconv"
	"strings"
)

const execHandshakePrefix = "spawnery-exec-v1 "

type execProcess struct {
	id string
}

func newExecProcess() (execProcess, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return execProcess{}, fmt.Errorf("generate exec process identity: %w", err)
	}
	return execProcess{id: hex.EncodeToString(raw[:])}, nil
}

func validExecProcessID(id string) bool {
	if len(id) != 32 {
		return false
	}
	for _, c := range id {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

func (p execProcess) wrapArgv(inner []string) ([]string, error) {
	if !validExecProcessID(p.id) {
		return nil, errors.New("invalid exec process identity")
	}
	if len(inner) == 0 || inner[0] == "" {
		return nil, errors.New("exec process requires non-empty argv")
	}
	argv := []string{"sh", "-c", execProcessWrapperScript, "spawnery-exec", p.id}
	return append(argv, inner...), nil
}

func execPrefixWithStdin(prefix []string) []string {
	withStdin := append([]string(nil), prefix...)
	return append(withStdin, "-i")
}

type execStreamResult struct {
	code int
	err  error
}

func verifyExecTermination(result execStreamResult, wantCode int) error {
	if result.err != nil {
		return fmt.Errorf("exec termination unconfirmed: %w", result.err)
	}
	if result.code != wantCode {
		return fmt.Errorf("exec termination unconfirmed: runtime exit %d, want %d", result.code, wantCode)
	}
	return nil
}

type execHandshakeResult struct {
	pgid int
	err  error
}

type execHandshakeWriter struct {
	id       string
	dst      io.Writer
	ready    chan execHandshakeResult
	buffer   bytes.Buffer
	resolved bool
}

func newExecHandshakeWriter(id string, dst io.Writer) *execHandshakeWriter {
	return &execHandshakeWriter{id: id, dst: dst, ready: make(chan execHandshakeResult, 1)}
}

func (w *execHandshakeWriter) Write(p []byte) (int, error) {
	if w.resolved {
		return w.dst.Write(p)
	}
	_, _ = w.buffer.Write(p)
	line, rest, found := strings.Cut(w.buffer.String(), "\n")
	if !found {
		if w.buffer.Len() > 256 {
			w.resolve(execHandshakeResult{err: errors.New("exec handshake exceeds 256 bytes")})
		}
		return len(p), nil
	}
	w.resolve(parseExecHandshake(w.id, line))
	if rest != "" {
		_, _ = io.WriteString(w.dst, rest)
	}
	return len(p), nil
}

func (w *execHandshakeWriter) resolve(result execHandshakeResult) {
	if w.resolved {
		return
	}
	w.resolved = true
	w.ready <- result
}

func parseExecHandshake(wantID, line string) execHandshakeResult {
	fields := strings.Fields(line)
	if len(fields) != 3 || fields[0] != strings.TrimSpace(execHandshakePrefix) || fields[1] != wantID {
		return execHandshakeResult{err: fmt.Errorf("invalid exec handshake %q", line)}
	}
	pgid, err := strconv.Atoi(fields[2])
	if err != nil || pgid <= 1 {
		return execHandshakeResult{err: fmt.Errorf("invalid exec process group %q", fields[2])}
	}
	return execHandshakeResult{pgid: pgid}
}

func runExecStreamCancelable(
	ctx context.Context,
	argv []string,
	stdout, stderr io.Writer,
	parseCrictlExit bool,
	process execProcess,
) (int, error) {
	cmd := exec.Command(argv[0], argv[1:]...)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return 1, fmt.Errorf("exec stdin: %w", err)
	}
	cmd.Stdout = stdout
	var errTail *bytes.Buffer
	stderrTarget := stderr
	if parseCrictlExit {
		errTail = &bytes.Buffer{}
		stderrTarget = io.MultiWriter(stderr, errTail)
	}
	handshake := newExecHandshakeWriter(process.id, stderrTarget)
	cmd.Stderr = handshake
	if err := cmd.Start(); err != nil {
		_ = stdin.Close()
		return 1, fmt.Errorf("exec %v: %w", argv, err)
	}
	done := make(chan execStreamResult, 1)
	go func() {
		code, err := waitExecStreamCommand(cmd, argv, errTail, parseCrictlExit)
		done <- execStreamResult{code: code, err: err}
	}()

	var identity execHandshakeResult
	select {
	case identity = <-handshake.ready:
	case result := <-done:
		_ = stdin.Close()
		if result.err != nil {
			return 1, fmt.Errorf("exec handshake missing before runtime exit %d: %w", result.code, result.err)
		}
		return 1, fmt.Errorf("exec handshake missing before runtime exit %d", result.code)
	case <-ctx.Done():
		_ = stdin.Close()
		result := <-done
		return 1, errors.Join(ctx.Err(), verifyExecTermination(result, 130))
	}
	if identity.err != nil {
		_ = stdin.Close()
		<-done
		return 1, identity.err
	}
	control := func(op string) error {
		_, err := fmt.Fprintf(stdin, "%s %s %d\n", op, process.id, identity.pgid)
		return err
	}
	if err := ctx.Err(); err != nil {
		controlErr := control("cancel")
		_ = stdin.Close()
		result := <-done
		return 1, errors.Join(err, controlErr, verifyExecTermination(result, 130))
	}
	if err := control("ack"); err != nil {
		_ = stdin.Close()
		<-done
		return 1, fmt.Errorf("acknowledge exec process identity: %w", err)
	}

	select {
	case result := <-done:
		_ = stdin.Close()
		return result.code, result.err
	case <-ctx.Done():
		select {
		case result := <-done:
			_ = stdin.Close()
			return result.code, result.err
		default:
		}
		controlErr := control("cancel")
		_ = stdin.Close()
		result := <-done
		terminationErr := verifyExecTermination(result, 137)
		if controlErr != nil {
			controlErr = fmt.Errorf("cancel exec process group: %w", controlErr)
		}
		return 1, errors.Join(ctx.Err(), controlErr, terminationErr)
	}
}

func waitExecStreamCommand(cmd *exec.Cmd, argv []string, errTail *bytes.Buffer, parseCrictlExit bool) (int, error) {
	if err := cmd.Wait(); err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			if parseCrictlExit && errTail != nil {
				if code, ok := parseCrictlExitCode(errTail.Bytes()); ok {
					return code, nil
				}
			}
			return exitErr.ExitCode(), nil
		}
		return 1, fmt.Errorf("exec %v: %w", argv, err)
	}
	return 0, nil
}

const execProcessWrapperScript = `
id=$1
shift
exec 3<&0
child=
watcher=
kill_group() {
  if [ -n "$child" ]; then
    kill -KILL "-$child" 2>/dev/null || kill -KILL "$child" 2>/dev/null || true
    wait "$child" 2>/dev/null || true
    child=
  fi
}
cleanup() {
  status=$?
  trap - EXIT HUP INT TERM
  if [ -n "$watcher" ]; then
    kill "$watcher" 2>/dev/null || true
    wait "$watcher" 2>/dev/null || true
  fi
  kill_group
  exit "$status"
}
trap cleanup EXIT HUP INT TERM
setsid sh -c '
  kill -STOP "$$"
  exec "$@"
' spawnery-exec-child "$@" </dev/null 3<&- &
child=$!
case "$child" in ''|*[!0-9]*) exit 125;; esac
if [ "$child" -le 1 ]; then exit 125; fi
while :; do
  if [ ! -r "/proc/$child/stat" ]; then
    wait "$child" 2>/dev/null || true
    child=
    exit 125
  fi
  IFS= read -r stat < "/proc/$child/stat" || exit 125
  rest=${stat#*) }
  set -- $rest
  state=$1
  pgrp=$3
  session=$4
  if { [ "$state" = T ] || [ "$state" = t ]; } && [ "$pgrp" = "$child" ] && [ "$session" = "$child" ]; then
    break
  fi
  if ! kill -0 "$child" 2>/dev/null; then
    wait "$child" 2>/dev/null || true
    child=
    exit 125
  fi
  sleep 0.001
done
printf 'spawnery-exec-v1 %s %s\n' "$id" "$child" >&2
exec 2>/dev/null
IFS= read -r control || control=
case "$control" in
  "ack $id $child") ;;
  *) exit 130;;
esac
(
  IFS= read -r control || control=
  case "$control" in
    "cancel $id $child"|'') ;;
    *) ;;
  esac
  kill -KILL "-$child" 2>/dev/null || true
) <&3 &
watcher=$!
if ! kill -CONT "-$child" 2>/dev/null; then exit 125; fi
wait "$child"
status=$?
child=
kill "$watcher" 2>/dev/null || true
wait "$watcher" 2>/dev/null || true
watcher=
exit "$status"
`
