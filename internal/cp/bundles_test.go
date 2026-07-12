package cp

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"

	cpv1 "spawnery/gen/cp/v1"
	"spawnery/internal/cp/skillfetch"
	"spawnery/internal/cp/skillstore"
	"spawnery/internal/cp/store"
)

// This file holds ALL tests for sp-mwco.1.7 (RPC + proto bundle surface): ReingestBundle,
// ListBundles, GetBundle, GetBundleDiff, the CP-wide refetch budget, and the diff-token gate.
// Kept separate from ingest_bundle_test.go / ingest_skill_test.go to minimize conflict surface;
// reuses their fakeBundleFetcher/makeMember/bundleTestOwner/memberMap scaffolding (same package).

// --- scaffolding: fakeConditionalBundleFetcher ---------------------------------------------

// conditionalScript is one scripted FetchBundleConditional call outcome.
type conditionalScript struct {
	result      skillfetch.BundleResult
	notModified bool
	err         error
}

// fakeConditionalBundleFetcher implements skillfetch.BundleFetcher AND
// skillfetch.ConditionalBundleFetcher, so ReingestBundle's conditional path can be exercised.
// scripted supports the same per-call script convention as fakeBundleFetcher: call N gets
// scripted[N], repeating the last entry once exhausted. etagsSeen records the etag argument of
// every FetchBundleConditional call, in order — tests assert against it to prove the CP actually
// sent the bundle's stored etag upstream.
type fakeConditionalBundleFetcher struct {
	mu        sync.Mutex
	scripted  []conditionalScript
	calls     int
	etagsSeen []string
}

func (f *fakeConditionalBundleFetcher) Fetch(_ context.Context, _ skillfetch.RepoRef, _, _, _, _ string) (skillfetch.Result, error) {
	return skillfetch.Result{}, fmt.Errorf("fakeConditionalBundleFetcher: Fetch is not implemented (bundle-only test double)")
}

func (f *fakeConditionalBundleFetcher) FetchBundle(ctx context.Context, ref skillfetch.RepoRef, gitRef, subdir string) (skillfetch.BundleResult, error) {
	res, _, err := f.FetchBundleConditional(ctx, ref, gitRef, subdir, "")
	return res, err
}

func (f *fakeConditionalBundleFetcher) FetchBundleConditional(_ context.Context, _ skillfetch.RepoRef, _, _, etag string) (skillfetch.BundleResult, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.etagsSeen = append(f.etagsSeen, etag)
	idx := f.calls
	if idx >= len(f.scripted) {
		idx = len(f.scripted) - 1
	}
	sc := f.scripted[idx]
	f.calls++
	return sc.result, sc.notModified, sc.err
}

func (f *fakeConditionalBundleFetcher) Etags() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.etagsSeen...)
}

// seedBundle ingests member(s) via IngestSkillFromURL using fcf's first script entry, and returns
// the resulting bundle row (fresh from the store, so ETag reflects what ingestBundle persisted).
func seedBundleForReingest(t *testing.T, s *Server, ctx context.Context, owner, url string) store.SkillBundle {
	t.Helper()
	resp, err := s.IngestSkillFromURL(ctx, connect.NewRequest(&cpv1.IngestSkillFromURLRequest{Url: url}))
	if err != nil {
		t.Fatalf("seed IngestSkillFromURL: %v", err)
	}
	bundle, err := s.st.SkillBundles().Get(context.Background(), resp.Msg.BundleId)
	if err != nil {
		t.Fatalf("seed Get bundle: %v", err)
	}
	return bundle
}

// --- ReingestBundle ---------------------------------------------------------------------------

