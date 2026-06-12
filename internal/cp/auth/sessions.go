package auth

import (
	"sync"
	"sync/atomic"
)

// SessionRegistry tracks live sessions by token_id and account_id so revocation can
// cancel them. Dev sessions (empty token_id) are added but never looked up by revocation.
type SessionRegistry struct {
	mu        sync.Mutex
	seq       atomic.Uint64
	byToken   map[string]map[uint64]sessionEntry // token_id -> seq -> entry
	byAccount map[string]map[uint64]sessionEntry // account_id -> seq -> entry
}

type sessionEntry struct {
	tokenID   string
	accountID string
	cancel    func()
}

// NewSessionRegistry builds an empty SessionRegistry.
func NewSessionRegistry() *SessionRegistry {
	return &SessionRegistry{
		byToken:   make(map[string]map[uint64]sessionEntry),
		byAccount: make(map[string]map[uint64]sessionEntry),
	}
}

// Add registers a live session and returns a release function the caller MUST defer.
// Dev sessions (tokenID == "") are registered under account only and are NOT revocable
// by token (they have no token_id); they ARE cancelled by account revocation.
func (r *SessionRegistry) Add(tokenID, accountID string, cancel func()) func() {
	seq := r.seq.Add(1)
	e := sessionEntry{tokenID: tokenID, accountID: accountID, cancel: cancel}

	r.mu.Lock()
	if tokenID != "" {
		if r.byToken[tokenID] == nil {
			r.byToken[tokenID] = make(map[uint64]sessionEntry)
		}
		r.byToken[tokenID][seq] = e
	}
	if accountID != "" {
		if r.byAccount[accountID] == nil {
			r.byAccount[accountID] = make(map[uint64]sessionEntry)
		}
		r.byAccount[accountID][seq] = e
	}
	r.mu.Unlock()

	return func() {
		r.mu.Lock()
		if tokenID != "" {
			if m := r.byToken[tokenID]; m != nil {
				delete(m, seq)
				if len(m) == 0 {
					delete(r.byToken, tokenID)
				}
			}
		}
		if accountID != "" {
			if m := r.byAccount[accountID]; m != nil {
				delete(m, seq)
				if len(m) == 0 {
					delete(r.byAccount, accountID)
				}
			}
		}
		r.mu.Unlock()
	}
}

// RevokeToken cancels all live sessions holding the given token_id.
func (r *SessionRegistry) RevokeToken(tokenID string) {
	r.mu.Lock()
	entries := copyMap(r.byToken[tokenID])
	r.mu.Unlock()
	for _, e := range entries {
		e.cancel()
	}
}

// RevokeAccount cancels all live sessions for the given account_id.
func (r *SessionRegistry) RevokeAccount(accountID string) {
	r.mu.Lock()
	entries := copyMap(r.byAccount[accountID])
	r.mu.Unlock()
	for _, e := range entries {
		e.cancel()
	}
}

func copyMap(m map[uint64]sessionEntry) map[uint64]sessionEntry {
	if len(m) == 0 {
		return nil
	}
	out := make(map[uint64]sessionEntry, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}
