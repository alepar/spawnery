package node

import (
	"bytes"
	"sync"
	"time"
)

type sessionAuthKey struct {
	spawnID   string
	sessionID string
	clientID  string
}

type sessionAuthRecord struct {
	accountID      string
	tokenID        string
	expiresAt      time.Time
	sessionKeyHash []byte
	generation     uint64
	nodeID         string
}

type liveSessionAuth struct {
	record sessionAuthRecord
	timer  sessionAuthTimer
	close  func(string)
}

type sessionAuthTimer interface{ Stop() bool }

type sessionAuthRegistry struct {
	mu      sync.Mutex
	records map[sessionAuthKey]*liveSessionAuth
	now     func() time.Time
	after   func(time.Duration, func()) sessionAuthTimer
}

func newSessionAuthRegistry() *sessionAuthRegistry {
	return newSessionAuthRegistryWithClock(time.Now, func(delay time.Duration, callback func()) sessionAuthTimer {
		return time.AfterFunc(delay, callback)
	})
}

func newSessionAuthRegistryWithClock(now func() time.Time, after func(time.Duration, func()) sessionAuthTimer) *sessionAuthRegistry {
	return &sessionAuthRegistry{records: make(map[sessionAuthKey]*liveSessionAuth), now: now, after: after}
}

func (r *sessionAuthRegistry) register(key sessionAuthKey, record sessionAuthRecord, closeFn func(string)) {
	r.mu.Lock()
	if old := r.records[key]; old != nil {
		old.timer.Stop()
	}
	live := &liveSessionAuth{record: cloneSessionAuthRecord(record), close: closeFn}
	r.records[key] = live
	live.timer = r.after(record.expiresAt.Sub(r.now()), func() { r.expire(key, record.tokenID) })
	r.mu.Unlock()
}

func (r *sessionAuthRegistry) replace(key sessionAuthKey, next sessionAuthRecord, liveOwner string) bool {
	r.mu.Lock()
	current := r.records[key]
	now := r.now()
	valid := current != nil && now.Before(current.record.expiresAt) && now.Before(next.expiresAt) &&
		current.record.accountID == next.accountID && next.accountID == liveOwner &&
		bytes.Equal(current.record.sessionKeyHash, next.sessionKeyHash) &&
		current.record.generation == next.generation && current.record.nodeID == next.nodeID
	if !valid {
		closeFn := r.removeLocked(key)
		r.mu.Unlock()
		if closeFn != nil {
			closeFn("session reauthentication rejected")
		}
		return false
	}
	current.timer.Stop()
	current.record = cloneSessionAuthRecord(next)
	current.timer = r.after(next.expiresAt.Sub(now), func() { r.expire(key, next.tokenID) })
	r.mu.Unlock()
	return true
}

func (r *sessionAuthRegistry) close(key sessionAuthKey, reason string) {
	r.mu.Lock()
	closeFn := r.removeLocked(key)
	r.mu.Unlock()
	if closeFn != nil {
		closeFn(reason)
	}
}

func (r *sessionAuthRegistry) remove(key sessionAuthKey) {
	r.mu.Lock()
	_ = r.removeLocked(key)
	r.mu.Unlock()
}

func (r *sessionAuthRegistry) removeSpawn(spawnID string) {
	r.mu.Lock()
	for key := range r.records {
		if key.spawnID == spawnID {
			_ = r.removeLocked(key)
		}
	}
	r.mu.Unlock()
}

func (r *sessionAuthRegistry) clear() {
	r.mu.Lock()
	for key := range r.records {
		_ = r.removeLocked(key)
	}
	r.mu.Unlock()
}

func (r *sessionAuthRegistry) contains(key sessionAuthKey) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	_, ok := r.records[key]
	return ok
}

func (r *sessionAuthRegistry) expire(key sessionAuthKey, tokenID string) {
	r.mu.Lock()
	current := r.records[key]
	if current == nil || current.record.tokenID != tokenID {
		r.mu.Unlock()
		return
	}
	closeFn := r.removeLocked(key)
	r.mu.Unlock()
	if closeFn != nil {
		closeFn("node authorization expired")
	}
}

func (r *sessionAuthRegistry) removeLocked(key sessionAuthKey) func(string) {
	current := r.records[key]
	if current == nil {
		return nil
	}
	delete(r.records, key)
	current.timer.Stop()
	return current.close
}

func cloneSessionAuthRecord(in sessionAuthRecord) sessionAuthRecord {
	in.sessionKeyHash = bytes.Clone(in.sessionKeyHash)
	return in
}
