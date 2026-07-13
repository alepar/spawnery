package cp

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"connectrpc.com/connect"

	cpv1 "spawnery/gen/cp/v1"
	"spawnery/internal/cp/store"
)

// shaHexRe matches a normalized (lowercase) sha256 hex digest.
var shaHexRe = regexp.MustCompile(`^[0-9a-f]{64}$`)

// normalizeSha256 lowercases and trims sha, then validates it against shaHexRe. Returns
// InvalidArgument on anything that doesn't come out looking like a sha256 hex digest.
func normalizeSha256(sha string) (string, error) {
	norm := strings.ToLower(strings.TrimSpace(sha))
	if !shaHexRe.MatchString(norm) {
		return "", connect.NewError(connect.CodeInvalidArgument,
			fmt.Errorf("sha256 must be a 64-character hex digest"))
	}
	return norm, nil
}

// --- DenySkillObject -------------------------------------------------------

// DenySkillObject is the real-revocation kill switch (sp-mwco.3.2 §4.2): admin-only, upserts a
// sha256 denial consulted by presignNodeArtifacts on every start path. reason is required — an
// unexplained kill switch is unauditable. Does NOT terminate already-running spawns; its
// contract is exactly that the object cannot be re-materialized on any subsequent start.
func (s *Server) DenySkillObject(ctx context.Context, req *connect.Request[cpv1.DenySkillObjectRequest]) (*connect.Response[cpv1.DenySkillObjectResponse], error) {
	owner, err := s.requireAdmin(ctx)
	if err != nil {
		return nil, err
	}
	sha, err := normalizeSha256(req.Msg.Sha256)
	if err != nil {
		return nil, err
	}
	reason := strings.TrimSpace(req.Msg.Reason)
	if reason == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("reason is required"))
	}
	if err := s.st.SkillDenylist().Deny(ctx, store.SkillObjectDenial{
		SHA256:    sha,
		Reason:    reason,
		DeniedBy:  owner,
		CreatedAt: time.Now().Unix(),
	}); err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	return connect.NewResponse(&cpv1.DenySkillObjectResponse{}), nil
}

// --- AllowSkillObject -------------------------------------------------------

// AllowSkillObject un-denies a sha (admin-only) — the undo for a mis-typed sha. NotFound when
// the sha is not currently denied.
func (s *Server) AllowSkillObject(ctx context.Context, req *connect.Request[cpv1.AllowSkillObjectRequest]) (*connect.Response[cpv1.AllowSkillObjectResponse], error) {
	if _, err := s.requireAdmin(ctx); err != nil {
		return nil, err
	}
	sha, err := normalizeSha256(req.Msg.Sha256)
	if err != nil {
		return nil, err
	}
	if err := s.st.SkillDenylist().Allow(ctx, sha); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("sha256 not denied"))
		}
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	return connect.NewResponse(&cpv1.AllowSkillObjectResponse{}), nil
}

// --- ListSkillObjectDenials -------------------------------------------------------

// ListSkillObjectDenials surfaces every recorded denial (§4.2's "reason recorded and
// surfaced"), ordered created_at DESC. Admin-only.
func (s *Server) ListSkillObjectDenials(ctx context.Context, _ *connect.Request[cpv1.ListSkillObjectDenialsRequest]) (*connect.Response[cpv1.ListSkillObjectDenialsResponse], error) {
	if _, err := s.requireAdmin(ctx); err != nil {
		return nil, err
	}
	list, err := s.st.SkillDenylist().List(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	out := make([]*cpv1.SkillObjectDenial, len(list))
	for i, d := range list {
		out[i] = &cpv1.SkillObjectDenial{
			Sha256:    d.SHA256,
			Reason:    d.Reason,
			DeniedBy:  d.DeniedBy,
			CreatedAt: d.CreatedAt,
		}
	}
	return connect.NewResponse(&cpv1.ListSkillObjectDenialsResponse{Denials: out}), nil
}