func TestReingestBundle_Unchanged_Idempotent(t *testing.T) {
	s, _, _ := newTestServer(t)
	owner := bundleTestOwner(t)
	ctx := makeOwnerCtx(owner)
	fakeStore := skillstore.NewFakeSkillStore()

	m1 := makeMember("skills/alpha", "alpha", "", "alpha-body")
	fcf := &fakeConditionalBundleFetcher{scripted: []conditionalScript{
		{result: skillfetch.BundleResult{Owner: "o", Repo: "unchanged", SourceCommit: "c1", Members: []skillfetch.Member{m1}, ETag: `"etag-1"`}},
		{result: skillfetch.BundleResult{Owner: "o", Repo: "unchanged", SourceCommit: "c1", Members: []skillfetch.Member{m1}, ETag: `"etag-2"`}},
	}}
	s.SetSkillIngest(fcf, fakeStore, skillfetch.DefaultPlainTarCapBytes)

	bundle := seedBundleForReingest(t, s, ctx, owner, "o/unchanged")

	resp, err := s.ReingestBundle(ctx, connect.NewRequest(&cpv1.ReingestBundleRequest{BundleId: bundle.BundleID}))
	if err != nil {
		t.Fatalf("ReingestBundle: %v", err)
	}
	if resp.Msg.Changed {
		t.Fatal("expected changed=false for an unchanged upstream")
	}
	if resp.Msg.DiffToken != "" {
		t.Fatalf("expected no diff_token when unchanged, got %q", resp.Msg.DiffToken)
	}

	versions, err := s.st.SkillBundles().ListVersions(context.Background(), bundle.BundleID)
	if err != nil {
		t.Fatalf("ListVersions: %v", err)
	}
	if len(versions) != 1 {
		t.Fatalf("got %d versions after an unchanged ReingestBundle, want 1 (idempotent)", len(versions))
	}
	if resp.Msg.VersionId != versions[0].VersionID {
		t.Fatalf("VersionId = %q, want the pinned version %q", resp.Msg.VersionId, versions[0].VersionID)
	}

	// Sent the stored etag from the first ingest.
	if etags := fcf.Etags(); len(etags) != 2 || etags[1] != `"etag-1"` {
		t.Fatalf("etags seen = %v, want [\"\" \"etag-1\"]", etags)
	}
}

func TestReingestBundle_304_ShortCircuitsToUpToDate(t *testing.T) {
	s, _, _ := newTestServer(t)
	owner := bundleTestOwner(t)
	ctx := makeOwnerCtx(owner)
	fakeStore := skillstore.NewFakeSkillStore()

	m1 := makeMember("", "solo", "", "body")
	fcf := &fakeConditionalBundleFetcher{scripted: []conditionalScript{
		{result: skillfetch.BundleResult{Owner: "o", Repo: "notmod", Members: []skillfetch.Member{m1}, ETag: `"v1-etag"`}},
		{notModified: true},
	}}
	s.SetSkillIngest(fcf, fakeStore, skillfetch.DefaultPlainTarCapBytes)

	bundle := seedBundleForReingest(t, s, ctx, owner, "o/notmod")
	if bundle.ETag != `"v1-etag"` {
		t.Fatalf("seeded bundle ETag = %q, want %q", bundle.ETag, `"v1-etag"`)
	}

	resp, err := s.ReingestBundle(ctx, connect.NewRequest(&cpv1.ReingestBundleRequest{BundleId: bundle.BundleID}))
	if err != nil {
		t.Fatalf("ReingestBundle: %v", err)
	}
	if !resp.Msg.NotModified {
		t.Fatal("expected not_modified=true for a 304")
	}
	if resp.Msg.Changed {
		t.Fatal("expected changed=false alongside not_modified=true")
	}

	versions, err := s.st.SkillBundles().ListVersions(context.Background(), bundle.BundleID)
	if err != nil {
		t.Fatalf("ListVersions: %v", err)
	}
	if len(versions) != 1 {
		t.Fatalf("got %d versions after a 304, want 1 (no repack, no DB write)", len(versions))
	}
	if resp.Msg.VersionId != versions[0].VersionID {
		t.Fatalf("VersionId = %q, want the pinned version %q", resp.Msg.VersionId, versions[0].VersionID)
	}

	// The stored etag was sent upstream (proves the conditional path fired, not an unconditional one).
	etags := fcf.Etags()
	if len(etags) != 2 || etags[1] != `"v1-etag"` {
		t.Fatalf("etags seen = %v, want [\"\" \"v1-etag\"]", etags)
	}
}

