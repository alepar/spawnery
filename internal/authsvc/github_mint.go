package authsvc

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"connectrpc.com/connect"

	authv1 "spawnery/gen/auth/v1"
	"spawnery/internal/authsvc/store"
)

const githubMintRefreshLead = 10 * time.Minute

func (s *Service) githubMintLock(secretID string) *sync.Mutex {
	s.githubMintLocksMu.Lock()
	defer s.githubMintLocksMu.Unlock()
	lock := s.githubMintLocks[secretID]
	if lock == nil {
		lock = &sync.Mutex{}
		s.githubMintLocks[secretID] = lock
	}
	return lock
}

func (s *Service) MintGitHubAccessToken(ctx context.Context, req *connect.Request[authv1.MintGitHubAccessTokenRequest]) (*connect.Response[authv1.MintGitHubAccessTokenResponse], error) {
	if s.githubMintStore == nil || s.githubMintProvider == nil {
		return nil, connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("github minting is not configured"))
	}
	if s.nodeIdentityExtractor == nil {
		return nil, connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("node identity extractor is not configured"))
	}

	msg := req.Msg
	if msg == nil || strings.TrimSpace(msg.GetRequestId()) == "" || strings.TrimSpace(msg.GetSpawnId()) == "" || msg.GetLinkRef() == nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("request_id, spawn_id, and link_ref are required"))
	}
	ref := msg.GetLinkRef()
	if strings.TrimSpace(ref.GetSecretId()) == "" || ref.GetVersion() == 0 || strings.TrimSpace(ref.GetDeliveryId()) == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("link_ref secret_id, version, and delivery_id are required"))
	}

	nodeID, ok := s.nodeIdentityExtractor(ctx)
	if !ok || strings.TrimSpace(nodeID) == "" {
		return nil, connect.NewError(connect.CodeUnauthenticated, fmt.Errorf("node identity required"))
	}
	if s.githubMintAuthorizer == nil {
		return nil, connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("github mint authorizer is not configured"))
	}

	authz := GitHubMintAuthorization{
		NodeID:       nodeID,
		SpawnID:      msg.GetSpawnId(),
		Generation:   msg.GetGeneration(),
		SecretID:     ref.GetSecretId(),
		Version:      ref.GetVersion(),
		DeliveryID:   ref.GetDeliveryId(),
		RepositoryID: msg.GetRepositoryId(),
	}
	if err := s.githubMintAuthorizer.AuthorizeGitHubMint(ctx, authz); err != nil {
		return nil, err
	}

	lock := s.githubMintLock(ref.GetSecretId())
	lock.Lock()
	defer lock.Unlock()

	link, err := s.githubMintStore.GitHubLinks().Get(ctx, ref.GetSecretId())
	if errors.Is(err, store.ErrNotFound) {
		return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("github link not found"))
	}
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	now := s.now()
	if link.Version != ref.GetVersion() || link.DeliveryID != ref.GetDeliveryId() {
		if ref.GetVersion() < link.Version && link.AccessToken != "" && link.AccessExpiresAtUnix > now.Add(githubMintRefreshLead).Unix() {
			if s.githubTokenFanout != nil {
				if err := s.githubTokenFanout.FanoutGitHubAccessToken(ctx, GitHubAccessTokenFanout{
					SecretID:            link.SecretID,
					AccountID:           link.AccountID,
					Version:             link.Version,
					DeliveryID:          link.DeliveryID,
					RepositoryID:        msg.GetRepositoryId(),
					AccessToken:         link.AccessToken,
					AccessExpiresAtUnix: link.AccessExpiresAtUnix,
					TokenType:           tokenTypeOrBearer(link.TokenType),
				}); err != nil {
					return nil, connect.NewError(connect.CodeUnavailable, err)
				}
			}
			return connect.NewResponse(&authv1.MintGitHubAccessTokenResponse{
				RequestId:           msg.GetRequestId(),
				AccessToken:         link.AccessToken,
				AccessExpiresAtUnix: link.AccessExpiresAtUnix,
				TokenType:           tokenTypeOrBearer(link.TokenType),
				RepositoryId:        msg.GetRepositoryId(),
				Refreshed:           false,
			}), nil
		}
		return nil, connect.NewError(connect.CodePermissionDenied, fmt.Errorf("github link reference does not match current delivery"))
	}

	if link.AccessToken != "" && link.AccessExpiresAtUnix > now.Add(githubMintRefreshLead).Unix() {
		return connect.NewResponse(&authv1.MintGitHubAccessTokenResponse{
			RequestId:           msg.GetRequestId(),
			AccessToken:         link.AccessToken,
			AccessExpiresAtUnix: link.AccessExpiresAtUnix,
			TokenType:           tokenTypeOrBearer(link.TokenType),
			RepositoryId:        msg.GetRepositoryId(),
			Refreshed:           false,
		}), nil
	}

	next, err := s.githubMintProvider.RefreshUserAccessToken(ctx, link.RefreshToken)
	if err != nil {
		return nil, connect.NewError(connect.CodeUnavailable, err)
	}
	if next.AccessToken == "" || next.RefreshToken == "" || next.AccessExpiresAtUnix == 0 || next.RefreshExpiresAtUnix == 0 {
		return nil, connect.NewError(connect.CodeUnavailable, fmt.Errorf("github refresh returned incomplete token tuple"))
	}
	nextVersion := link.Version + 1
	nextDeliveryID := githubAccessDeliveryID(link.SecretID, nextVersion)
	rotated, err := s.githubMintStore.GitHubLinks().Rotate(ctx, link.SecretID, store.GitHubTokenRotation{
		RefreshToken:         next.RefreshToken,
		RefreshExpiresAtUnix: next.RefreshExpiresAtUnix,
		AccessToken:          next.AccessToken,
		AccessExpiresAtUnix:  next.AccessExpiresAtUnix,
		TokenType:            tokenTypeOrBearer(next.TokenType),
		Version:              nextVersion,
		DeliveryID:           nextDeliveryID,
		UpdatedAt:            now.Unix(),
	})
	if errors.Is(err, store.ErrNotFound) {
		return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("github link not found"))
	}
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}

	if s.githubTokenFanout != nil {
		if err := s.githubTokenFanout.FanoutGitHubAccessToken(ctx, GitHubAccessTokenFanout{
			SecretID:            rotated.SecretID,
			AccountID:           rotated.AccountID,
			Version:             rotated.Version,
			DeliveryID:          rotated.DeliveryID,
			RepositoryID:        msg.GetRepositoryId(),
			AccessToken:         rotated.AccessToken,
			AccessExpiresAtUnix: rotated.AccessExpiresAtUnix,
			TokenType:           tokenTypeOrBearer(rotated.TokenType),
		}); err != nil {
			return nil, connect.NewError(connect.CodeUnavailable, err)
		}
	}

	return connect.NewResponse(&authv1.MintGitHubAccessTokenResponse{
		RequestId:           msg.GetRequestId(),
		AccessToken:         rotated.AccessToken,
		AccessExpiresAtUnix: rotated.AccessExpiresAtUnix,
		TokenType:           tokenTypeOrBearer(rotated.TokenType),
		RepositoryId:        msg.GetRepositoryId(),
		Refreshed:           true,
	}), nil
}

func tokenTypeOrBearer(t string) string {
	if strings.TrimSpace(t) == "" {
		return "bearer"
	}
	return t
}

func githubAccessDeliveryID(secretID string, version uint64) string {
	return fmt.Sprintf("github-access-%s-v%d", secretID, version)
}
