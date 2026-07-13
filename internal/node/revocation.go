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
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"spawnery/internal/authsvc/token"
)

const (
	userRevocationStoreVersion = 1
	maxUserRevocationStateSize = 32 << 20
	maxRevocationFeedSize      = 32 << 20
	maxRevocationFeedEntries   = 100_000
)

var (
	ErrUserRevocationStoreLocked   = errors.New("node: user-revocation store is already open")
	ErrUserRevocationStoreClosed   = errors.New("node: user-revocation store is closed")
	ErrUserRevocationStorePoisoned = errors.New("node: user-revocation store persistence is ambiguous")
)

type VerifiedUserRevocation struct {
	Seq       int64
	AccountID string
	FamilyID  string
	TokenIDs  []string
	RevokedAt int64
}

type userRevocationSnapshot struct {
	checkpoint int64
	tokens     map[string]struct{}
	accounts   map[string]struct{}
}

type persistedUserRevocations struct {
	Version    int      `json:"version"`
	Checkpoint int64    `json:"checkpoint"`
	TokenIDs   []string `json:"token_ids"`
	AccountIDs []string `json:"account_ids"`
}

type UserRevocationStore struct {
	mu           sync.Mutex
	path         string
	lockFile     *os.File
	snapshot     atomic.Pointer[userRevocationSnapshot]
	closed       bool
	poisoned     error
	beforeRename func() error
	afterRename  func() error
}

