package store

import (
	"bytes"
	"errors"
	"testing"
)

// --- helpers ------------------------------------------------------------------

func mkEntry(version uint64, prevHash, headHash []byte) (uint64, []byte, []byte, []byte, int64) {
	body := []byte(`{"version":` + uint64str(version) + `}`)
	return version, prevHash, headHash, body, int64(version) * 1000
}

func uint64str(n uint64) string {
	buf := make([]byte, 0, 20)
	if n == 0 {
		return "0"
	}
	for n > 0 {
		buf = append([]byte{byte('0' + n%10)}, buf...)
		n /= 10
	}
	return string(buf)
}

// --- tests --------------------------------------------------------------------

func TestDeviceSetHeadEmpty(t *testing.T) {
	st := NewTestStore(t)
	_, _, found, err := st.DeviceSets().Head(ctxT(), "acct-1")
	if err != nil {
		t.Fatalf("Head on empty: %v", err)
	}
	if found {
		t.Fatal("want found=false on empty account")
	}
}

func TestDeviceSetGenesisHappyPath(t *testing.T) {
	st := NewTestStore(t)
	headHash := []byte("hashG")
	entryBytes := []byte(`{"genesis":true}`)

	if err := st.DeviceSets().Append(ctxT(), "acct-1", 1, nil, headHash, entryBytes, 100); err != nil {
		t.Fatalf("Append genesis: %v", err)
	}

	gotHash, gotVer, found, err := st.DeviceSets().Head(ctxT(), "acct-1")
	if err != nil {
		t.Fatalf("Head: %v", err)
	}
	if !found {
		t.Fatal("want found=true after genesis")
	}
	if gotVer != 1 {
		t.Fatalf("want version=1, got %d", gotVer)
	}
	if !bytes.Equal(gotHash, headHash) {
		t.Fatalf("head hash mismatch")
	}
}

func TestDeviceSetGenesisWithPrevHashRejected(t *testing.T) {
	st := NewTestStore(t)
	err := st.DeviceSets().Append(ctxT(), "acct-1", 1, []byte("nonnil"), []byte("hash"), []byte("{}"), 100)
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("want ErrConflict for genesis with prevHash, got %v", err)
	}
}

func TestDeviceSetDuplicateGenesisRejected(t *testing.T) {
	st := NewTestStore(t)
	if err := st.DeviceSets().Append(ctxT(), "acct-1", 1, nil, []byte("hashG"), []byte("{}"), 100); err != nil {
		t.Fatal(err)
	}
	// Second attempt with version=1 and no prevHash must be rejected.
	err := st.DeviceSets().Append(ctxT(), "acct-1", 1, nil, []byte("hashG2"), []byte("{}"), 200)
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("want ErrConflict for duplicate genesis, got %v", err)
	}
}

func TestDeviceSetChainAppend(t *testing.T) {
	st := NewTestStore(t)
	hashG := []byte("hashG")
	hash1 := []byte("hash1")

	if err := st.DeviceSets().Append(ctxT(), "acct-1", 1, nil, hashG, []byte(`{"v":1}`), 100); err != nil {
		t.Fatal(err)
	}
	if err := st.DeviceSets().Append(ctxT(), "acct-1", 2, hashG, hash1, []byte(`{"v":2}`), 200); err != nil {
		t.Fatalf("Append v2: %v", err)
	}

	gotHash, gotVer, found, err := st.DeviceSets().Head(ctxT(), "acct-1")
	if err != nil || !found {
		t.Fatalf("Head: %v %v", found, err)
	}
	if gotVer != 2 {
		t.Fatalf("want version=2, got %d", gotVer)
	}
	if !bytes.Equal(gotHash, hash1) {
		t.Fatalf("head hash mismatch")
	}
}

