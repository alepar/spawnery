package node

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"time"

	nodev1 "spawnery/gen/node/v1"
	"spawnery/internal/safego"
)

// minPushRemaining is the freshness floor the node demands of the token it pushes: a token with less
// life left than the refresher's own lead (nodeRefreshLead) is re-minted before delivery, so the
// sidecar never starts out holding a credential that is about to die.
const minPushRemaining = int64(nodeRefreshLead / time.Second)

// PushCredentials delivers the spawn's MITM CA + a live GitHub access token to the sidecar's control
// listener (POST /control/github, bearer SIDECAR_CONTROL_TOKEN). This is the node->sidecar direction the
// whole epic rests on: the node can freely DIAL INTO a pod, it just cannot BIND the pod's IP.
//
// It is SYNCHRONOUS and used by the create path (spawnlet.Manager, after the sidecar-readiness probe and
// before StartAgent), where any error is FAIL-CLOSED: the agent must never start without a working proxy.
//
// A spawn with no GitHub link is a NO-OP (nil): there is no token to push, and the sidecar rejects a
// token-less push with a 400. Everything else — a mint failure, an unreachable sidecar, a non-2xx — is
// an error.
func (s *githubControlServer) PushCredentials(ctx context.Context, spawnID, controlURL, controlToken string) error {
	exp, err := s.pushOnce(ctx, spawnID, controlURL, controlToken, false)
	switch {
	case errors.Is(err, ErrGitHubNotLinked):
		return nil // not a github spawn: nothing to deliver
	case err != nil:
		return err
	}
	s.recordPushed(spawnID, exp)
	s.report(spawnID, nodev1.GitHubCredentialStatus_GITHUB_CREDENTIAL_STATUS_OK)
	// a spawn with no github link returned above, so ONLY github spawns get a watcher.
	s.startEventsWatch(spawnID, controlURL, controlToken)
	return nil
}

// PushAsync starts (or restarts) the background push loop for spawnID: it resolves the spawn's control
// endpoint from the Manager's store and re-delivers the CA + a live token with bounded backoff. Used by
// the two delivery points where the node cannot block on the pod:
//
//   - ROTATION: the githubRefresher's proactive mint succeeded (~8h lifetime, 8min lead).
//   - RE-ADOPT: a restarted node reunited with a still-running pod. The sidecar never stopped, so it
//     still HAS its secrets — this only covers a token that rotated while the node was down. Which is
//     why an adopt/rotation push failure is NOT fatal: the pod is healthy for everything that is not git.
//
// pushHandle identifies one PushAsync loop's cancel func by pointer identity, not just presence: a
// bare map[string]context.CancelFunc can't tell "my entry" from "a newer loop's entry" (func values are
// only comparable to nil in Go), so an older loop's cleanup could delete a newer loop's live cancel func
// out of the map. Comparing *pushHandle pointers makes cleanup precise.
type pushHandle struct {
	cancel context.CancelFunc
}

// A previous in-flight loop for the same spawn is cancelled (a fresher token supersedes it). ctx is
// detached from the caller's cancellation (a CP disconnect must not abort a credential delivery); the
// loop's own deadline is what bounds it.
func (s *githubControlServer) PushAsync(ctx context.Context, spawnID string) {
	if s == nil || s.lookup == nil {
		return
	}
	controlURL, controlToken, ok := s.lookup(spawnID)
	if !ok || controlURL == "" {
		log.Printf("github push: spawn %s has no sidecar control endpoint; skipping", spawnID)
		return
	}

	pctx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	handle := &pushHandle{cancel: cancel}
	s.mu.Lock()
	if prev := s.pushes[spawnID]; prev != nil {
		prev.cancel()
	}
	s.pushes[spawnID] = handle
	s.mu.Unlock()

	safego.Go("node.github-push", func() {
		defer cancel()
		s.pushWithRetry(pctx, spawnID, controlURL, controlToken, false)
		s.mu.Lock()
		// Only clear our own entry: a newer PushAsync may already have replaced it (pointer identity,
		// not just non-nil — see pushHandle's doc comment).
		if s.pushes[spawnID] == handle {
			delete(s.pushes, spawnID)
		}
		s.mu.Unlock()
	})
}

// pushOutcome is how a pushWithRetry loop ended. The events watcher (githubevents.go) acts on it: a
// revoked link means there is nothing left to detect, so the watch stops.
type pushOutcome int

