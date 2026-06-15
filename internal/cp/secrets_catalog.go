package cp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"connectrpc.com/connect"

	cpv1 "spawnery/gen/cp/v1"
	"spawnery/internal/cp/auth"
	"spawnery/internal/cp/store"
	"spawnery/internal/secrets/seal"
)

func (s *Server) CreateSecret(ctx context.Context, req *connect.Request[cpv1.CreateSecretRequest]) (*connect.Response[cpv1.CreateSecretResponse], error) {
	owner, ok := auth.OwnerFromContext(ctx)
	if !ok {
		return nil, connect.NewError(connect.CodeUnauthenticated, fmt.Errorf("no owner"))
	}
	secret, err := validateSecretWrite(req.Msg.GetSecret(), owner, 1)
	if err != nil {
		return nil, err
	}
	now := time.Now().Unix()
	row := store.Secret{
		AccountID:       owner,
		SecretID:        secret.secretID,
		Type:            secret.secretType,
		Name:            secret.name,
		Provider:        secret.provider,
		TargetContainer: int32(secret.targetContainer),
		EnvVarName:      secret.envVarName,
		DestPath:        secret.destPath,
		Version:         1,
		DevicesetEpoch:  secret.devicesetEpoch,
		Envelope:        append([]byte(nil), secret.envelope...),
		CreatedAt:       now,
		UpdatedAt:       now,
	}
	if err := s.st.Secrets().Create(ctx, row); err != nil {
		if _, gerr := s.st.Secrets().Get(ctx, owner, row.SecretID); gerr == nil {
			return nil, connect.NewError(connect.CodeAlreadyExists, fmt.Errorf("secret already exists"))
		}
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	return connect.NewResponse(&cpv1.CreateSecretResponse{Secret: secretToProto(row)}), nil
}

func (s *Server) GetSecret(ctx context.Context, req *connect.Request[cpv1.GetSecretRequest]) (*connect.Response[cpv1.GetSecretResponse], error) {
	owner, ok := auth.OwnerFromContext(ctx)
	if !ok {
		return nil, connect.NewError(connect.CodeUnauthenticated, fmt.Errorf("no owner"))
	}
	row, err := s.st.Secrets().Get(ctx, owner, strings.TrimSpace(req.Msg.GetSecretId()))
	if err != nil {
		return nil, mapSecretStoreErr(err)
	}
	return connect.NewResponse(&cpv1.GetSecretResponse{Secret: secretToProto(row)}), nil
}

func (s *Server) ListSecrets(ctx context.Context, req *connect.Request[cpv1.ListSecretsRequest]) (*connect.Response[cpv1.ListSecretsResponse], error) {
	owner, ok := auth.OwnerFromContext(ctx)
	if !ok {
		return nil, connect.NewError(connect.CodeUnauthenticated, fmt.Errorf("no owner"))
	}
	rows, err := s.st.Secrets().ListByOwner(ctx, owner, store.SecretListFilter{
		DevicesetEpochBefore: req.Msg.GetDevicesetEpochBefore(),
	})
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	out := make([]*cpv1.SecretSummary, len(rows))
	for i, row := range rows {
		out[i] = secretSummaryToProto(row)
	}
	return connect.NewResponse(&cpv1.ListSecretsResponse{Secrets: out}), nil
}

func (s *Server) PutSecret(ctx context.Context, req *connect.Request[cpv1.PutSecretRequest]) (*connect.Response[cpv1.PutSecretResponse], error) {
	owner, ok := auth.OwnerFromContext(ctx)
	if !ok {
		return nil, connect.NewError(connect.CodeUnauthenticated, fmt.Errorf("no owner"))
	}
	newVersion := req.Msg.GetExpectedVersion() + 1
	secret, err := validateSecretWrite(req.Msg.GetSecret(), owner, newVersion)
	if err != nil {
		return nil, err
	}
	now := time.Now().Unix()
	existing, err := s.st.Secrets().Get(ctx, owner, secret.secretID)
	if err != nil {
		return nil, mapSecretStoreErr(err)
	}
	row := store.Secret{
		AccountID:       owner,
		SecretID:        secret.secretID,
		Type:            secret.secretType,
		Name:            secret.name,
		Provider:        secret.provider,
		TargetContainer: int32(secret.targetContainer),
		EnvVarName:      secret.envVarName,
		DestPath:        secret.destPath,
		Version:         newVersion,
		DevicesetEpoch:  secret.devicesetEpoch,
		Envelope:        append([]byte(nil), secret.envelope...),
		CreatedAt:       existing.CreatedAt,
		UpdatedAt:       now,
	}
	version, err := s.st.Secrets().Put(ctx, owner, secret.secretID, req.Msg.GetExpectedVersion(), row)
	if err != nil {
		return nil, mapSecretStoreErr(err)
	}
	row.Version = version
	return connect.NewResponse(&cpv1.PutSecretResponse{Secret: secretToProto(row)}), nil
}

func (s *Server) DeleteSecret(ctx context.Context, req *connect.Request[cpv1.DeleteSecretRequest]) (*connect.Response[cpv1.DeleteSecretResponse], error) {
	owner, ok := auth.OwnerFromContext(ctx)
	if !ok {
		return nil, connect.NewError(connect.CodeUnauthenticated, fmt.Errorf("no owner"))
	}
	if err := s.st.Secrets().Delete(ctx, owner, strings.TrimSpace(req.Msg.GetSecretId())); err != nil {
		return nil, mapSecretStoreErr(err)
	}
	return connect.NewResponse(&cpv1.DeleteSecretResponse{}), nil
}

type validatedSecretWrite struct {
	secretID        string
	secretType      store.SecretType
	name            string
	provider        string
	targetContainer cpv1.ArtifactTarget
	envVarName      string
	destPath        string
	devicesetEpoch  uint64
	envelope        []byte
}

func validateSecretWrite(msg *cpv1.SecretWrite, owner string, newVersion uint64) (validatedSecretWrite, error) {
	if msg == nil {
		return validatedSecretWrite{}, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("secret is required"))
	}
	secretID := strings.TrimSpace(msg.GetSecretId())
	if secretID == "" {
		return validatedSecretWrite{}, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("secret_id is required"))
	}
	name := strings.TrimSpace(msg.GetName())
	if name == "" {
		return validatedSecretWrite{}, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("name is required"))
	}
	secretType, err := protoSecretTypeToStore(msg.GetType())
	if err != nil {
		return validatedSecretWrite{}, err
	}
	provider := strings.TrimSpace(msg.GetProvider())
	switch secretType {
	case store.SecretTypeInferenceKey:
		if provider == "" {
			return validatedSecretWrite{}, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("provider is required for inference-key"))
		}
	default:
		if provider != "" {
			return validatedSecretWrite{}, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("provider is only allowed for inference-key"))
		}
	}
	target := msg.GetTargetContainer()
	if target != cpv1.ArtifactTarget_ARTIFACT_TARGET_AGENT && target != cpv1.ArtifactTarget_ARTIFACT_TARGET_SIDECAR {
		return validatedSecretWrite{}, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("target_container must be agent or sidecar"))
	}
	envVarName := strings.TrimSpace(msg.GetEnvVarName())
	destPath := strings.TrimSpace(msg.GetDestPath())
	if envVarName == "" && destPath == "" {
		return validatedSecretWrite{}, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("env_var_name or dest_path is required"))
	}
	if err := validateEnvelopeAAD(msg.GetEnvelope(), owner, secretID, newVersion); err != nil {
		return validatedSecretWrite{}, err
	}
	return validatedSecretWrite{
		secretID:        secretID,
		secretType:      secretType,
		name:            name,
		provider:        provider,
		targetContainer: target,
		envVarName:      envVarName,
		destPath:        destPath,
		devicesetEpoch:  msg.GetDevicesetEpoch(),
		envelope:        append([]byte(nil), msg.GetEnvelope()...),
	}, nil
}

