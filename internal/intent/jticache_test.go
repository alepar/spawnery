package intent_test

import (
	"testing"
	"time"

	"spawnery/internal/intent"
)

func TestJTICacheAdmitAndDeduplicate(t *testing.T) {
	// processStart is T0; current "now" is T0+10s; issuedAt is T0+5s (after start, within window).
	processStart := time.Unix(1_770_000_000, 0)
	current := processStart
	clock := func() time.Time { return current }
	c := intent.NewJTICache(clock) // processStart = processStart (T0)

	// Advance the clock to 10 seconds after start.
	current = processStart.Add(10 * time.Second)
	issuedAt := processStart.Add(5 * time.Second) // T0+5s, valid

	// First admission must succeed.
	if err := c.Admit("jti-1", issuedAt); err != nil {
		t.Fatalf("first Admit: %v", err)
	}
	// Second admission of same jti must return ErrReplay.
	if err := c.Admit("jti-1", issuedAt); err != intent.ErrReplay {
		t.Fatalf("duplicate Admit: want ErrReplay, got %v", err)
	}
	// A different jti must be admitted.
	if err := c.Admit("jti-2", issuedAt); err != nil {
		t.Fatalf("different jti Admit: %v", err)
	}
}

// Cross-restart rule [AC1]: an intent whose issued_at predates process start is rejected.
func TestJTICacheRejectsPreStartIssuedAt(t *testing.T) {
	start := time.Unix(1_770_000_000, 0)
	clock := func() time.Time { return start }
	c := intent.NewJTICache(clock) // processStart = start

	pre := start.Add(-1 * time.Second)
	if err := c.Admit("jti-pre", pre); err != intent.ErrReplay {
		t.Fatalf("pre-start jti: want ErrReplay, got %v", err)
	}
	// Issued exactly at start must be admitted.
	if err := c.Admit("jti-at", start); err != nil {
		t.Fatalf("at-start jti: %v", err)
	}
}

// Pruning: entries older than FreshnessWindow + SkewBudget must be pruned so stale jti
// slots are freed (the window is the only required dedup span).
func TestJTICachePrunesOldEntries(t *testing.T) {
	base := time.Unix(1_770_000_000, 0)
	current := base
	clock := func() time.Time { return current }
	c := intent.NewJTICache(clock) // processStart = base

	// Admit a jti just after process start.
	issuedAt := base.Add(time.Second)
	if err := c.Admit("jti-old", issuedAt); err != nil {
		t.Fatalf("initial Admit: %v", err)
	}
	// Verify it blocks re-admission immediately.
	if err := c.Admit("jti-old", issuedAt); err != intent.ErrReplay {
		t.Fatalf("immediate re-admit: want ErrReplay, got %v", err)
	}

	// Advance time past the full window; a new admission should trigger pruning.
	current = base.Add(intent.FreshnessWindow + intent.SkewBudget + 2*time.Second)
	if err := c.Admit("jti-new", current); err != nil {
		t.Fatalf("new Admit after advance: %v", err)
	}

	// jti-old should now be pruned (its issuedAt is outside the window): re-admit must succeed.
	if err := c.Admit("jti-old", current); err != nil {
		t.Fatalf("re-admit of pruned jti: want nil, got %v", err)
	}
}

// The process-start floor has no skew exemption: a jti 1 nanosecond before start is refused.
func TestJTICacheRestartFloorAbsolute(t *testing.T) {
	start := time.Unix(1_770_000_000, 0)
	clock := func() time.Time { return start }
	c := intent.NewJTICache(clock)

	almostStart := start.Add(-time.Nanosecond)
	if err := c.Admit("jti-ns", almostStart); err != intent.ErrReplay {
		t.Fatalf("1ns before start: want ErrReplay, got %v", err)
	}
}
