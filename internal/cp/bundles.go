package cp

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"
	"unicode/utf8"

	"connectrpc.com/connect"
	"github.com/google/uuid"

	cpv1 "spawnery/gen/cp/v1"
	"spawnery/internal/cp/auth"
	"spawnery/internal/cp/skillfetch"
	"spawnery/internal/cp/store"
)

// This file holds the bundle RPC surface added by sp-mwco.1.7 (spec §4.6/§4.8/§4.9):
// ReingestBundle, ListBundles, GetBundle, GetBundleDiff — plus the CP-wide refetch budget (§4.8)
// and the in-memory diff-token gate (§4.9) that backs assertDiffViewed, the seam sp-mwco.1.8's
// re-pin RPC consumes.

// --- CP-wide refetch budget (§4.8) ------------------------------------------------------------

const (
	// refetchBudgetMax is the CP-wide (not per-owner) cap on ReingestBundle upstream refetches per
	// refetchBudgetWindow. GitHub's rate limit (~60 req/hr unauthenticated) is per source IP, and
	// the CP egresses from one IP shared across every tenant's ReingestBundle calls — the
	// per-owner ingestQuota (20/hr) does not bound that aggregate. 40 leaves headroom under 60 for
	// first-time IngestSkillFromURL traffic sharing the same IP. A GITHUB_TOKEN-aware raise (5000/hr
	// authenticated) is a follow-up, not this task.
	refetchBudgetMax    = 40
	refetchBudgetWindow = 1 * time.Hour
)

// refetchBudget is a CP-wide (single-key, not per-owner) rolling-window counter, mirroring
// ingestQuota's algorithm (ingest_skill.go) but with one shared counter instead of one per owner.
type refetchBudget struct {
	mu     sync.Mutex
	count  int
	window time.Time
	max    int
	period time.Duration
}

func newRefetchBudget(max int, period time.Duration) *refetchBudget {
	return &refetchBudget{max: max, period: period}
}

// allow returns true if the CP-wide budget has room, plus the time remaining until the window
// resets (so a rejection can tell the caller when to retry).
func (b *refetchBudget) allow(now time.Time) (bool, time.Duration) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.window.IsZero() || now.Sub(b.window) > b.period {
		b.window = now
		b.count = 0
	}
	remaining := b.period - now.Sub(b.window)
	if b.count >= b.max {
		return false, remaining
	}
	b.count++
	return true, remaining
}

// --- Diff-token gate (§4.9) -------------------------------------------------------------------

// diffTokenTTL bounds how long a ReingestBundle-minted diff token remains honorable. Ephemeral by
// design: a CP restart invalidates every token (the map is in-memory only), and an expired token
// fails closed — the caller re-runs "check for updates" to mint a fresh one.
const diffTokenTTL = 30 * time.Minute

// diffTokenRecord is one diff-gate token, minted either by ReingestBundle (unviewed, pending a
// GetBundleDiff view) or by GetBundleDiff itself (viewed on mint — the view IS the gate; see
// mintViewed). A record satisfies a re-pin only once viewed AND matching both fromVersionID and
// toVersionID.
type diffTokenRecord struct {
	owner                      string
	bundleID                   string
	fromVersionID, toVersionID string
	viewed                     bool
	expiresAt                  time.Time
}

// diffGate is the in-memory, TTL'd registry of diff tokens. mint is called by ReingestBundle on a
// real change (unviewed — an optimization so a subsequent GetBundleDiff of the same pair doesn't
// mint a second token, not itself sufficient to satisfy a re-pin); mintViewed by GetBundleDiff
// (the view IS the gate: minting and marking-viewed happen atomically for the exact pair served);
// isViewed by assertDiffViewed (the seam sp-mwco.1.8's re-pin RPC checks). Bounded by
// sweepLocked, called on every mint/mintViewed: at most one live record per distinct
// (owner, bundleID, fromVersionID, toVersionID) tuple within diffTokenTTL.
type diffGate struct {
	mu     sync.Mutex
	tokens map[string]*diffTokenRecord
}

