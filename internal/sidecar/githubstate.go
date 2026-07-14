package sidecar

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"time"
)

// defaultEventsTimeout bounds GET /control/github/events. It is LOAD-BEARING (spec §3.2): the
// sidecar must hang up periodically so the node re-dials — otherwise a silently-dead connection
// would stop rejection detection forever and nobody would find out.
const defaultEventsTimeout = 60 * time.Second

// GitHubState is the sidecar's GitHub credential state. It is PUSHED by the node
// (POST /control/github) and never fetched: the sidecar has no way to ask for it.
//
// It owns three things:
//   - the per-spawn MITM CA (parsed, with its per-host leaf cache);
//   - the GitHub access token and its expiry;
//   - a one-shot "the upstream rejected this token" latch that GET /control/github/events drains.
//
// All methods are safe for concurrent use and safe on a nil receiver (a nil state means the
// GitHub proxy is not enabled for this spawn).
type GitHubState struct {
	mu        sync.Mutex
	ca        *spawnCA
	caCertPEM []byte // kept so an identical re-push can keep the parsed CA (and its leaf cache)
	caKeyPEM  []byte
	token     string
	expiresAt int64 // unix seconds; 0 == unknown

	rejected bool            // latch: the upstream rejected the CURRENT token
	waiters  []chan struct{} // long-poll waiters, woken (closed) on a rejection

	// eventsTimeout is the long-poll bound. Package-internal so tests can shorten it.
	eventsTimeout time.Duration
}

// NewGitHubState returns an empty state: no CA, no token, no pending rejection.
func NewGitHubState() *GitHubState {
	return &GitHubState{eventsTimeout: defaultEventsTimeout}
}

// Set replaces the GitHub state wholesale with the node's push. It is idempotent, and it clears
// any pending rejection latch: a fresh token supersedes a rejection of the one it replaces.
//
// The CA PEMs are re-parsed only when they differ from the current ones, so the routine ~8h token
// rotation (same CA, new token) keeps the CA's JIT leaf cache warm.
func (s *GitHubState) Set(caCertPEM, caKeyPEM []byte, token string, expiresAtUnix int64) error {
	if s == nil {
		return errors.New("github state is nil")
	}
	if strings.TrimSpace(token) == "" {
		return errors.New("token is required")
	}
	if len(caCertPEM) == 0 || len(caKeyPEM) == 0 {
		return errors.New("ca_cert_pem and ca_key_pem are required")
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if s.ca == nil || !bytes.Equal(s.caCertPEM, caCertPEM) || !bytes.Equal(s.caKeyPEM, caKeyPEM) {
		ca, err := parseSpawnCA(caCertPEM, caKeyPEM)
		if err != nil {
			return err // state left untouched
		}
		s.ca = ca
		s.caCertPEM = bytes.Clone(caCertPEM)
		s.caKeyPEM = bytes.Clone(caKeyPEM)
	}
	s.token = token
	s.expiresAt = expiresAtUnix
	s.rejected = false
	return nil
}

// Token returns the pushed token and its expiry (unix seconds; 0 == unknown).
// An empty token means the node has not pushed yet.
func (s *GitHubState) Token() (string, int64) {
	if s == nil {
		return "", 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.token, s.expiresAt
}

// LeafFor returns a MITM leaf certificate for host, signed by the pushed CA.
// It errors when no CA has been pushed yet — the proxy MUST fail closed in that case.
func (s *GitHubState) LeafFor(host string) (*tls.Certificate, error) {
	if s == nil {
		return nil, errors.New("github state is nil")
	}
	s.mu.Lock()
	ca := s.ca
	s.mu.Unlock()
	if ca == nil {
		return nil, errors.New("no spawn CA has been pushed")
	}
	return ca.leafFor(host)
}

// RecordRejection latches a token rejection observed by the MITM proxy (a 401/403 from the GitHub
// upstream). It is a no-op unless token is the CURRENT token: a rejection of an already-replaced
// token is stale news and must not trigger another re-mint.
func (s *GitHubState) RecordRejection(token string) {
	if s == nil || token == "" {
		return
	}
	s.mu.Lock()
	if s.token != token {
		s.mu.Unlock()
		return
	}
	if !s.rejected {
		slog.Warn("githubstate: upstream rejected the pushed token; reporting to the node")
	}
	s.rejected = true
	waiters := s.waiters
	s.waiters = nil
	s.mu.Unlock()

	for _, ch := range waiters {
		close(ch)
	}
}

// WaitRejection blocks until a token rejection is latched (returning true and CONSUMING the latch),
// or until timeout elapses / ctx is done (returning false). It is the engine of the
// GET /control/github/events long-poll.
func (s *GitHubState) WaitRejection(ctx context.Context, timeout time.Duration) bool {
	if s == nil {
		return false
	}
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()

	for {
		s.mu.Lock()
		if s.rejected {
			s.rejected = false
			s.mu.Unlock()
			return true
		}
		ch := make(chan struct{})
		s.waiters = append(s.waiters, ch)
		s.mu.Unlock()

		select {
		case <-ch:
			// Woken by RecordRejection: loop and try to claim the latch. Another waiter may have
			// claimed it first, in which case we re-register and keep waiting until the deadline.
		case <-ctx.Done():
			s.dropWaiter(ch)
			return false
		case <-deadline.C:
			s.dropWaiter(ch)
			return false
		}
	}
}

// dropWaiter removes ch from the waiter list (best effort: RecordRejection may already have taken
// and closed it, in which case there is nothing to remove).
func (s *GitHubState) dropWaiter(ch chan struct{}) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, w := range s.waiters {
		if w == ch {
			s.waiters = append(s.waiters[:i], s.waiters[i+1:]...)
			return
		}
	}
}
