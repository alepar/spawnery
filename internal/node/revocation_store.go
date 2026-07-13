package node

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	_ "modernc.org/sqlite"
)

const userRevocationSchemaVersion = 1

var (
	ErrUserRevocationStoreLocked   = errors.New("node: user-revocation store is already open")
	ErrUserRevocationStoreClosed   = errors.New("node: user-revocation store is closed")
	ErrUserRevocationStorePoisoned = errors.New("node: user-revocation store persistence is ambiguous")
)

type VerifiedRevokedToken struct {
	TokenID     string
	RetainUntil int64
}

type VerifiedUserRevocation struct {
	Seq                      int64
	AccountID                string
	FamilyID                 string
	RevokedAt                int64
	RevokedTokens            []VerifiedRevokedToken
	RevokeTokensIssuedBefore int64
}

type userRevocationSnapshot struct {
	checkpoint int64
	tokens     map[string]int64
	accounts   map[string]int64
}

type UserRevocationStore struct {
	mu       sync.Mutex
	path     string
	db       *sql.DB
	lockFile *os.File
	now      func() time.Time
	snapshot atomic.Pointer[userRevocationSnapshot]
	closed   bool
	poisoned error

	// Transaction-boundary seams used only by durability tests.
	beforeRename func() error
	afterRename  func() error
}

