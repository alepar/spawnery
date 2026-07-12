package cp

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"connectrpc.com/connect"
	"github.com/google/uuid"

	cpv1 "spawnery/gen/cp/v1"
	"spawnery/internal/cp/auth"
	"spawnery/internal/cp/skillfetch"
	"spawnery/internal/cp/skillstore"
	"spawnery/internal/cp/store"
)

// ingestQuota is a per-owner in-memory rate-limit counter for URL skill ingestion.
// Lost on CP restart — acceptable for MVP (§4.1 note).
type ingestQuota struct {
	mu      sync.Mutex
	counts  map[string]int       // owner -> rolling count
	windows map[string]time.Time // owner -> window start
}

const (
	ingestQuotaWindow = 1 * time.Hour
	ingestQuotaMax    = 20 // max ingests per owner per hour

	// maxBundleMembers is the binding cap on skills per bundle (sp-mwco.1.4): a spawn's artifact
	// budget (maxArtifactsPerSpawn, artifacts.go) includes the inline manifest.json, so a bundle
	// with more members than this would ingest successfully, permanently upload never-GC'd Garage
	// objects, and then fail EVERY CreateSpawn that resolves it. Enforced at ingest, before any
	// Garage put or DB write.
	maxBundleMembers = maxArtifactsPerSpawn - 1

	// maxConcurrentIngest bounds CP-wide concurrent URL ingests. A whole-repo ingest buffers
	// roughly 3x the decompressed cap in memory at once (unpacked entries + the canonical plain
	// tar + the zstd encode buffer live simultaneously at EncodeAll), and the per-owner quota
	// above does not bound concurrency ACROSS owners.
	maxConcurrentIngest = 4
)

var globalIngestQuota = &ingestQuota{
	counts:  make(map[string]int),
	windows: make(map[string]time.Time),
}

// ingestSem is the CP-wide ingest concurrency semaphore (sp-mwco.1.4).
var ingestSem = make(chan struct{}, maxConcurrentIngest)

// acquireIngestSlot blocks until an ingest concurrency slot is free, honouring ctx cancellation.
// The returned release func must be called exactly once to free the slot.
func acquireIngestSlot(ctx context.Context) (release func(), err error) {
	select {
	case ingestSem <- struct{}{}:
		return func() { <-ingestSem }, nil
	case <-ctx.Done():
		return nil, connect.NewError(connect.CodeCanceled, fmt.Errorf("context canceled waiting for an ingest concurrency slot"))
	}
}

// allow returns true if the owner is within quota, plus the time remaining until the owner's
// rolling window resets (so a rejection can tell the caller when to retry — §4.7).
func (q *ingestQuota) allow(owner string, now time.Time) (bool, time.Duration) {
	q.mu.Lock()
	defer q.mu.Unlock()
	windowStart, ok := q.windows[owner]
	if !ok || now.Sub(windowStart) > ingestQuotaWindow {
		windowStart = now
		q.windows[owner] = now
		q.counts[owner] = 0
	}
	remaining := ingestQuotaWindow - now.Sub(windowStart)
	if q.counts[owner] >= ingestQuotaMax {
		return false, remaining
	}
	q.counts[owner]++
	return true, remaining
}

// materializedMember is one bundle member after the Garage-put + catalog-row dedup/create step,
// ready to become a store.SkillBundleMember once the version-cut decision lands.
type materializedMember struct {
	SourceSubdir string
	CatalogID    string
}

// memberLabel renders a member dir for error/log messages, using "<repo root>" for the "" case
// (mirrors skillfetch.memberDirLabel — kept local since that helper is unexported).
func memberLabel(dir string) string {
	if dir == "" {
		return "<repo root>"
	}
	return dir
}

// refLabel renders a git ref for error messages, naming the default branch when ref is "".
func refLabel(ref string) string {
	if ref == "" {
		return "the default branch"
	}
	return ref
}