func TestReingestBundle_Changed_CutsVersionAndMintsDiffToken(t *testing.T) {
	s, _, _ := newTestServer(t)
	owner := bundleTestOwner(t)
	ctx := makeOwnerCtx(owner)
	fakeStore := skillstore.NewFakeSkillStore()

	alpha := makeMember("skills/alpha", "alpha", "", "alpha-v1")
	beta := makeMember("skills/beta", "beta", "", "beta-body")
	alphaV2 := makeMember("skills/alpha", "alpha", "", "alpha-v2")
	gamma := makeMember("skills/gamma", "gamma", "", "gamma-body")

	fcf := &fakeConditionalBundleFetcher{scripted: []conditionalScript{
		{result: skillfetch.BundleResult{Owner: "o", Repo: "chg", Members: []skillfetch.Member{alpha, beta}}},
		{result: skillfetch.BundleResult{Owner: "o", Repo: "chg", Members: []skillfetch.Member{alphaV2, gamma}}},
	}}
	s.SetSkillIngest(fcf, fakeStore, skillfetch.DefaultPlainTarCapBytes)

	bundle := seedBundleForReingest(t, s, ctx, owner, "o/chg")
	v1, err := s.st.SkillBundles().LatestVersion(context.Background(), bundle.BundleID)
	if err != nil {
		t.Fatalf("LatestVersion: %v", err)
	}

	resp, err := s.ReingestBundle(ctx, connect.NewRequest(&cpv1.ReingestBundleRequest{BundleId: bundle.BundleID}))
	if err != nil {
		t.Fatalf("ReingestBundle: %v", err)
	}
	if !resp.Msg.Changed {
		t.Fatal("expected changed=true")
	}
	if resp.Msg.DiffToken == "" {
		t.Fatal("expected a non-empty diff_token when changed=true")
	}
	if resp.Msg.FromVersionId != v1.VersionID {
		t.Fatalf("FromVersionId = %q, want %q", resp.Msg.FromVersionId, v1.VersionID)
	}
	if resp.Msg.VersionId == v1.VersionID {
		t.Fatal("expected a new version_id")
	}

	if len(resp.Msg.UpdatedSubdirs) != 1 || resp.Msg.UpdatedSubdirs[0] != "skills/alpha" {
		t.Fatalf("UpdatedSubdirs = %v, want [skills/alpha]", resp.Msg.UpdatedSubdirs)
	}
	if len(resp.Msg.AddedSubdirs) != 1 || resp.Msg.AddedSubdirs[0] != "skills/gamma" {
		t.Fatalf("AddedSubdirs = %v, want [skills/gamma]", resp.Msg.AddedSubdirs)
	}
	if len(resp.Msg.RemovedSubdirs) != 1 || resp.Msg.RemovedSubdirs[0] != "skills/beta" {
		t.Fatalf("RemovedSubdirs = %v, want [skills/beta]", resp.Msg.RemovedSubdirs)
	}

	versions, err := s.st.SkillBundles().ListVersions(context.Background(), bundle.BundleID)
	if err != nil {
		t.Fatalf("ListVersions: %v", err)
	}
	if len(versions) != 2 {
		t.Fatalf("got %d versions, want 2", len(versions))
	}
}

func TestReingestBundle_LayoutGone_FailedPrecondition_PinIntact(t *testing.T) {
	s, _, _ := newTestServer(t)
	owner := bundleTestOwner(t)
	ctx := makeOwnerCtx(owner)
	fakeStore := skillstore.NewFakeSkillStore()

	m1 := makeMember("", "solo", "", "body")
	fcf := &fakeConditionalBundleFetcher{scripted: []conditionalScript{
		{result: skillfetch.BundleResult{Owner: "o", Repo: "gone", Members: []skillfetch.Member{m1}}},
		{err: skillfetch.ErrNoSkills},
	}}
	s.SetSkillIngest(fcf, fakeStore, skillfetch.DefaultPlainTarCapBytes)

	bundle := seedBundleForReingest(t, s, ctx, owner, "o/gone")

	_, err := s.ReingestBundle(ctx, connect.NewRequest(&cpv1.ReingestBundleRequest{BundleId: bundle.BundleID}))
	if err == nil {
		t.Fatal("expected FailedPrecondition for a layout change")
	}
	var ce *connect.Error
	if !errors.As(err, &ce) || ce.Code() != connect.CodeFailedPrecondition {
		t.Fatalf("expected CodeFailedPrecondition, got: %v", err)
	}

	versions, err := s.st.SkillBundles().ListVersions(context.Background(), bundle.BundleID)
	if err != nil {
		t.Fatalf("ListVersions: %v", err)
	}
	if len(versions) != 1 {
		t.Fatalf("got %d versions, want 1 (pin intact)", len(versions))
	}
}

