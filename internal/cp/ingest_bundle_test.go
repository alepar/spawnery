package cp

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/klauspost/compress/zstd"

	cpv1 "spawnery/gen/cp/v1"
	"spawnery/internal/cp/skillfetch"
	"spawnery/internal/cp/skillstore"
	"spawnery/internal/cp/store"
)

// This file holds ALL tests for sp-mwco.1.4 (bundle-of-one ingest, real caps, concurrency
// semaphore, version-cut on commit+sha) — kept separate from ingest_skill_test.go to minimize the
// conflict surface with other in-flight tasks touching that file.

// --- scaffolding: fakeBundleFetcher ---------------------------------------------------------

// bundleScript is one scripted FetchBundle call outcome.
type bundleScript struct {
	result skillfetch.BundleResult
	err    error
}

// fakeBundleFetcher is the skillfetch.BundleFetcher test double for this file. It also
// implements a stub Fetch to satisfy SetSkillIngest's skillfetch.Fetcher parameter type — the CP
// handler never calls it (IngestSkillFromURL always goes through FetchBundle).
//
// scripted supports a per-call script: call 1 gets scripted[0], call 2 gets scripted[1], etc.;
// once exhausted, the last entry repeats (so a single-entry script serves an idempotency test
// with no extra bookkeeping).
//
// When gate is non-nil, FetchBundle blocks reading from it before returning — closing gate
// releases every blocked call at once. inFlight/maxInFlight instrument how many calls are
// blocked concurrently, for the semaphore test.
type fakeBundleFetcher struct {
	mu       sync.Mutex
	scripted []bundleScript
	calls    int

	gate        chan struct{}
	inFlight    int32
	maxInFlight int32
}

func (f *fakeBundleFetcher) Fetch(_ context.Context, _ skillfetch.RepoRef, _, _, _, _ string) (skillfetch.Result, error) {
	return skillfetch.Result{}, fmt.Errorf("fakeBundleFetcher: Fetch is not implemented (bundle-only test double)")
}

func (f *fakeBundleFetcher) FetchBundle(ctx context.Context, _ skillfetch.RepoRef, _, _ string) (skillfetch.BundleResult, error) {
	if f.gate != nil {
		cur := atomic.AddInt32(&f.inFlight, 1)
		defer atomic.AddInt32(&f.inFlight, -1)
		for {
			prev := atomic.LoadInt32(&f.maxInFlight)
			if cur <= prev {
				break
			}
			if atomic.CompareAndSwapInt32(&f.maxInFlight, prev, cur) {
				break
			}
		}
		select {
		case <-f.gate:
		case <-ctx.Done():
			return skillfetch.BundleResult{}, ctx.Err()
		}
	}

	f.mu.Lock()
	idx := f.calls
	if idx >= len(f.scripted) {
		idx = len(f.scripted) - 1
	}
	sc := f.scripted[idx]
	f.calls++
	f.mu.Unlock()
	return sc.result, sc.err
}

// buildSingleFileTar builds a minimal one-entry plain tar (no wrapper dir — this is already the
// "repacked member" shape IngestSkillFromURL expects from a BundleFetcher).
func buildSingleFileTar(name, content string) []byte {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	_ = tw.WriteHeader(&tar.Header{
		Typeflag: tar.TypeReg,
		Name:     name,
		Size:     int64(len(content)),
		Mode:     0o644,
	})
	_, _ = tw.Write([]byte(content))
	_ = tw.Close()
	return buf.Bytes()
}

// makeMember builds a scripted skillfetch.Member with a real tar+sha256+zstd payload. content
// drives the member's identity (sha256) — tests vary it to produce distinct or byte-identical
// members on demand.
func makeMember(dir, name, description, content string) skillfetch.Member {
	skillMD := fmt.Sprintf("---\nname: %s\n---\n%s", name, content)
	plainTar := buildSingleFileTar("SKILL.md", skillMD)
	h := sha256.Sum256(plainTar)
	sha256hex := hex.EncodeToString(h[:])
	enc, _ := zstd.NewWriter(nil, zstd.WithEncoderLevel(zstd.SpeedDefault))
	compressed := enc.EncodeAll(plainTar, nil)
	return skillfetch.Member{
		Dir:             dir,
		Name:            name,
		Description:     description,
		PlainTarSHA256:  sha256hex,
		CompressedBytes: compressed,
		PlainSize:       int64(len(plainTar)),
	}
}