func TestDeviceSetStaleHashRejected(t *testing.T) {
	st := NewTestStore(t)
	hashG := []byte("hashG")
	if err := st.DeviceSets().Append(ctxT(), "acct-1", 1, nil, hashG, []byte(`{}`), 100); err != nil {
		t.Fatal(err)
	}
	// Wrong prevHash.
	err := st.DeviceSets().Append(ctxT(), "acct-1", 2, []byte("wrong"), []byte("hash2"), []byte(`{}`), 200)
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("want ErrConflict for stale hash, got %v", err)
	}
}

func TestDeviceSetVersionGapRejected(t *testing.T) {
	st := NewTestStore(t)
	hashG := []byte("hashG")
	if err := st.DeviceSets().Append(ctxT(), "acct-1", 1, nil, hashG, []byte(`{}`), 100); err != nil {
		t.Fatal(err)
	}
	// Version skip: should be 2, we send 3.
	err := st.DeviceSets().Append(ctxT(), "acct-1", 3, hashG, []byte("hash3"), []byte(`{}`), 200)
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("want ErrConflict for version gap, got %v", err)
	}
}

func TestDeviceSetFetchAllOrder(t *testing.T) {
	st := NewTestStore(t)
	hashG := []byte("hashG")
	hash1 := []byte("hash1")
	hash2 := []byte("hash2")

	body := func(n int) []byte { return []byte(`{"n":` + uint64str(uint64(n)) + `}`) }
	if err := st.DeviceSets().Append(ctxT(), "acct-1", 1, nil, hashG, body(1), 100); err != nil {
		t.Fatal(err)
	}
	if err := st.DeviceSets().Append(ctxT(), "acct-1", 2, hashG, hash1, body(2), 200); err != nil {
		t.Fatal(err)
	}
	if err := st.DeviceSets().Append(ctxT(), "acct-1", 3, hash1, hash2, body(3), 300); err != nil {
		t.Fatal(err)
	}

	entries, err := st.DeviceSets().FetchAll(ctxT(), "acct-1")
	if err != nil {
		t.Fatalf("FetchAll: %v", err)
	}
	if len(entries) != 3 {
		t.Fatalf("want 3 entries, got %d", len(entries))
	}
	for i, want := range [][]byte{body(1), body(2), body(3)} {
		if !bytes.Equal(entries[i], want) {
			t.Fatalf("entry[%d]: got %q, want %q", i, entries[i], want)
		}
	}
}

func TestDeviceSetAccountIsolation(t *testing.T) {
	st := NewTestStore(t)
	hashA := []byte("hashA")
	hashB := []byte("hashB")

	if err := st.DeviceSets().Append(ctxT(), "acct-A", 1, nil, hashA, []byte(`{"a":1}`), 100); err != nil {
		t.Fatal(err)
	}
	if err := st.DeviceSets().Append(ctxT(), "acct-B", 1, nil, hashB, []byte(`{"b":1}`), 100); err != nil {
		t.Fatal(err)
	}

	entriesA, _ := st.DeviceSets().FetchAll(ctxT(), "acct-A")
	entriesB, _ := st.DeviceSets().FetchAll(ctxT(), "acct-B")
	if len(entriesA) != 1 || len(entriesB) != 1 {
		t.Fatalf("isolation broken: A=%d B=%d", len(entriesA), len(entriesB))
	}
	if bytes.Equal(entriesA[0], entriesB[0]) {
		t.Fatal("entries should differ between accounts")
	}

	// A's head must not be influenced by B's genesis.
	gotHashA, gotVerA, _, _ := st.DeviceSets().Head(ctxT(), "acct-A")
	if gotVerA != 1 || !bytes.Equal(gotHashA, hashA) {
		t.Fatal("acct-A head corrupted by acct-B")
	}
}

func TestDeviceSetFetchAllEmpty(t *testing.T) {
	st := NewTestStore(t)
	entries, err := st.DeviceSets().FetchAll(ctxT(), "nobody")
	if err != nil {
		t.Fatalf("FetchAll empty: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("want 0, got %d", len(entries))
	}
}
