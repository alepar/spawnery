package node

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"time"

	"spawnery/internal/safego"
)

// eventTokenRejected is the sidecar's one event (internal/sidecar/override.go, sp-2tx8.9.1): the MITM
// proxy saw a 401/403 from the GitHub upstream, so the token the node pushed is dead.
const eventTokenRejected = "token_rejected"

// pollEventsOnce holds ONE long-poll against the sidecar's GET /control/github/events and reports
// whether it returned a token rejection.
//
//	200 {"event":"token_rejected"} -> (true, nil)   the pushed token was rejected upstream
//	204                            -> (false, nil)  the sidecar's BOUNDED timeout; re-dial immediately
//	anything else / transport err  -> (false, err)  the caller backs off and re-dials
//
// The 204 is the load-bearing half of the contract (spec §3.2): the sidecar hangs up periodically so a
// silently-dead connection cannot stop rejection detection forever.
func (s *githubControlServer) pollEventsOnce(ctx context.Context, controlURL, controlToken string) (bool, error) {
	if s.eventsDoer == nil {
		return false, fmt.Errorf("sidecar github events GET: no HTTP client")
	}
	u, err := url.Parse(controlURL)
	if err != nil {
		return false, fmt.Errorf("sidecar github events GET: parse control URL: %w", err)
	}
	if u.Scheme == "" || u.Host == "" {
		return false, fmt.Errorf("sidecar github events GET: invalid control URL %q", controlURL)
	}
	u.Path = "/control/github/events"
	u.RawQuery = ""
	u.Fragment = ""

	// Our own bound on the poll, on top of the sidecar's: a sidecar that accepts the connection and then
	// never answers must not park this goroutine forever.
	cctx, cancel := context.WithTimeout(ctx, s.eventsPollTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(cctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return false, fmt.Errorf("sidecar github events GET: build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+controlToken)

	resp, err := s.eventsDoer.Do(req)
	if err != nil {
		return false, fmt.Errorf("sidecar github events GET: %w", err)
	}
	defer func() { _, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10)); _ = resp.Body.Close() }()

	switch {
	case resp.StatusCode == http.StatusNoContent:
		return false, nil // bounded timeout: nothing happened, re-dial
	case resp.StatusCode < 200 || resp.StatusCode >= 300:
		return false, fmt.Errorf("sidecar github events GET: sidecar control returned %d", resp.StatusCode)
	}

	var body struct {
		Event string `json:"event"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&body); err != nil {
		return false, fmt.Errorf("sidecar github events GET: decode: %w", err)
	}
	return body.Event == eventTokenRejected, nil
}

// watchHandle is one spawn's events long-poll, identified by POINTER (like pushHandle): cleanup must
// never delete a NEWER watcher's cancel func out of the map.
type watchHandle struct {
	cancel context.CancelFunc
}

// startEventsWatch holds the spec's §3.2 long-poll for spawnID: dial -> wait -> handle -> re-dial, with
// bounded backoff. It is IDEMPOTENT: a spawn already being watched keeps its existing watcher (the
// rotation push calls this on every rotation; the re-adopt push is what RE-ESTABLISHES it after a node
// restart — without which a restarted node would silently lose rejection detection).
//
// It is called ONLY from the push-success paths, which is also the "github spawns only" filter: a spawn
// with no github link never gets that far (ErrGitHubNotLinked), so the node does not hold a connection
// for every spawn on the node.
//
// A nil eventsDoer disables rejection detection (the default; node.Run installs one).
func (s *githubControlServer) startEventsWatch(spawnID, controlURL, controlToken string) {
	if s == nil || s.eventsDoer == nil || controlURL == "" {
		return
	}
	s.mu.Lock()
	if s.watches[spawnID] != nil {
		s.mu.Unlock()
		return // already watching
	}
	ctx, cancel := context.WithCancel(s.baseCtx)
	h := &watchHandle{cancel: cancel}
	s.watches[spawnID] = h
	s.mu.Unlock()

	safego.Go("node.github-events", func() {
		defer cancel()
		s.watchEvents(ctx, spawnID, controlURL, controlToken)
		s.mu.Lock()
		if s.watches[spawnID] == h { // only clear OUR entry (pointer identity)
			delete(s.watches, spawnID)
		}
		s.mu.Unlock()
	})
}

// watchEvents is the loop. It ends only when ctx dies (the spawn stopped — githubControlServer.Stop — or
// the node process is shutting down) or when a forced re-mint proves the link is gone: a revoked link
// does not heal, so there is nothing left to detect and we stop rather than retry-loop on it.
func (s *githubControlServer) watchEvents(ctx context.Context, spawnID, controlURL, controlToken string) {
	backoff := s.eventsBackoffBase
	var lastForced time.Time
	for {
		if ctx.Err() != nil {
			return
		}
		rejected, err := s.pollEventsOnce(ctx, controlURL, controlToken)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			log.Printf("github events: spawn %s: %v (re-dialling in %s)", spawnID, err, backoff)
			select {
			case <-ctx.Done():
				return
			case <-time.After(backoff):
			}
			if backoff *= 2; backoff > s.eventsBackoffMax {
				backoff = s.eventsBackoffMax
			}
			continue
		}
		backoff = s.eventsBackoffBase // a healthy poll (200 or the bounded 204) resets the backoff
		if !rejected {
			continue // the sidecar's bounded timeout: re-dial at once
		}

		// GetToken(force=true) bypasses the refresher's minMintInterval floor, so an agent retrying git
		// against a permanently dead token could otherwise turn into a mint storm against the AS.
		if !lastForced.IsZero() && s.now().Sub(lastForced) < s.rejectCooldown {
			wait := s.rejectCooldown - s.now().Sub(lastForced)
			log.Printf("github events: spawn %s: token rejected again within the re-mint cooldown; waiting %s", spawnID, wait)
			select {
			case <-ctx.Done():
				return
			case <-time.After(wait):
			}
			continue
		}
		lastForced = s.now()

		log.Printf("github events: spawn %s: upstream rejected the token; forcing a re-mint", spawnID)
		// SYNCHRONOUS on purpose: the watcher must see the outcome (and must not re-dial while a
		// credential delivery is in flight). It deliberately does NOT cancel a concurrent PushAsync
		// loop: the forced mint refreshes the refresher's cache, so that loop's next GetToken returns
		// the same fresh token, and report() already dedups the status.
		switch out := s.pushWithRetry(ctx, spawnID, controlURL, controlToken, true); out {
		case pushRelinkRequired, pushNotLinked, pushCanceled:
			log.Printf("github events: spawn %s: ending the rejection watch (%s)", spawnID, out)
			return
		}
	}
}
