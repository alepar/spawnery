package cp

// pendingintent_test.go: unit tests for pendingIntentRegistry covering the branches not
// exercised by intent_threading_test.go: TTL expiry and the owner-mismatch guard [AC1].

import (
	"context"
	"testing"
	"time"

	authv1 "spawnery/gen/auth/v1"
	cpv1 "spawnery/gen/cp/v1"
	nodev1 "spawnery/gen/node/v1"
)

// testEnv returns a minimal non-nil AuthEnvelope for submit calls.
func testEnv() *authv1.AuthEnvelope {
	return &authv1.AuthEnvelope{AccessToken: "tok", Intent: &authv1.SignedIntent{Domain: "d"}}
}

func testSubmission() *pendingIntentSubmission {
	return &pendingIntentSubmission{Env: testEnv()}
}

// testPI returns a minimal PendingIntent.
func testPI(spawnID string) *cpv1.PendingIntent {
	return &cpv1.PendingIntent{SpawnId: spawnID, Generation: 1}
}

// TestPendingIntentTTLExpiry: await must return an error when no SubmitIntent arrives within
// the registry TTL. The lifecycle handler uses this error to abort provision and set spawn error.
func TestPendingIntentTTLExpiry(t *testing.T) {
	r := newPendingIntentRegistry()
	r.ttl = 20 * time.Millisecond // short TTL so the test completes quickly

	ch := r.register("spawn-ttl", "alice", testPI("spawn-ttl"))
	_, err := r.await(context.Background(), ch)
	if err == nil {
		t.Fatal("await must return error on TTL expiry; got nil")
	}
}

// TestPendingIntentContextCancel: await must return when the context is cancelled (e.g., client
// disconnect mid-provision), not wait for TTL.
func TestPendingIntentContextCancel(t *testing.T) {
	r := newPendingIntentRegistry()
	r.ttl = 10 * time.Second // long TTL; we cancel the ctx instead

	ch := r.register("spawn-cancel", "alice", testPI("spawn-cancel"))
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel immediately
	_, err := r.await(ctx, ch)
	if err == nil {
		t.Fatal("await must return error on context cancel; got nil")
	}
}

// TestPendingIntentOwnerMismatch: submit must refuse an envelope from a non-owner.
// This guards against a compromised CP routing a foreign SubmitIntent to steal a provision slot.
func TestPendingIntentOwnerMismatch(t *testing.T) {
	r := newPendingIntentRegistry()
	r.register("spawn-owner", "alice", testPI("spawn-owner"))

	// Bob tries to submit for Alice's spawn.
	err := r.submit("spawn-owner", "bob", testSubmission())
	if err == nil {
		t.Fatal("submit must return error for wrong owner; got nil")
	}
}

// TestPendingIntentDoubleSubmit: a second submit for the same spawnID must be refused.
// The buffered channel (cap 1) enforces exactly-once delivery without blocking.
func TestPendingIntentDoubleSubmit(t *testing.T) {
	r := newPendingIntentRegistry()
	r.register("spawn-double", "alice", testPI("spawn-double"))

	if err := r.submit("spawn-double", "alice", testSubmission()); err != nil {
		t.Fatalf("first submit should succeed; got: %v", err)
	}
	if err := r.submit("spawn-double", "alice", testSubmission()); err == nil {
		t.Fatal("second submit must return error (already submitted); got nil")
	}
}

func TestPendingIntentDuplicateSubmitAfterAwaitDoesNotBlock(t *testing.T) {
	r := newPendingIntentRegistry()
	r.ttl = 5 * time.Second

	ch := r.register("spawn-drained", "alice", testPI("spawn-drained"))
	if err := r.submit("spawn-drained", "alice", testSubmission()); err != nil {
		t.Fatalf("first submit should succeed; got: %v", err)
	}
	if _, err := r.await(context.Background(), ch); err != nil {
		t.Fatalf("await should drain first submission; got: %v", err)
	}

	done := make(chan error, 1)
	go func() {
		done <- r.submit("spawn-drained", "alice", testSubmission())
	}()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("second submit after await must return error; got nil")
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatal("second submit after await blocked; want quick duplicate-submit error")
	}
}

func TestPendingIntentSubmitRejectsNilSubmission(t *testing.T) {
	tests := []struct {
		name       string
		submission *pendingIntentSubmission
	}{
		{name: "nil submission", submission: nil},
		{name: "nil envelope", submission: &pendingIntentSubmission{}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := newPendingIntentRegistry()
			r.register("spawn-nil", "alice", testPI("spawn-nil"))

			defer func() {
				if rec := recover(); rec != nil {
					t.Fatalf("submit panicked for %s: %v", tt.name, rec)
				}
			}()
			if err := r.submit("spawn-nil", "alice", tt.submission); err == nil {
				t.Fatalf("submit must reject %s; got nil", tt.name)
			}
		})
	}
}

// TestPendingIntentGetNotReady: get returns (nil, false) when no entry exists.
func TestPendingIntentGetNotReady(t *testing.T) {
	r := newPendingIntentRegistry()
	pi, ready := r.get("no-such-spawn")
	if ready || pi != nil {
		t.Fatal("get for unknown spawn must return (nil, false)")
	}
}

// TestPendingIntentGetReady: get returns the PendingIntent and true after register.
func TestPendingIntentGetReady(t *testing.T) {
	r := newPendingIntentRegistry()
	want := testPI("spawn-ready")
	r.register("spawn-ready", "alice", want)

	pi, ready := r.get("spawn-ready")
	if !ready {
		t.Fatal("get must return ready=true after register")
	}
	if pi.GetSpawnId() != "spawn-ready" {
		t.Fatalf("get returned wrong PI: %+v", pi)
	}
}

