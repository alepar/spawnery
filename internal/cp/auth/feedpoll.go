package auth

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"
	"time"

	"spawnery/internal/authsvc/token"
)

// httpDoer is the interface for making HTTP requests (injectable for testing).
type httpDoer interface {
	Do(req *http.Request) (*http.Response, error)
}

// FeedPoller polls the AS revocation feed, applies valid entries to a RevocationRegistry,
// and advances a checkpoint past the highest processed seq.
type FeedPoller struct {
	doer       httpDoer
	url        string       // base URL of the revocation feed (without ?since=)
	bearer     string       // optional CP-to-AS bearer secret (CP_AS_CP_SECRET)
	keys       token.KeySet // same key set as session verification
	revreg     *RevocationRegistry
	interval   time.Duration
	checkpoint int64 // highest seq applied (0 = initial)
}

// NewFeedPoller builds a FeedPoller. interval=0 uses 30s default.
func NewFeedPoller(doer httpDoer, feedURL, bearer string, keys token.KeySet, revreg *RevocationRegistry, interval time.Duration) *FeedPoller {
	if interval <= 0 {
		interval = 30 * time.Second
	}
	return &FeedPoller{doer: doer, url: feedURL, bearer: bearer, keys: keys, revreg: revreg, interval: interval}
}

// Run polls on interval until ctx is cancelled.
func (p *FeedPoller) Run(ctx context.Context) {
	ticker := time.NewTicker(p.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := p.pollOnce(ctx); err != nil {
				log.Printf("revocation feed poll: %v", err)
			}
		}
	}
}

// pollOnce fetches one batch from the feed and applies valid entries.
func (p *FeedPoller) pollOnce(ctx context.Context) error {
	url := p.url + "?since=" + strconv.FormatInt(p.checkpoint, 10)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	if p.bearer != "" {
		req.Header.Set("Authorization", "Bearer "+p.bearer)
	}
	resp, err := p.doer.Do(req)
	if err != nil {
		return fmt.Errorf("GET %s: %w", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("GET %s: status %d: %s", url, resp.StatusCode, body)
	}

	var entries []SignedFeedEntry
	if err := json.NewDecoder(resp.Body).Decode(&entries); err != nil {
		return fmt.Errorf("decode feed: %w", err)
	}

	var maxSeq int64 = p.checkpoint
	for _, e := range entries {
		if err := p.revreg.Apply(e, p.keys); err != nil {
			// Log bad entries but don't corrupt checkpoint or stop processing.
			log.Printf("revocation feed: bad entry seq=%d: %v", e.Seq, err)
			continue
		}
		if e.Seq > maxSeq {
			maxSeq = e.Seq
		}
	}
	p.checkpoint = maxSeq
	return nil
}
