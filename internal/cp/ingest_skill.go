package cp

import (
	"context"
	"errors"
	"fmt"
	"log"
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
)

var globalIngestQuota = &ingestQuota{
	counts:  make(map[string]int),
	windows: make(map[string]time.Time),
}

// allow returns true if the owner is within quota.
func (q *ingestQuota) allow(owner string, now time.Time) bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	windowStart, ok := q.windows[owner]
	if !ok || now.Sub(windowStart) > ingestQuotaWindow {
		q.windows[owner] = now
		q.counts[owner] = 0
	}
	if q.counts[owner] >= ingestQuotaMax {
		return false
	}
	q.counts[owner]++
	return true
}

// IngestSkillFromURL fetches a skill from a GitHub URL, validates, repacks, stores in Garage,
// and writes a catalog row. Idempotent on (creator, sha256).
func (s *Server) IngestSkillFromURL(ctx context.Context, req *connect.Request[cpv1.IngestSkillFromURLRequest]) (*connect.Response[cpv1.IngestSkillFromURLResponse], error) {
	owner, ok := auth.OwnerFromContext(ctx)
	if !ok {
		return nil, connect.NewError(connect.CodeUnauthenticated, fmt.Errorf("no owner"))
	}

	// Check seams: both fetcher and store must be wired
	if s.skillFetcher == nil || s.skillStore == nil {
		return nil, connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("URL skill ingest requires Garage; configure skills.* in the CP config"))
	}

	// Per-user ingest quota
	if !globalIngestQuota.allow(owner, time.Now()) {
		return nil, connect.NewError(connect.CodeResourceExhausted, fmt.Errorf("ingest rate limit exceeded (max %d per hour); try again later", ingestQuotaMax))
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

	// Fetch + repack + zstd
	result, err := s.skillFetcher.Fetch(ctx, repoRef, ref, subdir, requestedName, description)
	if err != nil {
		var rl *skillfetch.ErrRateLimit
		if errors.As(err, &rl) {
			return nil, connect.NewError(connect.CodeResourceExhausted, fmt.Errorf("GitHub rate limit: %w", err))
		}
		var upstream *skillfetch.ErrUpstreamFailed
		if errors.As(err, &upstream) {
			return nil, connect.NewError(connect.CodeUnavailable, err)
		}
		// Genuine bad-input errors: no SKILL.md, unsafe path, invalid name, size/file cap,
		// disallowed redirect host, GitHub 4xx (bad repo/ref/credentials).
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}

	if result.NameWarning != "" {
		log.Printf("ingest_skill: owner=%s url=%s: %s", owner, repoRef.Owner+"/"+repoRef.Repo, result.NameWarning)
	}

	// Idempotency check: return existing catalog_id if same (owner, sha256) already exists
	existing, err := s.st.CustomizationCatalog().GetByCreatorSHA(ctx, owner, result.PlainTarSHA256)
	if err == nil {
		// Already ingested — idempotent return
		return connect.NewResponse(&cpv1.IngestSkillFromURLResponse{CatalogId: existing.CatalogID}), nil
	}
	if !errors.Is(err, store.ErrNotFound) {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("check existing skill: %w", err))
	}

	// Store in Garage
	tags := map[string]string{
		"source": repoRef.Owner + "/" + repoRef.Repo,
		"owner":  owner,
	}
	if err := s.skillStore.PutIfAbsent(ctx, result.PlainTarSHA256, result.CompressedBytes, tags); err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("store skill object: %w", err))
	}

	// Write catalog row
	now := time.Now().Unix()
	catalogID := uuid.NewString()
	sourceURL := repoRef.Owner + "/" + repoRef.Repo
	sourceRef := ref
	sourceSubdir := subdir
	sha256val := result.PlainTarSHA256
	size := result.PlainSize

	// Build entry with provenance
	e := store.CustomizationCatalogEntry{
		CatalogID:    catalogID,
		CreatorID:    owner,
		Kind:         string(store.ProfileEntrySkill),
		Name:         result.Name,
		Description:  description,
		Content:      nil, // URL skills: no inline content
		Listed:       true,
		CreatedAt:    now,
		UpdatedAt:    now,
		SourceURL:    &sourceURL,
		SourceRef:    nullableString(sourceRef),
		SourceSubdir: nullableString(sourceSubdir),
		SHA256:       &sha256val,
		Size:         &size,
	}

	if err := s.st.CustomizationCatalog().CreateSkill(ctx, e); err != nil {
		if errors.Is(err, store.ErrConflict) {
			// Lost a concurrent-ingest race; re-select and return the winner's catalog_id
			existing, err := s.st.CustomizationCatalog().GetByCreatorSHA(ctx, owner, result.PlainTarSHA256)
			if err != nil {
				return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("re-select after conflict: %w", err))
			}
			return connect.NewResponse(&cpv1.IngestSkillFromURLResponse{CatalogId: existing.CatalogID}), nil
		}
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("create skill catalog entry: %w", err))
	}

	// Update S3 tags with the now-known catalog_id (best-effort)
	tags["catalog_id"] = catalogID
	_ = s.skillStore.PutIfAbsent(ctx, result.PlainTarSHA256, nil, tags) // no-op body, tags-only update is out of scope

	return connect.NewResponse(&cpv1.IngestSkillFromURLResponse{CatalogId: catalogID}), nil
}

// nullableString returns nil for empty strings (maps to NULL in the DB).
func nullableString(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// SetSkillIngest wires the skill fetcher and skill store into the server.
// Both must be non-nil for IngestSkillFromURL to function; either nil causes a FailedPrecondition.
func (s *Server) SetSkillIngest(fetcher skillfetch.Fetcher, store skillstore.SkillStore) {
	s.skillFetcher = fetcher
	s.skillStore = store
}
