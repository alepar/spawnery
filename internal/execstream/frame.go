// Package execstream is the wire protocol for spawnctl's non-interactive `exec`: a length-prefixed
// stream of typed frames carrying a command's stdout/stderr and, distinct from the output, its exit
// code. The node side (internal/spawnlet) muxes a `docker`/`crictl exec`'s output into this stream
// over a streaming HTTP response; the client side (cmd/spawnctl) demuxes it back to os.Stdout/Stderr
// and exits with the propagated code. Kept dependency-free so the CLI can import it without pulling in
// the runtime/spawnlet stack.
package execstream

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"sync"
)

// FrameType tags each frame's payload.
type FrameType byte

const (
	Stdout FrameType = 1 // payload: raw stdout bytes
	Stderr FrameType = 2 // payload: raw stderr bytes
	Exit   FrameType = 3 // payload: 4-byte big-endian exit code (sent once, last)
	Error  FrameType = 4 // payload: UTF-8 message for a node-side failure after streaming began
)

// maxPayload bounds a single frame so a corrupt length prefix can't drive an unbounded allocation.
const maxPayload = 16 << 20 // 16 MiB

// WriteFrame writes one [type:1][len:4 big-endian][payload] frame.
func WriteFrame(w io.Writer, typ FrameType, payload []byte) error {
	var hdr [5]byte
	hdr[0] = byte(typ)
	binary.BigEndian.PutUint32(hdr[1:], uint32(len(payload)))
	if _, err := w.Write(hdr[:]); err != nil {
		return err
	}
	if len(payload) > 0 {
		if _, err := w.Write(payload); err != nil {
			return err
		}
	}
	return nil
}

// WriteExit writes the terminal exit frame carrying code as a 4-byte big-endian value.
func WriteExit(w io.Writer, code int) error {
	var p [4]byte
	binary.BigEndian.PutUint32(p[:], uint32(int32(code)))
	return WriteFrame(w, Exit, p[:])
}

// ReadFrame reads one frame. It returns io.EOF only at a clean frame boundary (no partial frame).
func ReadFrame(r io.Reader) (FrameType, []byte, error) {
	var hdr [5]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		if err == io.ErrUnexpectedEOF {
			return 0, nil, fmt.Errorf("execstream: truncated frame header: %w", err)
		}
		return 0, nil, err // io.EOF at a clean boundary
	}
	n := binary.BigEndian.Uint32(hdr[1:])
	if n > maxPayload {
		return 0, nil, fmt.Errorf("execstream: frame payload too large: %d bytes", n)
	}
	payload := make([]byte, n)
	if _, err := io.ReadFull(r, payload); err != nil {
		return 0, nil, fmt.Errorf("execstream: truncated frame payload: %w", err)
	}
	return FrameType(hdr[0]), payload, nil
}

// Demux reads frames from r, routing Stdout/Stderr to the given writers, until the stream ends. It
// returns the exit code from the Exit frame. An Error frame, a missing Exit frame, or a read failure
// yields a non-nil error (with a non-zero exit code), so callers can fail the command.
func Demux(r io.Reader, stdout, stderr io.Writer) (int, error) {
	gotExit := false
	exitCode := 0
	for {
		typ, payload, err := ReadFrame(r)
		if errors.Is(err, io.EOF) {
			if !gotExit {
				return 1, errors.New("execstream: stream ended without an exit frame")
			}
			return exitCode, nil
		}
		if err != nil {
			return 1, err
		}
		switch typ {
		case Stdout:
			if _, werr := stdout.Write(payload); werr != nil {
				return 1, werr
			}
		case Stderr:
			if _, werr := stderr.Write(payload); werr != nil {
				return 1, werr
			}
		case Exit:
			if len(payload) != 4 {
				return 1, fmt.Errorf("execstream: exit frame payload = %d bytes, want 4", len(payload))
			}
			exitCode = int(int32(binary.BigEndian.Uint32(payload)))
			gotExit = true
		case Error:
			return 1, fmt.Errorf("execstream: node error: %s", payload)
		default:
			// Unknown frame types are ignored for forward-compatibility.
		}
	}
}

// Muxer serializes concurrent stdout/stderr writes (the docker/crictl CLI writes both at once) into a
// single frame stream, flushing after every frame so output reaches the client live.
type Muxer struct {
	mu    sync.Mutex
	w     io.Writer
	flush func()
}

// NewMuxer returns a Muxer writing to w. flush (may be nil) is called after each frame, e.g. an
// http.Flusher's Flush, so streamed output is not buffered until the response closes.
func NewMuxer(w io.Writer, flush func()) *Muxer {
	return &Muxer{w: w, flush: flush}
}

// Writer returns an io.Writer that emits each Write as one typ frame.
func (m *Muxer) Writer(typ FrameType) io.Writer { return muxWriter{m: m, typ: typ} }

// WriteExit writes the terminal exit frame.
func (m *Muxer) WriteExit(code int) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	err := WriteExit(m.w, code)
	m.doFlush()
	return err
}

// WriteError writes an Error frame carrying msg.
func (m *Muxer) WriteError(msg string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	err := WriteFrame(m.w, Error, []byte(msg))
	m.doFlush()
	return err
}

func (m *Muxer) doFlush() {
	if m.flush != nil {
		m.flush()
	}
}

type muxWriter struct {
	m   *Muxer
	typ FrameType
}

func (w muxWriter) Write(p []byte) (int, error) {
	w.m.mu.Lock()
	defer w.m.mu.Unlock()
	if err := WriteFrame(w.m.w, w.typ, p); err != nil {
		return 0, err
	}
	w.m.doFlush()
	return len(p), nil
}
