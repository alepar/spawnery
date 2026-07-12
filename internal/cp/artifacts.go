package cp

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path"
	"strings"
	"sync"
	"time"

	"connectrpc.com/connect"

	cpv1 "spawnery/gen/cp/v1"
	nodev1 "spawnery/gen/node/v1"
	"spawnery/internal/cp/skillstore"
	"spawnery/internal/cp/store"
)

// Per-spawn artifact limits: inline-only, capped well under the Connect ~4MB message limit;
// reject oversize at CreateSpawn.
const (
	maxArtifactsPerSpawn   = 64
	maxArtifactInlineBytes = 1 << 20 // 1 MiB per artifact
	maxArtifactsTotalBytes = 3 << 20 // 3 MiB total
)

// skillStatParallelism bounds the number of concurrent StatObject calls the HEAD-before-presign
// gate (statSkillObjects) issues at once.
const skillStatParallelism = 8

// skillStatTimeout bounds the aggregate wall time of one statSkillObjects call, across every
// distinct sha it checks. A package var (not a const) so tests can shrink it; production code
// must never override it.
var skillStatTimeout = 10 * time.Second

// validateAndMergeArtifacts merges publisher manifest-declared defaults with owner-supplied
// per-spawn artifacts (owner overrides by id), validates the union, and returns store rows.
// CP-blindness rule: a sensitive artifact MUST NOT carry inline plaintext — its value rides the
// separate DeliverSecrets/SealedSecret path (internal/cp/secrets.go), bound by
// SealedSecret.SecretId == ArtifactSpec.env_var_name. Returns a Connect InvalidArgument on any
// violation.
func validateAndMergeArtifacts(manifest, owner []*cpv1.ArtifactSpec) ([]store.Artifact, error) {
	merged := map[string]*cpv1.ArtifactSpec{}
	var order []string
	add := func(a *cpv1.ArtifactSpec) {
		if _, ok := merged[a.Id]; !ok {
			order = append(order, a.Id)
		}
		merged[a.Id] = a
	}
	for _, a := range manifest {
		add(a)
	}
	for _, a := range owner {
		add(a)
	}
	if len(order) > maxArtifactsPerSpawn {
		return nil, connect.NewError(connect.CodeInvalidArgument,
			fmt.Errorf("too many artifacts: %d (max %d)", len(order), maxArtifactsPerSpawn))
	}
	out := make([]store.Artifact, 0, len(order))
	var total int
	for _, id := range order {
		a := merged[id]
		if strings.TrimSpace(a.Id) == "" {
			return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("artifact id is required"))
		}
		switch a.ContentType {
		case cpv1.ArtifactContentType_ARTIFACT_CONTENT_TYPE_BYTES,
			cpv1.ArtifactContentType_ARTIFACT_CONTENT_TYPE_TAR:
		default:
			return nil, connect.NewError(connect.CodeInvalidArgument,
				fmt.Errorf("artifact %q: content_type must be BYTES or TAR", a.Id))
		}
		target := a.TargetContainer
		if target == cpv1.ArtifactTarget_ARTIFACT_TARGET_UNSPECIFIED {
			target = cpv1.ArtifactTarget_ARTIFACT_TARGET_AGENT
		}
		if err := confineDestPath(a.DestPath); err != nil {
			return nil, connect.NewError(connect.CodeInvalidArgument,
				fmt.Errorf("artifact %q: %w", a.Id, err))
		}
		if a.Sensitive {
			if len(a.Inline) > 0 {
				return nil, connect.NewError(connect.CodeInvalidArgument,
					fmt.Errorf("artifact %q: sensitive artifacts must not carry inline plaintext (deliver via DeliverSecrets)", a.Id))
			}
			if strings.TrimSpace(a.EnvVarName) == "" {
				return nil, connect.NewError(connect.CodeInvalidArgument,
					fmt.Errorf("artifact %q: sensitive artifact requires env_var_name", a.Id))
			}
			if a.GetObjectref() != nil {
				return nil, connect.NewError(connect.CodeInvalidArgument,
					fmt.Errorf("artifact %q: sensitive artifacts may not use by-ref delivery", a.Id))
			}
		} else if a.GetObjectref() != nil {
			// By-ref delivery (sp-nrzf.3.14.5): non-sensitive skill payload stored in Garage.
			ref := a.GetObjectref()
			if strings.TrimSpace(ref.ObjectKey) == "" {
				return nil, connect.NewError(connect.CodeInvalidArgument,
					fmt.Errorf("artifact %q: by-ref objectref requires object_key", a.Id))
			}
			if strings.TrimSpace(ref.Sha256) == "" {
				return nil, connect.NewError(connect.CodeInvalidArgument,
					fmt.Errorf("artifact %q: by-ref objectref requires sha256", a.Id))
			}
			if len(a.Inline) > 0 {
				return nil, connect.NewError(connect.CodeInvalidArgument,
					fmt.Errorf("artifact %q: objectref and inline are mutually exclusive", a.Id))
			}
			// By-ref bytes are not counted toward total (they live in Garage, not in the message).
			out = append(out, store.Artifact{
				ArtifactID:      a.Id,
				Inline:          nil,
				ContentType:     int32(a.ContentType),
				TargetContainer: int32(target),
				DestPath:        a.DestPath,
				Mode:            a.Mode,
				Sensitive:       false,
				EnvVarName:      "",
				ObjectKey:       ref.ObjectKey,
				ObjectSHA256:    ref.Sha256,
			})
			continue
		} else {
			if len(a.Inline) == 0 {
				return nil, connect.NewError(connect.CodeInvalidArgument,
					fmt.Errorf("artifact %q: non-sensitive artifact has empty inline content", a.Id))
			}
			if len(a.Inline) > maxArtifactInlineBytes {
				return nil, connect.NewError(connect.CodeInvalidArgument,
					fmt.Errorf("artifact %q: inline content %d bytes exceeds %d", a.Id, len(a.Inline), maxArtifactInlineBytes))
			}
			total += len(a.Inline)
		}
		out = append(out, store.Artifact{
			ArtifactID:      a.Id,
			Inline:          append([]byte(nil), a.Inline...), // nil for sensitive (Inline is empty)
			ContentType:     int32(a.ContentType),
			TargetContainer: int32(target),
			DestPath:        a.DestPath,
			Mode:            a.Mode,
			Sensitive:       a.Sensitive,
			EnvVarName:      a.EnvVarName,
		})
	}
	if total > maxArtifactsTotalBytes {
		return nil, connect.NewError(connect.CodeInvalidArgument,
			fmt.Errorf("total inline artifact bytes %d exceeds %d", total, maxArtifactsTotalBytes))
	}
	return out, nil
}