func newDiffGate() *diffGate {
	return &diffGate{tokens: make(map[string]*diffTokenRecord)}
}

// sweepLocked deletes every token expired as of now. Callers must hold g.mu.
func (g *diffGate) sweepLocked(now time.Time) {
	for tok, rec := range g.tokens {
		if now.After(rec.expiresAt) {
			delete(g.tokens, tok)
		}
	}
}

// liveLocked returns the token/record for a live (unexpired) record matching the given tuple, if
// any. Callers must hold g.mu.
func (g *diffGate) liveLocked(owner, bundleID, fromVersionID, toVersionID string, now time.Time) (string, *diffTokenRecord) {
	for tok, rec := range g.tokens {
		if rec.owner == owner && rec.bundleID == bundleID && rec.fromVersionID == fromVersionID &&
			rec.toVersionID == toVersionID && now.Before(rec.expiresAt) {
			return tok, rec
		}
	}
	return "", nil
}

// mint records a new UNVIEWED token for (owner, bundleID, fromVersionID, toVersionID) and returns
// it. Called by ReingestBundle on a real change.
func (g *diffGate) mint(owner, bundleID, fromVersionID, toVersionID string, now time.Time) string {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.sweepLocked(now)
	tok := uuid.NewString()
	g.tokens[tok] = &diffTokenRecord{
		owner: owner, bundleID: bundleID, fromVersionID: fromVersionID, toVersionID: toVersionID,
		expiresAt: now.Add(diffTokenTTL),
	}
	return tok
}

// mintViewed mints-and-marks-viewed a token for (owner, bundleID, fromVersionID, toVersionID) and
// returns it — the view IS the gate (§4.9, sp-mwco.1.13): GetBundleDiff calls this for the exact
// pair it just served, unconditionally, whether or not ReingestBundle minted an (unviewed) token
// for it first. If a live record already exists for this tuple (typically ReingestBundle's
// unviewed mint, or a repeat GetBundleDiff view), it is reused and marked viewed in place rather
// than minting a duplicate — so GetBundleDiff always returns a token that satisfies
// assertDiffViewed for this pair, and the map never grows past one live record per distinct tuple.
func (g *diffGate) mintViewed(owner, bundleID, fromVersionID, toVersionID string, now time.Time) string {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.sweepLocked(now)
	if tok, rec := g.liveLocked(owner, bundleID, fromVersionID, toVersionID, now); rec != nil {
		rec.viewed = true
		return tok
	}
	tok := uuid.NewString()
	g.tokens[tok] = &diffTokenRecord{
		owner: owner, bundleID: bundleID, fromVersionID: fromVersionID, toVersionID: toVersionID,
		viewed: true, expiresAt: now.Add(diffTokenTTL),
	}
	return tok
}

// isViewed reports whether token is a live, viewed record matching (owner, bundleID,
// fromVersionID, toVersionID). Requiring fromVersionID (not just toVersionID) closes a loophole:
// a token minted for a narrower pair (e.g. v2->v3) must not satisfy a re-pin from an
// earlier-pinned version (v1) onto v3 — the caller must have seen the FULL pinned->target delta.
func (g *diffGate) isViewed(owner, bundleID, fromVersionID, toVersionID, token string, now time.Time) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	rec, ok := g.tokens[token]
	if !ok || rec.owner != owner || rec.bundleID != bundleID ||
		rec.fromVersionID != fromVersionID || rec.toVersionID != toVersionID {
		return false
	}
	if now.After(rec.expiresAt) {
		return false
	}
	return rec.viewed
}

// assertDiffViewed reports FailedPrecondition unless token is a live diff-gate record for
// (owner, bundleID) covering exactly fromVersionID->versionID, AND it has been marked viewed —
// either by GetBundleDiff's mint-on-view (the ordinary path) or by a GetBundleDiff call following
// an earlier ReingestBundle mint. sp-mwco.1.8's re-pin RPC is this method's caller, passing the
// entry's CURRENTLY PINNED version as fromVersionID. Fails CLOSED: an unknown/unviewed/expired/
// wrong-pair token is always rejected, never treated as "no gate configured, allow it".
func (s *Server) assertDiffViewed(owner, bundleID, fromVersionID, versionID, token string) error {
	if s.bundleDiffGate == nil || !s.bundleDiffGate.isViewed(owner, bundleID, fromVersionID, versionID, token, time.Now()) {
		return connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf(
			"re-pin requires viewing the diff first (call GetBundleDiff for %s -> %s, then retry)", fromVersionID, versionID))
	}
	return nil
}

