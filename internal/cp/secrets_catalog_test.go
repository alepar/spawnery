package cp

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"

	"connectrpc.com/connect"

	cpv1 "spawnery/gen/cp/v1"
	"spawnery/internal/cp/store"
	"spawnery/internal/secrets/seal"
)

func TestSecretCatalogCRUDOwnerScoped(t *testing.T) {
	s, _, _ := newTestServer(t)

	if _, err := s.CreateSecret(noAuthCtx(), connect.NewRequest(&cpv1.CreateSecretRequest{})); connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Fatalf("CreateSecret unauth code = %v, want Unauthenticated", connect.CodeOf(err))
	}

	createResp, err := s.CreateSecret(aliceCtx(), connect.NewRequest(&cpv1.CreateSecretRequest{
		Secret: &cpv1.SecretWrite{
			SecretId:        "s1",
			Type:            cpv1.UserSecretType_USER_SECRET_TYPE_GENERIC_KV,
			Name:            "GitHub token",
			TargetContainer: cpv1.ArtifactTarget_ARTIFACT_TARGET_AGENT,
			EnvVarName:      "GITHUB_TOKEN",
			DevicesetEpoch:  2,
			Envelope:        envelopeBytes(t, "alice", "s1", 1),
		},
	}))
	if err != nil {
		t.Fatalf("CreateSecret: %v", err)
	}
	if createResp.Msg.Secret.GetVersion() != 1 {
		t.Fatalf("CreateSecret version = %d, want 1", createResp.Msg.Secret.GetVersion())
	}

	getResp, err := s.GetSecret(aliceCtx(), connect.NewRequest(&cpv1.GetSecretRequest{SecretId: "s1"}))
	if err != nil {
		t.Fatalf("GetSecret: %v", err)
	}
	if getResp.Msg.Secret.GetSecretId() != "s1" || getResp.Msg.Secret.GetName() != "GitHub token" {
		t.Fatalf("GetSecret = %+v", getResp.Msg.Secret)
	}

	listResp, err := s.ListSecrets(aliceCtx(), connect.NewRequest(&cpv1.ListSecretsRequest{}))
	if err != nil {
		t.Fatalf("ListSecrets: %v", err)
	}
	if len(listResp.Msg.Secrets) != 1 || listResp.Msg.Secrets[0].GetSecretId() != "s1" {
		t.Fatalf("ListSecrets = %+v", listResp.Msg.Secrets)
	}

	if _, err := s.GetSecret(bobCtx(), connect.NewRequest(&cpv1.GetSecretRequest{SecretId: "s1"})); connect.CodeOf(err) != connect.CodeNotFound {
		t.Fatalf("bob GetSecret code = %v, want NotFound", connect.CodeOf(err))
	}

	if _, err := s.DeleteSecret(bobCtx(), connect.NewRequest(&cpv1.DeleteSecretRequest{SecretId: "s1"})); connect.CodeOf(err) != connect.CodeNotFound {
		t.Fatalf("bob DeleteSecret code = %v, want NotFound", connect.CodeOf(err))
	}

	if _, err := s.DeleteSecret(aliceCtx(), connect.NewRequest(&cpv1.DeleteSecretRequest{SecretId: "s1"})); err != nil {
		t.Fatalf("DeleteSecret: %v", err)
	}
	if _, err := s.GetSecret(aliceCtx(), connect.NewRequest(&cpv1.GetSecretRequest{SecretId: "s1"})); connect.CodeOf(err) != connect.CodeNotFound {
		t.Fatalf("GetSecret after delete code = %v, want NotFound", connect.CodeOf(err))
	}
}

func TestCreateSecretRejectsEnvelopeAADMismatch(t *testing.T) {
	s, _, _ := newTestServer(t)

	for _, tc := range []struct {
		name     string
		envelope []byte
	}{
		{name: "wrong owner", envelope: envelopeBytes(t, "bob", "s1", 1)},
		{name: "wrong secret id", envelope: envelopeBytes(t, "alice", "other", 1)},
		{name: "wrong version", envelope: envelopeBytes(t, "alice", "s1", 2)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := s.CreateSecret(aliceCtx(), connect.NewRequest(&cpv1.CreateSecretRequest{
				Secret: &cpv1.SecretWrite{
					SecretId:        "s1",
					Type:            cpv1.UserSecretType_USER_SECRET_TYPE_GENERIC_KV,
					Name:            "secret",
					TargetContainer: cpv1.ArtifactTarget_ARTIFACT_TARGET_AGENT,
					EnvVarName:      "SECRET_VALUE",
					Envelope:        tc.envelope,
				},
			}))
			if connect.CodeOf(err) != connect.CodeInvalidArgument {
				t.Fatalf("CreateSecret code = %v, want InvalidArgument (err=%v)", connect.CodeOf(err), err)
			}
		})
	}
}

