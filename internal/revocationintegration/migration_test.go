package revocationintegration_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/pressly/goose/v3/database"
	_ "modernc.org/sqlite"

	"spawnery/internal/authsvc"
	"spawnery/internal/authsvc/store"
	"spawnery/internal/authsvc/token"
	cpauth "spawnery/internal/cp/auth"
	"spawnery/internal/mtls"
	"spawnery/internal/node"
	"spawnery/internal/pki"
)

type unusedGitHubProvider struct{}

func (unusedGitHubProvider) AuthorizeURL(string, string, string) string { return "" }
func (unusedGitHubProvider) Exchange(context.Context, string, string, string) (string, error) {
	return "", errors.New("unused")
}
func (unusedGitHubProvider) FetchUser(context.Context, string) (authsvc.GitHubUser, error) {
	return authsvc.GitHubUser{}, errors.New("unused")
}
func (unusedGitHubProvider) RefreshUserAccessToken(context.Context, string) (authsvc.GitHubUserToken, error) {
	return authsvc.GitHubUserToken{}, errors.New("unused")
}

func TestSchemaSevenAccountRevocationMigratesThroughNodeAndCP(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	dsn := "file:" + filepath.Join(t.TempDir(), "authsvc.db")
	seedSchemaSeven(t, dsn, now.Unix()-10)

	st, err := store.Open(t.Context(), store.Config{Driver: "sqlite", DSN: dsn})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	root, err := pki.NewRootCA("revocation migration root")
	if err != nil {
		t.Fatal(err)
	}
	signer, err := authsvc.NewDevelopmentSigningCredential(root, "prod", now)
	if err != nil {
		t.Fatal(err)
	}
	idp, err := authsvc.NewIdP(authsvc.IdPConfig{
		Store: st, GitHub: unusedGitHubProvider{}, Signer: signer, Now: func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	service := authsvc.New(root.Cert, nil, authsvc.WithIdP(idp))
	feed := httptest.NewTLSServer(service.InternalHandler(mtls.Policy{
		"anonymous": {"authsvc.revocations": {}},
	}))
	defer feed.Close()
	verifier, err := token.NewVerifier(root.Cert, "prod", nil)
	if err != nil {
		t.Fatal(err)
	}

	nodeStore, err := node.OpenUserRevocationStore(filepath.Join(t.TempDir(), "node", "revocations.db"), func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	defer nodeStore.Close()
	nodeConsumer, err := node.NewRevocationConsumer(feed.Client(), feed.URL+"/revocations", verifier, nodeStore, time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	cpRegistry := cpauth.NewRevocationRegistry(nil)
	cpConsumer := cpauth.NewFeedPoller(feed.Client(), feed.URL+"/revocations", verifier, cpRegistry, time.Millisecond)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go nodeConsumer.Run(ctx, nil)
	go cpConsumer.Run(ctx)

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		explicitAtCutoff := now.Unix() - 10
		older := explicitAtCutoff - 1
		if nodeStore.IsRevoked("same-second", "acct", explicitAtCutoff) &&
			nodeStore.IsRevoked("other", "acct", older) &&
			cpRegistry.IsRevoked("same-second", "acct", explicitAtCutoff, now) &&
			cpRegistry.IsRevoked("other", "acct", older, now) {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("migrated account revocation did not converge through node and CP consumers")
}

func seedSchemaSeven(t *testing.T, dsn string, revokedAt int64) {
	t.Helper()
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, statement := range []string{
		`CREATE TABLE users (
			account_id TEXT PRIMARY KEY, github_sub INTEGER NOT NULL UNIQUE, handle TEXT NOT NULL,
			status TEXT NOT NULL, created_at INTEGER NOT NULL)`,
		`CREATE TABLE refresh_sessions (
			token_hash TEXT PRIMARY KEY, account_id TEXT NOT NULL REFERENCES users(account_id), family_id TEXT NOT NULL,
			client_kind TEXT NOT NULL CHECK (client_kind IN ('web','cli')), session_pubkey_spki BLOB NOT NULL,
			cp_access_token_id TEXT NOT NULL, node_access_token_id TEXT NOT NULL, created_at INTEGER NOT NULL,
			last_used_at INTEGER NOT NULL, expires_at INTEGER NOT NULL, family_created_at INTEGER NOT NULL,
			superseded_by TEXT, superseded_at INTEGER, successor_cache TEXT, revoked INTEGER NOT NULL DEFAULT 0)`,
		`CREATE TABLE revocation_events (
			seq INTEGER PRIMARY KEY AUTOINCREMENT, account_id TEXT NOT NULL, family_id TEXT NOT NULL,
			token_ids TEXT NOT NULL, revoked_at INTEGER NOT NULL)`,
		`INSERT INTO users VALUES ('acct', 1, 'alice', 'active', 1)`,
		`INSERT INTO revocation_events(seq, account_id, family_id, token_ids, revoked_at)
			VALUES (7, 'acct', '', '["same-second"]', ` + sqlInt(revokedAt) + `)`,
	} {
		if _, err := db.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	versions, err := database.NewStore(database.DialectSQLite3, "goose_db_version")
	if err != nil {
		t.Fatal(err)
	}
	if err := versions.CreateVersionTable(t.Context(), db); err != nil {
		t.Fatal(err)
	}
	for version := int64(0); version <= 7; version++ {
		if err := versions.Insert(t.Context(), db, database.InsertRequest{Version: version}); err != nil {
			t.Fatal(err)
		}
	}
}

func sqlInt(value int64) string {
	return fmt.Sprintf("%d", value)
}
