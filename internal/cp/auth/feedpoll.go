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
	maxFeedPageEntries        = 256
	maxFeedPageBytes          = 4 << 20
	defaultFeedRequestTimeout = 10 * time.Second
	defaultFeedMaxBackoff     = time.Minute
)

type httpDoer interface {
	Do(req *http.Request) (*http.Response, error)
}

type FeedPoller struct {
	doer           httpDoer
	url            string
	artifacts      *token.Verifier
	now            func() time.Time
	revreg         *RevocationRegistry
	interval       time.Duration
	checkpoint     int64
	requestTimeout time.Duration
	maxBackoff     time.Duration
	wait           func(context.Context, time.Duration) error
}

func NewFeedPoller(doer httpDoer, feedURL string, artifacts *token.Verifier, revreg *RevocationRegistry, interval time.Duration) *FeedPoller {
	if interval <= 0 {
		interval = 30 * time.Second
	}
	maxBackoff := defaultFeedMaxBackoff
	if maxBackoff < interval {
		maxBackoff = interval
	}
	return &FeedPoller{
		doer: doer, url: feedURL, artifacts: artifacts, revreg: revreg, interval: interval, now: time.Now,
		requestTimeout: defaultFeedRequestTimeout, maxBackoff: maxBackoff, wait: waitForFeedPoll,
	}
}

func (p *FeedPoller) Run(ctx context.Context) {
	backoff := p.interval
	for {
		err := p.pollOnce(ctx)
		if ctx.Err() != nil {
			return
		}
		delay := p.interval
		if err != nil {
			log.Printf("revocation feed poll: %v", err)
			delay = backoff
			backoff = doubledFeedDuration(backoff, p.maxBackoff)
		} else {
			backoff = p.interval
		}
		if p.wait(ctx, delay) != nil {
			return
		}
	}
}

func waitForFeedPoll(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func doubledFeedDuration(current, maximum time.Duration) time.Duration {
	if current >= maximum-current {
		return maximum
	}
	current *= 2
	if current > maximum {
		return maximum
	}
	return current
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
	requestContext, cancel := context.WithTimeout(ctx, p.requestTimeout)
	defer cancel()
	parsed, err := url.Parse(p.url)
	if err != nil {
		return SignedFeedPage{}, fmt.Errorf("revocation feed URL: %w", err)
	}
	query := parsed.Query()
	query.Set("limit", strconv.Itoa(maxFeedPageEntries))
	query.Set("since", strconv.FormatInt(p.checkpoint, 10))
	parsed.RawQuery = query.Encode()
	req, err := http.NewRequestWithContext(requestContext, http.MethodGet, parsed.String(), nil)
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