func TestReingestBundle_NonCreator_PermissionDenied(t *testing.T) {
	s, _, _ := newTestServer(t)
	owner := bundleTestOwner(t)
	ctx := makeOwnerCtx(owner)
	fakeStore := skillstore.NewFakeSkillStore()

	m1 := makeMember("", "solo", "", "body")
	fcf := &fakeConditionalBundleFetcher{scripted: []conditionalScript{
		{result: skillfetch.BundleResult{Owner: "o", Repo: "priv", Members: []skillfetch.Member{m1}}},
	}}
	s.SetSkillIngest(fcf, fakeStore, skillfetch.DefaultPlainTarCapBytes)

	bundle := seedBundleForReingest(t, s, ctx, owner, "o/priv")

	other := makeOwnerCtx(bundleTestOwner(t))
	_, err := s.ReingestBundle(other, connect.NewRequest(&cpv1.ReingestBundleRequest{BundleId: bundle.BundleID}))
	if err == nil {
		t.Fatal("expected PermissionDenied for a non-creator")
	}
	var ce *connect.Error
	if !errors.As(err, &ce) || ce.Code() != connect.CodePermissionDenied {
		t.Fatalf("expected CodePermissionDenied, got: %v", err)
	}
}

func TestReingestBundle_UnknownBundle_NotFound(t *testing.T) {
	s, _, _ := newTestServer(t)
	ctx := makeOwnerCtx(bundleTestOwner(t))

	_, err := s.ReingestBundle(ctx, connect.NewRequest(&cpv1.ReingestBundleRequest{BundleId: "does-not-exist"}))
	if err == nil {
		t.Fatal("expected NotFound for an unknown bundle")
	}
	var ce *connect.Error
	if !errors.As(err, &ce) || ce.Code() != connect.CodeNotFound {
		t.Fatalf("expected CodeNotFound, got: %v", err)
	}
}

func TestReingestBundle_BudgetExhausted_ResourceExhausted(t *testing.T) {
	s, _, _ := newTestServer(t)
	owner := bundleTestOwner(t)
	ctx := makeOwnerCtx(owner)
	fakeStore := skillstore.NewFakeSkillStore()

	m1 := makeMember("", "solo", "", "body")
	fcf := &fakeConditionalBundleFetcher{scripted: []conditionalScript{
		{result: skillfetch.BundleResult{Owner: "o", Repo: "budget", Members: []skillfetch.Member{m1}}},
	}}
	s.SetSkillIngest(fcf, fakeStore, skillfetch.DefaultPlainTarCapBytes)

	bundle := seedBundleForReingest(t, s, ctx, owner, "o/budget")

	// Shrink the CP-wide budget to zero remaining (same package: direct field access).
	s.reingestBudget = newRefetchBudget(0, time.Hour)

	_, err := s.ReingestBundle(ctx, connect.NewRequest(&cpv1.ReingestBundleRequest{BundleId: bundle.BundleID}))
	if err == nil {
		t.Fatal("expected ResourceExhausted when the CP-wide refetch budget is exhausted")
	}
	var ce *connect.Error
	if !errors.As(err, &ce) || ce.Code() != connect.CodeResourceExhausted {
		t.Fatalf("expected CodeResourceExhausted, got: %v", err)
	}
	if !strings.Contains(err.Error(), "CP-wide") {
		t.Fatalf("error does not name the CP-wide budget: %v", err)
	}
}

// --- ListBundles / GetBundle -------------------------------------------------------------------

