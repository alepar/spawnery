package auth

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
	"strconv"
	"time"

	"spawnery/internal/authsvc/token"
)

const (
	maxFeedPageEntries = 256
	maxFeedPageBytes   = 4 << 20
)

type httpDoer interface {
	Do(req *http.Request) (*http.Response, error)
}

type FeedPoller struct {
	doer       httpDoer
	url        string
	artifacts  *token.Verifier
	now        func() time.Time
	revreg     *RevocationRegistry
	interval   time.Duration
	checkpoint int64
}

func NewFeedPoller(doer httpDoer, feedURL string, artifacts *token.Verifier, revreg *RevocationRegistry, interval time.Duration) *FeedPoller {
	if interval <= 0 {
		interval = 30 * time.Second
	}
	return &FeedPoller{doer: doer, url: feedURL, artifacts: artifacts, revreg: revreg, interval: interval, now: time.Now}
}

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

func (p *FeedPoller) pollOnce(ctx context.Context) error {
	for {
		page, err := p.fetchPage(ctx)
		if err != nil {
			return err
		}
		if len(page.Entries) == 0 && page.HasMore {
			return errors.New("revocation feed: empty page claims more entries")
		}
		last, err := p.revreg.ApplyPage(page.Entries, p.artifacts, p.now(), p.checkpoint)
		if err != nil {
			return err
		}
		p.checkpoint = last
		if !page.HasMore {
			return nil
		}
	}
}

func (p *FeedPoller) fetchPage(ctx context.Context) (SignedFeedPage, error) {
	parsed, err := url.Parse(p.url)
	if err != nil {
		return SignedFeedPage{}, fmt.Errorf("revocation feed URL: %w", err)
	}
	query := parsed.Query()
	query.Set("limit", strconv.Itoa(maxFeedPageEntries))
	query.Set("since", strconv.FormatInt(p.checkpoint, 10))
	parsed.RawQuery = query.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, parsed.String(), nil)
	if err != nil {
		return SignedFeedPage{}, fmt.Errorf("build request: %w", err)
	}
	resp, err := p.doer.Do(req)
	if err != nil {
		return SignedFeedPage{}, fmt.Errorf("GET %s: %w", parsed.String(), err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return SignedFeedPage{}, fmt.Errorf("GET %s: status %d: %s", parsed.String(), resp.StatusCode, body)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxFeedPageBytes+1))
	if err != nil {
		return SignedFeedPage{}, err
	}
	if len(raw) > maxFeedPageBytes {
		return SignedFeedPage{}, errors.New("revocation feed page is too large")
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	var page SignedFeedPage
	if err := decoder.Decode(&page); err != nil {
		return SignedFeedPage{}, fmt.Errorf("decode feed: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return SignedFeedPage{}, errors.New("revocation feed has trailing data")
	}
	if page.Entries == nil || len(page.Entries) > maxFeedPageEntries {
		return SignedFeedPage{}, errors.New("revocation feed entries are missing or too large")
	}
	return page, nil
}