// confineDestPath rejects absolute paths and any path that escapes its staging root (contains "..").
func confineDestPath(p string) error {
	if strings.TrimSpace(p) == "" {
		return fmt.Errorf("dest_path is required")
	}
	if path.IsAbs(p) {
		return fmt.Errorf("dest_path %q must be relative", p)
	}
	cleaned := path.Clean(p)
	if cleaned == ".." || strings.HasPrefix(cleaned, "../") {
		return fmt.Errorf("dest_path %q escapes the staging root", p)
	}
	return nil
}

// storeToNodeArtifacts converts persisted artifacts to the node wire form for StartSpawn.
// Sensitive artifacts are relayed metadata-only (empty inline) — their values arrive via
// the separate SealedSecret/DeliverSecrets channel keyed by env_var_name.
// By-ref artifacts (ObjectKey != "") populate Objectref, stamped with plainTarCap
// (MaxPlainTarBytes — sp-mwco.4.6: the CP is the single source of truth for the node's
// decoded-tar cap; the node uses this verbatim); PresignedUrl is left empty here and filled by
// presignNodeArtifacts before the node RPC. Inline specs never get an Objectref.
func storeToNodeArtifacts(in []store.Artifact, plainTarCap int64) []*nodev1.ArtifactSpec {
	if len(in) == 0 {
		return nil
	}
	out := make([]*nodev1.ArtifactSpec, len(in))
	for i, a := range in {
		spec := &nodev1.ArtifactSpec{
			Id:              a.ArtifactID,
			Inline:          append([]byte(nil), a.Inline...),
			ContentType:     nodev1.ArtifactContentType(a.ContentType),
			TargetContainer: nodev1.ArtifactTarget(a.TargetContainer),
			DestPath:        a.DestPath,
			Mode:            a.Mode,
			Sensitive:       a.Sensitive,
			EnvVarName:      a.EnvVarName,
		}
		if a.ObjectKey != "" {
			spec.Objectref = &nodev1.ObjectRef{
				ObjectKey:        a.ObjectKey,
				Sha256:           a.ObjectSHA256,
				MaxPlainTarBytes: plainTarCap,
				// PresignedUrl left empty; presignNodeArtifacts fills it at start.
			}
		}
		out[i] = spec
	}
	return out
}

