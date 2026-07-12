package store_test

// Tests for the safe-delete primitives added by sp-mwco.3.3: the portable row-lock idiom
// (LockRow/LockBundle/LockVersion), the bundle-ref reference-check queries backing
// DeleteBundle/DeleteBundleVersion's counts-only guard, and the lock's serialization guarantee
// under two concurrent transactions.

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"

	"spawnery/internal/cp/store"
)

// --- LockRow (customization_catalog) ----------------------------------------------------------

// TestLockRow_OutsideTxRejected proves LockRow refuses to run against the top-level store (not
// inside WithTx) — the guard that makes "called it wrong" a loud error instead of a silent no-op.
func TestLockRow_OutsideTxRejected(t *testing.T) {
	st := store.NewTestStore(t)
	ctx := context.Background()

	if err := st.CustomizationCatalog().Create(ctx, makeEntry("cat-1", "alice", "sk")); err != nil {
		t.Fatalf("seed: %v", err)
	}

	if err := st.CustomizationCatalog().LockRow(ctx, "cat-1"); err == nil {
		t.Fatal("expected an error locking outside WithTx, got nil")
	}
}

// TestLockRow_MissingRow_ErrNotFound proves LockRow doubles as the existence check inside a tx.
func TestLockRow_MissingRow_ErrNotFound(t *testing.T) {
	st := store.NewTestStore(t)
	ctx := context.Background()

	err := st.WithTx(ctx, func(tx store.Store) error {
		return tx.CustomizationCatalog().LockRow(ctx, "no-such")
	})
	if !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got: %v", err)
	}
}