// bundleTestOwner returns a unique owner id per call, so parallel/sequential tests in this file
// never collide on the (creator_id, source_url, ref, subdir) bundle key or the ingest quota.
var bundleTestOwnerSeq int64

func bundleTestOwner(t *testing.T) string {
	t.Helper()
	n := atomic.AddInt64(&bundleTestOwnerSeq, 1)
	return fmt.Sprintf("bundle-owner-%d-%d", time.Now().UnixNano(), n)
}

// waitForNextUnixSecond blocks until the wall clock crosses into a new Unix second. Used only by
// the revert A->B->A test, which must prove a reused catalog row's created_at (Unix-second
// resolution) is strictly older than the reverting version's created_at — a same-second race
// would make that specific assertion meaningless.
func waitForNextUnixSecond(t *testing.T) {
	t.Helper()
	start := time.Now().Unix()
	for time.Now().Unix() == start {
		time.Sleep(20 * time.Millisecond)
	}
}

// memberMap flattens SkillBundleMember rows into source_subdir -> catalog_id.
func memberMap(members []store.SkillBundleMember) map[string]string {
	m := make(map[string]string, len(members))
	for _, mm := range members {
		m[mm.SourceSubdir] = mm.CatalogID
	}
	return m
}

// --- S1: bundle-of-one ------------------------------------------------------------------------

func TestIngestBundle_SingleMember(t *testing.T) {
	s, _, _ := newTestServer(t)
	owner := bundleTestOwner(t)
	ctx := makeOwnerCtx(owner)
	fakeStore := skillstore.NewFakeSkillStore()

	member := makeMember("", "solo-skill", "a solo skill", "body-v1")
	fbf := &fakeBundleFetcher{scripted: []bundleScript{{result: skillfetch.BundleResult{
		Owner: "acme", Repo: "solo", SourceCommit: "abc123",
		Members: []skillfetch.Member{member},
	}}}}
	s.SetSkillIngest(fbf, fakeStore, skillfetch.DefaultPlainTarCapBytes)

	resp, err := s.IngestSkillFromURL(ctx, connect.NewRequest(&cpv1.IngestSkillFromURLRequest{
		Url: "acme/solo",
	}))
	if err != nil {
		t.Fatalf("IngestSkillFromURL: %v", err)
	}
	if resp.Msg.CatalogId == "" {
		t.Fatal("expected non-empty catalog_id")
	}

	entry, err := s.st.CustomizationCatalog().Get(context.Background(), resp.Msg.CatalogId)
	if err != nil {
		t.Fatalf("Get catalog entry: %v", err)
	}
	if !entry.BundleMember {
		t.Error("expected BundleMember=true")
	}
	if entry.Listed {
		t.Error("expected Listed=false")
	}
	if entry.SourceCommit != "abc123" {
		t.Errorf("SourceCommit = %q, want %q", entry.SourceCommit, "abc123")
	}
	if entry.SourceSubdir != "" {
		t.Errorf("SourceSubdir = %q, want \"\" for a repo-root member", entry.SourceSubdir)
	}

	bundle, err := s.st.SkillBundles().GetByKey(context.Background(), owner, "acme/solo", "", "")
	if err != nil {
		t.Fatalf("GetByKey: %v", err)
	}
	versions, err := s.st.SkillBundles().ListVersions(context.Background(), bundle.BundleID)
	if err != nil {
		t.Fatalf("ListVersions: %v", err)
	}
	if len(versions) != 1 {
		t.Fatalf("got %d versions, want 1", len(versions))
	}
	if versions[0].Seq != 1 {
		t.Errorf("Seq = %d, want 1", versions[0].Seq)
	}
	members, err := s.st.SkillBundles().Members(context.Background(), versions[0].VersionID)
	if err != nil {
		t.Fatalf("Members: %v", err)
	}
	if len(members) != 1 {
		t.Fatalf("got %d members, want 1", len(members))
	}
	if members[0].CatalogID != resp.Msg.CatalogId {
		t.Errorf("member catalog_id = %q, want %q", members[0].CatalogID, resp.Msg.CatalogId)
	}
	if members[0].Position != 0 {
		t.Errorf("member position = %d, want 0", members[0].Position)
	}
}

// --- S2: multi-member -------------------------------------------------------------------------