func TestListBundles_OnlyOwnBundles_WithSummary(t *testing.T) {
	s, _, _ := newTestServer(t)
	owner := bundleTestOwner(t)
	ctx := makeOwnerCtx(owner)
	fakeStore := skillstore.NewFakeSkillStore()

	m1 := makeMember("skills/a", "a", "", "a-body")
	m2 := makeMember("skills/b", "b", "", "b-body")
	fcf := &fakeConditionalBundleFetcher{scripted: []conditionalScript{
		{result: skillfetch.BundleResult{Owner: "o", Repo: "listme", SourceCommit: "abc1234", Members: []skillfetch.Member{m1, m2}}},
	}}
	s.SetSkillIngest(fcf, fakeStore, skillfetch.DefaultPlainTarCapBytes)
	bundle := seedBundleForReingest(t, s, ctx, owner, "o/listme")

	// A second owner's bundle must not appear.
	otherOwner := bundleTestOwner(t)
	otherCtx := makeOwnerCtx(otherOwner)
	fcf2 := &fakeConditionalBundleFetcher{scripted: []conditionalScript{
		{result: skillfetch.BundleResult{Owner: "o", Repo: "other", Members: []skillfetch.Member{makeMember("", "x", "", "x-body")}}},
	}}
	s.SetSkillIngest(fcf2, fakeStore, skillfetch.DefaultPlainTarCapBytes)
	seedBundleForReingest(t, s, otherCtx, otherOwner, "o/other")

	s.SetSkillIngest(fcf, fakeStore, skillfetch.DefaultPlainTarCapBytes)
	resp, err := s.ListBundles(ctx, connect.NewRequest(&cpv1.ListBundlesRequest{}))
	if err != nil {
		t.Fatalf("ListBundles: %v", err)
	}
	if len(resp.Msg.Bundles) != 1 {
		t.Fatalf("got %d bundles, want 1 (own only)", len(resp.Msg.Bundles))
	}
	got := resp.Msg.Bundles[0]
	if got.BundleId != bundle.BundleID {
		t.Fatalf("BundleId = %q, want %q", got.BundleId, bundle.BundleID)
	}
	if got.LatestSeq != 1 {
		t.Fatalf("LatestSeq = %d, want 1", got.LatestSeq)
	}
	if got.MemberCount != 2 {
		t.Fatalf("MemberCount = %d, want 2", got.MemberCount)
	}
	if got.SourceUrl != "o/listme" {
		t.Fatalf("SourceUrl = %q, want %q", got.SourceUrl, "o/listme")
	}
}

func TestGetBundle_VersionsAndLatestMembers(t *testing.T) {
	s, _, _ := newTestServer(t)
	owner := bundleTestOwner(t)
	ctx := makeOwnerCtx(owner)
	fakeStore := skillstore.NewFakeSkillStore()

	alpha := makeMember("skills/alpha", "alpha", "", "alpha-v1")
	alphaV2 := makeMember("skills/alpha", "alpha", "", "alpha-v2")
	fcf := &fakeConditionalBundleFetcher{scripted: []conditionalScript{
		{result: skillfetch.BundleResult{Owner: "o", Repo: "getb", SourceCommit: "c1", Members: []skillfetch.Member{alpha}}},
		{result: skillfetch.BundleResult{Owner: "o", Repo: "getb", SourceCommit: "c2", Members: []skillfetch.Member{alphaV2}}},
	}}
	s.SetSkillIngest(fcf, fakeStore, skillfetch.DefaultPlainTarCapBytes)
	bundle := seedBundleForReingest(t, s, ctx, owner, "o/getb")

	if _, err := s.ReingestBundle(ctx, connect.NewRequest(&cpv1.ReingestBundleRequest{BundleId: bundle.BundleID})); err != nil {
		t.Fatalf("ReingestBundle: %v", err)
	}

	resp, err := s.GetBundle(ctx, connect.NewRequest(&cpv1.GetBundleRequest{BundleId: bundle.BundleID}))
	if err != nil {
		t.Fatalf("GetBundle: %v", err)
	}
	if len(resp.Msg.Versions) != 2 {
		t.Fatalf("got %d versions, want 2", len(resp.Msg.Versions))
	}
	if resp.Msg.Versions[0].Seq != 1 || resp.Msg.Versions[1].Seq != 2 {
		t.Fatalf("versions not seq ASC: %+v", resp.Msg.Versions)
	}
	if resp.Msg.Versions[0].SourceCommit != "c1" || resp.Msg.Versions[1].SourceCommit != "c2" {
		t.Fatalf("source_commit provenance wrong: %+v", resp.Msg.Versions)
	}
	if len(resp.Msg.Members) != 1 {
		t.Fatalf("got %d members, want 1 (latest version only)", len(resp.Msg.Members))
	}
	if resp.Msg.Members[0].Sha256 != alphaV2.PlainTarSHA256 {
		t.Fatalf("latest member sha = %q, want the v2 sha %q", resp.Msg.Members[0].Sha256, alphaV2.PlainTarSHA256)
	}
	if resp.Msg.Bundle.BundleId != bundle.BundleID {
		t.Fatalf("Bundle.BundleId = %q, want %q", resp.Msg.Bundle.BundleId, bundle.BundleID)
	}
}