// --- ReingestBundle --------------------------------------------------------------------------

// ReingestBundle re-fetches bundle_id's upstream source, conditionally (§4.8: If-None-Match on
// the bundle's stored etag) and against the CP-wide refetch budget. Creator-only.
func (s *Server) ReingestBundle(ctx context.Context, req *connect.Request[cpv1.ReingestBundleRequest]) (*connect.Response[cpv1.ReingestBundleResponse], error) {
	owner, ok := auth.OwnerFromContext(ctx)
	if !ok {
		return nil, connect.NewError(connect.CodeUnauthenticated, fmt.Errorf("no owner"))
	}
	bundleID := req.Msg.BundleId

	bundle, err := s.st.SkillBundles().Get(ctx, bundleID)
	if errors.Is(err, store.ErrNotFound) {
		return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("bundle not found"))
	}
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	if bundle.CreatorID != owner {
		return nil, connect.NewError(connect.CodePermissionDenied, fmt.Errorf("not the creator of %q", bundleID))
	}

	// Per-owner ingest quota: ReingestBundle is an owner-initiated GitHub fetch, same as
	// IngestSkillFromURL (§4.8 D5).
	if allowed, retryAfter := globalIngestQuota.allow(owner, time.Now()); !allowed {
		mins := int(retryAfter.Round(time.Minute) / time.Minute)
		return nil, connect.NewError(connect.CodeResourceExhausted,
			fmt.Errorf("ingest rate limit exceeded (max %d per hour); retry after ~%dm", ingestQuotaMax, mins))
	}

	// CP-wide refetch budget: charged BEFORE the fetch, never refunded on a 304 — conservative by
	// design (§4.8 D5).
	if allowed, retryAfter := s.reingestBudget.allow(time.Now()); !allowed {
		mins := int(retryAfter.Round(time.Minute) / time.Minute)
		return nil, connect.NewError(connect.CodeResourceExhausted, fmt.Errorf(
			"CP-wide refetch budget exhausted (max %d per hour, shared across all owners — GitHub's rate limit is per source IP); retry after ~%dm",
			refetchBudgetMax, mins))
	}

	repoRef, err := skillfetch.ParseRepoURL(bundle.SourceURL)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("stored bundle source_url %q: %w", bundle.SourceURL, err))
	}

	out, err := s.ingestBundle(ctx, owner, repoRef, bundle.SourceRef, bundle.SourceSubdir, "", "", bundle.ETag)
	if err != nil {
		return nil, err
	}

	diffToken := ""
	if out.Changed && out.FromVersionID != "" {
		diffToken = s.bundleDiffGate.mint(owner, bundle.BundleID, out.FromVersionID, out.VersionID, time.Now())
	}

	return connect.NewResponse(&cpv1.ReingestBundleResponse{
		VersionId:      out.VersionID,
		Changed:        out.Changed,
		AddedSubdirs:   out.AddedSubdirs,
		UpdatedSubdirs: out.UpdatedSubdirs,
		RemovedSubdirs: out.RemovedSubdirs,
		DiffToken:      diffToken,
		FromVersionId:  out.FromVersionID,
		NotModified:    out.NotModified,
		Warnings:       out.Warnings,
	}), nil
}

// --- ListBundles / GetBundle -------------------------------------------------------------------

