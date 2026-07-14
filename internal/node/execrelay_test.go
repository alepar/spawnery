package node

import (
	"bytes"
	"context"
	"errors"
	"io"
	"sync"
	"testing"
	"time"

	"spawnery/internal/execstream"
)

type scriptedExecRunner struct {
	stdout string
	stderr string
	code   int
	wait   bool
	done   chan struct{}
}

func (r *scriptedExecRunner) ExecStream(ctx context.Context, _ string, _ []string, stdout, stderr io.Writer) (int, error) {
	if r.wait {
		<-ctx.Done()
		if r.done != nil {
			close(r.done)
		}
		return 1, ctx.Err()
	}
	_, _ = io.WriteString(stdout, r.stdout)
	_, _ = io.WriteString(stderr, r.stderr)
	return r.code, nil
}

type relayCapture struct {
	mu     sync.Mutex
	bytes  bytes.Buffer
	events []string
}

func (c *relayCapture) send(p []byte) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.events = append(c.events, "data")
	_, _ = c.bytes.Write(p)
	return nil
}

func (c *relayCapture) close() {
	c.mu.Lock()
	c.events = append(c.events, "close")
	c.mu.Unlock()
}

func (c *relayCapture) snapshot() ([]byte, []string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return bytes.Clone(c.bytes.Bytes()), append([]string(nil), c.events...)
}

func TestExecRelayFramesOutputAndClosesAfterExit(t *testing.T) {
	capture := &relayCapture{}
	relay := newExecRelay(context.Background(), &scriptedExecRunner{
		stdout: "out\n", stderr: "err\n", code: 23,
	}, "sp-1", []string{"sh", "-lc", "exit 23"}, capture.send, capture.close)
	<-relay.Done()
	wire, events := capture.snapshot()
	var stdout, stderr bytes.Buffer
	code, err := execstream.Demux(bytes.NewReader(wire), &stdout, &stderr)
	if err != nil {
		t.Fatal(err)
	}
	if stdout.String() != "out\n" || stderr.String() != "err\n" || code != 23 {
		t.Fatalf("demux = stdout %q stderr %q code %d", stdout.String(), stderr.String(), code)
	}
	if len(events) == 0 || events[len(events)-1] != "close" {
		t.Fatalf("events = %v, want close last after exit bytes", events)
	}
}

func TestExecRelayCancellationStopsOnlyAddressedRunner(t *testing.T) {
	firstDone := make(chan struct{})
	secondDone := make(chan struct{})
	first := newExecRelay(context.Background(), &scriptedExecRunner{wait: true, done: firstDone}, "sp-1", []string{"first"}, func([]byte) error { return nil }, func() {})
	second := newExecRelay(context.Background(), &scriptedExecRunner{wait: true, done: secondDone}, "sp-1", []string{"second"}, func([]byte) error { return nil }, func() {})
	first.Cancel()
	select {
	case <-firstDone:
	case <-time.After(time.Second):
		t.Fatal("addressed exec did not observe cancellation")
	}
	select {
	case <-secondDone:
		t.Fatal("cancelling one exec cancelled another session")
	default:
	}
	second.Cancel()
	<-second.Done()
}

func TestExecRelayLaunchErrorIsFramedBeforeClose(t *testing.T) {
	capture := &relayCapture{}
	runner := execRunnerFunc(func(context.Context, string, []string, io.Writer, io.Writer) (int, error) {
		return 1, errors.New("launch failed")
	})
	relay := newExecRelay(context.Background(), runner, "sp-1", []string{"missing"}, capture.send, capture.close)
	<-relay.Done()
	wire, events := capture.snapshot()
	typ, payload, err := execstream.ReadFrame(bytes.NewReader(wire))
	if err != nil || typ != execstream.Error || !bytes.Contains(payload, []byte("launch failed")) {
		t.Fatalf("first frame = %v %q %v", typ, payload, err)
	}
	if events[len(events)-1] != "close" {
		t.Fatalf("events = %v", events)
	}
}