// IngestSkillFromURL fetches every skill discovered in a GitHub repo (or subdir) — a
// bundle-of-one for a single-skill repo, more for a superpowers-style multi-skill repo — repacks
// each independently, and writes/updates a skill_bundle + version cut. Idempotent: an unchanged
// re-fetch (same member set, same content) cuts no new version (§4.2/§4.3).
func (s *Server) IngestSkillFromURL(ctx context.Context, req *connect.Request[cpv1.IngestSkillFromURLRequest]) (*connect.Response[cpv1.IngestSkillFromURLResponse], error) {
	owner, ok := auth.OwnerFromContext(ctx)
	if !ok {
		return nil, connect.NewError(connect.CodeUnauthenticated, fmt.Errorf("no owner"))
	}

	// Check seams: both fetcher and store must be wired
	if s.skillFetcher == nil || s.skillStore == nil {
		return nil, connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("URL skill ingest requires Garage; configure skills.* in the CP config"))
	}
	bf, ok := s.skillFetcher.(skillfetch.BundleFetcher)
	if !ok {
		return nil, connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("skill ingest fetcher does not support bundles"))
	}

	// Per-user ingest quota
	if allowed, retryAfter := globalIngestQuota.allow(owner, time.Now()); !allowed {
		mins := int(retryAfter.Round(time.Minute) / time.Minute)
		return nil, connect.NewError(connect.CodeResourceExhausted,
			fmt.Errorf("ingest rate limit exceeded (max %d per hour); retry after ~%dm", ingestQuotaMax, mins))
	}

	rawURL := req.Msg.Url
	ref := req.Msg.Ref
	subdir := req.Msg.Subdir
	requestedName := req.Msg.Name
	description := req.Msg.Description

	// Parse the repo URL
	repoRef, err := skillfetch.ParseRepoURL(rawURL)
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}
	sourceURL := repoRef.Owner + "/" + repoRef.Repo

	// Acquire the CP-wide ingest concurrency slot across the fetch+repack only — that's where the
	// memory hog lives; holding it across the Garage puts/DB writes below would needlessly
	// serialize I/O that doesn't need bounding.
	release, err := acquireIngestSlot(ctx)
	if err != nil {
		return nil, err
	}
	var res skillfetch.BundleResult
	var fetchErr error
	func() {
		defer release()
		res, fetchErr = bf.FetchBundle(ctx, repoRef, ref, subdir)
	}()

	if fetchErr != nil {
		var rl *skillfetch.ErrRateLimit
		if errors.As(fetchErr, &rl) {
			return nil, connect.NewError(connect.CodeResourceExhausted, fmt.Errorf("GitHub rate limit: %w", fetchErr))
		}
		var upstream *skillfetch.ErrUpstreamFailed
		if errors.As(fetchErr, &upstream) {
			return nil, connect.NewError(connect.CodeUnavailable, fetchErr)
		}
		if errors.Is(fetchErr, skillfetch.ErrNoSkills) {
			// §4.5: a re-ingest that no longer discovers a member set fails loud and leaves the
			// pin intact — it never silently converts/deletes the bundle. Only applies when the
			// bundle already exists; a first-ever ingest of a skill-less repo is a plain bad input.
			if _, getErr := s.st.SkillBundles().GetByKey(ctx, owner, sourceURL, ref, subdir); getErr == nil {
				return nil, connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf(
					"upstream layout changed: no SKILL.md found at %s; the pinned version is unchanged", refLabel(ref)))
			} else if !errors.Is(getErr, store.ErrNotFound) {
				return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("check existing bundle: %w", getErr))
			}
		}
		// Genuine bad-input errors: no SKILL.md, unsafe path, invalid name, size/file cap,
		// duplicate member, disallowed redirect host, GitHub 4xx (bad repo/ref/credentials).
		return nil, connect.NewError(connect.CodeInvalidArgument, fetchErr)
	}

	// Member cap, BEFORE any Garage put or DB write (sp-mwco.1.4): uploading first would leave
	// never-GC'd Garage objects and then fail every CreateSpawn that resolves this bundle.
	if len(res.Members) > maxBundleMembers {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf(
			"bundle has %d skills; max %d per bundle (a spawn is capped at %d artifacts including its manifest) — narrow the ingest with subdir",
			len(res.Members), maxBundleMembers, maxArtifactsPerSpawn))
	}

	if len(res.Warnings) > 0 {
		log.Printf("ingest_skill: owner=%s repo=%s: %s", owner, sourceURL, strings.Join(res.Warnings, "; "))
	}
	if len(res.SkippedEntries) > 0 {
		skippedList := res.SkippedEntries
		truncated := ""
		if len(skippedList) > 10 {
			skippedList = skippedList[:10]
			truncated = ", ..."
		}
		log.Printf("ingest_skill: owner=%s repo=%s: %d non-regular entries skipped: %s%s",
			owner, sourceURL, len(res.SkippedEntries), strings.Join(skippedList, ", "), truncated)
	}

	if len(res.Members) > 1 && (requestedName != "" || description != "") {
		log.Printf("ingest_skill: owner=%s repo=%s: %d members discovered; ignoring the request name/description overrides (they would name the bundle, not a specific member)",
			owner, sourceURL, len(res.Members))
	}

	now := time.Now().Unix()

	// Materialize members: content-addressed Garage put + catalog row dedup/create, outside any
	// tx (a unique-violation inside a PG tx poisons it, and store.WithTx is flat/no-savepoints, so
	// expected-conflict inserts must stay out of the tx). Never mutate an existing (creator, sha)
	// row's Listed/Name/SourceSubdir/BundleMember/SourceCommit on a dedup hit (§4.3).
	materialized := make([]materializedMember, 0, len(res.Members))
	for _, m := range res.Members {
		tags := map[string]string{
			"source": sourceURL,
			"owner":  owner,
		}
		if err := s.skillStore.PutIfAbsent(ctx, m.PlainTarSHA256, m.CompressedBytes, tags); err != nil {
			return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("store skill object for member %q: %w", memberLabel(m.Dir), err))
		}

		name := m.Name
		desc := m.Description
		if len(res.Members) == 1 {
			// Bundle-of-one from a single-skill repo: preserve today's UX of letting the request
			// name/description override the discovered ones.
			if requestedName != "" {
				name = requestedName
			}
			if description != "" {
				desc = description
			}
		}

		var catalogID string
		existing, err := s.st.CustomizationCatalog().GetByCreatorSHA(ctx, owner, m.PlainTarSHA256)
		switch {
		case err == nil:
			catalogID = existing.CatalogID
		case errors.Is(err, store.ErrNotFound):
			sha256val := m.PlainTarSHA256
			sizeVal := m.PlainSize
			newID := uuid.NewString()
			entry := store.CustomizationCatalogEntry{
				CatalogID:   newID,
				CreatorID:   owner,
				Kind:        string(store.ProfileEntrySkill),
				Name:        name,
				Description: desc,
				// Unlisted by default (sp-mwco.3.4 §4.6 D1): creator-visible only until an admin
				// PublishCatalogEntry's it onto the global catalog.
				Listed:       false,
				CreatedAt:    now,
				UpdatedAt:    now,
				SourceURL:    sourceURL,
				SourceRef:    ref,
				SourceSubdir: m.Dir,
				SHA256:       &sha256val,
				Size:         &sizeVal,
				BundleMember: true,
				SourceCommit: res.SourceCommit,
			}
			if err := s.st.CustomizationCatalog().CreateSkill(ctx, entry); err != nil {
				if !errors.Is(err, store.ErrConflict) {
					return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("create catalog entry for member %q: %w", memberLabel(m.Dir), err))
				}
				// Lost a concurrent-ingest race; re-select and use the winner.
				winner, gerr := s.st.CustomizationCatalog().GetByCreatorSHA(ctx, owner, m.PlainTarSHA256)
				if gerr != nil {
					return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("re-select member %q after conflict: %w", memberLabel(m.Dir), gerr))
				}
				catalogID = winner.CatalogID
			} else {
				catalogID = newID
			}
		default:
			return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("check existing member %q: %w", memberLabel(m.Dir), err))
		}

		// Best-effort catalog_id tag update (no-op body, tags-only update is out of scope).
		tags["catalog_id"] = catalogID
		_ = s.skillStore.PutIfAbsent(ctx, m.PlainTarSHA256, nil, tags)

		materialized = append(materialized, materializedMember{SourceSubdir: m.Dir, CatalogID: catalogID})
	}

	// Bundle row: fetch-or-create for (owner, source_url, ref, subdir), also outside any tx.
	bundle, err := s.st.SkillBundles().GetByKey(ctx, owner, sourceURL, ref, subdir)
	if err != nil {
		if !errors.Is(err, store.ErrNotFound) {
			return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("check existing bundle: %w", err))
		}
		bundleName := requestedName
		if bundleName == "" {
			bundleName = repoRef.Repo
		}
		newBundle := store.SkillBundle{
			BundleID:     uuid.NewString(),
			CreatorID:    owner,
			Name:         bundleName,
			SourceURL:    sourceURL,
			SourceRef:    ref,
			SourceSubdir: subdir,
			ETag:         "",
			CreatedAt:    now,
			UpdatedAt:    now,
		}
		if cerr := s.st.SkillBundles().Create(ctx, newBundle); cerr != nil {
			if !errors.Is(cerr, store.ErrConflict) {
				return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("create bundle: %w", cerr))
			}
			// Lost a concurrent first-ingest race; re-select.
			bundle, err = s.st.SkillBundles().GetByKey(ctx, owner, sourceURL, ref, subdir)
			if err != nil {
				return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("re-select bundle after conflict: %w", err))
			}
		} else {
			bundle = newBundle
		}
	}

	// Version cut, inside a tx that locks the bundle row — serializes concurrent re-ingests of the
	// same bundle so the compare-then-cut decision and seq allocation are race-free.
	var responseCatalogID string
	txErr := s.st.WithTx(ctx, func(tx store.Store) error {
		if err := tx.SkillBundles().LockBundle(ctx, bundle.BundleID); err != nil {
			return err
		}

		newBySubdir := make(map[string]string, len(res.Members))
		for _, m := range res.Members {
			newBySubdir[m.Dir] = m.PlainTarSHA256
		}

		latest, err := tx.SkillBundles().LatestVersion(ctx, bundle.BundleID)
		changed := false
		seq := int64(1)
		switch {
		case errors.Is(err, store.ErrNotFound):
			changed = true
		case err != nil:
			return err
		default:
			seq = latest.Seq + 1
			oldMembers, merr := tx.SkillBundles().Members(ctx, latest.VersionID)
			if merr != nil {
				return merr
			}
			if len(oldMembers) != len(newBySubdir) {
				changed = true
			} else {
				for _, om := range oldMembers {
					newSha, ok := newBySubdir[om.SourceSubdir]
					if !ok {
						changed = true
						break
					}
					// Compare content sha, NOT catalog_id identity: a deleted-then-recreated
					// catalog row for byte-identical content gets a fresh catalog_id, and
					// catalog_id-identity comparison would report a phantom change. Never compare
					// created_at/updated_at either — a revert A->B->A dedups back onto the
					// ORIGINAL A row, whose created_at predates the reverting version, so any
					// created_at-based comparison would report "changed" forever (§4.3).
					oldEntry, gerr := tx.CustomizationCatalog().Get(ctx, om.CatalogID)
					if gerr != nil {
						return gerr
					}
					if oldEntry.SHA256 == nil || *oldEntry.SHA256 != newSha {
						changed = true
						break
					}
				}
			}
		}

		if !changed {
			responseCatalogID = materialized[0].CatalogID
			return nil
		}

		versionID := uuid.NewString()
		v := store.SkillBundleVersion{
			VersionID:    versionID,
			BundleID:     bundle.BundleID,
			Seq:          seq,
			SourceCommit: res.SourceCommit,
			CreatedAt:    now,
		}
		members := make([]store.SkillBundleMember, len(materialized))
		for i, m := range materialized {
			members[i] = store.SkillBundleMember{
				SourceSubdir: m.SourceSubdir,
				CatalogID:    m.CatalogID,
				Position:     i,
			}
		}
		if err := tx.SkillBundles().CreateVersion(ctx, v, members); err != nil {
			return err
		}
		responseCatalogID = materialized[0].CatalogID
		return nil
	})
	if txErr != nil {
		var ce *connect.Error
		if errors.As(txErr, &ce) {
			return nil, txErr
		}
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("version cut: %w", txErr))
	}

	// TODO(sp-mwco.1.7): the response should also carry bundle_id, version_id, members, and
	// warnings — today's IngestSkillFromURLResponse only has catalog_id (the proto surface lands
	// with the ReingestBundle/GetBundleDiff RPCs). For a single-skill repo (bundle-of-one, the
	// overwhelmingly common case) this is exactly today's behaviour.
	return connect.NewResponse(&cpv1.IngestSkillFromURLResponse{CatalogId: responseCatalogID}), nil
}