func OpenUserRevocationStore(path string, clocks ...func() time.Time) (*UserRevocationStore, error) {
	if path == "" || len(clocks) > 1 {
		return nil, errors.New("node: invalid user-revocation store configuration")
	}
	now := time.Now
	if len(clocks) == 1 {
		if clocks[0] == nil {
			return nil, errors.New("node: nil user-revocation clock")
		}
		now = clocks[0]
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
	if info, err := os.Lstat(path); err == nil {
		if !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 {
			return nil, errors.New("node: user-revocation database must be a regular 0600 file")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}

	dsnURL := &url.URL{Scheme: "file", Path: path}
	query := dsnURL.Query()
	query.Add("_pragma", "busy_timeout(5000)")
	query.Add("_pragma", "foreign_keys(1)")
	query.Add("_pragma", "synchronous(FULL)")
	dsnURL.RawQuery = query.Encode()
	db, err := sql.Open("sqlite", dsnURL.String())
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	closeDB := true
	defer func() {
		if closeDB {
			_ = db.Close()
		}
	}()
	if err := db.Ping(); err != nil {
		return nil, fmt.Errorf("node: open user-revocation database: %w", err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		return nil, err
	}
	if err := ensureUserRevocationSchema(db); err != nil {
		return nil, err
	}
	store := &UserRevocationStore{path: path, db: db, lockFile: lock, now: now}
	snapshot, err := store.pruneAndLoad(context.Background(), now().Unix())
	if err != nil {
		return nil, err
	}
	store.snapshot.Store(snapshot)
	release = false
	closeDB = false
	return store, nil
}

func ensureUserRevocationSchema(db *sql.DB) error {
	var version int
	if err := db.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		return fmt.Errorf("node: read user-revocation schema: %w", err)
	}
	if version == 0 {
		var objects int
		if err := db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type IN ('table','index','trigger') AND name NOT LIKE 'sqlite_%'`).Scan(&objects); err != nil {
			return err
		}
		if objects != 0 {
			return errors.New("node: unversioned user-revocation database")
		}
		tx, err := db.Begin()
		if err != nil {
			return err
		}
		defer func() { _ = tx.Rollback() }()
		statements := []string{
			`CREATE TABLE revocation_meta (singleton INTEGER PRIMARY KEY CHECK(singleton = 1), checkpoint INTEGER NOT NULL CHECK(checkpoint >= 0))`,
			`INSERT INTO revocation_meta(singleton, checkpoint) VALUES (1, 0)`,
			`CREATE TABLE revoked_tokens (token_id TEXT PRIMARY KEY, retain_until INTEGER NOT NULL CHECK(retain_until > 0))`,
			`CREATE INDEX idx_revoked_tokens_expiry ON revoked_tokens(retain_until)`,
			`CREATE TABLE account_cutoffs (account_id TEXT PRIMARY KEY, revoke_tokens_issued_before INTEGER NOT NULL CHECK(revoke_tokens_issued_before > 0))`,
			fmt.Sprintf("PRAGMA user_version = %d", userRevocationSchemaVersion),
		}
		for _, statement := range statements {
			if _, err := tx.Exec(statement); err != nil {
				return fmt.Errorf("node: create user-revocation schema: %w", err)
			}
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("node: commit user-revocation schema: %w", err)
		}
		version = userRevocationSchemaVersion
	}
	if version != userRevocationSchemaVersion {
		return fmt.Errorf("node: unsupported user-revocation schema %d", version)
	}
	for _, query := range []string{
		`SELECT checkpoint FROM revocation_meta WHERE singleton = 1`,
		`SELECT token_id, retain_until FROM revoked_tokens LIMIT 0`,
		`SELECT account_id, revoke_tokens_issued_before FROM account_cutoffs LIMIT 0`,
	} {
		rows, err := db.Query(query)
		if err != nil {
			return fmt.Errorf("node: validate user-revocation schema: %w", err)
		}
		rows.Close()
	}
	return nil
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
	dbErr := s.db.Close()
	lockErr := releaseUserRevocationLock(s.lockFile)
	s.lockFile = nil
	if dbErr != nil {
		return dbErr
	}
	return lockErr
}

func (s *UserRevocationStore) Checkpoint() int64 {
	if s == nil || s.snapshot.Load() == nil {
		return 0
	}
	return s.snapshot.Load().checkpoint
}

func (s *UserRevocationStore) IsRevoked(tokenID, accountID string, issuedAt int64) bool {
	if s == nil || s.snapshot.Load() == nil {
		return false
	}
	snapshot := s.snapshot.Load()
	if retainUntil, ok := snapshot.tokens[tokenID]; ok && s.now().Unix() < retainUntil {
		return true
	}
	cutoff, ok := snapshot.accounts[accountID]
	return ok && issuedAt < cutoff
}

func (s *UserRevocationStore) ApplyPage(page []VerifiedUserRevocation, now time.Time) error {
	if s == nil {
		return ErrUserRevocationStoreClosed
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return ErrUserRevocationStoreClosed
	}
	if s.poisoned != nil {
		return s.poisoned
	}
	checkpoint := s.snapshot.Load().checkpoint
	if err := validateUserRevocationPage(page, checkpoint); err != nil {
		return err
	}
	tx, err := s.db.BeginTx(context.Background(), nil)
	if err != nil {
		return err
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()
	tokenStatement, err := tx.Prepare(`
		INSERT INTO revoked_tokens(token_id, retain_until) VALUES (?, ?)
		ON CONFLICT(token_id) DO UPDATE SET retain_until = MAX(revoked_tokens.retain_until, excluded.retain_until)`)
	if err != nil {
		return err
	}
	defer tokenStatement.Close()
	cutoffStatement, err := tx.Prepare(`
		INSERT INTO account_cutoffs(account_id, revoke_tokens_issued_before) VALUES (?, ?)
		ON CONFLICT(account_id) DO UPDATE SET revoke_tokens_issued_before = MAX(account_cutoffs.revoke_tokens_issued_before, excluded.revoke_tokens_issued_before)`)
	if err != nil {
		return err
	}
	defer cutoffStatement.Close()
	for _, entry := range page {
		for _, token := range entry.RevokedTokens {
			if _, err := tokenStatement.Exec(token.TokenID, token.RetainUntil); err != nil {
				return err
			}
		}
		if entry.RevokeTokensIssuedBefore > 0 {
			if _, err := cutoffStatement.Exec(entry.AccountID, entry.RevokeTokensIssuedBefore); err != nil {
				return err
			}
		}
	}
	if len(page) > 0 {
		if _, err := tx.Exec(`UPDATE revocation_meta SET checkpoint = ? WHERE singleton = 1`, page[len(page)-1].Seq); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(`DELETE FROM revoked_tokens WHERE retain_until <= ?`, now.Unix()); err != nil {
		return err
	}
	if s.beforeRename != nil {
		if err := s.beforeRename(); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		s.poisoned = fmt.Errorf("%w: %v", ErrUserRevocationStorePoisoned, err)
		return s.poisoned
	}
	committed = true
	if s.afterRename != nil {
		if err := s.afterRename(); err != nil {
			s.poisoned = fmt.Errorf("%w: %v", ErrUserRevocationStorePoisoned, err)
			return s.poisoned
		}
	}
	snapshot, err := s.loadSnapshot(context.Background())
	if err != nil {
		s.poisoned = fmt.Errorf("%w: refresh snapshot: %v", ErrUserRevocationStorePoisoned, err)
		return s.poisoned
	}
	s.snapshot.Store(snapshot)
	return nil
}

func validateUserRevocationPage(page []VerifiedUserRevocation, checkpoint int64) error {
	previous := checkpoint
	for _, entry := range page {
		if entry.Seq <= previous || entry.AccountID == "" || entry.RevokedAt <= 0 {
			return errors.New("node: invalid user revocation sequence or identity")
		}
		previous = entry.Seq
		if entry.FamilyID == "" {
			if entry.RevokeTokensIssuedBefore <= 0 {
				return errors.New("node: account revocation cutoff required")
			}
		} else if entry.RevokeTokensIssuedBefore != 0 || len(entry.RevokedTokens) == 0 {
			return errors.New("node: invalid family revocation")
		}
		seen := make(map[string]struct{}, len(entry.RevokedTokens))
		for _, token := range entry.RevokedTokens {
			if token.TokenID == "" || token.RetainUntil <= entry.RevokedAt {
				return errors.New("node: invalid revoked token")
			}
			if _, ok := seen[token.TokenID]; ok {
				return errors.New("node: duplicate revoked token")
			}
			seen[token.TokenID] = struct{}{}
		}
	}
	return nil
}

func (s *UserRevocationStore) pruneAndLoad(ctx context.Context, now int64) (*userRevocationSnapshot, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `DELETE FROM revoked_tokens WHERE retain_until <= ?`, now); err != nil {
		return nil, err
	}
	snapshot, err := loadUserRevocationSnapshot(ctx, tx)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return snapshot, nil
}

func (s *UserRevocationStore) loadSnapshot(ctx context.Context) (*userRevocationSnapshot, error) {
	return loadUserRevocationSnapshot(ctx, s.db)
}

type revocationQueryer interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}

func loadUserRevocationSnapshot(ctx context.Context, db revocationQueryer) (*userRevocationSnapshot, error) {
	snapshot := &userRevocationSnapshot{tokens: map[string]int64{}, accounts: map[string]int64{}}
	if err := db.QueryRowContext(ctx, `SELECT checkpoint FROM revocation_meta WHERE singleton = 1`).Scan(&snapshot.checkpoint); err != nil {
		return nil, err
	}
	tokenRows, err := db.QueryContext(ctx, `SELECT token_id, retain_until FROM revoked_tokens`)
	if err != nil {
		return nil, err
	}
	for tokenRows.Next() {
		var tokenID string
		var retainUntil int64
		if err := tokenRows.Scan(&tokenID, &retainUntil); err != nil {
			tokenRows.Close()
			return nil, err
		}
		snapshot.tokens[tokenID] = retainUntil
	}
	if err := tokenRows.Close(); err != nil {
		return nil, err
	}
	accountRows, err := db.QueryContext(ctx, `SELECT account_id, revoke_tokens_issued_before FROM account_cutoffs`)
	if err != nil {
		return nil, err
	}
	for accountRows.Next() {
		var accountID string
		var cutoff int64
		if err := accountRows.Scan(&accountID, &cutoff); err != nil {
			accountRows.Close()
			return nil, err
		}
		snapshot.accounts[accountID] = cutoff
	}
	if err := accountRows.Close(); err != nil {
		return nil, err
	}
	return snapshot, nil
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
