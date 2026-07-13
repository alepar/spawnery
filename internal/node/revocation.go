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

	"spawnery/internal/authsvc/token"
)

const (
	maxRevocationFeedSize    = 32 << 20
	maxRevocationFeedEntries = 100_000
)

// RevocationConsumer is migrated to bounded protobuf pages in the next change. Keeping it in a
// separate file from SQLite state makes the storage transition independently testable.
type RevocationConsumer struct {
	doer      httpDoer
	feedURL   *url.URL
	artifacts *token.Verifier
	store     *UserRevocationStore
	interval  time.Duration
	now       func() time.Time
}

func NewRevocationConsumer(doer httpDoer, feedURL string, artifacts *token.Verifier, store *UserRevocationStore, interval time.Duration) (*RevocationConsumer, error) {
	parsed, err := url.Parse(feedURL)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.RawQuery != "" || parsed.Fragment != "" || doer == nil || artifacts == nil || store == nil || interval <= 0 {
		return nil, errors.New("node: invalid revocation consumer configuration")
	}
	return &RevocationConsumer{doer: doer, feedURL: parsed, artifacts: artifacts, store: store, interval: interval, now: time.Now}, nil
}

func (c *RevocationConsumer) Run(ctx context.Context, onApplied func([]VerifiedUserRevocation)) {
	for {
		if err := c.pollOnce(ctx, onApplied); err != nil && ctx.Err() == nil {
			slog.Warn("node: revocation feed poll failed", "err", err)
		}
		timer := time.NewTimer(c.interval)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			return
		case <-timer.C:
		}
	}
}

type signedUserRevocationEntry struct {
	Seq       int64  `json:"seq"`
	AccountID string `json:"account_id"`
	FamilyID  string `json:"family_id"`
	TokenIDs  string `json:"token_ids"`
	RevokedAt int64  `json:"revoked_at"`
	Sig       string `json:"sig"`
}

type verifiedUserRevocationPayload struct {
	Seq       int64  `json:"seq"`
	AccountID string `json:"account_id"`
	FamilyID  string `json:"family_id"`
	TokenIDs  string `json:"token_ids"`
	RevokedAt int64  `json:"revoked_at"`
}

func (c *RevocationConsumer) pollOnce(ctx context.Context, onApplied func([]VerifiedUserRevocation)) error {
	u := *c.feedURL
	query := u.Query()
	query.Set("since", strconv.FormatInt(c.store.Checkpoint(), 10))
	u.RawQuery = query.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return err
	}
	resp, err := c.doer.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("node: revocation feed status %d", resp.StatusCode)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxRevocationFeedSize+1))
	if err != nil {
		return fmt.Errorf("node: read revocation feed: %w", err)
	}
	if len(raw) > maxRevocationFeedSize {
		return errors.New("node: revocation feed is too large")
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var outer []signedUserRevocationEntry
	if err := dec.Decode(&outer); err != nil {
		return fmt.Errorf("node: decode revocation feed: %w", err)
	}
	var extra any
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		return errors.New("node: trailing revocation feed data")
	}
	if len(outer) > maxRevocationFeedEntries {
		return errors.New("node: revocation feed has too many entries")
	}
	if len(outer) == 0 {
		return nil
	}
	batch := make([]VerifiedUserRevocation, 0, len(outer))
	now := c.now()
	for _, entry := range outer {
		payload, err := c.artifacts.Verify(entry.Sig, token.ArtifactTypeRevocation, now)
		if err != nil {
			return fmt.Errorf("node: verify revocation entry: %w", err)
		}
		payloadDecoder := json.NewDecoder(bytes.NewReader(payload))
		payloadDecoder.DisallowUnknownFields()
		var verified verifiedUserRevocationPayload
		if err := payloadDecoder.Decode(&verified); err != nil {
			return fmt.Errorf("node: decode verified revocation entry: %w", err)
		}
		var payloadExtra any
		if err := payloadDecoder.Decode(&payloadExtra); !errors.Is(err, io.EOF) {
			return errors.New("node: trailing verified revocation payload")
		}
		if entry.Seq != verified.Seq {
			return errors.New("node: outer revocation sequence differs from signed payload")
		}
		var ids []string
		idsDecoder := json.NewDecoder(bytes.NewReader([]byte(verified.TokenIDs)))
		if err := idsDecoder.Decode(&ids); err != nil {
			return errors.New("node: invalid verified revocation token_ids")
		}
		if ids == nil {
			return errors.New("node: verified revocation token_ids must be an array")
		}
		var idsExtra any
		if err := idsDecoder.Decode(&idsExtra); !errors.Is(err, io.EOF) {
			return errors.New("node: trailing verified revocation token_ids")
		}
		batch = append(batch, VerifiedUserRevocation{Seq: verified.Seq, AccountID: verified.AccountID, FamilyID: verified.FamilyID, TokenIDs: ids, RevokedAt: verified.RevokedAt})
	}
	beforeApply := c.store.Checkpoint()
	applyErr := c.store.ApplyBatch(batch)
	afterApply := c.store.Checkpoint()
	applied := afterApply > beforeApply && afterApply == batch[len(batch)-1].Seq
	if applied && onApplied != nil {
		onApplied(batch)
	}
	return applyErr
}
