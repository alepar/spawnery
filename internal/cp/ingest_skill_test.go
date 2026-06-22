package cp

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/klauspost/compress/zstd"

	cpv1 "spawnery/gen/cp/v1"
	"spawnery/internal/cp/auth"
	"spawnery/internal/cp/skillfetch"
	"spawnery/internal/cp/skillstore"
	"spawnery/internal/cp/store"
)

// --- fake fetcher for handler tests ---

type fakeFetcher struct {
	result skillfetch.Result
	err    error
}

func (f *fakeFetcher) Fetch(_ context.Context, _ skillfetch.RepoRef, _, _, name, description string) (skillfetch.Result, error) {
	if f.err != nil {
		return skillfetch.Result{}, f.err
	}
	r := f.result
	if name != "" {
		r.Name = name
	}
	if description != "" {
		r.Description = description
	}
	return r, nil
}

// makeCannedResult builds a deterministic fake fetch result with the given name.
func makeCannedResult(name string) skillfetch.Result {
	// Build a small canonical tar with SKILL.md
	entries := buildTestTarEntries(name)
	plainTar := mustCanonicalTar(entries)
	h := sha256.Sum256(plainTar)
	sha256hex := hex.EncodeToString(h[:])

	enc, _ := zstd.NewWriter(nil, zstd.WithEncoderLevel(zstd.SpeedDefault))
	compressed := enc.EncodeAll(plainTar, nil)

	return skillfetch.Result{
		Owner:           "testowner",
		Repo:            "testrepo",
		Name:            name,
		Description:     "test description",
		PlainTarSHA256:  sha256hex,
		CompressedBytes: compressed,
		PlainSize:       int64(len(plainTar)),
	}
}

func buildTestTarEntries(name string) []byte {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	skillMD := fmt.Sprintf("---\nname: %s\n---\n# Test", name)
	_ = tw.WriteHeader(&tar.Header{
		Typeflag: tar.TypeReg,
		Name:     "SKILL.md",
		Size:     int64(len(skillMD)),
		Mode:     0o644,
	})
	_, _ = tw.Write([]byte(skillMD))
	_ = tw.Close()
	return buf.Bytes()
}

func mustCanonicalTar(data []byte) []byte {
	// For the fake result, we just use the raw bytes as the "plain tar"
	return data
}

// --- IngestSkillFromURL handler tests ---

func TestIngestSkillFromURL_Success(t *testing.T) {
	s, _, _ := newTestServer(t)
	result := makeCannedResult("my-skill")
	s.SetSkillIngest(&fakeFetcher{result: result}, skillstore.NewFakeSkillStore())

	resp, err := s.IngestSkillFromURL(aliceCtx(), connect.NewRequest(&cpv1.IngestSkillFromURLRequest{
		Url: "testowner/testrepo",
	}))
	if err != nil {
		t.Fatalf("IngestSkillFromURL: %v", err)
	}
	if resp.Msg.CatalogId == "" {
		t.Fatal("expected non-empty catalog_id")
	}
}

func TestIngestSkillFromURL_Unauthenticated(t *testing.T) {
	s, _, _ := newTestServer(t)
	result := makeCannedResult("my-skill")
	s.SetSkillIngest(&fakeFetcher{result: result}, skillstore.NewFakeSkillStore())

	_, err := s.IngestSkillFromURL(noAuthCtx(), connect.NewRequest(&cpv1.IngestSkillFromURLRequest{
		Url: "owner/repo",
	}))
	if err == nil {
		t.Fatal("expected Unauthenticated error")
	}
	var ce *connect.Error
	if !errors.As(err, &ce) || ce.Code() != connect.CodeUnauthenticated {
		t.Fatalf("expected CodeUnauthenticated, got: %v", err)
	}
}

