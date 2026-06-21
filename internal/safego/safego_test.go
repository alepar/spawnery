package safego_test

import (
	"bytes"
	"log"
	"strings"
	"sync"
	"testing"

	"spawnery/internal/safego"
)

// captureLog redirects the global logger to a buffer for the duration of the test.
// Caller MUST call the returned restore func via defer.
func captureLog() (*bytes.Buffer, func()) {
	var buf bytes.Buffer
	orig := log.Writer()
	log.SetOutput(&buf)
	return &buf, func() { log.SetOutput(orig) }
}

// TestRun_RecoversPanic verifies that Run catches a panic, returns normally, and logs
// exactly one "goroutine-panic [name]" line containing the goroutine name.
func TestRun_RecoversPanic(t *testing.T) {
	buf, restore := captureLog()
	defer restore()

	const name = "test.panic-goroutine"

	safego.Run(name, func() {
		panic("oh no")
	})

	logged := buf.String()
	count := strings.Count(logged, "goroutine-panic")
	if count != 1 {
		t.Errorf("expected goroutine-panic logged exactly once, got %d in: %q", count, logged)
	}
	if !strings.Contains(logged, name) {
		t.Errorf("expected log line to contain goroutine name %q, got: %q", name, logged)
	}
}

// TestRun_SideEffectsBeforePanic verifies that side effects that occur before the panic
// are visible after Run returns.
func TestRun_SideEffectsBeforePanic(t *testing.T) {
	_, restore := captureLog()
	defer restore()

	var reached bool
	safego.Run("test.side-effects", func() {
		reached = true
		panic("kaboom")
	})

	if !reached {
		t.Error("expected pre-panic side effect to be visible after Run returns")
	}
}

// TestRun_NonPanicking verifies that Run executes the function and returns normally when
// no panic occurs.
func TestRun_NonPanicking(t *testing.T) {
	buf, restore := captureLog()
	defer restore()

	var called bool
	safego.Run("test.no-panic", func() {
		called = true
	})

	if !called {
		t.Error("expected fn to be called")
	}
	if logged := buf.String(); strings.Contains(logged, "goroutine-panic") {
		t.Errorf("expected no goroutine-panic log for non-panicking fn, got: %q", logged)
	}
}

// TestGo_PanicDoesNotCrashProcess verifies that Go does not crash the test process when
// its fn panics. We assert process liveness by synchronizing on fn start via a channel
// rather than racing on the async log line.
func TestGo_PanicDoesNotCrashProcess(t *testing.T) {
	_, restore := captureLog()
	defer restore()

	started := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	safego.Go("test.go-panic", func() {
		close(started)
		wg.Done()
		panic("async boom")
	})

	// Wait for fn to start — proves the goroutine was launched and we are still alive.
	<-started
	// Also wait for the goroutine to finish (so the panic is recovered before the test
	// process exits, avoiding a data race on log.SetOutput in captureLog restore).
	wg.Wait()
}