func validateEnvelopeAAD(raw []byte, owner, secretID string, version uint64) error {
	if len(raw) == 0 {
		return connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("envelope is required"))
	}
	var env seal.Envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("invalid envelope JSON: %w", err))
	}
	want := seal.AtRestAAD{AccountID: owner, SecretID: secretID, Version: version}
	if env.AtRest != want {
		return connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("envelope at_rest must match owner, secret_id, and version"))
	}
	return nil
}

func mapSecretStoreErr(err error) error {
	switch {
	case errors.Is(err, store.ErrNotFound):
		return connect.NewError(connect.CodeNotFound, fmt.Errorf("secret not found"))
	case errors.Is(err, store.ErrConflict):
		return connect.NewError(connect.CodeAborted, fmt.Errorf("version conflict - retry with current version"))
	default:
		return connect.NewError(connect.CodeInternal, err)
	}
}

func protoSecretTypeToStore(t cpv1.UserSecretType) (store.SecretType, error) {
	switch t {
	case cpv1.UserSecretType_USER_SECRET_TYPE_GITHUB_TOKEN:
		return store.SecretTypeGitHubToken, nil
	case cpv1.UserSecretType_USER_SECRET_TYPE_INFERENCE_KEY:
		return store.SecretTypeInferenceKey, nil
	case cpv1.UserSecretType_USER_SECRET_TYPE_GENERIC_KV:
		return store.SecretTypeGenericKV, nil
	default:
		return "", connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("type is required"))
	}
}

func storeSecretTypeToProto(t store.SecretType) cpv1.UserSecretType {
	switch t {
	case store.SecretTypeGitHubToken:
		return cpv1.UserSecretType_USER_SECRET_TYPE_GITHUB_TOKEN
	case store.SecretTypeInferenceKey:
		return cpv1.UserSecretType_USER_SECRET_TYPE_INFERENCE_KEY
	case store.SecretTypeGenericKV:
		return cpv1.UserSecretType_USER_SECRET_TYPE_GENERIC_KV
	default:
		return cpv1.UserSecretType_USER_SECRET_TYPE_UNSPECIFIED
	}
}

func secretToProto(row store.Secret) *cpv1.Secret {
	return &cpv1.Secret{
		SecretId:        row.SecretID,
		Type:            storeSecretTypeToProto(row.Type),
		Name:            row.Name,
		Provider:        row.Provider,
		TargetContainer: cpv1.ArtifactTarget(row.TargetContainer),
		EnvVarName:      row.EnvVarName,
		DestPath:        row.DestPath,
		Version:         row.Version,
		DevicesetEpoch:  row.DevicesetEpoch,
		Envelope:        append([]byte(nil), row.Envelope...),
		CreatedAt:       row.CreatedAt,
		UpdatedAt:       row.UpdatedAt,
	}
}

func secretSummaryToProto(row store.Secret) *cpv1.SecretSummary {
	return &cpv1.SecretSummary{
		SecretId:        row.SecretID,
		Type:            storeSecretTypeToProto(row.Type),
		Name:            row.Name,
		Provider:        row.Provider,
		TargetContainer: cpv1.ArtifactTarget(row.TargetContainer),
		EnvVarName:      row.EnvVarName,
		DestPath:        row.DestPath,
		Version:         row.Version,
		DevicesetEpoch:  row.DevicesetEpoch,
		UpdatedAt:       row.UpdatedAt,
	}
}

var _ = context.Background