// newLockTestStore opens a dedicated store (NOT store.NewTestStore's shared DSN — that DSN is
// blast radius for every store test) backed by a real temp file with `_txlock=immediate` and a
// busy_timeout, so a second writer transaction blocks until the first commits. This is the
// concurrency mode LockRow depends on (sp-mwco.3.3 §2).
//
// Deliberately NOT `mode=memory&cache=shared`: SQLite's shared-cache mode uses per-table locks
// that, on contention between two connections in the same process, return
// SQLITE_LOCKED_SHAREDCACHE immediately — busy_timeout's retry loop does not apply to that code
// path, only to ordinary SQLITE_BUSY from real (file-level) lock contention. A temp file gives the
// same busy_timeout-blocks-until-commit behavior a real deployment (file-backed SQLite, or
// Postgres row locks) actually has.
func newLockTestStore(t *testing.T, name string) store.Store {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), name+".sqlite")
	dsn := "file:" + dbPath + "?_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)&_txlock=immediate"
	st, err := store.Open(context.Background(), store.Config{Driver: "sqlite", DSN: dsn})
	if err != nil {
		t.Fatalf("open lock test store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

// TestLockRow_SerializesTwoTxs is the load-bearing proof for §2's lock primitive: two concurrent
// transactions both taking LockRow on the same catalog row must serialize (never interleave), and
// neither must observe a BUSY/driver error. We assert the *invariant* (a causal ordering in a
// shared, mutex-guarded log) rather than a specific winner.
func TestLockRow_SerializesTwoTxs(t *testing.T) {
	st := newLockTestStore(t, "TestLockRow_SerializesTwoTxs")
	ctx := context.Background()

	if err := st.CustomizationCatalog().Create(ctx, makeEntry("cat-1", "alice", "sk")); err != nil {
		t.Fatalf("seed: %v", err)
	}

	var mu sync.Mutex
	var log []string
	record := func(s string) {
		mu.Lock()
		log = append(log, s)
		mu.Unlock()
	}

	locked1 := make(chan struct{})
	proceed1 := make(chan struct{})

	var wg sync.WaitGroup
	wg.Add(2)
	var err1, err2 error

	// tx1: acquires the lock, signals it holds it, waits to be released, then commits.
	go func() {
		defer wg.Done()
		err1 = st.WithTx(ctx, func(tx store.Store) error {
			if lerr := tx.CustomizationCatalog().LockRow(ctx, "cat-1"); lerr != nil {
				return lerr
			}
			record("1-locked")
			close(locked1)
			<-proceed1
			return nil
		})
		record("1-committed")
	}()

	// tx2: waits for tx1 to hold the lock, releases it to commit, then races to acquire the same
	// lock. Because BEGIN is promoted to a write transaction at start (`_txlock=immediate`), tx2's
	// WithTx cannot proceed past its own BEGIN until tx1 actually commits — regardless of
	// goroutine scheduling — so "2-locked" can only ever be recorded after "1-committed".
	go func() {
		defer wg.Done()
		<-locked1
		close(proceed1)
		err2 = st.WithTx(ctx, func(tx store.Store) error {
			if lerr := tx.CustomizationCatalog().LockRow(ctx, "cat-1"); lerr != nil {
				return lerr
			}
			record("2-locked")
			return nil
		})
		record("2-committed")
	}()

	wg.Wait()
	if err1 != nil {
		t.Fatalf("tx1: %v", err1)
	}
	if err2 != nil {
		t.Fatalf("tx2 (want no BUSY/driver error): %v", err2)
	}

	idx := func(s string) int {
		for i, v := range log {
			if v == s {
				return i
			}
		}
		return -1
	}
	if i1, i2 := idx("1-committed"), idx("2-locked"); i1 == -1 || i2 == -1 || i2 < i1 {
		t.Fatalf("lock not serialized (want 2-locked after 1-committed): log=%v", log)
	}
}

// --- CountBundleRefs / CountBundleVersionRefs / ListProfileIDsByBundle* ----------------------

// TestProfiles_CountBundleRefs mirrors TestProfiles_CountRefsByCatalogRef for bundle_ref entries:
// bob has two profiles referencing bundle-1, carol has one; a decoy profile references a
// different bundle and must not be counted, and a catalog_ref entry on the same bundle_id column
// value must not be counted either (source_kind gates it).
func TestProfiles_CountBundleRefs(t *testing.T) {
	st := store.NewTestStore(t)
	ctx := context.Background()

	profiles := []struct {
		id, owner, bundleRef string
	}{
		{"pf-1", "bob", "bundle-1"},
		{"pf-2", "bob", "bundle-1"},
		{"pf-3", "carol", "bundle-1"},
		{"pf-4", "dave", "bundle-2"},
	}
	for _, p := range profiles {
		if err := st.Profiles().Create(ctx, store.Profile{
			ProfileID: p.id, OwnerID: p.owner, Name: "p", Version: 1, UpdatedAt: 1000,
		}); err != nil {
			t.Fatalf("Create %s: %v", p.id, err)
		}
		if _, err := st.Profiles().AddEntry(ctx, p.id, 1, store.ProfileEntry{
			EntryID: "e-" + p.id, Kind: store.ProfileEntrySkill, Name: "sk",
			SourceKind: store.ProfileSourceBundle, BundleID: p.bundleRef, VersionID: "v-" + p.bundleRef,
		}, 2000); err != nil {
			t.Fatalf("AddEntry %s: %v", p.id, err)
		}
	}
	// Decoy: a catalog_ref entry that happens to share the bundle_id column's default value must
	// not be double-counted as a bundle ref.
	if err := st.Profiles().Create(ctx, store.Profile{
		ProfileID: "pf-5", OwnerID: "erin", Name: "p", Version: 1, UpdatedAt: 1000,
	}); err != nil {
		t.Fatalf("Create pf-5: %v", err)
	}
	if _, err := st.Profiles().AddEntry(ctx, "pf-5", 1, store.ProfileEntry{
		EntryID: "e-pf-5", Kind: store.ProfileEntrySkill, Name: "sk",
		SourceKind: store.ProfileSourceCatalog, CatalogID: "cat-x",
	}, 2000); err != nil {
		t.Fatalf("AddEntry pf-5: %v", err)
	}

	gotProfiles, gotOwners, err := st.Profiles().CountBundleRefs(ctx, "bundle-1")
	if err != nil {
		t.Fatalf("CountBundleRefs: %v", err)
	}
	if gotProfiles != 3 {
		t.Errorf("expected 3 referencing profiles, got %d", gotProfiles)
	}
	if gotOwners != 2 {
		t.Errorf("expected 2 distinct owners, got %d", gotOwners)
	}
}

func TestProfiles_CountBundleRefs_NoRefs(t *testing.T) {
	st := store.NewTestStore(t)
	ctx := context.Background()

	gotProfiles, gotOwners, err := st.Profiles().CountBundleRefs(ctx, "no-such-bundle")
	if err != nil {
		t.Fatalf("CountBundleRefs: %v", err)
	}
	if gotProfiles != 0 || gotOwners != 0 {
		t.Errorf("expected (0, 0), got (%d, %d)", gotProfiles, gotOwners)
	}
}

// TestProfiles_CountBundleVersionRefs proves the count is scoped to versionID, not bundleID: two
// versions of the same bundle must not be conflated.
func TestProfiles_CountBundleVersionRefs(t *testing.T) {
	st := store.NewTestStore(t)
	ctx := context.Background()

	rows := []struct {
		id, owner, versionRef string
	}{
		{"pf-1", "bob", "v1"},
		{"pf-2", "carol", "v1"},
		{"pf-3", "dave", "v2"}, // same bundle, different version — must not count toward v1
	}
	for _, r := range rows {
		if err := st.Profiles().Create(ctx, store.Profile{
			ProfileID: r.id, OwnerID: r.owner, Name: "p", Version: 1, UpdatedAt: 1000,
		}); err != nil {
			t.Fatalf("Create %s: %v", r.id, err)
		}
		if _, err := st.Profiles().AddEntry(ctx, r.id, 1, store.ProfileEntry{
			EntryID: "e-" + r.id, Kind: store.ProfileEntrySkill, Name: "sk",
			SourceKind: store.ProfileSourceBundle, BundleID: "bundle-1", VersionID: r.versionRef,
		}, 2000); err != nil {
			t.Fatalf("AddEntry %s: %v", r.id, err)
		}
	}

	gotProfiles, gotOwners, err := st.Profiles().CountBundleVersionRefs(ctx, "v1")
	if err != nil {
		t.Fatalf("CountBundleVersionRefs: %v", err)
	}
	if gotProfiles != 2 {
		t.Errorf("expected 2 referencing profiles, got %d", gotProfiles)
	}
	if gotOwners != 2 {
		t.Errorf("expected 2 distinct owners, got %d", gotOwners)
	}
}

func TestProfiles_CountBundleVersionRefs_NoRefs(t *testing.T) {
	st := store.NewTestStore(t)
	ctx := context.Background()

	gotProfiles, gotOwners, err := st.Profiles().CountBundleVersionRefs(ctx, "no-such-version")
	if err != nil {
		t.Fatalf("CountBundleVersionRefs: %v", err)
	}
	if gotProfiles != 0 || gotOwners != 0 {
		t.Errorf("expected (0, 0), got (%d, %d)", gotProfiles, gotOwners)
	}
}

// TestProfiles_ListProfileIDsByBundleRef mirrors ListProfileIDsByCatalogRef's shape for the
// bundle-ref kill-switch resolution (catalog_delete.go §3, local until sp-mwco.1.6 lands).
func TestProfiles_ListProfileIDsByBundleRef(t *testing.T) {
	st := store.NewTestStore(t)
	ctx := context.Background()

	for _, p := range []struct{ id, bundleRef string }{
		{"pf-1", "bundle-1"},
		{"pf-2", "bundle-1"},
		{"pf-3", "bundle-2"},
	} {
		if err := st.Profiles().Create(ctx, store.Profile{
			ProfileID: p.id, OwnerID: "bob", Name: "p", Version: 1, UpdatedAt: 1000,
		}); err != nil {
			t.Fatalf("Create %s: %v", p.id, err)
		}
		if _, err := st.Profiles().AddEntry(ctx, p.id, 1, store.ProfileEntry{
			EntryID: "e-" + p.id, Kind: store.ProfileEntrySkill, Name: "sk",
			SourceKind: store.ProfileSourceBundle, BundleID: p.bundleRef, VersionID: "v-" + p.bundleRef,
		}, 2000); err != nil {
			t.Fatalf("AddEntry %s: %v", p.id, err)
		}
	}

	got, err := st.Profiles().ListProfileIDsByBundleRef(ctx, "bundle-1")
	if err != nil {
		t.Fatalf("ListProfileIDsByBundleRef: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 profile ids, got %d: %v", len(got), got)
	}

	gotNone, err := st.Profiles().ListProfileIDsByBundleRef(ctx, "no-such-bundle")
	if err != nil {
		t.Fatalf("ListProfileIDsByBundleRef (no match): %v", err)
	}
	if len(gotNone) != 0 {
		t.Fatalf("expected empty slice, got %v", gotNone)
	}
}

// TestProfiles_ListProfileIDsByBundleVersionRef proves version-scoping, mirroring
// TestProfiles_CountBundleVersionRefs.
func TestProfiles_ListProfileIDsByBundleVersionRef(t *testing.T) {
	st := store.NewTestStore(t)
	ctx := context.Background()

	for _, p := range []struct{ id, versionRef string }{
		{"pf-1", "v1"},
		{"pf-2", "v2"},
	} {
		if err := st.Profiles().Create(ctx, store.Profile{
			ProfileID: p.id, OwnerID: "bob", Name: "p", Version: 1, UpdatedAt: 1000,
		}); err != nil {
			t.Fatalf("Create %s: %v", p.id, err)
		}
		if _, err := st.Profiles().AddEntry(ctx, p.id, 1, store.ProfileEntry{
			EntryID: "e-" + p.id, Kind: store.ProfileEntrySkill, Name: "sk",
			SourceKind: store.ProfileSourceBundle, BundleID: "bundle-1", VersionID: p.versionRef,
		}, 2000); err != nil {
			t.Fatalf("AddEntry %s: %v", p.id, err)
		}
	}

	got, err := st.Profiles().ListProfileIDsByBundleVersionRef(ctx, "v1")
	if err != nil {
		t.Fatalf("ListProfileIDsByBundleVersionRef: %v", err)
	}
	if len(got) != 1 || got[0] != "pf-1" {
		t.Fatalf("expected [pf-1], got %v", got)
	}
}