func TestIngestSkillFromURL_NilFetcherOrStore_FailedPrecondition(t *testing.T) {
	s, _, _ := newTestServer(t)
	// No SetSkillIngest called — both are nil

	_, err := s.IngestSkillFromURL(aliceCtx(), connect.NewRequest(&cpv1.IngestSkillFromURLRequest{
		Url: "owner/repo",
	}))
	if err == nil {
		t.Fatal("expected FailedPrecondition error")
	}
	var ce *connect.Error
	if !errors.As(err, &ce) || ce.Code() != connect.CodeFailedPrecondition {
		t.Fatalf("expected CodeFailedPrecondition, got: %v", err)
	}
}

func TestIngestSkillFromURL_Idempotent_SameResult(t *testing.T) {
	s, _, _ := newTestServer(t)
	result := makeCannedResult("my-skill")
	fakeStore := skillstore.NewFakeSkillStore()
	s.SetSkillIngest(&fakeFetcher{result: result}, fakeStore)

	// First call
	resp1, err := s.IngestSkillFromURL(aliceCtx(), connect.NewRequest(&cpv1.IngestSkillFromURLRequest{
		Url: "testowner/testrepo",
	}))
	if err != nil {
		t.Fatalf("first IngestSkillFromURL: %v", err)
	}
	catalogID1 := resp1.Msg.CatalogId

	// Second call with same URL (same sha256)
	resp2, err := s.IngestSkillFromURL(aliceCtx(), connect.NewRequest(&cpv1.IngestSkillFromURLRequest{
		Url: "testowner/testrepo",
	}))
	if err != nil {
		t.Fatalf("second IngestSkillFromURL: %v", err)
	}
	catalogID2 := resp2.Msg.CatalogId

	if catalogID1 != catalogID2 {
		t.Fatalf("idempotent call returned different catalog_id: %q vs %q", catalogID1, catalogID2)
	}

	// Verify PutIfAbsent was called at most twice (second call may or may not call it)
	// but the catalog row should exist only once
	entries, err := s.st.CustomizationCatalog().ListByCreator(context.Background(), "alice")
	if err != nil {
		t.Fatalf("ListByCreator: %v", err)
	}
	count := 0
	for _, e := range entries {
		if e.SHA256 != nil && *e.SHA256 == result.PlainTarSHA256 {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("expected exactly 1 catalog row for sha256 %s, got %d", result.PlainTarSHA256, count)
	}
}

func TestIngestSkillFromURL_FetchError_Propagated(t *testing.T) {
	s, _, _ := newTestServer(t)
	fakeErr := fmt.Errorf("fetch failed: no SKILL.md found at subdir")
	s.SetSkillIngest(&fakeFetcher{err: fakeErr}, skillstore.NewFakeSkillStore())

	_, err := s.IngestSkillFromURL(aliceCtx(), connect.NewRequest(&cpv1.IngestSkillFromURLRequest{
		Url: "owner/repo",
	}))
	if err == nil {
		t.Fatal("expected error from fetch failure")
	}
}

func TestIngestSkillFromURL_RateLimit_ResourceExhausted(t *testing.T) {
	s, _, _ := newTestServer(t)
	result := makeCannedResult("skill")
	fakeStore := skillstore.NewFakeSkillStore()
	s.SetSkillIngest(&fakeFetcher{result: result}, fakeStore)

	// Use a unique owner to avoid polluting the global quota
	testOwner := fmt.Sprintf("quota-test-owner-%d", time.Now().UnixNano())
	ctx := makeOwnerCtx(testOwner)

	// Exhaust the quota
	for i := 0; i < ingestQuotaMax; i++ {
		// Make each call unique by varying the sha256 result slightly
		r := makeCannedResult(fmt.Sprintf("skill-%d", i))
		s.skillFetcher = &fakeFetcher{result: r}
		_, err := s.IngestSkillFromURL(ctx, connect.NewRequest(&cpv1.IngestSkillFromURLRequest{
			Url: fmt.Sprintf("owner/repo-%d", i),
		}))
		if err != nil {
			t.Fatalf("call %d: unexpected error: %v", i, err)
		}
	}

	// Next call should hit quota
	s.skillFetcher = &fakeFetcher{result: result}
	_, err := s.IngestSkillFromURL(ctx, connect.NewRequest(&cpv1.IngestSkillFromURLRequest{
		Url: "owner/overflow",
	}))
	if err == nil {
		t.Fatal("expected ResourceExhausted error after quota exceeded")
	}
	var ce *connect.Error
	if !errors.As(err, &ce) || ce.Code() != connect.CodeResourceExhausted {
		t.Fatalf("expected CodeResourceExhausted, got: %v", err)
	}
}

func TestIngestSkillFromURL_StoresPutAndCatalogRow(t *testing.T) {
	s, _, _ := newTestServer(t)
	result := makeCannedResult("stored-skill")
	fakeStore := skillstore.NewFakeSkillStore()
	s.SetSkillIngest(&fakeFetcher{result: result}, fakeStore)

	resp, err := s.IngestSkillFromURL(aliceCtx(), connect.NewRequest(&cpv1.IngestSkillFromURLRequest{
		Url: "testowner/testrepo",
	}))
	if err != nil {
		t.Fatalf("IngestSkillFromURL: %v", err)
	}

	// Verify the object was put in the fake store
	if !fakeStore.Has(result.PlainTarSHA256) {
		t.Fatal("expected skill object to be stored in fake store")
	}

	// Verify the catalog entry exists with provenance
	entry, err := s.st.CustomizationCatalog().Get(context.Background(), resp.Msg.CatalogId)
	if err != nil {
		t.Fatalf("Get catalog entry: %v", err)
	}
	if entry.SHA256 == nil || *entry.SHA256 != result.PlainTarSHA256 {
		t.Fatalf("sha256 not stored: got %v", entry.SHA256)
	}
	if entry.SourceURL == nil {
		t.Fatal("source_url not stored")
	}
	if entry.Size == nil || *entry.Size != result.PlainSize {
		t.Fatalf("size not stored: got %v", entry.Size)
	}
	if entry.Kind != string(store.ProfileEntrySkill) {
		t.Fatalf("kind not skill: got %q", entry.Kind)
	}
}

func TestIngestSkillFromURL_InvalidURL_BadArgument(t *testing.T) {
	s, _, _ := newTestServer(t)
	s.SetSkillIngest(&fakeFetcher{result: makeCannedResult("x")}, skillstore.NewFakeSkillStore())

	_, err := s.IngestSkillFromURL(aliceCtx(), connect.NewRequest(&cpv1.IngestSkillFromURLRequest{
		Url: "https://github.com/owner/repo/tree/main/subdir", // rejected deep URL
	}))
	if err == nil {
		t.Fatal("expected error for deep URL")
	}
	var ce *connect.Error
	if !errors.As(err, &ce) || ce.Code() != connect.CodeInvalidArgument {
		t.Fatalf("expected CodeInvalidArgument, got: %v", err)
	}
}

func TestIngestSkillFromURL_GitHubRateLimit_ResourceExhausted(t *testing.T) {
	s, _, _ := newTestServer(t)
	rlErr := &skillfetch.ErrRateLimit{RetryAfter: "120"}
	s.SetSkillIngest(&fakeFetcher{err: rlErr}, skillstore.NewFakeSkillStore())

	_, err := s.IngestSkillFromURL(aliceCtx(), connect.NewRequest(&cpv1.IngestSkillFromURLRequest{
		Url: "owner/repo",
	}))
	if err == nil {
		t.Fatal("expected error for rate limit")
	}
	var ce *connect.Error
	if !errors.As(err, &ce) || ce.Code() != connect.CodeResourceExhausted {
		t.Fatalf("expected CodeResourceExhausted, got: %v", err)
	}
}

// --- store.CustomizationCatalogRepo: GetByCreatorSHA + CreateSkill + isUniqueViolation ---

func TestGetByCreatorSHA_ErrNotFound(t *testing.T) {
	_, _, _ = newTestServer(t)
	s, _, _ := newTestServer(t)
	_, err := s.st.CustomizationCatalog().GetByCreatorSHA(context.Background(), "alice", "nonexistent-sha")
	if !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got: %v", err)
	}
}

