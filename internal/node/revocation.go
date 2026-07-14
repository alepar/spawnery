package node

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"google.golang.org/protobuf/proto"

	authv1 "spawnery/gen/auth/v1"
	"spawnery/internal/authsvc/token"
)

const (
	maxRevocationPageEntries        = 256
	maxRevocationPageBytes          = 4 << 20
	defaultRevocationRequestTimeout = 10 * time.Second
	defaultRevocationMaxBackoff     = time.Minute
)

type signedUserRevocationEntry struct {
	Seq int64  `json:"seq"`
	Sig string `json:"sig"`
}

type signedUserRevocationPage struct {
	Entries []signedUserRevocationEntry `json:"entries"`
	HasMore bool                        `json:"has_more"`
}

type RevocationConsumerOption func(*RevocationConsumer) error

func WithRevocationRequestTimeout(timeout time.Duration) RevocationConsumerOption {
	return func(consumer *RevocationConsumer) error {
		if timeout <= 0 {
			return errors.New("node: revocation request timeout must be positive")
		}
		consumer.requestTimeout = timeout
		return nil
	}
}

func WithRevocationMaxBackoff(maxBackoff time.Duration) RevocationConsumerOption {
	return func(consumer *RevocationConsumer) error {
		if maxBackoff <= 0 {
			return errors.New("node: revocation max backoff must be positive")
		}
		consumer.maxBackoff = maxBackoff
		return nil
	}
}

type RevocationConsumer struct {
	doer           httpDoer
	feedURL        *url.URL
	artifacts      *token.Verifier
	store          *UserRevocationStore
	interval       time.Duration
	requestTimeout time.Duration
	maxBackoff     time.Duration
	now            func() time.Time
	wait           func(context.Context, time.Duration) error
}

func NewRevocationConsumer(doer httpDoer, feedURL string, artifacts *token.Verifier, store *UserRevocationStore, interval time.Duration, options ...RevocationConsumerOption) (*RevocationConsumer, error) {
	parsed, err := url.Parse(feedURL)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.RawQuery != "" || parsed.Fragment != "" || doer == nil || artifacts == nil || store == nil || interval <= 0 {
		return nil, errors.New("node: invalid revocation consumer configuration")
	}
	consumer := &RevocationConsumer{
		doer: doer, feedURL: parsed, artifacts: artifacts, store: store, interval: interval,
		requestTimeout: defaultRevocationRequestTimeout, maxBackoff: defaultRevocationMaxBackoff,
		now: time.Now, wait: waitForRevocationPoll,
	}
	if consumer.maxBackoff < interval {
		consumer.maxBackoff = interval
	}
	for _, option := range options {
		if option == nil {
			return nil, errors.New("node: nil revocation consumer option")
		}
		if err := option(consumer); err != nil {
			return nil, err
		}
	}
	if consumer.maxBackoff < interval {
		return nil, errors.New("node: revocation max backoff is below poll interval")
	}
	return consumer, nil
}

func (c *RevocationConsumer) Run(ctx context.Context, onApplied func([]VerifiedUserRevocation)) {
	backoff := c.interval
	for {
		err := c.pollOnce(ctx, onApplied)
		if ctx.Err() != nil {
			return
		}
		delay := c.interval
		if err != nil {
			slog.Warn("node: revocation feed poll failed", "err", err)
			delay = backoff
			backoff = doubledDuration(backoff, c.maxBackoff)
		} else {
			backoff = c.interval
		}
		if c.wait(ctx, delay) != nil {
			return
		}
	}
}

func doubledDuration(current, maximum time.Duration) time.Duration {
	if current >= maximum-current {
		return maximum
	}
	current *= 2
	if current > maximum {
		return maximum
	}
	return current
}

