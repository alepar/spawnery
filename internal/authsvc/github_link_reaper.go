package authsvc

import (
	"context"
	"time"
)

// reapGitHubLinkFlows evicts expired flow records + state correlators (TTL expiry is NOT lazy-only)
// and, for an exchanged-but-abandoned READY flow, targets DELETE /token on its access token so it
// doesn't linger ~8h. The discarded refresh token is unreferenced and is NOT separately revoked.
func (s *Service) reapGitHubLinkFlows(now time.Time) {
	if s.githubLinkFlows == nil {
		return
	}
	var abandoned []string
	s.githubLinkMu.Lock()
	for id, fl := range s.githubLinkFlows {
		if now.After(fl.expiresAt) {
			if fl.status == flowReady && fl.pending != nil && fl.pending.tuple.AccessToken != "" {
				abandoned = append(abandoned, fl.pending.tuple.AccessToken)
			}
			delete(s.githubLinkFlows, id)
		}
	}
	for st, sc := range s.githubLinkStates {
		if now.After(sc.expiresAt) {
			delete(s.githubLinkStates, st)
		}
	}
	s.githubLinkMu.Unlock()
	for _, at := range abandoned {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		_ = s.githubLinkExchanger.RevokeAppToken(ctx, at) // best-effort; idempotent
		cancel()
	}
}

// RunGitHubLinkReaper runs the reaper on a ticker until ctx is cancelled. Production wires this in
// a goroutine; hermetic tests call reapGitHubLinkFlows directly for determinism.
func (s *Service) RunGitHubLinkReaper(ctx context.Context) {
	t := time.NewTicker(reaperInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.reapGitHubLinkFlows(s.now())
		}
	}
}