func TestGetBundle_NonCreator_PermissionDenied(t *testing.T) {
	s, _, _ := newTestServer(t)
	owner := bundleTestOwner(t)
	ctx := makeOwnerCtx(owner)
	fakeStore := skillstore.NewFakeSkillStore()

	m1 := makeMember("", "solo", "", "body")
	fcf := &fakeConditionalBundleFetcher{scripted: []conditionalScript{
		{result: skillfetch.BundleResult{Owner: "o", Repo: "priv2", Members: []skillfetch.Member{m1}}},
	}}
	s.SetSkillIngest(fcf, fakeStore, skillfetch.DefaultPlainTarCapBytes)
	bundle := seedBundleForReingest(t, s, ctx, owner, "o/priv2")

	other := makeOwnerCtx(bundleTestOwner(t))
	_, err := s.GetBundle(other, connect.NewRequest(&cpv1.GetBundleRequest{BundleId: bundle.BundleID}))
	if err == nil {
		t.Fatal("expected PermissionDenied for a non-creator")
	}
	var ce *connect.Error
	if !errors.As(err, &ce) || ce.Code() != connect.CodePermissionDenied {
		t.Fatalf("expected CodePermissionDenied, got: %v", err)
	}
}

// --- GetBundleDiff ------------------------------------------------------------------------------

func TestGetBundleDiff_AddedRemovedChanged_UnchangedAbsent(t *testing.T) {
	s, _, _ := newTestServer(t)
	owner := bundleTestOwner(t)
	ctx := makeOwnerCtx(owner)
	fakeStore := skillstore.NewFakeSkillStore()

	a1 := makeMember("skills/a", "a", "", "a-body-v1")
	a2 := makeMember("skills/a", "a", "", "a-body-v2")
	b := makeMember("skills/b", "b", "", "b-body")
	c := makeMember("skills/c", "c", "", "c-body")
	d := makeMember("skills/d", "d", "", "d-body-unchanged") // present in both versions, identical content

	fcf := &fakeConditionalBundleFetcher{scripted: []conditionalScript{
		{result: skillfetch.BundleResult{Owner: "o", Repo: "diffme", SourceCommit: "c1", Members: []skillfetch.Member{a1, b, d}}},
		{result: skillfetch.BundleResult{Owner: "o", Repo: "diffme", SourceCommit: "c2", Members: []skillfetch.Member{a2, c, d}}},
	}}
	s.SetSkillIngest(fcf, fakeStore, skillfetch.DefaultPlainTarCapBytes)
	bundle := seedBundleForReingest(t, s, ctx, owner, "o/diffme")
	v1, err := s.st.SkillBundles().LatestVersion(context.Background(), bundle.BundleID)
	if err != nil {
		t.Fatalf("LatestVersion: %v", err)
	}

	reResp, err := s.ReingestBundle(ctx, connect.NewRequest(&cpv1.ReingestBundleRequest{BundleId: bundle.BundleID}))
	if err != nil {
		t.Fatalf("ReingestBundle: %v", err)
	}
	if reResp.Msg.DiffToken == "" {
		t.Fatal("expected a diff_token from the changed ReingestBundle")
	}

	diffResp, err := s.GetBundleDiff(ctx, connect.NewRequest(&cpv1.GetBundleDiffRequest{
		BundleId: bundle.BundleID, FromVersion: v1.VersionID, ToVersion: reResp.Msg.VersionId,
	}))
	if err != nil {
		t.Fatalf("GetBundleDiff: %v", err)
	}
	if diffResp.Msg.FromCommit != "c1" || diffResp.Msg.ToCommit != "c2" {
		t.Fatalf("commits = (%q, %q), want (c1, c2)", diffResp.Msg.FromCommit, diffResp.Msg.ToCommit)
	}
	if diffResp.Msg.DiffToken != reResp.Msg.DiffToken {
		t.Fatalf("DiffToken = %q, want the ReingestBundle token %q echoed back", diffResp.Msg.DiffToken, reResp.Msg.DiffToken)
	}

	byDir := map[string]*cpv1.MemberDiff{}
	for _, m := range diffResp.Msg.Members {
		byDir[m.SourceSubdir] = m
	}
	if len(byDir) != 3 {
		t.Fatalf("got %d diff entries, want 3 (a changed, b removed, c added); unchanged members must be absent", len(byDir))
	}
	if _, ok := byDir["skills/d"]; ok {
		t.Fatal("skills/d is unchanged between versions and must be absent from the diff (never fetched, §4.9)")
	}

	aDiff, ok := byDir["skills/a"]
	if !ok || aDiff.Change != cpv1.MemberDiff_CHANGED {
		t.Fatalf("skills/a diff = %+v, want CHANGED", aDiff)
	}
	if !strings.Contains(aDiff.OldSkillMd, "a-body-v1") {
		t.Fatalf("old_skill_md missing v1 body: %q", aDiff.OldSkillMd)
	}
	if !strings.Contains(aDiff.NewSkillMd, "a-body-v2") {
		t.Fatalf("new_skill_md missing v2 body: %q", aDiff.NewSkillMd)
	}
	if aDiff.BodyUnavailable {
		t.Fatal("skills/a body should be available")
	}

	bDiff, ok := byDir["skills/b"]
	if !ok || bDiff.Change != cpv1.MemberDiff_REMOVED {
		t.Fatalf("skills/b diff = %+v, want REMOVED", bDiff)
	}
	if bDiff.NewSkillMd != "" {
		t.Fatalf("REMOVED member should carry no new_skill_md, got %q", bDiff.NewSkillMd)
	}

	cDiff, ok := byDir["skills/c"]
	if !ok || cDiff.Change != cpv1.MemberDiff_ADDED {
		t.Fatalf("skills/c diff = %+v, want ADDED", cDiff)
	}
	if !strings.Contains(cDiff.NewSkillMd, "c-body") {
		t.Fatalf("new_skill_md missing c body: %q", cDiff.NewSkillMd)
	}
	if cDiff.OldSkillMd != "" {
		t.Fatalf("ADDED member should carry no old_skill_md, got %q", cDiff.OldSkillMd)
	}

	// The diff view satisfies assertDiffViewed for the minted token.
	if err := s.assertDiffViewed(owner, bundle.BundleID, reResp.Msg.VersionId, reResp.Msg.DiffToken); err != nil {
		t.Fatalf("assertDiffViewed after GetBundleDiff: %v", err)
	}
}