func TestIngestBundle_MultiMember(t *testing.T) {
	s, _, _ := newTestServer(t)
	owner := bundleTestOwner(t)
	ctx := makeOwnerCtx(owner)
	fakeStore := skillstore.NewFakeSkillStore()

	m1 := makeMember("skills/alpha", "alpha", "first skill", "alpha-body")
	m2 := makeMember("skills/beta", "beta", "second skill", "beta-body")
	m3 := makeMember("skills/gamma", "gamma", "third skill", "gamma-body")
	fbf := &fakeBundleFetcher{scripted: []bundleScript{{result: skillfetch.BundleResult{
		Owner: "o", Repo: "multi", SourceCommit: "deadbee",
		Members: []skillfetch.Member{m1, m2, m3},
	}}}}
	s.SetSkillIngest(fbf, fakeStore, skillfetch.DefaultPlainTarCapBytes)

	_, err := s.IngestSkillFromURL(ctx, connect.NewRequest(&cpv1.IngestSkillFromURLRequest{Url: "o/multi"}))
	if err != nil {
		t.Fatalf("IngestSkillFromURL: %v", err)
	}

	entries, err := s.st.CustomizationCatalog().ListByCreator(context.Background(), owner)
	if err != nil {
		t.Fatalf("ListByCreator: %v", err)
	}
	if len(entries) != 3 {
		t.Fatalf("got %d catalog rows, want 3", len(entries))
	}
	seen := map[string]bool{}
	for _, e := range entries {
		if e.SHA256 == nil {
			t.Fatal("expected SHA256 set")
		}
		seen[*e.SHA256] = true
		if !fakeStore.Has(*e.SHA256) {
			t.Fatalf("garage object missing for sha %s", *e.SHA256)
		}
	}
	if len(seen) != 3 {
		t.Fatalf("expected 3 distinct shas, got %d", len(seen))
	}

	bundle, err := s.st.SkillBundles().GetByKey(context.Background(), owner, "o/multi", "", "")
	if err != nil {
		t.Fatalf("GetByKey: %v", err)
	}
	versions, err := s.st.SkillBundles().ListVersions(context.Background(), bundle.BundleID)
	if err != nil {
		t.Fatalf("ListVersions: %v", err)
	}
	if len(versions) != 1 {
		t.Fatalf("got %d versions, want 1", len(versions))
	}
	members, err := s.st.SkillBundles().Members(context.Background(), versions[0].VersionID)
	if err != nil {
		t.Fatalf("Members: %v", err)
	}
	if len(members) != 3 {
		t.Fatalf("got %d members, want 3", len(members))
	}
	wantDirs := []string{"skills/alpha", "skills/beta", "skills/gamma"}
	for i, m := range members {
		if m.Position != i {
			t.Errorf("member %d: Position = %d, want %d", i, m.Position, i)
		}
		if m.SourceSubdir != wantDirs[i] {
			t.Errorf("member %d: SourceSubdir = %q, want %q", i, m.SourceSubdir, wantDirs[i])
		}
	}
}

// --- S3: idempotent re-ingest, no phantom cut -------------------------------------------------

