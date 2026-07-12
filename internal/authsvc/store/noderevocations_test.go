package store

import (
	"database/sql"
	"errors"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/pressly/goose/v3"
	_ "modernc.org/sqlite"
)

func TestNodeRevocationsRevokeAndListSorted(t *testing.T) {
	st := NewTestStore(t)
	ctx := ctxT()

	if _, err := st.NodeRevocations().Revoke(ctx, NodeRevocation{NodeID: "node-b", IssuerSerial: "aa", LeafSerial: "bb", Reason: "stolen host", RevokedAt: 200}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.NodeRevocations().Revoke(ctx, NodeRevocation{NodeID: "node-a", IssuerSerial: "aa", LeafSerial: "cc", Reason: "decommissioned", RevokedAt: 100}); err != nil {
		t.Fatal(err)
	}

	revoked, err := st.NodeRevocations().IsRevoked(ctx, "node-a")
	if err != nil {
		t.Fatal(err)
	}
	if !revoked {
		t.Fatal("node-a should be revoked")
	}

	revoked, err = st.NodeRevocations().IsRevoked(ctx, "node-z")
	if err != nil {
		t.Fatal(err)
	}
	if revoked {
		t.Fatal("node-z should not be revoked")
	}

	rows, err := st.NodeRevocations().List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got := []string{rows[0].NodeID, rows[1].NodeID}; !reflect.DeepEqual(got, []string{"node-a", "node-b"}) {
		t.Fatalf("sorted node ids = %v", got)
	}
	if rows[0].IssuerSerial != "aa" || rows[0].LeafSerial != "cc" {
		t.Fatalf("certificate identity not stored: %+v", rows[0])
	}
}

func TestNodeRevocationMigrationUpgradesLegacyRowsWithoutCertificateAuthority(t *testing.T) {
	path := filepath.Join(t.TempDir(), "authsvc.db")
	dsn := "file:" + path
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatal(err)
	}
	goose.SetBaseFS(migrationsFS)
	goose.SetLogger(goose.NopLogger())
	if err := goose.SetDialect("sqlite3"); err != nil {
		t.Fatal(err)
	}
	if err := goose.UpTo(db, "migrations/sqlite", 3); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO node_revocations (node_id, reason, revoked_at) VALUES ('legacy-node', 'lost', 100)`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	st, err := Open(ctxT(), Config{Driver: "sqlite", DSN: dsn})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	legacy, err := st.NodeRevocations().Get(ctxT(), "legacy-node")
	if err != nil || legacy.IssuerSerial != "" || legacy.LeafSerial != "" {
		t.Fatalf("legacy row = %+v, %v", legacy, err)
	}
	rows, err := st.NodeRevocations().ListByIssuer(ctxT(), "aa")
	if err != nil || len(rows) != 0 {
		t.Fatalf("legacy row entered certificate authority view: %+v, %v", rows, err)
	}
	if _, err := st.NodeRevocations().GetCRL(ctxT(), "aa"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("legacy row synthesized CRL checkpoint: %v", err)
	}
}

func TestNodeRevocationsRevokeIsIdempotentUpdate(t *testing.T) {
	st := NewTestStore(t)
	ctx := ctxT()

	inserted, err := st.NodeRevocations().Revoke(ctx, NodeRevocation{NodeID: "node-a", IssuerSerial: "aa", LeafSerial: "bb", Reason: "old", RevokedAt: 100})
	if err != nil || !inserted {
		t.Fatal(err)
	}
	inserted, err = st.NodeRevocations().Revoke(ctx, NodeRevocation{NodeID: "node-a", IssuerSerial: "aa", LeafSerial: "bb", Reason: "new", RevokedAt: 200})
	if err != nil || inserted {
		t.Fatal(err)
	}

	rows, err := st.NodeRevocations().List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Reason != "new" || rows[0].RevokedAt != 100 {
		t.Fatalf("rows = %+v", rows)
	}
}

func TestNodeRevocationsPersistIssuerCRLInsideTransaction(t *testing.T) {
	st := NewTestStore(t)
	ctx := ctxT()
	want := NodeRevocationCRL{IssuerSerial: "aa", Number: "7", PEM: []byte("crl")}
	if err := st.WithTx(ctx, func(tx Store) error {
		if _, err := tx.NodeRevocations().Revoke(ctx, NodeRevocation{NodeID: "node-a", IssuerSerial: "aa", LeafSerial: "bb", RevokedAt: 100}); err != nil {
			return err
		}
		return tx.NodeRevocations().PutCRL(ctx, want)
	}); err != nil {
		t.Fatal(err)
	}
	got, err := st.NodeRevocations().GetCRL(ctx, "aa")
	if err != nil || got.Number != want.Number || string(got.PEM) != string(want.PEM) {
		t.Fatalf("CRL = %+v, %v", got, err)
	}
	if _, err := st.NodeRevocations().GetCRL(ctx, "missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing CRL error = %v", err)
	}
}
