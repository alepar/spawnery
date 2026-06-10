package journal

import (
	"sync"
	"testing"
	"time"
)

// fakeClock is an injectable, manually-advanced clock for deterministic timing
// tests — no real sleeps.
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func newFakeClock(t time.Time) *fakeClock { return &fakeClock{now: t} }

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

func TestDebouncerNeverFiredHasNoCooldown(t *testing.T) {
	clk := newFakeClock(time.Unix(0, 0))
	d := NewDebouncer(3, time.Second)
	if got := d.Delay(clk.Now()); got != 0 {
		t.Fatalf("never-fired debouncer should allow immediate fire, got delay %v", got)
	}
	if !d.Ready(clk.Now()) {
		t.Fatal("never-fired debouncer should be Ready")
	}
}

func TestDebouncerAdaptiveCooldownTracksScanDuration(t *testing.T) {
	clk := newFakeClock(time.Unix(100, 0))
	d := NewDebouncer(3, time.Second) // k=3, floor 1s

	// A 2s scan ⇒ cooldown = max(3×2s, 1s) = 6s.
	d.RecordScan(2 * time.Second)
	d.MarkFired(clk.Now())

	// Immediately after firing: must wait the full 6s.
	if got, want := d.Delay(clk.Now()), 6*time.Second; got != want {
		t.Fatalf("delay right after fire: got %v want %v", got, want)
	}
	// 4s later: 2s remaining.
	clk.Advance(4 * time.Second)
	if got, want := d.Delay(clk.Now()), 2*time.Second; got != want {
		t.Fatalf("delay after 4s: got %v want %v", got, want)
	}
	// 2s more: cooldown elapsed, fire allowed.
	clk.Advance(2 * time.Second)
	if got := d.Delay(clk.Now()); got != 0 {
		t.Fatalf("delay after cooldown elapsed: got %v want 0", got)
	}
	if !d.Ready(clk.Now()) {
		t.Fatal("should be Ready after cooldown")
	}
}

func TestDebouncerHonorsMinFloorOnTinyScan(t *testing.T) {
	clk := newFakeClock(time.Unix(0, 0))
	d := NewDebouncer(4, 5*time.Second) // floor 5s dominates a sub-ms scan

	d.RecordScan(1 * time.Millisecond) // 4×1ms = 4ms < 5s floor
	d.MarkFired(clk.Now())

	if got, want := d.Delay(clk.Now()), 5*time.Second; got != want {
		t.Fatalf("delay with tiny scan should honor floor: got %v want %v", got, want)
	}
	clk.Advance(5 * time.Second)
	if !d.Ready(clk.Now()) {
		t.Fatal("should be Ready after the floor elapses")
	}
}

func TestDebouncerLaterScanAdaptsCooldown(t *testing.T) {
	clk := newFakeClock(time.Unix(0, 0))
	d := NewDebouncer(2, time.Second)

	// First a cheap scan, then an expensive one: the expensive scan must
	// lengthen the next cooldown (the scan-bound floor, design §2 roast M9).
	d.RecordScan(500 * time.Millisecond) // 2×500ms = 1s == floor
	d.MarkFired(clk.Now())
	if got, want := d.Delay(clk.Now()), time.Second; got != want {
		t.Fatalf("cheap-scan cooldown: got %v want %v", got, want)
	}

	d.RecordScan(10 * time.Second) // 2×10s = 20s
	d.MarkFired(clk.Now())
	if got, want := d.Delay(clk.Now()), 20*time.Second; got != want {
		t.Fatalf("expensive-scan cooldown: got %v want %v", got, want)
	}
}

func TestNewDebouncerDefaults(t *testing.T) {
	d := NewDebouncer(0, 0)
	if d.K != 2 || d.Min != time.Second {
		t.Fatalf("defaults: got k=%v min=%v want k=2 min=1s", d.K, d.Min)
	}
}