func waitForRevocationPoll(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func (c *RevocationConsumer) pollOnce(ctx context.Context, onApplied func([]VerifiedUserRevocation)) error {
	for {
		page, err := c.fetchPage(ctx)
		if err != nil {
			return err
		}
		if len(page.entries) == 0 && page.hasMore {
			return errors.New("node: empty revocation page claims more entries")
		}
		if err := c.store.ApplyPage(page.entries, page.verifiedAt); err != nil {
			if errors.Is(err, ErrUserRevocationStorePoisoned) && len(page.entries) > 0 &&
				c.store.Checkpoint() == page.entries[len(page.entries)-1].Seq && onApplied != nil {
				onApplied(page.entries)
			}
			return err
		}
		if len(page.entries) > 0 && onApplied != nil {
			onApplied(page.entries)
		}
		if !page.hasMore {
			return nil
		}
	}
}

type verifiedRevocationPage struct {
	entries    []VerifiedUserRevocation
	hasMore    bool
	verifiedAt time.Time
}

func (c *RevocationConsumer) fetchPage(ctx context.Context) (verifiedRevocationPage, error) {
	requestContext, cancel := context.WithTimeout(ctx, c.requestTimeout)
	defer cancel()
	u := *c.feedURL
	query := u.Query()
	query.Set("limit", strconv.Itoa(maxRevocationPageEntries))
	query.Set("since", strconv.FormatInt(c.store.Checkpoint(), 10))
	u.RawQuery = query.Encode()
	req, err := http.NewRequestWithContext(requestContext, http.MethodGet, u.String(), nil)
	if err != nil {
		return verifiedRevocationPage{}, err
	}
	resp, err := c.doer.Do(req)
	if err != nil {
		return verifiedRevocationPage{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		return verifiedRevocationPage{}, fmt.Errorf("node: revocation feed status %d", resp.StatusCode)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxRevocationPageBytes+1))
	if err != nil {
		return verifiedRevocationPage{}, fmt.Errorf("node: read revocation page: %w", err)
	}
	if len(raw) > maxRevocationPageBytes {
		return verifiedRevocationPage{}, errors.New("node: revocation page is too large")
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	var outer signedUserRevocationPage
	if err := decoder.Decode(&outer); err != nil {
		return verifiedRevocationPage{}, fmt.Errorf("node: decode revocation page: %w", err)
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return verifiedRevocationPage{}, errors.New("node: trailing revocation page data")
	}
	if outer.Entries == nil {
		return verifiedRevocationPage{}, errors.New("node: revocation page entries must be an array")
	}
	if len(outer.Entries) > maxRevocationPageEntries {
		return verifiedRevocationPage{}, errors.New("node: revocation page has too many entries")
	}
	verifiedAt := c.now()
	entries := make([]VerifiedUserRevocation, 0, len(outer.Entries))
	previous := c.store.Checkpoint()
	for _, outerEntry := range outer.Entries {
		payload, err := c.artifacts.Verify(outerEntry.Sig, token.ArtifactTypeRevocation, verifiedAt)
		if err != nil {
			return verifiedRevocationPage{}, fmt.Errorf("node: verify revocation entry: %w", err)
		}
		var body authv1.RevocationEntry
		if err := (proto.UnmarshalOptions{DiscardUnknown: false}).Unmarshal(payload, &body); err != nil {
			return verifiedRevocationPage{}, fmt.Errorf("node: decode verified revocation entry: %w", err)
		}
		if len(body.ProtoReflect().GetUnknown()) != 0 {
			return verifiedRevocationPage{}, errors.New("node: verified revocation entry has unknown fields")
		}
		canonical, err := (proto.MarshalOptions{Deterministic: true}).Marshal(&body)
		if err != nil || !bytes.Equal(canonical, payload) {
			return verifiedRevocationPage{}, errors.New("node: verified revocation entry is not deterministic protobuf")
		}
		if outerEntry.Seq != body.Seq {
			return verifiedRevocationPage{}, errors.New("node: outer revocation sequence differs from signed payload")
		}
		if body.Seq <= previous {
			return verifiedRevocationPage{}, errors.New("node: revocation sequence did not advance")
		}
		previous = body.Seq
		entry := VerifiedUserRevocation{
			Seq: body.Seq, AccountID: body.AccountId, FamilyID: body.FamilyId, RevokedAt: body.RevokedAt,
			RevokeTokensIssuedBefore: body.RevokeTokensIssuedBefore,
			RevokedTokens:            make([]VerifiedRevokedToken, 0, len(body.RevokedTokens)),
		}
		for _, revoked := range body.RevokedTokens {
			if revoked == nil {
				return verifiedRevocationPage{}, errors.New("node: nil revoked token")
			}
			entry.RevokedTokens = append(entry.RevokedTokens, VerifiedRevokedToken{TokenID: revoked.TokenId, RetainUntil: revoked.RetainUntil})
		}
		entries = append(entries, entry)
	}
	if err := validateUserRevocationPage(entries, c.store.Checkpoint()); err != nil {
		return verifiedRevocationPage{}, err
	}
	return verifiedRevocationPage{entries: entries, hasMore: outer.HasMore, verifiedAt: verifiedAt}, nil
}