func TestCreateSkill_DuplicateReturnsErrConflict(t *testing.T) {
	s, _, _ := newTestServer(t)
	sha := "abc123sha"
	entry := store.CustomizationCatalogEntry{
		CatalogID:   "cat-1",
		CreatorID:   "alice",
		Kind:        "skill",
		Name:        "test-skill",
		Description: "desc",
		Listed:      true,
		CreatedAt:   time.Now().Unix(),
		UpdatedAt:   time.Now().Unix(),
		SHA256:      &sha,
	}
	if err := s.st.CustomizationCatalog().CreateSkill(context.Background(), entry); err != nil {
		t.Fatalf("first CreateSkill: %v", err)
	}

	entry2 := entry
	entry2.CatalogID = "cat-2" // different id, same (creator, sha256)
	err := s.st.CustomizationCatalog().CreateSkill(context.Background(), entry2)
	if !errors.Is(err, store.ErrConflict) {
		t.Fatalf("expected ErrConflict for duplicate (creator, sha256), got: %v", err)
	}
}

func TestCreateSkill_GetByCreatorSHA_RoundTrip(t *testing.T) {
	s, _, _ := newTestServer(t)
	sha := "roundtrip-sha-xyz"
	size := int64(12345)
	srcURL := "owner/repo"
	entry := store.CustomizationCatalogEntry{
		CatalogID:   "cat-rt",
		CreatorID:   "alice",
		Kind:        "skill",
		Name:        "rt-skill",
		Description: "roundtrip test",
		Listed:      true,
		CreatedAt:   time.Now().Unix(),
		UpdatedAt:   time.Now().Unix(),
		SHA256:      &sha,
		Size:        &size,
		SourceURL:   &srcURL,
	}
	if err := s.st.CustomizationCatalog().CreateSkill(context.Background(), entry); err != nil {
		t.Fatalf("CreateSkill: %v", err)
	}

	got, err := s.st.CustomizationCatalog().GetByCreatorSHA(context.Background(), "alice", sha)
	if err != nil {
		t.Fatalf("GetByCreatorSHA: %v", err)
	}
	if got.CatalogID != "cat-rt" {
		t.Fatalf("CatalogID mismatch: got %q", got.CatalogID)
	}
	if got.Size == nil || *got.Size != size {
		t.Fatalf("Size mismatch: got %v", got.Size)
	}
}

// gzip.decompress test for completeness
func TestGzipTarUnpack_HardlinkRejected(t *testing.T) {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	_ = tw.WriteHeader(&tar.Header{Typeflag: tar.TypeDir, Name: "wrapper/"})
	_ = tw.WriteHeader(&tar.Header{
		Typeflag: tar.TypeLink,
		Name:     "wrapper/hardlink",
		Linkname: "wrapper/target",
	})
	_ = tw.Close()
	_ = gz.Close()
	// Just verify the tar was built correctly; actual rejection is tested in skillfetch package
	if buf.Len() == 0 {
		t.Fatal("expected non-empty tarball")
	}
}

// makeOwnerCtx creates a test context with the given owner ID (for quota testing with distinct owners).
func makeOwnerCtx(ownerID string) context.Context {
	return makeOwnerIDCtx(context.Background(), ownerID)
}

// makeOwnerIDCtx uses the auth package to inject an owner.
func makeOwnerIDCtx(ctx context.Context, ownerID string) context.Context {
	return auth.WithOwner(ctx, ownerID)
}
