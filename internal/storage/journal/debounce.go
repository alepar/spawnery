package journal

import (
	"sync"
	"time"
)

// Clock is the injectable time source used by the debounce timing logic so unit
// tests can drive time deterministically (no real sleeps).
type Clock interface {
	Now() time.Time
}

// systemClock is the production Clock.
type systemClock struct{}

func (systemClock) Now() time.Time { return time.Now() }

// Debouncer implements the adaptive snapshot debounce (design §2, roast M9):
// the next snapshot for a mount is never scheduled sooner than k× the last
// measured scan duration. Because Kopia has no dirty-path API — every snapshot
// re-walks the tree — the floor must track measured scan cost, not a fixed
// interval. A minimum floor (Min) guards against thrash on a tiny tree where the
// scan is sub-millisecond.
//
// Debouncer is pure timing logic: it holds no timers and starts no goroutines,
// so its decisions are fully deterministic under an injected Clock. The runner
// loop that actually fires snapshots lives in the SerialQueue + Manager.
type Debouncer struct {
	K   float64       // cooldown multiplier (k×); typical 2–4
	Min time.Duration // floor cooldown regardless of scan cost

	mu       sync.Mutex
	lastScan time.Duration // most recent measured scan duration
	lastFire time.Time     // when the last snapshot fired (zero = never)
}

// NewDebouncer builds a Debouncer. k<=0 defaults to 2; min<=0 defaults to 1s.
func NewDebouncer(k float64, min time.Duration) *Debouncer {
	if k <= 0 {
		k = 2
	}
	if min <= 0 {
		min = time.Second
	}
	return &Debouncer{K: k, Min: min}
}

// cooldown is the minimum gap required after the last fire: max(k×lastScan, Min).
func (d *Debouncer) cooldown() time.Duration {
	cd := time.Duration(float64(d.lastScan) * d.K)
	if cd < d.Min {
		cd = d.Min
	}
	return cd
}

// RecordScan records the duration of the most recent scan/snapshot, adapting the
// cooldown for the next one.
func (d *Debouncer) RecordScan(dur time.Duration) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if dur > 0 {
		d.lastScan = dur
	}
}

// MarkFired records that a snapshot fired at t, starting the cooldown window.
func (d *Debouncer) MarkFired(t time.Time) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.lastFire = t
}

// Delay returns how long to wait from now before the next snapshot may fire.
// Zero means "fire now" (cooldown already elapsed, or nothing has fired yet).
func (d *Debouncer) Delay(now time.Time) time.Duration {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.lastFire.IsZero() {
		return 0 // never fired — no cooldown to honor
	}
	earliest := d.lastFire.Add(d.cooldown())
	if !now.Before(earliest) {
		return 0
	}
	return earliest.Sub(now)
}

// Ready reports whether a snapshot may fire at now (Delay == 0).
func (d *Debouncer) Ready(now time.Time) bool { return d.Delay(now) == 0 }
