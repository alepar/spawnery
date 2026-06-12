package intent

import (
	"errors"
	"sync"
	"time"
)

// ErrReplay is returned when a jti is already seen or predates the process start [AC1].
var ErrReplay = errors.New("intent: jti already seen or predates process start")

// JTICache enforces jti uniqueness over FreshnessWindow + SkewBudget and refuses intents
// whose issued_at is before processStart (the restart rule [AC1]: cross-restart jti reuse
// cannot be detected after a process restart; refusing anything issued before startup is a
// safe conservative floor that is both sound and implementation-simple — a jti valid at
// shutdown would need to be persisted to survive; refusing it on restart avoids that).
type JTICache struct {
	mu           sync.Mutex
	processStart time.Time
	seen         map[string]time.Time // jti -> issued_at
	now          func() time.Time
}

// NewJTICache creates a cache. processStart is set to now() at construction, establishing the
// restart floor. If now is nil it defaults to time.Now.
func NewJTICache(now func() time.Time) *JTICache {
	if now == nil {
		now = time.Now
	}
	return &JTICache{
		processStart: now(),
		seen:         map[string]time.Time{},
		now:          now,
	}
}

// Admit admits jti for the first time if:
//   - issuedAt is not before processStart (restart rule), AND
//   - jti has not already been seen within the current window.
//
// It also prunes entries older than FreshnessWindow + SkewBudget from now. Returns ErrReplay
// on any rejection.
func (c *JTICache) Admit(jti string, issuedAt time.Time) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if issuedAt.Before(c.processStart) {
		return ErrReplay
	}
	if _, seen := c.seen[jti]; seen {
		return ErrReplay
	}
	c.seen[jti] = issuedAt
	c.pruneOlderThan(c.now().Add(-(FreshnessWindow + SkewBudget)))
	return nil
}

// ProcessStart returns the cache's process-start floor (useful in tests and NACK details).
func (c *JTICache) ProcessStart() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.processStart
}

// pruneOlderThan removes entries whose issued_at is before cutoff. Called under the lock.
func (c *JTICache) pruneOlderThan(cutoff time.Time) {
	for jti, t := range c.seen {
		if t.Before(cutoff) {
			delete(c.seen, jti)
		}
	}
}