func OpenUserRevocationStore(path string) (*UserRevocationStore, error) {
	if path == "" {
		return nil, errors.New("node: missing user-revocation state path")
	}
	dir := filepath.Dir(path)
	if err := ensureUserRevocationDirectory(dir); err != nil {
		return nil, err
	}
	lock, err := acquireUserRevocationLock(path + ".lock")
	if err != nil {
		return nil, err
	}
	release := true
	defer func() {
		if release {
			_ = releaseUserRevocationLock(lock)
		}
	}()
	snapshot := &userRevocationSnapshot{tokens: map[string]struct{}{}, accounts: map[string]struct{}{}}
	if record, err := readUserRevocationState(path); err == nil {
		snapshot.checkpoint = record.Checkpoint
		for _, id := range record.TokenIDs {
			snapshot.tokens[id] = struct{}{}
		}
		for _, id := range record.AccountIDs {
			snapshot.accounts[id] = struct{}{}
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	store := &UserRevocationStore{path: path, lockFile: lock}
	store.snapshot.Store(snapshot)
	release = false
	return store, nil
}

func (s *UserRevocationStore) Close() error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	err := releaseUserRevocationLock(s.lockFile)
	s.lockFile = nil
	return err
}

func (s *UserRevocationStore) Checkpoint() int64 {
	if s == nil || s.snapshot.Load() == nil {
		return 0
	}
	return s.snapshot.Load().checkpoint
}

func (s *UserRevocationStore) IsRevoked(tokenID, accountID string) bool {
	if s == nil || s.snapshot.Load() == nil {
		return false
	}
	snapshot := s.snapshot.Load()
	_, tokenDenied := snapshot.tokens[tokenID]
	_, accountDenied := snapshot.accounts[accountID]
	return tokenDenied || accountDenied
}

func (s *UserRevocationStore) ApplyBatch(batch []VerifiedUserRevocation) error {
	if s == nil {
		return ErrUserRevocationStoreClosed
	}
	if len(batch) == 0 {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return ErrUserRevocationStoreClosed
	}
	if s.poisoned != nil {
		return s.poisoned
	}
	current := s.snapshot.Load()
	previous := current.checkpoint
	seen := make(map[string]struct{})
	for _, entry := range batch {
		if entry.Seq <= previous || entry.AccountID == "" || entry.RevokedAt < 0 {
			return errors.New("node: invalid user revocation sequence or identity")
		}
		previous = entry.Seq
		for _, id := range entry.TokenIDs {
			if id == "" {
				return errors.New("node: empty revoked token id")
			}
			if _, exists := seen[id]; exists {
				return errors.New("node: duplicate revoked token id")
			}
			seen[id] = struct{}{}
		}
	}
	candidate := &userRevocationSnapshot{checkpoint: previous, tokens: cloneStringSet(current.tokens), accounts: cloneStringSet(current.accounts)}
	for _, entry := range batch {
		for _, id := range entry.TokenIDs {
			candidate.tokens[id] = struct{}{}
		}
		if entry.FamilyID == "" {
			candidate.accounts[entry.AccountID] = struct{}{}
		}
	}
	renamed := false
	if err := s.persist(candidate, func() { s.snapshot.Store(candidate); renamed = true }); err != nil {
		if renamed {
			s.poisoned = fmt.Errorf("%w: %w", ErrUserRevocationStorePoisoned, err)
			return s.poisoned
		}
		return err
	}
	return nil
}

func cloneStringSet(in map[string]struct{}) map[string]struct{} {
	out := make(map[string]struct{}, len(in))
	for k := range in {
		out[k] = struct{}{}
	}
	return out
}
func sortedStringSet(in map[string]struct{}) []string {
	out := make([]string, 0, len(in))
	for k := range in {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func (s *UserRevocationStore) persist(snapshot *userRevocationSnapshot, renamed func()) error {
	record := persistedUserRevocations{Version: userRevocationStoreVersion, Checkpoint: snapshot.checkpoint, TokenIDs: sortedStringSet(snapshot.tokens), AccountIDs: sortedStringSet(snapshot.accounts)}
	raw, err := json.Marshal(record)
	if err != nil {
		return err
	}
	raw = append(raw, '\n')
	dir := filepath.Dir(s.path)
	tmp, err := os.CreateTemp(dir, ".user-revocations-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	remove := true
	defer func() {
		_ = tmp.Close()
		if remove {
			_ = os.Remove(tmpPath)
		}
	}()
	if err := tmp.Chmod(0o600); err != nil {
		return err
	}
	if _, err := tmp.Write(raw); err != nil {
		return err
	}
	if err := tmp.Sync(); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if s.beforeRename != nil {
		if err := s.beforeRename(); err != nil {
			return err
		}
	}
	if err := os.Rename(tmpPath, s.path); err != nil {
		return err
	}
	remove = false
	renamed()
	if s.afterRename != nil {
		if err := s.afterRename(); err != nil {
			return err
		}
	}
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

func ensureUserRevocationDirectory(path string) error {
	if err := os.MkdirAll(path, 0o700); err != nil {
		return err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode().Perm() != 0o700 {
		return errors.New("node: user-revocation directory must be private")
	}
	return nil
}

func acquireUserRevocationLock(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	info, statErr := f.Stat()
	if statErr != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 {
		_ = f.Close()
		return nil, errors.New("node: user-revocation lock must be a regular 0600 file")
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
			return nil, ErrUserRevocationStoreLocked
		}
		return nil, err
	}
	return f, nil
}

func releaseUserRevocationLock(f *os.File) error {
	if f == nil {
		return nil
	}
	unlock := syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	closeErr := f.Close()
	if unlock != nil {
		return unlock
	}
	return closeErr
}

func readUserRevocationState(path string) (*persistedUserRevocations, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 || info.Size() > maxUserRevocationStateSize {
		return nil, errors.New("node: invalid persisted user-revocation state file")
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	dec := json.NewDecoder(io.LimitReader(f, maxUserRevocationStateSize+1))
	dec.DisallowUnknownFields()
	var record persistedUserRevocations
	if err := dec.Decode(&record); err != nil {
		return nil, fmt.Errorf("node: decode persisted user revocations: %w", err)
	}
	var extra any
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		return nil, errors.New("node: trailing persisted user-revocation data")
	}
	if record.Version != userRevocationStoreVersion || record.Checkpoint < 0 {
		return nil, errors.New("node: invalid persisted user-revocation state")
	}
	if !strictUniqueStrings(record.TokenIDs) || !strictUniqueStrings(record.AccountIDs) {
		return nil, errors.New("node: invalid persisted user-revocation deny set")
	}
	return &record, nil
}

func strictUniqueStrings(values []string) bool {
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		if value == "" {
			return false
		}
		if _, ok := seen[value]; ok {
			return false
		}
		seen[value] = struct{}{}
	}
	return true
}

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
	if err := c.store.ApplyBatch(batch); err != nil {
		return err
	}
	if onApplied != nil {
		onApplied(batch)
	}
	return nil
}