const (
	pushOK             pushOutcome = iota // delivered; github_credential_status=OK
	pushNotLinked                         // the spawn has no github link (or it was forgotten mid-flight)
	pushRelinkRequired                    // the AS says the link is broken/revoked; RELINK_REQUIRED reported
	pushStale                             // still undeliverable at the token's expiry; STALE reported
	pushCanceled                          // ctx died (spawn stopped, or the process is going away)
)

func (o pushOutcome) String() string {
	switch o {
	case pushOK:
		return "ok"
	case pushNotLinked:
		return "not-linked"
	case pushRelinkRequired:
		return "relink-required"
	case pushStale:
		return "stale"
	case pushCanceled:
		return "canceled"
	}
	return "unknown"
}

// pushWithRetry retries the push with exponential backoff until it lands, the spawn turns out to have no
// link, the link is revoked, or the deadline passes — the spec's failure semantics (§4):
//
//	push lands                    -> github_credential_status=OK
//	link revoked (relink needed)  -> RELINK_REQUIRED, no retry loop (a revoked link does not heal)
//	still undeliverable at the
//	  deadline (= the expiry of
//	  the token the sidecar holds) -> STALE
//
// force applies to the FIRST attempt only: after it, the freshly minted token is in the refresher's
// cache, and re-forcing on every backoff retry (whose failures are POST failures, not mint failures)
// would be a mint storm against the AS.
func (s *githubControlServer) pushWithRetry(ctx context.Context, spawnID, controlURL, controlToken string, force bool) pushOutcome {
	deadline := s.staleDeadline(spawnID)
	backoff := s.pushBackoffBase
	attemptForce := force
	for {
		exp, err := s.pushOnce(ctx, spawnID, controlURL, controlToken, attemptForce)
		attemptForce = false
		switch {
		case err == nil:
			s.recordPushed(spawnID, exp)
			s.report(spawnID, nodev1.GitHubCredentialStatus_GITHUB_CREDENTIAL_STATUS_OK)
			// delivery point 2/3 (rotation, re-adopt) is also where a restarted node RE-ESTABLISHES the
			// rejection long-poll (§3.2) — idempotent, so a rotation push finds it already running.
			s.startEventsWatch(spawnID, controlURL, controlToken)
			return pushOK
		case errors.Is(err, ErrGitHubNotLinked):
			return pushNotLinked // not a github spawn (or the link was forgotten mid-flight)
		case errors.Is(err, ErrGitHubRelinkRequired):
			log.Printf("github push: spawn %s: %v (reporting RELINK_REQUIRED)", spawnID, err)
			s.report(spawnID, nodev1.GitHubCredentialStatus_GITHUB_CREDENTIAL_STATUS_RELINK_REQUIRED)
			return pushRelinkRequired
		}
		log.Printf("github push: spawn %s: %v (retrying in %s)", spawnID, err, backoff)

		select {
		case <-ctx.Done():
			return pushCanceled // spawn stopped, or the process is going away
		case <-time.After(backoff):
		}
		if s.now().After(deadline) {
			log.Printf("github push: spawn %s: undeliverable past the token's expiry; reporting STALE", spawnID)
			s.report(spawnID, nodev1.GitHubCredentialStatus_GITHUB_CREDENTIAL_STATUS_STALE)
			return pushStale
		}
		if backoff *= 2; backoff > s.pushBackoffMax {
			backoff = s.pushBackoffMax
		}
	}
}

// pushOnce performs one delivery attempt: the spawn's CA (from the persistent store) plus a token with
// at least minPushRemaining seconds of life. force bypasses the refresher's token cache and its
// minMintInterval floor — the rejection path (§3.2) needs a genuinely NEW token, because the cached one
// is exactly the one GitHub just refused. Returns the pushed token's expiry (unix; 0 = unknown).
func (s *githubControlServer) pushOnce(ctx context.Context, spawnID, controlURL, controlToken string, force bool) (int64, error) {
	s.mu.Lock()
	pair, err := s.caForLocked(spawnID)
	s.mu.Unlock()
	if err != nil {
		return 0, err
	}

	tok, exp, err := s.refresher.GetToken(ctx, spawnID, minPushRemaining, force)
	// A rate-limited GetToken that still handed back a live cached token is good enough to push: the
	// alternative is failing a delivery over the node's own mint floor.
	if err != nil && !(tok != "" && errors.Is(err, ErrGitHubMintRateLimited)) {
		return 0, err
	}
	if tok == "" {
		return 0, ErrGitHubNotLinked
	}

	if err := postSidecarGitHub(ctx, s.doer, controlURL, controlToken, pair, tok, exp); err != nil {
		return exp, err
	}
	return exp, nil
}

