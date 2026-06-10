package main

import (
	"io"
	"log"
	"net"
	"os"
)

// acpdial is the inverse of acpexec: a tiny stdio<->TCP ACP shim. nori (the Rust
// ACP TUI) spawns its configured agent as a CHILD over stdio — it cannot dial an
// external ACP server. So we configure nori's custom "spawnery" agent to launch
// acpdial, which dials acpmux (127.0.0.1:7000 inside the agent container) and
// relays nori's stdio ACP JSON-RPC to it. nori thinks it spawned an agent; it's
// really now a SECOND ACP client of acpmux -> the SAME shared goose session the
// web ChatView is on (sp-9xr.12.2).
//
// Usage: acpdial [addr]
// addr resolution: os.Args[1] > $ACP_DIAL > default 127.0.0.1:7000.
func main() {
	log.SetPrefix("acpdial: ")
	log.SetFlags(0)

	addr := dialAddr(os.Args[1:])
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		log.Fatalf("dial %s: %v", addr, err)
	}
	if err := run(conn, os.Stdin, os.Stdout); err != nil {
		log.Fatalf("relay: %v", err)
	}
}

// dialAddr resolves the target address: first positional arg > $ACP_DIAL env >
// default 127.0.0.1:7000 (where acpmux listens inside the agent container).
func dialAddr(args []string) string {
	if len(args) > 0 && args[0] != "" {
		return args[0]
	}
	if a := os.Getenv("ACP_DIAL"); a != "" {
		return a
	}
	return "127.0.0.1:7000"
}

// run bidirectionally copies in->conn and conn->out. It returns when the
// SERVER (conn->out) side finishes — that is the meaningful end of the ACP
// session (acpmux closed, or goose died). It mirrors the deadlock-safe pattern
// from acpexec:
//
//   - When `in` (nori's stdin) EOFs, we half-close the write side of the conn
//     (CloseWrite) to propagate EOF to acpmux WITHOUT killing the response
//     stream, so any in-flight responses still reach `out`. If the conn doesn't
//     support half-close, we fall back to a full Close (which also unblocks the
//     conn->out copy via EOF).
//   - When the conn->out copy finishes (server closed), we Close the whole conn
//     to unblock the in->conn copy that may still be reading stdin — without
//     this it would block forever on in.Read(), hanging the process and leaking
//     a goroutine.
//
// The result channels are buffered so neither goroutine ever blocks publishing
// its result, even after run has returned.
func run(conn net.Conn, in io.Reader, out io.Writer) error {
	type halfCloser interface{ CloseWrite() error }
	// in -> conn (nori's requests to acpmux).
	go func() {
		_, _ = io.Copy(conn, in)
		// stdin EOF: half-close the conn write side so acpmux sees EOF but can
		// still send remaining responses. Fall back to full Close otherwise.
		if hc, ok := conn.(halfCloser); ok {
			_ = hc.CloseWrite()
		} else {
			_ = conn.Close()
		}
	}()
	// conn -> out (acpmux's responses/notifications to nori). This is the
	// MEANINGFUL end of the ACP session; we wait on it specifically.
	srvDone := make(chan error, 1)
	go func() {
		_, err := io.Copy(out, conn)
		// Server side done: Close the whole conn to unblock the in->conn copy
		// still reading stdin (deadlock-safe — no goroutine leak, no hang).
		_ = conn.Close()
		srvDone <- err
	}()
	err := <-srvDone
	if err == io.EOF {
		return nil
	}
	return err
}