// bundleSummary builds a BundleSummary (§4.7 provenance card) for b, including its latest
// version's seq and member count. A bundle with no version yet (a narrow ingest-vs-lookup race)
// reports latest_version_id="", latest_seq=0, member_count=0.
func (s *Server) bundleSummary(ctx context.Context, b store.SkillBundle) (*cpv1.BundleSummary, error) {
	summary := &cpv1.BundleSummary{
		BundleId:     b.BundleID,
		Name:         b.Name,
		SourceUrl:    b.SourceURL,
		SourceRef:    b.SourceRef,
		SourceSubdir: b.SourceSubdir,
		CreatedAt:    b.CreatedAt,
		UpdatedAt:    b.UpdatedAt,
	}
	latest, err := s.st.SkillBundles().LatestVersion(ctx, b.BundleID)
	if errors.Is(err, store.ErrNotFound) {
		return summary, nil
	}
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	summary.LatestVersionId = latest.VersionID
	summary.LatestSeq = latest.Seq
	members, err := s.st.SkillBundles().Members(ctx, latest.VersionID)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	summary.MemberCount = int32(len(members))
	return summary, nil
}

// ListBundles lists the caller's own bundles (creator-only; bundles are unlisted by default).
func (s *Server) ListBundles(ctx context.Context, req *connect.Request[cpv1.ListBundlesRequest]) (*connect.Response[cpv1.ListBundlesResponse], error) {
	owner, ok := auth.OwnerFromContext(ctx)
	if !ok {
		return nil, connect.NewError(connect.CodeUnauthenticated, fmt.Errorf("no owner"))
	}
	bundles, err := s.st.SkillBundles().ListByCreator(ctx, owner)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	out := make([]*cpv1.BundleSummary, 0, len(bundles))
	for _, b := range bundles {
		summary, err := s.bundleSummary(ctx, b)
		if err != nil {
			return nil, err
		}
		out = append(out, summary)
	}
	return connect.NewResponse(&cpv1.ListBundlesResponse{Bundles: out}), nil
}

// GetBundle returns one bundle's provenance, every version (seq ASC), and its latest version's
// members. Creator-only.
func (s *Server) GetBundle(ctx context.Context, req *connect.Request[cpv1.GetBundleRequest]) (*connect.Response[cpv1.GetBundleResponse], error) {
	owner, ok := auth.OwnerFromContext(ctx)
	if !ok {
		return nil, connect.NewError(connect.CodeUnauthenticated, fmt.Errorf("no owner"))
	}
	b, err := s.st.SkillBundles().Get(ctx, req.Msg.BundleId)
	if errors.Is(err, store.ErrNotFound) {
		return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("bundle not found"))
	}
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	if b.CreatorID != owner {
		return nil, connect.NewError(connect.CodePermissionDenied, fmt.Errorf("not the creator of %q", req.Msg.BundleId))
	}

	summary, err := s.bundleSummary(ctx, b)
	if err != nil {
		return nil, err
	}

	versions, err := s.st.SkillBundles().ListVersions(ctx, b.BundleID)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	pbVersions := make([]*cpv1.BundleVersion, len(versions))
	for i, v := range versions {
		pbVersions[i] = &cpv1.BundleVersion{
			VersionId:    v.VersionID,
			Seq:          v.Seq,
			SourceCommit: v.SourceCommit,
			CreatedAt:    v.CreatedAt,
		}
	}

	var pbMembers []*cpv1.BundleMember
	if summary.LatestVersionId != "" {
		members, err := s.st.SkillBundles().Members(ctx, summary.LatestVersionId)
		if err != nil {
			return nil, connect.NewError(connect.CodeInternal, err)
		}
		pbMembers = make([]*cpv1.BundleMember, len(members))
		for i, m := range members {
			entry, err := s.st.CustomizationCatalog().Get(ctx, m.CatalogID)
			if err != nil {
				return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("get member %s: %w", m.CatalogID, err))
			}
			sha := ""
			if entry.SHA256 != nil {
				sha = *entry.SHA256
			}
			pbMembers[i] = &cpv1.BundleMember{
				CatalogId:    m.CatalogID,
				SourceSubdir: m.SourceSubdir,
				Name:         entry.Name,
				Description:  entry.Description,
				Sha256:       sha,
				Position:     int32(m.Position),
			}
		}
	}

	return connect.NewResponse(&cpv1.GetBundleResponse{
		Bundle:   summary,
		Versions: pbVersions,
		Members:  pbMembers,
	}), nil
}