// postSidecarGitHub POSTs the CA + token to the sidecar's /control/github (sp-2tx8.9.1's wire contract).
// It mirrors postSidecarCredentials: same control URL rewrite, same bearer, plain JSON (not protojson).
func postSidecarGitHub(ctx context.Context, doer httpDoer, controlURL, controlToken string, pair caPair, token string, expiresAt int64) error {
	if doer == nil {
		return fmt.Errorf("sidecar github POST: no HTTP client")
	}
	u, err := url.Parse(controlURL)
	if err != nil {
		return fmt.Errorf("sidecar github POST: parse control URL: %w", err)
	}
	if u.Scheme == "" || u.Host == "" {
		return fmt.Errorf("sidecar github POST: invalid control URL %q", controlURL)
	}
	u.Path = "/control/github"
	u.RawQuery = ""
	u.Fragment = ""

	body, err := json.Marshal(struct {
		CACertPEM      string `json:"ca_cert_pem"`
		CAKeyPEM       string `json:"ca_key_pem"`
		Token          string `json:"token"`
		TokenExpiresAt int64  `json:"token_expires_at"`
	}{
		CACertPEM:      string(pair.certPEM),
		CAKeyPEM:       string(pair.keyPEM),
		Token:          token,
		TokenExpiresAt: expiresAt,
	})
	if err != nil {
		return fmt.Errorf("sidecar github POST: marshal body: %w", err)
	}
	defer zeroBytes(body) // the CA private key and the token were just serialized into it

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u.String(), bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("sidecar github POST: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+controlToken)

	resp, err := doer.Do(req)
	if err != nil {
		return fmt.Errorf("sidecar github POST: %w", err)
	}
	defer func() { _, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10)); _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("sidecar github POST: sidecar control returned %d", resp.StatusCode)
	}
	return nil
}

// staleDeadline is when an undeliverable push becomes a STALE credential: the expiry of the token the
// sidecar is currently holding (the last one we successfully pushed). Until then the spawn's git works
// fine and there is nothing to report. With no such token — or no expiry for it — fall back to a bounded
// window, because "retry forever, report nothing" is the one outcome the spec forbids.
func (s *githubControlServer) staleDeadline(spawnID string) time.Time {
	now := s.now()
	s.mu.Lock()
	exp := s.pushedExpiry[spawnID]
	s.mu.Unlock()
	if exp > 0 {
		if t := time.Unix(exp, 0); t.After(now) {
			return t
		}
	}
	return now.Add(s.pushFallbackWindow)
}

func (s *githubControlServer) recordPushed(spawnID string, expiry int64) {
	s.mu.Lock()
	s.pushedExpiry[spawnID] = expiry
	s.mu.Unlock()
}

// SetStatusReporter installs the CP-connection-scoped reporter for the github credential condition and
// immediately REPLAYS every sticky non-OK condition through it. The replay is load-bearing: a STALE
// computed while the CP was disconnected would otherwise never reach anyone, and a silent failure to
// deliver a credential is exactly the failure this design must never have.
func (s *githubControlServer) SetStatusReporter(fn func(spawnID string, st nodev1.GitHubCredentialStatus)) {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.reporter = fn
	sticky := make(map[string]nodev1.GitHubCredentialStatus, len(s.lastStatus))
	for id, st := range s.lastStatus {
		if st != nodev1.GitHubCredentialStatus_GITHUB_CREDENTIAL_STATUS_OK &&
			st != nodev1.GitHubCredentialStatus_GITHUB_CREDENTIAL_STATUS_UNSPECIFIED {
			sticky[id] = st
		}
	}
	s.mu.Unlock()
	if fn == nil {
		return
	}
	for id, st := range sticky {
		fn(id, st)
	}
}

// report records the spawn's credential condition and forwards it to the CP, skipping a report that
// would restate the condition the CP already has. UNSPECIFIED is never sent: on the wire it means
// "not reported" and must not clobber a stored STALE/RELINK_REQUIRED.
func (s *githubControlServer) report(spawnID string, st nodev1.GitHubCredentialStatus) {
	if st == nodev1.GitHubCredentialStatus_GITHUB_CREDENTIAL_STATUS_UNSPECIFIED {
		return
	}
	s.mu.Lock()
	if s.lastStatus[spawnID] == st {
		s.mu.Unlock()
		return
	}
	s.lastStatus[spawnID] = st
	fn := s.reporter
	s.mu.Unlock()
	if fn == nil {
		return // no CP connection: SetStatusReporter replays this on the next one
	}
	fn(spawnID, st)
}