// presignNodeArtifacts fills PresignedUrl on every by-ref spec in specs. Called by
// nodeArtifactsForStart AFTER statSkillObjects, so by the time this runs every by-ref sha has
// already been confirmed present (or memoized present from an earlier gate pass) — a presign
// failure here is therefore a live Garage/config fault, not a missing-object condition.
// Returns CodeFailedPrecondition if any by-ref spec is present but skillStore is nil
// (Garage not configured). Returns CodeUnavailable if PresignedGet fails (signs an HMAC
// offline, so this indicates a config error rather than a live Garage connection failure).
// Redact presigned URLs from logs — they are short-lived bearer capabilities.
//
// NOTE(sp-nrzf.3.14.2 S4): spike S4 may later gate by-ref materialize to first-create only
// (resume/fork skip it when journal snapshot captures skill files). Until then, this is called
// on every start unconditionally.
func (s *Server) presignNodeArtifacts(ctx context.Context, specs []*nodev1.ArtifactSpec) error {
	for _, spec := range specs {
		if spec.Objectref == nil {
			continue
		}
		if s.skillStore == nil {
			return connect.NewError(connect.CodeFailedPrecondition,
				fmt.Errorf("spawn has by-ref skill artifacts but Garage skill storage is not configured"))
		}
		u, err := s.skillStore.PresignedGet(ctx, spec.Objectref.Sha256)
		if err != nil {
			return connect.NewError(connect.CodeUnavailable,
				fmt.Errorf("presign skill object %q: %w", spec.Objectref.ObjectKey, err))
		}
		spec.Objectref.PresignedUrl = u
		slog.Debug("presignNodeArtifacts: presigned URL set", "artifact", spec.Id, "key", spec.Objectref.ObjectKey)
	}
	return nil
}

