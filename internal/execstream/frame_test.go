package execstream

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"
)

func TestFrameRoundTrip(t *testing.T) {
	cases := []struct {
		typ     FrameType
		payload []byte
	}{
		{Stdout, []byte("hello")},
		{Stderr, []byte("")},                      // empty payload is valid
		{Stdout, bytes.Repeat([]byte("x"), 70000)}, // larger than any single read buffer
		{Exit, []byte{0, 0, 0, 7}},
	}
	var buf bytes.Buffer
	for _, c := range cases {
		if err := WriteFrame(&buf, c.typ, c.payload); err != nil {
			t.Fatalf("WriteFrame(%d): %v", c.typ, err)
		}
	}
	for i, c := range cases {
		typ, payload, err := ReadFrame(&buf)
		if err != nil {
			t.Fatalf("ReadFrame #%d: %v", i, err)
		}
		if typ != c.typ {
			t.Fatalf("frame #%d type = %d, want %d", i, typ, c.typ)
		}
		if !bytes.Equal(payload, c.payload) {
			t.Fatalf("frame #%d payload len = %d, want %d", i, len(payload), len(c.payload))
		}
	}
	if _, _, err := ReadFrame(&buf); !errors.Is(err, io.EOF) {
		t.Fatalf("ReadFrame at end = %v, want io.EOF", err)
	}
}

func TestDemuxRoutesAndReturnsExit(t *testing.T) {
	var wire bytes.Buffer
	_ = WriteFrame(&wire, Stdout, []byte("out-chunk-1"))
	_ = WriteFrame(&wire, Stderr, []byte("err-chunk"))
	_ = WriteFrame(&wire, Stdout, []byte("out-chunk-2"))
	if err := WriteExit(&wire, 7); err != nil {
		t.Fatalf("WriteExit: %v", err)
	}

	var stdout, stderr bytes.Buffer
	code, err := Demux(&wire, &stdout, &stderr)
	if err != nil {
		t.Fatalf("Demux: %v", err)
	}
	if code != 7 {
		t.Fatalf("exit code = %d, want 7", code)
	}
	if stdout.String() != "out-chunk-1out-chunk-2" {
		t.Fatalf("stdout = %q", stdout.String())
	}
	if stderr.String() != "err-chunk" {
		t.Fatalf("stderr = %q", stderr.String())
	}
}

func TestDemuxErrorFrame(t *testing.T) {
	var wire bytes.Buffer
	_ = WriteFrame(&wire, Stdout, []byte("partial"))
	_ = WriteFrame(&wire, Error, []byte("exec: container vanished"))

	var stdout, stderr bytes.Buffer
	code, err := Demux(&wire, &stdout, &stderr)
	if err == nil {
		t.Fatalf("Demux returned nil error, want the error-frame message")
	}
	if !strings.Contains(err.Error(), "container vanished") {
		t.Fatalf("error = %v, want it to contain the frame message", err)
	}
	if code == 0 {
		t.Fatalf("exit code = 0 on error frame, want non-zero")
	}
	if stdout.String() != "partial" {
		t.Fatalf("stdout = %q, want the bytes streamed before the error", stdout.String())
	}
}

func TestDemuxMissingExitIsError(t *testing.T) {
	var wire bytes.Buffer
	_ = WriteFrame(&wire, Stdout, []byte("done but no exit frame"))

	_, err := Demux(&wire, io.Discard, io.Discard)
	if err == nil {
		t.Fatalf("Demux returned nil error for a stream that ended without an exit frame")
	}
}

func TestMuxerWritesDemuxableFrames(t *testing.T) {
	var wire bytes.Buffer
	flushed := 0
	m := NewMuxer(&wire, func() { flushed++ })
	if _, err := io.WriteString(m.Writer(Stdout), "a"); err != nil {
		t.Fatalf("stdout write: %v", err)
	}
	if _, err := io.WriteString(m.Writer(Stderr), "b"); err != nil {
		t.Fatalf("stderr write: %v", err)
	}
	if err := m.WriteExit(5); err != nil {
		t.Fatalf("WriteExit: %v", err)
	}
	if flushed == 0 {
		t.Fatalf("expected the muxer to flush after frames")
	}

	var stdout, stderr bytes.Buffer
	code, err := Demux(&wire, &stdout, &stderr)
	if err != nil {
		t.Fatalf("Demux: %v", err)
	}
	if code != 5 || stdout.String() != "a" || stderr.String() != "b" {
		t.Fatalf("got code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
}
