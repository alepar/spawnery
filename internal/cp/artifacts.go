package cp

import (
	"context"
	"fmt"
	"log/slog"
	"path"
	"strings"

	"connectrpc.com/connect"

	cpv1 "spawnery/gen/cp/v1"
	nodev1 "spawnery/gen/node/v1"
	"spawnery/internal/cp/store"
)

// Per-spawn artifact limits: inline-only, capped well under the Connect ~4MB message limit;
// reject oversize at CreateSpawn.
const (
	maxArtifactsPerSpawn   = 64
	maxArtifactInlineBytes = 1 << 20 // 1 MiB per artifact
	maxArtifactsTotalBytes = 3 << 20 // 3 MiB total
)

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
// By-ref artifacts (ObjectKey != "") populate Objectref; PresignedUrl is left empty here
// and filled by presignNodeArtifacts before the node RPC.
func storeToNodeArtifacts(in []store.Artifact) []*nodev1.ArtifactSpec {
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
				ObjectKey: a.ObjectKey,
				Sha256:    a.ObjectSHA256,
				// PresignedUrl left empty; presignNodeArtifacts fills it at start.
			}
		}
		out[i] = spec
	}
	return out
}

// presignNodeArtifacts fills PresignedUrl on every by-ref spec in specs.
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

// nodeArtifactsForStart converts persisted artifacts to node wire form and presigns
// any by-ref specs. It is the single call site for all four start paths
// (CreateSpawn/ResumeSpawn/RecreateSpawn/ForkSpawn).
func (s *Server) nodeArtifactsForStart(ctx context.Context, arts []store.Artifact) ([]*nodev1.ArtifactSpec, error) {
	out := storeToNodeArtifacts(arts)
	if err := s.presignNodeArtifacts(ctx, out); err != nil {
		return nil, err
	}
	return out, nil
}