func TestIngestBundle_Idempotent_NoPhantomVersion(t *testing.T) {
	s, _, _ := newTestServer(t)
	owner := bundleTestOwner(t)
	ctx := makeOwnerCtx(owner)
	fakeStore := skillstore.NewFakeSkillStore()

	m1 := makeMember("skills/alpha", "alpha", "", "alpha-body")
	m2 := makeMember("skills/beta", "beta", "", "beta-body")
	fbf := &fakeBundleFetcher{scripted: []bundleScript{{result: skillfetch.BundleResult{
		Owner: "o", Repo: "idem", Members: []skillfetch.Member{m1, m2},
	}}}}
	s.SetSkillIngest(fbf, fakeStore, skillfetch.DefaultPlainTarCapBytes)

	req := func() *connect.Request[cpv1.IngestSkillFromURLRequest] {
		return connect.NewRequest(&cpv1.IngestSkillFromURLRequest{Url: "o/idem"})
	}

	resp1, err := s.IngestSkillFromURL(ctx, req())
	if err != nil {
		t.Fatalf("first ingest: %v", err)
	}
	resp2, err := s.IngestSkillFromURL(ctx, req())
	if err != nil {
		t.Fatalf("second ingest: %v", err)
	}
	if resp1.Msg.CatalogId != resp2.Msg.CatalogId {
		t.Fatalf("idempotent re-ingest returned different catalog_id: %q vs %q", resp1.Msg.CatalogId, resp2.Msg.CatalogId)
	}

	bundle, err := s.st.SkillBundles().GetByKey(context.Background(), owner, "o/idem", "", "")
	if err != nil {
		t.Fatalf("GetByKey: %v", err)
	}
	versions, err := s.st.SkillBundles().ListVersions(context.Background(), bundle.BundleID)
	if err != nil {
		t.Fatalf("ListVersions: %v", err)
	}
	if len(versions) != 1 {
		t.Fatalf("got %d versions after 2 identical ingests, want 1", len(versions))
	}
	entries, err := s.st.CustomizationCatalog().ListByCreator(context.Background(), owner)
	if err != nil {
		t.Fatalf("ListByCreator: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("got %d catalog rows after 2 identical ingests, want 2 (rows reused, not duplicated)", len(entries))
	}
}

// --- S4: version cut on member sha change ------------------------------------------------------

func TestIngestBundle_VersionCut_OnMemberShaChange(t *testing.T) {
	s, _, _ := newTestServer(t)
	owner := bundleTestOwner(t)
	ctx := makeOwnerCtx(owner)
	fakeStore := skillstore.NewFakeSkillStore()

	alpha := makeMember("skills/alpha", "alpha", "", "alpha-body")
	betaV1 := makeMember("skills/beta", "beta", "", "beta-body-v1")
	betaV2 := makeMember("skills/beta", "beta", "", "beta-body-v2")

	fbf := &fakeBundleFetcher{scripted: []bundleScript{
		{result: skillfetch.BundleResult{Owner: "o", Repo: "shachange", Members: []skillfetch.Member{alpha, betaV1}}},
		{result: skillfetch.BundleResult{Owner: "o", Repo: "shachange", Members: []skillfetch.Member{alpha, betaV2}}},
	}}
	s.SetSkillIngest(fbf, fakeStore, skillfetch.DefaultPlainTarCapBytes)

	req := func() *connect.Request[cpv1.IngestSkillFromURLRequest] {
		return connect.NewRequest(&cpv1.IngestSkillFromURLRequest{Url: "o/shachange"})
	}
	if _, err := s.IngestSkillFromURL(ctx, req()); err != nil {
		t.Fatalf("first ingest: %v", err)
	}
	if _, err := s.IngestSkillFromURL(ctx, req()); err != nil {
		t.Fatalf("second ingest: %v", err)
	}

	bundle, err := s.st.SkillBundles().GetByKey(context.Background(), owner, "o/shachange", "", "")
	if err != nil {
		t.Fatalf("GetByKey: %v", err)
	}
	versions, err := s.st.SkillBundles().ListVersions(context.Background(), bundle.BundleID)
	if err != nil {
		t.Fatalf("ListVersions: %v", err)
	}
	if len(versions) != 2 {
		t.Fatalf("got %d versions, want 2", len(versions))
	}

	v1members, err := s.st.SkillBundles().Members(context.Background(), versions[0].VersionID)
	if err != nil {
		t.Fatalf("v1 Members: %v", err)
	}
	v2members, err := s.st.SkillBundles().Members(context.Background(), versions[1].VersionID)
	if err != nil {
		t.Fatalf("v2 Members: %v", err)
	}
	v1ByDir := memberMap(v1members)
	v2ByDir := memberMap(v2members)

	if v1ByDir["skills/alpha"] != v2ByDir["skills/alpha"] {
		t.Fatalf("unchanged member alpha got a new catalog_id: v1=%s v2=%s", v1ByDir["skills/alpha"], v2ByDir["skills/alpha"])
	}
	if v1ByDir["skills/beta"] == v2ByDir["skills/beta"] {
		t.Fatalf("changed member beta reused the same catalog_id: %s", v1ByDir["skills/beta"])
	}

	// v1's member rows must still be intact after the cut.
	v1membersAfter, err := s.st.SkillBundles().Members(context.Background(), versions[0].VersionID)
	if err != nil {
		t.Fatalf("v1 Members after cut: %v", err)
	}
	if len(v1membersAfter) != 2 {
		t.Fatalf("v1 member rows disturbed: got %d, want 2", len(v1membersAfter))
	}
}

// --- S5: version cut on member set change -------------------------------------------------------

func TestIngestBundle_VersionCut_OnMemberSetChange(t *testing.T) {
	s, _, _ := newTestServer(t)
	owner := bundleTestOwner(t)
	ctx := makeOwnerCtx(owner)
	fakeStore := skillstore.NewFakeSkillStore()

	alpha := makeMember("skills/alpha", "alpha", "", "alpha-body")
	beta := makeMember("skills/beta", "beta", "", "beta-body")
	gamma := makeMember("skills/gamma", "gamma", "", "gamma-body")

	fbf := &fakeBundleFetcher{scripted: []bundleScript{
		{result: skillfetch.BundleResult{Owner: "o", Repo: "setchange", Members: []skillfetch.Member{alpha, beta}}},
		{result: skillfetch.BundleResult{Owner: "o", Repo: "setchange", Members: []skillfetch.Member{alpha, beta, gamma}}},
	}}
	s.SetSkillIngest(fbf, fakeStore, skillfetch.DefaultPlainTarCapBytes)

	req := func() *connect.Request[cpv1.IngestSkillFromURLRequest] {
		return connect.NewRequest(&cpv1.IngestSkillFromURLRequest{Url: "o/setchange"})
	}
	if _, err := s.IngestSkillFromURL(ctx, req()); err != nil {
		t.Fatalf("first ingest: %v", err)
	}
	if _, err := s.IngestSkillFromURL(ctx, req()); err != nil {
		t.Fatalf("second ingest: %v", err)
	}

	bundle, err := s.st.SkillBundles().GetByKey(context.Background(), owner, "o/setchange", "", "")
	if err != nil {
		t.Fatalf("GetByKey: %v", err)
	}
	versions, err := s.st.SkillBundles().ListVersions(context.Background(), bundle.BundleID)
	if err != nil {
		t.Fatalf("ListVersions: %v", err)
	}
	if len(versions) != 2 {
		t.Fatalf("got %d versions, want 2", len(versions))
	}
	v2members, err := s.st.SkillBundles().Members(context.Background(), versions[1].VersionID)
	if err != nil {
		t.Fatalf("v2 Members: %v", err)
	}
	if len(v2members) != 3 {
		t.Fatalf("got %d members in v2, want 3", len(v2members))
	}
	gotDirs := map[string]bool{}
	for _, m := range v2members {
		gotDirs[m.SourceSubdir] = true
	}
	for _, want := range []string{"skills/alpha", "skills/beta", "skills/gamma"} {
		if !gotDirs[want] {
			t.Fatalf("v2 members missing %q: %v", want, gotDirs)
		}
	}
}

// --- S6: revert A->B->A cuts no phantom version (acceptance criterion) --------------------------

func TestIngestBundle_RevertAToBToA_NoPhantomVersion(t *testing.T) {
	s, _, _ := newTestServer(t)
	owner := bundleTestOwner(t)
	ctx := makeOwnerCtx(owner)
	fakeStore := skillstore.NewFakeSkillStore()

	memberA := makeMember("", "revert-skill", "", "content-A")
	memberB := makeMember("", "revert-skill", "", "content-B")

	fbf := &fakeBundleFetcher{scripted: []bundleScript{
		{result: skillfetch.BundleResult{Owner: "o", Repo: "revert", Members: []skillfetch.Member{memberA}}}, // v1: A
		{result: skillfetch.BundleResult{Owner: "o", Repo: "revert", Members: []skillfetch.Member{memberB}}}, // v2: B
		{result: skillfetch.BundleResult{Owner: "o", Repo: "revert", Members: []skillfetch.Member{memberA}}}, // v3: back to A
	}}
	s.SetSkillIngest(fbf, fakeStore, skillfetch.DefaultPlainTarCapBytes)

	req := func() *connect.Request[cpv1.IngestSkillFromURLRequest] {
		return connect.NewRequest(&cpv1.IngestSkillFromURLRequest{Url: "o/revert"})
	}

	resp1, err := s.IngestSkillFromURL(ctx, req())
	if err != nil {
		t.Fatalf("ingest A (v1): %v", err)
	}
	catalogA := resp1.Msg.CatalogId

	if _, err := s.IngestSkillFromURL(ctx, req()); err != nil {
		t.Fatalf("ingest B (v2): %v", err)
	}

	// Force the wall clock into a new Unix second before the revert, so the assertion below
	// (reused row's created_at strictly older than v3's) is not a same-second coin flip.
	waitForNextUnixSecond(t)

	resp3, err := s.IngestSkillFromURL(ctx, req())
	if err != nil {
		t.Fatalf("ingest A again (v3): %v", err)
	}
	if resp3.Msg.CatalogId != catalogA {
		t.Fatalf("v3 catalog_id = %q, want reused v1 catalog_id %q (content-addressed dedup)", resp3.Msg.CatalogId, catalogA)
	}

	bundle, err := s.st.SkillBundles().GetByKey(context.Background(), owner, "o/revert", "", "")
	if err != nil {
		t.Fatalf("GetByKey: %v", err)
	}
	versions, err := s.st.SkillBundles().ListVersions(context.Background(), bundle.BundleID)
	if err != nil {
		t.Fatalf("ListVersions: %v", err)
	}
	if len(versions) != 3 {
		t.Fatalf("got %d versions after A->B->A, want 3", len(versions))
	}
	v3 := versions[2]

	catalogEntry, err := s.st.CustomizationCatalog().Get(context.Background(), catalogA)
	if err != nil {
		t.Fatalf("Get catalog entry: %v", err)
	}
	if catalogEntry.CreatedAt >= v3.CreatedAt {
		t.Fatalf("reused row created_at (%d) is not strictly older than v3's created_at (%d) — "+
			"a created_at-based comparison would see this as \"changed\" forever", catalogEntry.CreatedAt, v3.CreatedAt)
	}

	// The critical assertion: re-ingesting A again cuts NO further version.
	if _, err := s.IngestSkillFromURL(ctx, req()); err != nil {
		t.Fatalf("re-ingest A again: %v", err)
	}
	versionsAfter, err := s.st.SkillBundles().ListVersions(context.Background(), bundle.BundleID)
	if err != nil {
		t.Fatalf("ListVersions after re-ingest: %v", err)
	}
	if len(versionsAfter) != 3 {
		t.Fatalf("got %d versions after re-ingesting A again, want still 3 (no phantom version)", len(versionsAfter))
	}
}

// --- S7: member cap rejected at ingest, nothing written (acceptance criterion) ------------------

func TestIngestBundle_MemberCap_RejectedWithNoWrites(t *testing.T) {
	s, _, _ := newTestServer(t)
	owner := bundleTestOwner(t)
	ctx := makeOwnerCtx(owner)
	fakeStore := skillstore.NewFakeSkillStore()

	members := make([]skillfetch.Member, maxBundleMembers+1)
	for i := range members {
		members[i] = makeMember(fmt.Sprintf("skills/m%03d", i), fmt.Sprintf("skill-%03d", i), "", fmt.Sprintf("body-%d", i))
	}
	fbf := &fakeBundleFetcher{scripted: []bundleScript{{result: skillfetch.BundleResult{Owner: "o", Repo: "huge", Members: members}}}}
	s.SetSkillIngest(fbf, fakeStore, skillfetch.DefaultPlainTarCapBytes)

	_, err := s.IngestSkillFromURL(ctx, connect.NewRequest(&cpv1.IngestSkillFromURLRequest{Url: "o/huge"}))
	if err == nil {
		t.Fatal("expected InvalidArgument for a bundle exceeding the member cap")
	}
	var ce *connect.Error
	if !errors.As(err, &ce) || ce.Code() != connect.CodeInvalidArgument {
		t.Fatalf("expected CodeInvalidArgument, got: %v", err)
	}
	if !strings.Contains(err.Error(), fmt.Sprintf("%d", maxBundleMembers)) {
		t.Fatalf("error does not name the cap %d: %v", maxBundleMembers, err)
	}
	if !strings.Contains(err.Error(), "subdir") {
		t.Fatalf("error does not suggest subdir scoping: %v", err)
	}

	// Nothing was written: no Garage puts, no catalog rows, no bundle row — the whole point of
	// enforcing the cap before any write.
	if calls := fakeStore.Calls(); len(calls) != 0 {
		t.Fatalf("expected zero Garage store calls, got %v", calls)
	}
	entries, err := s.st.CustomizationCatalog().ListByCreator(context.Background(), owner)
	if err != nil {
		t.Fatalf("ListByCreator: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("expected zero catalog rows, got %d", len(entries))
	}
	if _, err := s.st.SkillBundles().GetByKey(context.Background(), owner, "o/huge", "", ""); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("expected no bundle row, got err=%v", err)
	}
}

// --- S8: layout-change branch (§4.5) -------------------------------------------------------------

func TestIngestBundle_LayoutChange_UpstreamLayoutChanged(t *testing.T) {
	s, _, _ := newTestServer(t)
	owner := bundleTestOwner(t)
	ctx := makeOwnerCtx(owner)
	fakeStore := skillstore.NewFakeSkillStore()

	m1 := makeMember("skills/a", "a", "", "a-body")
	m2 := makeMember("skills/b", "b", "", "b-body")
	fbf := &fakeBundleFetcher{scripted: []bundleScript{
		{result: skillfetch.BundleResult{Owner: "o", Repo: "layout", Members: []skillfetch.Member{m1, m2}}},
		{err: skillfetch.ErrNoSkills},
	}}
	s.SetSkillIngest(fbf, fakeStore, skillfetch.DefaultPlainTarCapBytes)

	req := func() *connect.Request[cpv1.IngestSkillFromURLRequest] {
		return connect.NewRequest(&cpv1.IngestSkillFromURLRequest{Url: "o/layout"})
	}

	if _, err := s.IngestSkillFromURL(ctx, req()); err != nil {
		t.Fatalf("first ingest: %v", err)
	}

	_, err := s.IngestSkillFromURL(ctx, req())
	if err == nil {
		t.Fatal("expected FailedPrecondition for a layout change")
	}
	var ce *connect.Error
	if !errors.As(err, &ce) || ce.Code() != connect.CodeFailedPrecondition {
		t.Fatalf("expected CodeFailedPrecondition, got: %v", err)
	}
	if !strings.Contains(err.Error(), "upstream layout changed") {
		t.Fatalf("error does not mention upstream layout changed: %v", err)
	}

	// Pin intact: still 1 version with both original members.
	bundle, err := s.st.SkillBundles().GetByKey(context.Background(), owner, "o/layout", "", "")
	if err != nil {
		t.Fatalf("GetByKey: %v", err)
	}
	versions, err := s.st.SkillBundles().ListVersions(context.Background(), bundle.BundleID)
	if err != nil {
		t.Fatalf("ListVersions: %v", err)
	}
	if len(versions) != 1 {
		t.Fatalf("got %d versions, want 1 (pin intact)", len(versions))
	}
	members, err := s.st.SkillBundles().Members(context.Background(), versions[0].VersionID)
	if err != nil {
		t.Fatalf("Members: %v", err)
	}
	if len(members) != 2 {
		t.Fatalf("got %d members, want 2 (pin intact)", len(members))
	}
}

func TestIngestBundle_LayoutChange_FirstIngest_InvalidArgument(t *testing.T) {
	s, _, _ := newTestServer(t)
	owner := bundleTestOwner(t)
	ctx := makeOwnerCtx(owner)
	fakeStore := skillstore.NewFakeSkillStore()

	fbf := &fakeBundleFetcher{scripted: []bundleScript{{err: skillfetch.ErrNoSkills}}}
	s.SetSkillIngest(fbf, fakeStore, skillfetch.DefaultPlainTarCapBytes)

	_, err := s.IngestSkillFromURL(ctx, connect.NewRequest(&cpv1.IngestSkillFromURLRequest{Url: "o/empty"}))
	if err == nil {
		t.Fatal("expected an error for a skill-less repo on first ingest")
	}
	var ce *connect.Error
	if !errors.As(err, &ce) || ce.Code() != connect.CodeInvalidArgument {
		t.Fatalf("expected CodeInvalidArgument (not FailedPrecondition) for a first-ever ingest, got: %v", err)
	}
	if _, err := s.st.SkillBundles().GetByKey(context.Background(), owner, "o/empty", "", ""); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("expected no bundle row, got err=%v", err)
	}
}