// TestPendingIntentSubmitNoEntry: submit for a non-registered spawn must return an error.
func TestPendingIntentSubmitNoEntry(t *testing.T) {
	r := newPendingIntentRegistry()
	if err := r.submit("ghost-spawn", "alice", testSubmission()); err == nil {
		t.Fatal("submit for unknown spawn must return error; got nil")
	}
}

// TestPendingIntentCleanup: after cleanup, get returns (nil, false) and submit returns an error.
func TestPendingIntentCleanup(t *testing.T) {
	r := newPendingIntentRegistry()
	r.register("spawn-clean", "alice", testPI("spawn-clean"))
	r.cleanup("spawn-clean")

	if _, ready := r.get("spawn-clean"); ready {
		t.Fatal("get after cleanup must return ready=false")
	}
	if err := r.submit("spawn-clean", "alice", testSubmission()); err == nil {
		t.Fatal("submit after cleanup must return error; got nil")
	}
}

// TestPendingIntentAwaitSuccess: await returns the envelope delivered by submit.
func TestPendingIntentAwaitSuccess(t *testing.T) {
	r := newPendingIntentRegistry()
	r.ttl = 5 * time.Second

	ch := r.register("spawn-ok", "alice", testPI("spawn-ok"))
	env := testEnv()
	// Submit in background so await and submit race as they would in production.
	go func() { _ = r.submit("spawn-ok", "alice", &pendingIntentSubmission{Env: env}) }()

	got, err := r.await(context.Background(), ch)
	if err != nil {
		t.Fatalf("await must succeed; got: %v", err)
	}
	if got.Env.GetAccessToken() != env.GetAccessToken() {
		t.Fatalf("await returned wrong envelope: %+v", got)
	}
}

// TestPendingIntentAwaitCarriesSubmittedSecrets: await returns the exact submission payload
// delivered by SubmitIntent, including owner-sealed secrets destined for StartSpawn.
func TestPendingIntentAwaitCarriesSubmittedSecrets(t *testing.T) {
	r := newPendingIntentRegistry()
	r.ttl = 5 * time.Second

	ch := r.register("spawn-secrets", "alice", testPI("spawn-secrets"))
	env := testEnv()
	secrets := []*nodev1.SealedSecret{{
		TargetPath: "github/workspace/legacy-target",
		Sealed:     []byte("node-sealed-refresh-tuple"),
		SecretId:   "gh-main",
		Type:       nodev1.SecretType_SECRET_TYPE_GITHUB_TOKEN,
		Version:    11,
		DeliveryId: "delivery-spawn-secrets-gen1-gh-main-v11",
		Usages: []nodev1.SecretUsage{
			nodev1.SecretUsage_SECRET_USAGE_NODE_STORAGE,
			nodev1.SecretUsage_SECRET_USAGE_AGENT_RENDER,
		},
		MountNames: []string{"workspace"},
		Render: &nodev1.SecretRenderSpec{
			Profile:              "gh-cli-v1",
			TargetPath:           "github/workspace",
			GhConfigDir:          "github/workspace/gh",
			HostsPath:            "github/workspace/gh/hosts.yml",
			GitConfigPath:        "github/workspace/gitconfig",
			CredentialHelperPath: "github/workspace/git-credential-spawnery",
		},
		GithubToken: &nodev1.GitHubTokenClearMetadata{
			Host:                 "github.com",
			Login:                "alice",
			GithubUserId:         "123456",
			RefreshExpiresAtUnix: 1893456000,
			AppClientId:          "Iv1.spawnerytest",
		},
	}}

	go func() { _ = r.submit("spawn-secrets", "alice", &pendingIntentSubmission{Env: env, Secrets: secrets}) }()

	got, err := r.await(context.Background(), ch)
	if err != nil {
		t.Fatalf("await must succeed; got: %v", err)
	}
	if got.Env.GetAccessToken() != env.GetAccessToken() {
		t.Fatalf("await returned wrong envelope: %+v", got.Env)
	}
	if len(got.Secrets) != 1 {
		t.Fatalf("await returned %d secrets, want 1", len(got.Secrets))
	}
	gotSecret := got.Secrets[0]
	if gotSecret.GetSecretId() != "gh-main" || gotSecret.GetVersion() != 11 || gotSecret.GetDeliveryId() != "delivery-spawn-secrets-gen1-gh-main-v11" {
		t.Fatalf("secret identity not carried through await: %+v", gotSecret)
	}
	if gotSecret.GetType() != nodev1.SecretType_SECRET_TYPE_GITHUB_TOKEN {
		t.Fatalf("secret type=%v want github-token", gotSecret.GetType())
	}
	if len(gotSecret.GetUsages()) != 2 || gotSecret.GetUsages()[1] != nodev1.SecretUsage_SECRET_USAGE_AGENT_RENDER {
		t.Fatalf("secret usages not carried through await: %+v", gotSecret.GetUsages())
	}
	if gotSecret.GetRender().GetCredentialHelperPath() != "github/workspace/git-credential-spawnery" {
		t.Fatalf("secret render routing not carried through await: %+v", gotSecret.GetRender())
	}
	if gotSecret.GetGithubToken().GetHost() != "github.com" || gotSecret.GetGithubToken().GetGithubUserId() != "123456" {
		t.Fatalf("github metadata not carried through await: %+v", gotSecret.GetGithubToken())
	}
}
