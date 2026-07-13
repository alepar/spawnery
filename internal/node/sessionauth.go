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
	accountID          string
	tokenID            string
	issuedAt           int64
	expiresAt          time.Time
	sessionKeyHash     []byte
	generation         uint64
	nodeID             string
	attachmentID       string
	attachmentSequence uint64
}

type liveSessionAuth struct {
	record sessionAuthRecord
	timer  sessionAuthTimer
	close  func(string)
}

type sessionAuthTimer interface{ Stop() bool }

type sessionAuthRegistry struct {
	mu          sync.Mutex
	records     map[sessionAuthKey]*liveSessionAuth
	now         func() time.Time
	after       func(time.Duration, func()) sessionAuthTimer
	revocations UserRevocationLookup
}

func newSessionAuthRegistry(revocations ...UserRevocationLookup) *sessionAuthRegistry {
	return newSessionAuthRegistryWithClock(time.Now, func(delay time.Duration, callback func()) sessionAuthTimer {
		return time.AfterFunc(delay, callback)
	}, revocations...)
}

func newSessionAuthRegistryWithClock(now func() time.Time, after func(time.Duration, func()) sessionAuthTimer, revocations ...UserRevocationLookup) *sessionAuthRegistry {
	var lookup UserRevocationLookup
	if len(revocations) > 0 {
		lookup = revocations[0]
	}
	return &sessionAuthRegistry{records: make(map[sessionAuthKey]*liveSessionAuth), now: now, after: after, revocations: lookup}
}

func (r *sessionAuthRegistry) register(key sessionAuthKey, record sessionAuthRecord, closeFn func(string)) {
	r.registerRecord(key, record, closeFn, false)
}

func (r *sessionAuthRegistry) registerIfNewer(key sessionAuthKey, record sessionAuthRecord, closeFn func(string)) bool {
	return r.registerRecord(key, record, closeFn, true)
}

func (r *sessionAuthRegistry) acceptsOpen(key sessionAuthKey, attachmentSequence uint64) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	current := r.records[key]
	return current == nil || current.record.attachmentSequence < attachmentSequence
}

func (r *sessionAuthRegistry) registerRecord(key sessionAuthKey, record sessionAuthRecord, closeFn func(string), requireNewer bool) bool {
	r.mu.Lock()
	if r.revocations != nil && r.revocations.IsRevoked(record.tokenID, record.accountID, record.issuedAt) {
		r.mu.Unlock()
		return false
	}
	if old := r.records[key]; old != nil {
		if requireNewer && old.record.attachmentSequence >= record.attachmentSequence {
			r.mu.Unlock()
			return false
		}
		old.timer.Stop()
	}
	live := &liveSessionAuth{record: cloneSessionAuthRecord(record), close: closeFn}
	r.records[key] = live
	live.timer = r.after(record.expiresAt.Sub(r.now()), func() { r.expire(key, live) })
	r.mu.Unlock()
	return true
}

func (r *sessionAuthRegistry) replace(key sessionAuthKey, next sessionAuthRecord, liveOwner string) (replaced, found bool) {
	r.mu.Lock()
	current := r.records[key]
	found = current != nil
	now := r.now()
	valid := current != nil && now.Before(current.record.expiresAt) && now.Before(next.expiresAt) &&
		(r.revocations == nil || !r.revocations.IsRevoked(next.tokenID, next.accountID, next.issuedAt)) &&
		current.record.accountID == next.accountID && next.accountID == liveOwner &&
		bytes.Equal(current.record.sessionKeyHash, next.sessionKeyHash) &&
		current.record.generation == next.generation && current.record.nodeID == next.nodeID &&
		current.record.attachmentID == next.attachmentID
	if !valid {
		closeFn := r.removeLocked(key)
		r.mu.Unlock()
		if closeFn != nil {
			closeFn("session reauthentication rejected")
		}
		return false, found
	}
	next.attachmentSequence = current.record.attachmentSequence
	current.timer.Stop()
	replacement := &liveSessionAuth{record: cloneSessionAuthRecord(next), close: current.close}
	r.records[key] = replacement
	replacement.timer = r.after(next.expiresAt.Sub(now), func() { r.expire(key, replacement) })
	r.mu.Unlock()
	return true, true
}

func (r *sessionAuthRegistry) revoke(batch []VerifiedUserRevocation) {
	tokens := make(map[string]struct{})
	accountCutoffs := make(map[string]int64)
	for _, entry := range batch {
		for _, token := range entry.RevokedTokens {
			tokens[token.TokenID] = struct{}{}
		}
		if entry.RevokeTokensIssuedBefore > accountCutoffs[entry.AccountID] {
			accountCutoffs[entry.AccountID] = entry.RevokeTokensIssuedBefore
		}
	}
	type closure struct{ fn func(string) }
	closures := make([]closure, 0)
	r.mu.Lock()
	for key, current := range r.records {
		_, tokenMatch := tokens[current.record.tokenID]
		cutoff := accountCutoffs[current.record.accountID]
		accountMatch := cutoff > 0 && current.record.issuedAt < cutoff
		if !tokenMatch && !accountMatch {
			continue
		}
		delete(r.records, key)
		current.timer.Stop()
		closures = append(closures, closure{fn: current.close})
	}
	r.mu.Unlock()
	for _, item := range closures {
		item.fn("node authorization revoked")
	}
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

func (r *sessionAuthRegistry) removeIfAttachment(key sessionAuthKey, attachmentID string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	current := r.records[key]
	if current == nil || attachmentID == "" || current.record.attachmentID != attachmentID {
		return false
	}
	_ = r.removeLocked(key)
	return true
}

// closeAttachment serializes transport teardown with attachment registration. If a newer
// incarnation is already present, teardown is skipped; otherwise registration waits until the old
// transport has been detached and can then install cleanly.
func (r *sessionAuthRegistry) closeAttachment(key sessionAuthKey, attachmentID string, detach func()) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	current := r.records[key]
	if current != nil {
		if attachmentID == "" || current.record.attachmentID != attachmentID {
			return false
		}
		_ = r.removeLocked(key)
	}
	detach()
	return true
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

func (r *sessionAuthRegistry) removeSession(spawnID, sessionID string) {
	r.mu.Lock()
	for key := range r.records {
		if key.spawnID == spawnID && key.sessionID == sessionID {
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

func (r *sessionAuthRegistry) attachment(key sessionAuthKey) (string, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	current := r.records[key]
	if current == nil {
		return "", false
	}
	return current.record.attachmentID, true
}

func (r *sessionAuthRegistry) expire(key sessionAuthKey, expected *liveSessionAuth) {
	r.mu.Lock()
	current := r.records[key]
	if current != expected {
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