// --- S9: CP-wide concurrency semaphore (acceptance criterion) -----------------------------------

func TestIngestBundle_Semaphore_BoundsConcurrency(t *testing.T) {
	s, _, _ := newTestServer(t)
	fakeStore := skillstore.NewFakeSkillStore()

	gate := make(chan struct{})
	member := makeMember("", "concurrent-skill", "", "same-body")
	fbf := &fakeBundleFetcher{
		gate:     gate,
		scripted: []bundleScript{{result: skillfetch.BundleResult{Owner: "o", Repo: "r", Members: []skillfetch.Member{member}}}},
	}
	s.SetSkillIngest(fbf, fakeStore, skillfetch.DefaultPlainTarCapBytes)

	const n = maxConcurrentIngest + 2
	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ctx := makeOwnerCtx(fmt.Sprintf("sem-owner-%d", i))
			_, err := s.IngestSkillFromURL(ctx, connect.NewRequest(&cpv1.IngestSkillFromURLRequest{
				Url: fmt.Sprintf("owner%d/repo%d", i, i),
			}))
			errs[i] = err
		}(i)
	}

	// Poll until in-flight reaches the cap (bounded wait — all n goroutines must have reached
	// either "blocked inside FetchBundle on the gate" or "blocked in acquireIngestSlot").
	deadline := time.Now().Add(5 * time.Second)
	for atomic.LoadInt32(&fbf.maxInFlight) < maxConcurrentIngest && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if got := atomic.LoadInt32(&fbf.maxInFlight); got != maxConcurrentIngest {
		t.Fatalf("max in-flight = %d, want exactly %d before the gate opens", got, maxConcurrentIngest)
	}
	// Give any (buggy) extra admission a moment to show up before we release the gate.
	time.Sleep(20 * time.Millisecond)
	if got := atomic.LoadInt32(&fbf.maxInFlight); got > maxConcurrentIngest {
		t.Fatalf("in-flight exceeded the semaphore bound: %d > %d", got, maxConcurrentIngest)
	}

	close(gate) // release every blocked call at once
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("ingest %d: unexpected error: %v", i, err)
		}
	}
}