func TestPutSecretCASAndEnvelopeVersionValidation(t *testing.T) {
	s, _, _ := newTestServer(t)

	createResp, err := s.CreateSecret(aliceCtx(), connect.NewRequest(&cpv1.CreateSecretRequest{
		Secret: &cpv1.SecretWrite{
			SecretId:        "s1",
			Type:            cpv1.UserSecretType_USER_SECRET_TYPE_INFERENCE_KEY,
			Name:            "OpenRouter",
			Provider:        "openrouter",
			TargetContainer: cpv1.ArtifactTarget_ARTIFACT_TARGET_SIDECAR,
			EnvVarName:      "OPENROUTER_API_KEY",
			DevicesetEpoch:  1,
			Envelope:        envelopeBytes(t, "alice", "s1", 1),
		},
	}))
	if err != nil {
		t.Fatalf("CreateSecret: %v", err)
	}
	originalEnvelope := append([]byte(nil), createResp.Msg.Secret.GetEnvelope()...)

	if _, err := s.PutSecret(aliceCtx(), connect.NewRequest(&cpv1.PutSecretRequest{
		ExpectedVersion: 1,
		Secret: &cpv1.SecretWrite{
			SecretId:        "s1",
			Type:            cpv1.UserSecretType_USER_SECRET_TYPE_INFERENCE_KEY,
			Name:            "OpenRouter",
			Provider:        "openrouter",
			TargetContainer: cpv1.ArtifactTarget_ARTIFACT_TARGET_SIDECAR,
			EnvVarName:      "OPENROUTER_API_KEY",
			DevicesetEpoch:  2,
			Envelope:        envelopeBytes(t, "alice", "s1", 1),
		},
	})); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("PutSecret wrong version code = %v, want InvalidArgument", connect.CodeOf(err))
	}

	getResp, err := s.GetSecret(aliceCtx(), connect.NewRequest(&cpv1.GetSecretRequest{SecretId: "s1"}))
	if err != nil {
		t.Fatalf("GetSecret after rejected Put: %v", err)
	}
	if !bytes.Equal(getResp.Msg.Secret.GetEnvelope(), originalEnvelope) || getResp.Msg.Secret.GetVersion() != 1 {
		t.Fatalf("secret changed after rejected Put: %+v", getResp.Msg.Secret)
	}

	putResp, err := s.PutSecret(aliceCtx(), connect.NewRequest(&cpv1.PutSecretRequest{
		ExpectedVersion: 1,
		Secret: &cpv1.SecretWrite{
			SecretId:        "s1",
			Type:            cpv1.UserSecretType_USER_SECRET_TYPE_INFERENCE_KEY,
			Name:            "OpenRouter rotated",
			Provider:        "openrouter",
			TargetContainer: cpv1.ArtifactTarget_ARTIFACT_TARGET_SIDECAR,
			EnvVarName:      "OPENROUTER_API_KEY",
			DevicesetEpoch:  2,
			Envelope:        envelopeBytes(t, "alice", "s1", 2),
		},
	}))
	if err != nil {
		t.Fatalf("PutSecret: %v", err)
	}
	if putResp.Msg.Secret.GetVersion() != 2 {
		t.Fatalf("PutSecret version = %d, want 2", putResp.Msg.Secret.GetVersion())
	}

	if _, err := s.PutSecret(aliceCtx(), connect.NewRequest(&cpv1.PutSecretRequest{
		ExpectedVersion: 1,
		Secret: &cpv1.SecretWrite{
			SecretId:        "s1",
			Type:            cpv1.UserSecretType_USER_SECRET_TYPE_INFERENCE_KEY,
			Name:            "stale",
			Provider:        "openrouter",
			TargetContainer: cpv1.ArtifactTarget_ARTIFACT_TARGET_SIDECAR,
			EnvVarName:      "OPENROUTER_API_KEY",
			DevicesetEpoch:  3,
			Envelope:        envelopeBytes(t, "alice", "s1", 2),
		},
	})); connect.CodeOf(err) != connect.CodeAborted {
		t.Fatalf("stale PutSecret code = %v, want Aborted", connect.CodeOf(err))
	}
}

