package store_test

import (
	"context"
	"errors"
	"testing"

	"spawnery/internal/cp/store"
)

func TestSecretsCreateGetListAndDelete(t *testing.T) {
	st := store.NewTestStore(t)
	ctx := context.Background()
	seedSecretOwners(t, ctx, st)

	createdAt := int64(101)
	err := st.Secrets().Create(ctx, store.Secret{
		AccountID:       "alice",
		SecretID:        "s1",
		Type:            store.SecretTypeGenericKV,
		Name:            "GitHub token",
		TargetContainer: 1,
		EnvVarName:      "GITHUB_TOKEN",
		Version:         99,
		DevicesetEpoch:  2,
		Envelope:        []byte(`{"at_rest":{"account_id":"alice","secret_id":"s1","version":1}}`),
		CreatedAt:       createdAt,
		UpdatedAt:       createdAt,
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	got, err := st.Secrets().Get(ctx, "alice", "s1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.AccountID != "alice" || got.SecretID != "s1" {
		t.Fatalf("Get identity = %+v", got)
	}
	if got.Version != 1 {
		t.Fatalf("Get version = %d, want 1", got.Version)
	}
	if string(got.Envelope) != `{"at_rest":{"account_id":"alice","secret_id":"s1","version":1}}` {
		t.Fatalf("Get envelope = %s", string(got.Envelope))
	}

	list, err := st.Secrets().ListByOwner(ctx, "alice", store.SecretListFilter{})
	if err != nil {
		t.Fatalf("ListByOwner: %v", err)
	}
	if len(list) != 1 || list[0].SecretID != "s1" {
		t.Fatalf("ListByOwner = %+v, want s1", list)
	}

	if err := st.Secrets().Delete(ctx, "alice", "s1"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := st.Secrets().Get(ctx, "alice", "s1"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("Get after delete err = %v, want ErrNotFound", err)
	}
	if err := st.Secrets().Delete(ctx, "alice", "s1"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("Delete missing err = %v, want ErrNotFound", err)
	}
}

func TestSecretsAreOwnerScoped(t *testing.T) {
	st := store.NewTestStore(t)
	ctx := context.Background()
	seedSecretOwners(t, ctx, st)

	if err := st.Secrets().Create(ctx, store.Secret{
		AccountID:       "alice",
		SecretID:        "s1",
		Type:            store.SecretTypeGenericKV,
		Name:            "secret",
		TargetContainer: 1,
		EnvVarName:      "SECRET_VALUE",
		Envelope:        []byte(`{"at_rest":{"account_id":"alice","secret_id":"s1","version":1}}`),
		CreatedAt:       10,
		UpdatedAt:       10,
	}); err != nil {
		t.Fatalf("Create: %v", err)
	}

	if _, err := st.Secrets().Get(ctx, "bob", "s1"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("Get as bob err = %v, want ErrNotFound", err)
	}
	list, err := st.Secrets().ListByOwner(ctx, "bob", store.SecretListFilter{})
	if err != nil {
		t.Fatalf("ListByOwner bob: %v", err)
	}
	if len(list) != 0 {
		t.Fatalf("ListByOwner bob len = %d, want 0", len(list))
	}
	if err := st.Secrets().Delete(ctx, "bob", "s1"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("Delete as bob err = %v, want ErrNotFound", err)
	}
}

func TestSecretsPutUsesStrictCASAndBumpsVersion(t *testing.T) {
	st := store.NewTestStore(t)
	ctx := context.Background()
	seedSecretOwners(t, ctx, st)

	if err := st.Secrets().Create(ctx, store.Secret{
		AccountID:       "alice",
		SecretID:        "s1",
		Type:            store.SecretTypeInferenceKey,
		Name:            "OpenRouter",
		Provider:        "openrouter",
		TargetContainer: 2,
		EnvVarName:      "OPENROUTER_API_KEY",
		Envelope:        []byte(`{"at_rest":{"account_id":"alice","secret_id":"s1","version":1}}`),
		CreatedAt:       10,
		UpdatedAt:       10,
	}); err != nil {
		t.Fatalf("Create: %v", err)
	}

	newVersion, err := st.Secrets().Put(ctx, "alice", "s1", 1, store.Secret{
		AccountID:       "alice",
		SecretID:        "s1",
		Type:            store.SecretTypeInferenceKey,
		Name:            "OpenRouter rotated",
		Provider:        "openrouter",
		TargetContainer: 2,
		EnvVarName:      "OPENROUTER_API_KEY",
		Version:         2,
		DevicesetEpoch:  4,
		Envelope:        []byte(`{"at_rest":{"account_id":"alice","secret_id":"s1","version":2}}`),
		UpdatedAt:       40,
	})
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	if newVersion != 2 {
		t.Fatalf("Put newVersion = %d, want 2", newVersion)
	}

	got, err := st.Secrets().Get(ctx, "alice", "s1")
	if err != nil {
		t.Fatalf("Get after Put: %v", err)
	}
	if got.Version != 2 || got.Name != "OpenRouter rotated" || got.DevicesetEpoch != 4 {
		t.Fatalf("Get after Put = %+v", got)
	}

	_, err = st.Secrets().Put(ctx, "alice", "s1", 1, store.Secret{
		AccountID:       "alice",
		SecretID:        "s1",
		Type:            store.SecretTypeInferenceKey,
		Name:            "stale",
		Provider:        "openrouter",
		TargetContainer: 2,
		EnvVarName:      "OPENROUTER_API_KEY",
		Version:         2,
		DevicesetEpoch:  5,
		Envelope:        []byte(`{"at_rest":{"account_id":"alice","secret_id":"s1","version":2}}`),
		UpdatedAt:       50,
	})
	if !errors.Is(err, store.ErrConflict) {
		t.Fatalf("stale Put err = %v, want ErrConflict", err)
	}

	_, err = st.Secrets().Put(ctx, "alice", "missing", 1, store.Secret{
		AccountID: "alice",
		SecretID:  "missing",
	})
	if !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("missing Put err = %v, want ErrNotFound", err)
	}
}

func TestSecretsListFiltersDevicesetEpochBefore(t *testing.T) {
	st := store.NewTestStore(t)
	ctx := context.Background()
	seedSecretOwners(t, ctx, st)

	for _, tc := range []struct {
		secretID string
		epoch    uint64
	}{
		{secretID: "s2", epoch: 3},
		{secretID: "s1", epoch: 1},
		{secretID: "s3", epoch: 2},
	} {
		if err := st.Secrets().Create(ctx, store.Secret{
			AccountID:       "alice",
			SecretID:        tc.secretID,
			Type:            store.SecretTypeGenericKV,
			Name:            tc.secretID,
			TargetContainer: 1,
			DestPath:        "/run/secrets/" + tc.secretID,
			DevicesetEpoch:  tc.epoch,
			Envelope:        []byte(`{"at_rest":{"account_id":"alice","secret_id":"` + tc.secretID + `","version":1}}`),
			CreatedAt:       10,
			UpdatedAt:       10,
		}); err != nil {
			t.Fatalf("Create %s: %v", tc.secretID, err)
		}
	}

	list, err := st.Secrets().ListByOwner(ctx, "alice", store.SecretListFilter{DevicesetEpochBefore: 3})
	if err != nil {
		t.Fatalf("ListByOwner filtered: %v", err)
	}
	if got := []string{list[0].SecretID, list[1].SecretID}; len(list) != 2 || got[0] != "s1" || got[1] != "s3" {
		t.Fatalf("filtered list = %+v, want s1,s3", list)
	}
}

func seedSecretOwners(t *testing.T, ctx context.Context, st store.Store) {
	t.Helper()
	for _, ownerID := range []string{"alice", "bob"} {
		if err := st.Owners().Upsert(ctx, store.Owner{ID: ownerID, Email: ownerID + "@example.com", CreatedAt: 1}); err != nil {
			t.Fatalf("Owners.Upsert(%s): %v", ownerID, err)
		}
	}
}