// TestIngestBundle_Semaphore_CtxCanceled_DoesNotLeakSlot fills the semaphore, then proves a
// canceled ctx waiting on a full semaphore returns CodeCanceled without acquiring (and therefore
// without leaking) a slot: a subsequent ingest still succeeds once the fillers are released.
func TestIngestBundle_Semaphore_CtxCanceled_DoesNotLeakSlot(t *testing.T) {
	s, _, _ := newTestServer(t)
	fakeStore := skillstore.NewFakeSkillStore()

	gate := make(chan struct{})
	member := makeMember("", "cancel-skill", "", "body")
	fbf := &fakeBundleFetcher{
		gate:     gate,
		scripted: []bundleScript{{result: skillfetch.BundleResult{Owner: "o", Repo: "r", Members: []skillfetch.Member{member}}}},
	}
	s.SetSkillIngest(fbf, fakeStore, skillfetch.DefaultPlainTarCapBytes)

	var wg sync.WaitGroup
	for i := 0; i < maxConcurrentIngest; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ctx := makeOwnerCtx(fmt.Sprintf("fill-owner-%d", i))
			_, _ = s.IngestSkillFromURL(ctx, connect.NewRequest(&cpv1.IngestSkillFromURLRequest{Url: fmt.Sprintf("fill%d/repo", i)}))
		}(i)
	}

	deadline := time.Now().Add(5 * time.Second)
	for atomic.LoadInt32(&fbf.inFlight) < maxConcurrentIngest && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if got := atomic.LoadInt32(&fbf.inFlight); got != maxConcurrentIngest {
		t.Fatalf("failed to fill the semaphore: in-flight = %d, want %d", got, maxConcurrentIngest)
	}

	cancelCtx, cancel := context.WithCancel(makeOwnerCtx("cancel-owner"))
	cancel()
	_, err := s.IngestSkillFromURL(cancelCtx, connect.NewRequest(&cpv1.IngestSkillFromURLRequest{Url: "cancel/repo"}))
	if err == nil {
		t.Fatal("expected an error for a canceled context waiting on a full semaphore")
	}
	var ce *connect.Error
	if !errors.As(err, &ce) || ce.Code() != connect.CodeCanceled {
		t.Fatalf("expected CodeCanceled, got: %v", err)
	}

	close(gate)
	wg.Wait()

	// A subsequent ingest must still succeed — the canceled attempt did not leak a slot.
	_, err = s.IngestSkillFromURL(makeOwnerCtx("after-cancel-owner"), connect.NewRequest(&cpv1.IngestSkillFromURLRequest{Url: "after/repo"}))
	if err != nil {
		t.Fatalf("post-cancellation ingest: unexpected error: %v", err)
	}
}