// statSkillObjects is the HEAD-before-presign gate (sp-mwco.4.4): before a spawn start hands
// by-ref artifacts to a node, it confirms every DISTINCT referenced object actually exists in
// the skill store, failing fast (FailedPrecondition) rather than letting the node discover a
// 404 mid-provisioning. Applied UNIFORMLY to every by-ref spec on the spawn — no size carve-out
// (a bundle, the flagship multi-artifact case, is exactly where this matters most).
//
// StatObject calls run in a bounded parallel pool (skillStatParallelism in-flight) over
// distinct shas only — a bundle may reuse one payload sha across several artifacts, so it is
// HEAD'd once. Confirmed-present shas are memoized in s.skillPresent (POSITIVE ONLY: objects
// are immutable and content-addressed, so "present" never goes stale, but "missing" must never
// be cached — a later ingest can make an object present that wasn't a moment ago).
//
// Error split, transport WINS over missing (this is the acceptance property — a Garage
// brownout must never mass-report "skill object missing"): if ANY stat returns a
// transport/timeout error, the whole gate fails CodeUnavailable; only when EVERY failing stat
// is skillstore.ErrObjectMissing does it fail CodeFailedPrecondition, naming the first missing
// object key (and the total missing count, if more than one). Neither message embeds a
// presigned URL or signature (sp-mwco.4.2's leak rule).
//
// Skipped when there are no by-ref specs, and when s.skillStore is nil — the nil-store
// precondition ("Garage skill storage is not configured") is reported by presignNodeArtifacts,
// which runs immediately after this gate; statSkillObjects must not pre-empt that error with a
// nil-deref or a different code.
func (s *Server) statSkillObjects(ctx context.Context, specs []*nodev1.ArtifactSpec) error {
	if s.skillStore == nil {
		return nil
	}

	// Distinct shas only, skipping any already memoized present.
	keyBySha := make(map[string]string)
	for _, spec := range specs {
		if spec.Objectref == nil {
			continue
		}
		sha := spec.Objectref.Sha256
		if _, known := s.skillPresent.Load(sha); known {
			continue
		}
		if _, seen := keyBySha[sha]; !seen {
			keyBySha[sha] = spec.Objectref.ObjectKey
		}
	}
	if len(keyBySha) == 0 {
		return nil
	}

	statCtx, cancel := context.WithTimeout(ctx, skillStatTimeout)
	defer cancel()

	type statResult struct {
		sha string
		key string
		err error
	}
	results := make(chan statResult, len(keyBySha))
	sem := make(chan struct{}, skillStatParallelism)
	var wg sync.WaitGroup
	for sha, key := range keyBySha {
		wg.Add(1)
		sem <- struct{}{}
		go func(sha, key string) {
			defer wg.Done()
			defer func() { <-sem }()
			results <- statResult{sha: sha, key: key, err: s.skillStore.StatObject(statCtx, sha)}
		}(sha, key)
	}
	go func() {
		wg.Wait()
		close(results)
	}()

	var (
		transportErr error
		missingCount int
		firstMissing string
	)
	for res := range results {
		switch {
		case res.err == nil:
			s.skillPresent.Store(res.sha, struct{}{})
		case errors.Is(res.err, skillstore.ErrObjectMissing):
			missingCount++
			if firstMissing == "" {
				firstMissing = res.key
			}
		default:
			if transportErr == nil {
				transportErr = res.err
			}
		}
	}

	if transportErr != nil {
		slog.Warn("statSkillObjects: skill object storage unavailable",
			"distinct_shas", len(keyBySha), "missing_count", missingCount)
		return connect.NewError(connect.CodeUnavailable,
			fmt.Errorf("skill object storage unavailable: %w", transportErr))
	}
	if missingCount > 0 {
		slog.Debug("statSkillObjects: skill object missing",
			"missing_count", missingCount, "first_missing_key", firstMissing)
		if missingCount > 1 {
			return connect.NewError(connect.CodeFailedPrecondition,
				fmt.Errorf("skill object missing: %s (and %d more)", firstMissing, missingCount-1))
		}
		return connect.NewError(connect.CodeFailedPrecondition,
			fmt.Errorf("skill object missing: %s", firstMissing))
	}
	return nil
}

// nodeArtifactsForStart converts persisted artifacts to node wire form, runs the
// HEAD-before-presign gate (statSkillObjects), and presigns any by-ref specs. It is the single
// call site for all four start paths (CreateSpawn/ResumeSpawn/RecreateSpawn/ForkSpawn) — the
// gate therefore runs on every start, and is skipped exactly where re-materialize would be
// skipped (sp-mwco.4.5, not yet implemented).
func (s *Server) nodeArtifactsForStart(ctx context.Context, arts []store.Artifact) ([]*nodev1.ArtifactSpec, error) {
	out := storeToNodeArtifacts(arts, s.effectiveSkillPlainTarCap())
	if err := s.statSkillObjects(ctx, out); err != nil {
		return nil, err
	}
	if err := s.presignNodeArtifacts(ctx, out); err != nil {
		return nil, err
	}
	return out, nil
}