func TestPutSecretReturnsWrittenRowDespiteConcurrentUpdate(t *testing.T) {
	s, _, _ := newTestServer(t)
	baseSecrets := s.st.Secrets()
	s.st = secretRaceStore{
		Store:   s.st,
		secrets: &advancingSecretRepo{SecretRepo: baseSecrets, t: t},
	}

	if _, err := s.CreateSecret(aliceCtx(), connect.NewRequest(&cpv1.CreateSecretRequest{
		Secret: &cpv1.SecretWrite{
			SecretId:        "s1",
			Type:            cpv1.UserSecretType_USER_SECRET_TYPE_GENERIC_KV,
			Name:            "initial",
			TargetContainer: cpv1.ArtifactTarget_ARTIFACT_TARGET_AGENT,
			EnvVarName:      "SECRET_VALUE",
			DevicesetEpoch:  1,
			Envelope:        envelopeBytes(t, "alice", "s1", 1),
		},
	})); err != nil {
		t.Fatalf("CreateSecret: %v", err)
	}

	writtenEnvelope := envelopeBytes(t, "alice", "s1", 2)
	putResp, err := s.PutSecret(aliceCtx(), connect.NewRequest(&cpv1.PutSecretRequest{
		ExpectedVersion: 1,
		Secret: &cpv1.SecretWrite{
			SecretId:        "s1",
			Type:            cpv1.UserSecretType_USER_SECRET_TYPE_GENERIC_KV,
			Name:            "caller write",
			TargetContainer: cpv1.ArtifactTarget_ARTIFACT_TARGET_AGENT,
			EnvVarName:      "SECRET_VALUE",
			DevicesetEpoch:  2,
			Envelope:        writtenEnvelope,
		},
	}))
	if err != nil {
		t.Fatalf("PutSecret: %v", err)
	}
	got := putResp.Msg.Secret
	if got.GetVersion() != 2 || got.GetName() != "caller write" || got.GetDevicesetEpoch() != 2 || !bytes.Equal(got.GetEnvelope(), writtenEnvelope) {
		t.Fatalf("PutSecret response = %+v, want the caller's version-2 write", got)
	}
}

func TestListSecretsFiltersByDevicesetEpoch(t *testing.T) {
	s, _, _ := newTestServer(t)

	for _, tc := range []struct {
		secretID string
		epoch    uint64
	}{
		{secretID: "s-old", epoch: 1},
		{secretID: "s-current", epoch: 3},
	} {
		if _, err := s.CreateSecret(aliceCtx(), connect.NewRequest(&cpv1.CreateSecretRequest{
			Secret: &cpv1.SecretWrite{
				SecretId:        tc.secretID,
				Type:            cpv1.UserSecretType_USER_SECRET_TYPE_GENERIC_KV,
				Name:            tc.secretID,
				TargetContainer: cpv1.ArtifactTarget_ARTIFACT_TARGET_AGENT,
				DestPath:        "/run/secrets/" + tc.secretID,
				DevicesetEpoch:  tc.epoch,
				Envelope:        envelopeBytes(t, "alice", tc.secretID, 1),
			},
		})); err != nil {
			t.Fatalf("CreateSecret %s: %v", tc.secretID, err)
		}
	}

	listResp, err := s.ListSecrets(aliceCtx(), connect.NewRequest(&cpv1.ListSecretsRequest{DevicesetEpochBefore: 3}))
	if err != nil {
		t.Fatalf("ListSecrets filtered: %v", err)
	}
	if len(listResp.Msg.Secrets) != 1 || listResp.Msg.Secrets[0].GetSecretId() != "s-old" {
		t.Fatalf("filtered ListSecrets = %+v", listResp.Msg.Secrets)
	}
}

func envelopeBytes(t *testing.T, accountID, secretID string, version uint64) []byte {
	t.Helper()
	b, err := json.Marshal(seal.Envelope{AtRest: seal.AtRestAAD{AccountID: accountID, SecretID: secretID, Version: version}})
	if err != nil {
		t.Fatalf("json.Marshal envelope: %v", err)
	}
	return b
}

type secretRaceStore struct {
	store.Store
	secrets store.SecretRepo
}

func (s secretRaceStore) Secrets() store.SecretRepo { return s.secrets }

type advancingSecretRepo struct {
	store.SecretRepo
	t *testing.T
}

func (r *advancingSecretRepo) Put(ctx context.Context, accountID, secretID string, expectedVersion uint64, next store.Secret) (uint64, error) {
	newVersion, err := r.SecretRepo.Put(ctx, accountID, secretID, expectedVersion, next)
	if err != nil {
		return 0, err
	}
	later := next
	later.Name = "concurrent write"
	later.DevicesetEpoch = 99
	later.Envelope = envelopeBytes(r.t, accountID, secretID, newVersion+1)
	later.UpdatedAt = next.UpdatedAt + 1
	if _, err := r.SecretRepo.Put(ctx, accountID, secretID, newVersion, later); err != nil {
		r.t.Fatalf("advance secret version after Put: %v", err)
	}
	return newVersion, nil
}