func TestGetBundleDiff_TruncatesLongBody(t *testing.T) {
	s, _, _ := newTestServer(t)
	owner := bundleTestOwner(t)
	ctx := makeOwnerCtx(owner)
	fakeStore := skillstore.NewFakeSkillStore()

	longBody := strings.Repeat("x", maxDiffBodyBytes+1024)
	a1 := makeMember("", "big", "", "short")
	a2 := makeMember("", "big", "", longBody)

	fcf := &fakeConditionalBundleFetcher{scripted: []conditionalScript{
		{result: skillfetch.BundleResult{Owner: "o", Repo: "trunc", Members: []skillfetch.Member{a1}}},
		{result: skillfetch.BundleResult{Owner: "o", Repo: "trunc", Members: []skillfetch.Member{a2}}},
	}}
	s.SetSkillIngest(fcf, fakeStore, skillfetch.DefaultPlainTarCapBytes)
	bundle := seedBundleForReingest(t, s, ctx, owner, "o/trunc")
	v1, err := s.st.SkillBundles().LatestVersion(context.Background(), bundle.BundleID)
	if err != nil {
		t.Fatalf("LatestVersion: %v", err)
	}
	reResp, err := s.ReingestBundle(ctx, connect.NewRequest(&cpv1.ReingestBundleRequest{BundleId: bundle.BundleID}))
	if err != nil {
		t.Fatalf("ReingestBundle: %v", err)
	}

	diffResp, err := s.GetBundleDiff(ctx, connect.NewRequest(&cpv1.GetBundleDiffRequest{
		BundleId: bundle.BundleID, FromVersion: v1.VersionID, ToVersion: reResp.Msg.VersionId,
	}))
	if err != nil {
		t.Fatalf("GetBundleDiff: %v", err)
	}
	if len(diffResp.Msg.Members) != 1 {
		t.Fatalf("got %d diff entries, want 1", len(diffResp.Msg.Members))
	}
	d := diffResp.Msg.Members[0]
	if !d.NewTruncated {
		t.Fatal("expected new_truncated=true for a body exceeding the 64 KiB cap")
	}
	if len(d.NewSkillMd) > maxDiffBodyBytes {
		t.Fatalf("new_skill_md length %d exceeds the cap %d", len(d.NewSkillMd), maxDiffBodyBytes)
	}
}

