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

	// Relink fast-fail: the refresh chain is provably broken; callers must relink (not retry).
	if link.RelinkRequired {
		return nil, relinkRequiredError(link.SecretID)
	}
	// Pending recovery: a prior attempt rotated at GitHub and staged the new tuple but failed
	// to commit (version bump + delivery_id). Promote it WITHOUT calling GitHub — the predecessor
	// refresh token is already dead at GitHub (single-use rotation, sp-v40s.3).
	if link.PendingRefreshToken != "" {
		rotated, err := s.commitGitHubRotation(ctx, link.SecretID, store.GitHubTokenRotation{
			RefreshToken:         link.PendingRefreshToken,
			RefreshExpiresAtUnix: link.PendingRefreshExpiresAtUnix,
			AccessToken:          link.PendingAccessToken,
			AccessExpiresAtUnix:  link.PendingAccessExpiresAtUnix,
			TokenType:            tokenTypeOrBearer(link.PendingTokenType),
			Version:              link.PendingVersion,
			DeliveryID:           githubAccessDeliveryID(link.SecretID, link.PendingVersion),
			UpdatedAt:            s.now().Unix(),
		}, msg.GetRepositoryId())
		if err != nil {
			return nil, err
		}
		return mintRefreshedResponse(msg, rotated), nil
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
		if errors.Is(err, ErrRefreshRejected) {
			// Provably-broken chain: mark terminal and surface relink_required (not retryable).
			_ = s.githubMintStore.GitHubLinks().MarkRelinkRequired(ctx, link.SecretID, now.Unix())
			return nil, relinkRequiredError(link.SecretID)
		}
		return nil, connect.NewError(connect.CodeUnavailable, err)
	}
	if next.AccessToken == "" || next.RefreshToken == "" || next.AccessExpiresAtUnix == 0 || next.RefreshExpiresAtUnix == 0 {
		return nil, connect.NewError(connect.CodeUnavailable, fmt.Errorf("github refresh returned incomplete token tuple"))
	}
	nextVersion := link.Version + 1
	// WRITE-AHEAD: durably stage the rotated tuple BEFORE committing. If staging fails the rotation
	// cannot be made durable; return retryable — the next attempt finds the old refresh dead at GitHub
	// (ErrRefreshRejected) and converges to relink_required.
	if err := s.githubMintStore.GitHubLinks().StageRotation(ctx, link.SecretID, store.GitHubStagedRotation{
		RefreshToken:         next.RefreshToken,
		RefreshExpiresAtUnix: next.RefreshExpiresAtUnix,
		AccessToken:          next.AccessToken,
		AccessExpiresAtUnix:  next.AccessExpiresAtUnix,
		TokenType:            tokenTypeOrBearer(next.TokenType),
		Version:              nextVersion,
	}); err != nil {
		return nil, connect.NewError(connect.CodeUnavailable, err)
	}
	rotated, err := s.commitGitHubRotation(ctx, link.SecretID, store.GitHubTokenRotation{
		RefreshToken:         next.RefreshToken,
		RefreshExpiresAtUnix: next.RefreshExpiresAtUnix,
		AccessToken:          next.AccessToken,
		AccessExpiresAtUnix:  next.AccessExpiresAtUnix,
		TokenType:            tokenTypeOrBearer(next.TokenType),
		Version:              nextVersion,
		DeliveryID:           githubAccessDeliveryID(link.SecretID, nextVersion),
		UpdatedAt:            now.Unix(),
	}, msg.GetRepositoryId())
	if err != nil {
		return nil, err
	}
	return mintRefreshedResponse(msg, rotated), nil
}

// relinkRequiredError is the typed terminal outcome of a provably-broken refresh chain. It is NOT
// retryable: callers must surface a relink prompt rather than retrying into a dead single-use chain.
func relinkRequiredError(secretID string) error {
	return connect.NewError(connect.CodeFailedPrecondition,
		fmt.Errorf("github link %s: relink_required: refresh chain is broken", secretID))
}

// commitGitHubRotation promotes a rotation to the live tuple (Rotate clears any pending stage) and
// fans the new shared access token out to currently-hosting nodes (CP relays sealed bytes only).
func (s *Service) commitGitHubRotation(ctx context.Context, secretID string, rot store.GitHubTokenRotation, repositoryID string) (store.GitHubLink, error) {
	rotated, err := s.githubMintStore.GitHubLinks().Rotate(ctx, secretID, rot)
	if errors.Is(err, store.ErrNotFound) {
		return store.GitHubLink{}, connect.NewError(connect.CodeNotFound, fmt.Errorf("github link not found"))
	}
	if err != nil {
		return store.GitHubLink{}, connect.NewError(connect.CodeInternal, err)
	}
	if s.githubTokenFanout != nil {
		if err := s.githubTokenFanout.FanoutGitHubAccessToken(ctx, GitHubAccessTokenFanout{
			SecretID:            rotated.SecretID,
			AccountID:           rotated.AccountID,
			Version:             rotated.Version,
			DeliveryID:          rotated.DeliveryID,
			RepositoryID:        repositoryID,
			AccessToken:         rotated.AccessToken,
			AccessExpiresAtUnix: rotated.AccessExpiresAtUnix,
			TokenType:           tokenTypeOrBearer(rotated.TokenType),
		}); err != nil {
			return store.GitHubLink{}, connect.NewError(connect.CodeUnavailable, err)
		}
	}
	return rotated, nil
}

// mintRefreshedResponse builds a response indicating a fresh rotation was performed.
func mintRefreshedResponse(msg *authv1.MintGitHubAccessTokenRequest, rotated store.GitHubLink) *connect.Response[authv1.MintGitHubAccessTokenResponse] {
	return connect.NewResponse(&authv1.MintGitHubAccessTokenResponse{
		RequestId:           msg.GetRequestId(),
		AccessToken:         rotated.AccessToken,
		AccessExpiresAtUnix: rotated.AccessExpiresAtUnix,
		TokenType:           tokenTypeOrBearer(rotated.TokenType),
		RepositoryId:        msg.GetRepositoryId(),
		Refreshed:           true,
	})
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