// --- GetBundleDiff (§4.9) -----------------------------------------------------------------------

// maxDiffBodyBytes caps each SKILL.md body surfaced in a MemberDiff. Untrusted content (it
// round-trips through an arbitrary GitHub repo) — the cap is mandatory, mirroring the caps
// enforced elsewhere in the ingest path.
const maxDiffBodyBytes = 64 * 1024

// fetchDiffBody fetches sha256hex's SKILL.md body for a GetBundleDiff entry, truncated at
// maxDiffBodyBytes on a rune boundary. ANY failure — the Garage object is gone, or extraction
// otherwise fails — yields body_unavailable=true rather than failing the whole diff (§4.9: a diff
// must still be viewable when one object was lost). sha256hex == "" (should not happen for a real
// member, but defensive) also reports unavailable.
func (s *Server) fetchDiffBody(ctx context.Context, sha256hex string) (body string, truncated bool, unavailable bool) {
	if sha256hex == "" || s.skillStore == nil {
		return "", false, true
	}
	compressed, err := s.skillStore.Get(ctx, sha256hex)
	if err != nil {
		return "", false, true
	}
	plain, err := skillfetch.ExtractSkillMD(compressed, s.effectiveSkillPlainTarCap())
	if err != nil {
		return "", false, true
	}
	text := string(plain)
	if len(text) <= maxDiffBodyBytes {
		return text, false, false
	}
	return truncateUTF8(text, maxDiffBodyBytes), true, false
}

// truncateUTF8 truncates s to at most maxBytes bytes without splitting a multi-byte rune.
func truncateUTF8(s string, maxBytes int) string {
	if len(s) <= maxBytes {
		return s
	}
	b := s[:maxBytes]
	for len(b) > 0 {
		r, size := utf8.DecodeLastRuneInString(b)
		if r != utf8.RuneError || size != 1 {
			break
		}
		b = b[:len(b)-1]
	}
	return b
}

// memberSha returns entry's content sha256, or "" when it has none (an inline, non-URL entry —
// should not occur for a bundle member, but defensive).
func memberSha(entry store.CustomizationCatalogEntry) string {
	if entry.SHA256 == nil {
		return ""
	}
	return *entry.SHA256
}