// SetSkillIngest wires the skill fetcher and skill store into the server, plus the effective
// plain-tar cap this CP enforces at ingest (sp-mwco.4.6). fetcher and store must be non-nil for
// IngestSkillFromURL to function; either nil causes a FailedPrecondition. fetcher must also
// implement skillfetch.BundleFetcher (sp-mwco.1.4) — *skillfetch's real fetcher does; a fetcher
// that doesn't causes a FailedPrecondition. plainTarCap is stamped onto every by-ref
// ObjectRef.MaxPlainTarBytes at StartSpawn (see effectiveSkillPlainTarCap); 0 falls back to
// skillfetch.DefaultPlainTarCapBytes.
func (s *Server) SetSkillIngest(fetcher skillfetch.Fetcher, store skillstore.SkillStore, plainTarCap int64) {
	s.skillFetcher = fetcher
	s.skillStore = store
	s.skillPlainTarCap = plainTarCap
}

// effectiveSkillPlainTarCap returns the cap to stamp on the wire: the configured
// skillPlainTarCap when set, else skillfetch.DefaultPlainTarCapBytes. Never 0 — a live CP always
// states its cap explicitly, so an older node (or one reading a zero cap) cannot mistake "CP
// didn't say" for "no cap".
func (s *Server) effectiveSkillPlainTarCap() int64 {
	if s.skillPlainTarCap > 0 {
		return s.skillPlainTarCap
	}
	return skillfetch.DefaultPlainTarCapBytes
}