func TestGetBundleDiff_MissingGarageObject_BodyUnavailable(t *testing.T) {
	s, _, _ := newTestServer(t)
	owner := bundleTestOwner(t)
	ctx := makeOwnerCtx(owner)
	fakeStore := skillstore.NewFakeSkillStore()

	a1 := makeMember("", "vanish", "", "body-v1")
	a2 := makeMember("", "vanish", "", "body-v2")
	fcf := &fakeConditionalBundleFetcher{scripted: []conditionalScript{
		{result: skillfetch.BundleResult{Owner: "o", Repo: "vanish", Members: []skillfetch.Member{a1}}},
		{result: skillfetch.BundleResult{Owner: "o", Repo: "vanish", Members: []skillfetch.Member{a2}}},
	}}
	s.SetSkillIngest(fcf, fakeStore, skillfetch.DefaultPlainTarCapBytes)
	bundle := seedBundleForReingest(t, s, ctx, owner, "o/vanish")
	v1, err := s.st.SkillBundles().LatestVersion(context.Background(), bundle.BundleID)
	if err != nil {
		t.Fatalf("LatestVersion: %v", err)
	}
	reResp, err := s.ReingestBundle(ctx, connect.NewRequest(&cpv1.ReingestBundleRequest{BundleId: bundle.BundleID}))
	if err != nil {
		t.Fatalf("ReingestBundle: %v", err)
	}

	// Simulate the v1 object having been lost from Garage.
	fakeStore.Delete(a1.PlainTarSHA256)

	diffResp, err := s.GetBundleDiff(ctx, connect.NewRequest(&cpv1.GetBundleDiffRequest{
		BundleId: bundle.BundleID, FromVersion: v1.VersionID, ToVersion: reResp.Msg.VersionId,
	}))
	if err != nil {
		t.Fatalf("GetBundleDiff: %v", err)
	}
	if len(diffResp.Msg.Members) != 1 {
		t.Fatalf("got %d diff entries, want 1", len(diffResp.Msg.Members))
	}
	d := diffResp.Msg.Members[0]
	if !d.BodyUnavailable {
		t.Fatal("expected body_unavailable=true when the old object is missing")
	}
	if d.OldSkillMd != "" {
		t.Fatalf("expected empty old_skill_md when unavailable, got %q", d.OldSkillMd)
	}
	// The new body is still available — the diff must remain otherwise viewable.
	if !strings.Contains(d.NewSkillMd, "body-v2") {
		t.Fatalf("new_skill_md missing despite only the OLD object being gone: %q", d.NewSkillMd)
	}
}

func TestAssertDiffViewed_UnmintedToken_FailedPrecondition(t *testing.T) {
	s, _, _ := newTestServer(t)
	owner := bundleTestOwner(t)

	err := s.assertDiffViewed(owner, "some-bundle", "some-version", "not-a-real-token")
	if err == nil {
		t.Fatal("expected FailedPrecondition for an unminted token")
	}
	var ce *connect.Error
	if !errors.As(err, &ce) || ce.Code() != connect.CodeFailedPrecondition {
		t.Fatalf("expected CodeFailedPrecondition, got: %v", err)
	}
}

func TestAssertDiffViewed_MintedButUnviewed_FailedPrecondition(t *testing.T) {
	s, _, _ := newTestServer(t)
	owner := bundleTestOwner(t)

	tok := s.bundleDiffGate.mint(owner, "bundle-x", "v1", "v2", time.Now())

	err := s.assertDiffViewed(owner, "bundle-x", "v2", tok)
	if err == nil {
		t.Fatal("expected FailedPrecondition for a minted-but-unviewed token")
	}
	var ce *connect.Error
	if !errors.As(err, &ce) || ce.Code() != connect.CodeFailedPrecondition {
		t.Fatalf("expected CodeFailedPrecondition, got: %v", err)
	}
}

func TestAssertDiffViewed_Expired_FailedPrecondition(t *testing.T) {
	s, _, _ := newTestServer(t)
	owner := bundleTestOwner(t)

	// Mint far enough in the past that it is already expired.
	tok := s.bundleDiffGate.mint(owner, "bundle-y", "v1", "v2", time.Now().Add(-2*diffTokenTTL))
	s.bundleDiffGate.markViewed(owner, "bundle-y", "v1", "v2", time.Now().Add(-2*diffTokenTTL))

	err := s.assertDiffViewed(owner, "bundle-y", "v2", tok)
	if err == nil {
		t.Fatal("expected FailedPrecondition for an expired token")
	}
	var ce *connect.Error
	if !errors.As(err, &ce) || ce.Code() != connect.CodeFailedPrecondition {
		t.Fatalf("expected CodeFailedPrecondition, got: %v", err)
	}
}