// GetBundleDiff computes a per-member diff between two versions of a bundle, including SKILL.md
// body diffs for added/changed members (fetched ONLY for those — never for the unchanged
// majority). Mints-and-marks-viewed a diff-gate token for the exact (fromVersion, toVersion) pair
// served — the view IS the gate (§4.9, sp-mwco.1.13): no prior ReingestBundle in this CP process
// is required. Creator-only.
func (s *Server) GetBundleDiff(ctx context.Context, req *connect.Request[cpv1.GetBundleDiffRequest]) (*connect.Response[cpv1.GetBundleDiffResponse], error) {
	owner, ok := auth.OwnerFromContext(ctx)
	if !ok {
		return nil, connect.NewError(connect.CodeUnauthenticated, fmt.Errorf("no owner"))
	}
	b, err := s.st.SkillBundles().Get(ctx, req.Msg.BundleId)
	if errors.Is(err, store.ErrNotFound) {
		return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("bundle not found"))
	}
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	if b.CreatorID != owner {
		return nil, connect.NewError(connect.CodePermissionDenied, fmt.Errorf("not the creator of %q", req.Msg.BundleId))
	}

	fromV, err := s.st.SkillBundles().GetVersion(ctx, req.Msg.FromVersion)
	if errors.Is(err, store.ErrNotFound) {
		return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("from_version not found"))
	}
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	if fromV.BundleID != b.BundleID {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("from_version %q is not a version of bundle %q", req.Msg.FromVersion, req.Msg.BundleId))
	}
	toV, err := s.st.SkillBundles().GetVersion(ctx, req.Msg.ToVersion)
	if errors.Is(err, store.ErrNotFound) {
		return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("to_version not found"))
	}
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	if toV.BundleID != b.BundleID {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("to_version %q is not a version of bundle %q", req.Msg.ToVersion, req.Msg.BundleId))
	}

	fromMembers, err := s.st.SkillBundles().Members(ctx, fromV.VersionID)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	toMembers, err := s.st.SkillBundles().Members(ctx, toV.VersionID)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}

	fromByDir := make(map[string]store.SkillBundleMember, len(fromMembers))
	for _, m := range fromMembers {
		fromByDir[m.SourceSubdir] = m
	}
	toByDir := make(map[string]store.SkillBundleMember, len(toMembers))
	for _, m := range toMembers {
		toByDir[m.SourceSubdir] = m
	}

	seenDir := make(map[string]bool, len(fromByDir)+len(toByDir))
	allDirs := make([]string, 0, len(fromByDir)+len(toByDir))
	for dir := range fromByDir {
		if !seenDir[dir] {
			seenDir[dir] = true
			allDirs = append(allDirs, dir)
		}
	}
	for dir := range toByDir {
		if !seenDir[dir] {
			seenDir[dir] = true
			allDirs = append(allDirs, dir)
		}
	}
	sort.Strings(allDirs)

	var diffs []*cpv1.MemberDiff
	for _, dir := range allDirs {
		fromM, hasFrom := fromByDir[dir]
		toM, hasTo := toByDir[dir]

		switch {
		case hasFrom && !hasTo:
			entry, err := s.st.CustomizationCatalog().Get(ctx, fromM.CatalogID)
			if err != nil {
				return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("get member %s: %w", fromM.CatalogID, err))
			}
			diffs = append(diffs, &cpv1.MemberDiff{
				SourceSubdir: dir,
				Change:       cpv1.MemberDiff_REMOVED,
				Name:         entry.Name,
				OldSha256:    memberSha(entry),
			})
		case !hasFrom && hasTo:
			entry, err := s.st.CustomizationCatalog().Get(ctx, toM.CatalogID)
			if err != nil {
				return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("get member %s: %w", toM.CatalogID, err))
			}
			sha := memberSha(entry)
			body, truncated, unavailable := s.fetchDiffBody(ctx, sha)
			diffs = append(diffs, &cpv1.MemberDiff{
				SourceSubdir:    dir,
				Change:          cpv1.MemberDiff_ADDED,
				Name:            entry.Name,
				NewSha256:       sha,
				NewSkillMd:      body,
				NewTruncated:    truncated,
				BodyUnavailable: unavailable,
			})
		default:
			fromEntry, err := s.st.CustomizationCatalog().Get(ctx, fromM.CatalogID)
			if err != nil {
				return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("get member %s: %w", fromM.CatalogID, err))
			}
			toEntry, err := s.st.CustomizationCatalog().Get(ctx, toM.CatalogID)
			if err != nil {
				return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("get member %s: %w", toM.CatalogID, err))
			}
			fromSha, toSha := memberSha(fromEntry), memberSha(toEntry)
			if fromSha == toSha {
				continue // unchanged member: absent from the diff, never fetched (§4.9)
			}
			oldBody, oldTruncated, oldUnavailable := s.fetchDiffBody(ctx, fromSha)
			newBody, newTruncated, newUnavailable := s.fetchDiffBody(ctx, toSha)
			diffs = append(diffs, &cpv1.MemberDiff{
				SourceSubdir:    dir,
				Change:          cpv1.MemberDiff_CHANGED,
				Name:            toEntry.Name,
				OldSha256:       fromSha,
				NewSha256:       toSha,
				OldSkillMd:      oldBody,
				NewSkillMd:      newBody,
				OldTruncated:    oldTruncated,
				NewTruncated:    newTruncated,
				BodyUnavailable: oldUnavailable || newUnavailable,
			})
		}
	}

	diffToken := ""
	if s.bundleDiffGate != nil {
		diffToken = s.bundleDiffGate.mintViewed(owner, b.BundleID, fromV.VersionID, toV.VersionID, time.Now())
	}

	return connect.NewResponse(&cpv1.GetBundleDiffResponse{
		Members:    diffs,
		FromCommit: fromV.SourceCommit,
		ToCommit:   toV.SourceCommit,
		DiffToken:  diffToken,
	}), nil
}
