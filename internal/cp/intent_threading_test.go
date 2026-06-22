package cp

// intent_threading_test.go: hermetic tests that every lifecycle handler (Create/Resume/Recreate/Migrate)
// threads a non-nil AuthEnvelope into the StartSpawn sent to the node [AC1][sp-gzvo regression guard].
//
// All four tests follow the same pattern:
//   - devMode=false on the Server (enables the two-phase A4 intent flow).
//   - A background goroutine acts as the "client": polls GetPendingIntent until ready, then
//     builds and submits a signed intent.
//   - The main goroutine drives the lifecycle handler (which blocks until the client submits).
//   - Assertion: the StartSpawn captured by the fake node sender has Auth != nil, with both
//     AccessToken and Intent non-nil.

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"connectrpc.com/connect"

	authv1 "spawnery/gen/auth/v1"
	cpv1 "spawnery/gen/cp/v1"
	nodev1 "spawnery/gen/node/v1"
	"spawnery/internal/cp/auth"
	"spawnery/internal/cp/registry"
	"spawnery/internal/cp/store"
	"spawnery/internal/intent"
)

var intentTestJTI atomic.Int64

func nextTestJTI() string {
	return fmt.Sprintf("test-jti-%d", intentTestJTI.Add(1))
}

// intentTestServer builds a Server with intentEnabled=true (A4 flow active) and a fresh node.
// Returns (server, capSender for the node, stopFn to clean up the ACK goroutine).
func intentTestServer(t *testing.T) (*Server, *capSender, func()) {
	t.Helper()
	s, _, _ := newTestServer(t)
	s.SetIntentEnabled(true)
	// Short TTL so tests fail fast if the signing goroutine hangs.
	s.pendingIntents.ttl = 5 * time.Second

	sender := &capSender{}
	s.reg.Add(&registry.Node{ID: "n-intent", Sender: sender, Max: 10, Free: 10})

	// Background ACK loop: for every StartSpawn the node receives, feed ACTIVE back to the scheduler.
	stopACK := make(chan struct{})
	go func() {
		seen := 0
		for {
			select {
			case <-stopACK:
				return
			case <-time.After(2 * time.Millisecond):
			}
			starts := sender.starts()
			for len(starts) > seen {
				s.sched.OnStatus(starts[seen].GetSpawnId(), nodev1.SpawnPhase_ACTIVE, "")
				seen++
			}
		}
	}()
	return s, sender, func() { close(stopACK) }
}

// goSubmitIntent launches a goroutine that polls GetPendingIntent until ready, then builds
// and submits a SignedIntent. The submitter uses the given session key. Errors land in errCh.
func goSubmitIntent(ctx context.Context, s *Server, spawnID, owner string, sessionKey *ecdsa.PrivateKey, errCh chan<- error) {
	goSubmitIntentWithSecrets(ctx, s, spawnID, owner, sessionKey, nil, errCh)
}

func goSubmitIntentWithSecrets(ctx context.Context, s *Server, spawnID, owner string, sessionKey *ecdsa.PrivateKey, secrets []*cpv1.SealedSecret, errCh chan<- error) {
	goSubmitIntentWithSecretsAfterReady(ctx, s, spawnID, owner, sessionKey, secrets, nil, errCh)
}

func goSubmitIntentWithSecretsAfterReady(ctx context.Context, s *Server, spawnID, owner string, sessionKey *ecdsa.PrivateKey, secrets []*cpv1.SealedSecret, onReady func(context.Context, *cpv1.PendingIntent) error, errCh chan<- error) {
	go func() {
		ownerCtx := auth.WithOwner(ctx, owner)
		var pi *cpv1.PendingIntent
		deadline := time.Now().Add(4 * time.Second)
		for {
			resp, err := s.GetPendingIntent(ownerCtx, connect.NewRequest(&cpv1.GetPendingIntentRequest{SpawnId: spawnID}))
			if err != nil {
				errCh <- fmt.Errorf("GetPendingIntent: %w", err)
				return
			}
			if resp.Msg.Ready {
				pi = resp.Msg.Pending
				if len(secrets) > 0 {
					required := map[string]struct{}{}
					for _, id := range pi.GetAttachedSecretIds() {
						required[id] = struct{}{}
					}
					if len(required) != len(secrets) {
						errCh <- fmt.Errorf("pending intent attached_secret_ids=%+v, want %d submitted secret ids", pi.GetAttachedSecretIds(), len(secrets))
						return
					}
					for _, sec := range secrets {
						if _, ok := required[sec.GetSecretId()]; !ok {
							errCh <- fmt.Errorf("pending intent attached_secret_ids=%+v missing submitted secret %q", pi.GetAttachedSecretIds(), sec.GetSecretId())
							return
						}
					}
				}
				if onReady != nil {
					if err := onReady(ownerCtx, pi); err != nil {
						errCh <- err
						return
					}
				}
				break
			}
			if time.Now().After(deadline) {
				errCh <- fmt.Errorf("GetPendingIntent never became ready")
				return
			}
			time.Sleep(5 * time.Millisecond)
		}

		op := intent.Op(pi.GetOp())
		body := &authv1.IntentBody{
			Jti:          nextTestJTI(),
			IssuedAt:     time.Now().Unix(),
			SpawnId:      pi.GetSpawnId(),
			Generation:   pi.GetGeneration(),
			TargetNodeId: pi.GetTargetNodeId(),
			Op:           string(op),
			AppRef:       pi.GetAppRef(),
			Image:        pi.GetImage(),
			Model:        pi.GetModel(),
			DataRef:      pi.GetDataRef(),
		}
		si, err := intent.Build(op, body, sessionKey)
		if err != nil {
			errCh <- fmt.Errorf("intent.Build: %w", err)
			return
		}
		_, err = s.SubmitIntent(ownerCtx, connect.NewRequest(&cpv1.SubmitIntentRequest{
			SpawnId:         spawnID,
			Intent:          si,
			NodeAccessToken: "fake-node-token-for-test",
			Secrets:         secrets,
		}))
		errCh <- err
	}()
}

func intentThreadingCPSecret() *cpv1.SealedSecret {
	return &cpv1.SealedSecret{
		TargetPath: "github/workspace/legacy-target",
		Sealed:     []byte("cp-sealed-refresh-tuple"),
		SecretId:   "gh-main",
		Type:       cpv1.SecretType_SECRET_TYPE_GITHUB_TOKEN,
		Version:    11,
		DeliveryId: "delivery-sp-intent-secrets-gen1-gh-main-v11",
		Usages: []cpv1.SecretUsage{
			cpv1.SecretUsage_SECRET_USAGE_NODE_STORAGE,
			cpv1.SecretUsage_SECRET_USAGE_AGENT_RENDER,
		},
		MountNames: []string{"workspace"},
		Render: &cpv1.SecretRenderSpec{
			Profile:              "gh-cli-v1",
			TargetPath:           "github/workspace",
			GhConfigDir:          "github/workspace/gh",
			HostsPath:            "github/workspace/gh/hosts.yml",
			GitConfigPath:        "github/workspace/gitconfig",
			CredentialHelperPath: "github/workspace/git-credential-spawnery",
		},
		GithubToken: &cpv1.GitHubTokenClearMetadata{
			Host:                 "github.com",
			Login:                "alice",
			GithubUserId:         "123456",
			RefreshExpiresAtUnix: 1893456000,
			AppClientId:          "Iv1.spawnerytest",
		},
	}
}

func createIntentCatalogSecret(t *testing.T, s *Server, owner, secretID string, target cpv1.ArtifactTarget, destPath, envName string) {
	t.Helper()
	_, err := s.CreateSecret(auth.WithOwner(context.Background(), owner), connect.NewRequest(&cpv1.CreateSecretRequest{
		Secret: &cpv1.SecretWrite{
			SecretId:        secretID,
			Type:            cpv1.UserSecretType_USER_SECRET_TYPE_GENERIC_KV,
			Name:            "Secret " + secretID,
			TargetContainer: target,
			EnvVarName:      envName,
			DestPath:        destPath,
			Envelope:        envelopeBytes(t, owner, secretID, 1),
		},
	}))
	if err != nil {
		t.Fatalf("CreateSecret(%s): %v", secretID, err)
	}
}

func deleteIntentCatalogSecret(t *testing.T, s *Server, owner, secretID string) {
	t.Helper()
	if _, err := s.DeleteSecret(auth.WithOwner(context.Background(), owner), connect.NewRequest(&cpv1.DeleteSecretRequest{SecretId: secretID})); err != nil {
		t.Fatalf("DeleteSecret(%s): %v", secretID, err)
	}
}

func addIntentStartupSecretArtifact(t *testing.T, s *Server, spawnID, secretID string) {
	t.Helper()
	ctx := context.Background()
	err := s.st.WithTx(ctx, func(tx store.Store) error {
		return tx.Spawns().AddArtifacts(ctx, spawnID, []store.Artifact{{
			ArtifactID:      "secret:" + secretID,
			ContentType:     int32(cpv1.ArtifactContentType_ARTIFACT_CONTENT_TYPE_BYTES),
			TargetContainer: int32(cpv1.ArtifactTarget_ARTIFACT_TARGET_AGENT),
			DestPath:        "github/token",
			Mode:            0o600,
			Sensitive:       true,
			EnvVarName:      secretID,
		}})
	})
	if err != nil {
		t.Fatalf("AddArtifacts(%s): %v", spawnID, err)
	}
}

func seedStartingSpawn(t *testing.T, s *Server, id, owner string) {
	t.Helper()
	ctx := context.Background()
	sp := store.Spawn{
		ID: id, OwnerID: owner, AppID: "secret-app", AppVersion: "1.0.0",
		AppRef: "examples/secret-app", Model: "m", Image: "",
		Status: store.Starting, CreatedAt: 1, LastUsedAt: 1,
	}
	if err := s.st.WithTx(ctx, func(tx store.Store) error { return tx.Spawns().Create(ctx, sp, nil) }); err != nil {
		t.Fatalf("seed spawn %s: %v", id, err)
	}
}

// seedSuspendedSpawn inserts a spawn directly into the store in Suspended state.
func seedSuspendedSpawn(t *testing.T, s *Server, id, owner string) {
	t.Helper()
	ctx := context.Background()
	sp := store.Spawn{
		ID: id, OwnerID: owner, AppID: "secret-app", AppVersion: "1.0.0",
		AppRef: "examples/secret-app", Model: "m", Image: "",
		Status: store.Starting, CreatedAt: 1, LastUsedAt: 1,
	}
	if err := s.st.WithTx(ctx, func(tx store.Store) error { return tx.Spawns().Create(ctx, sp, nil) }); err != nil {
		t.Fatalf("seed spawn %s: %v", id, err)
	}
	if err := s.st.Spawns().SetActive(ctx, id, "n-setup", 1); err != nil {
		t.Fatalf("SetActive %s: %v", id, err)
	}
	if err := s.st.WithTx(ctx, func(tx store.Store) error { return tx.Spawns().SetSuspending(ctx, id, 1) }); err != nil {
		t.Fatalf("SetSuspending %s: %v", id, err)
	}
	if err := s.st.WithTx(ctx, func(tx store.Store) error { return tx.Spawns().SetSuspended(ctx, id, 1) }); err != nil {
		t.Fatalf("SetSuspended %s: %v", id, err)
	}
}

// seedErroredSpawn inserts a spawn directly into the store in Errored state.
func seedErroredSpawn(t *testing.T, s *Server, id, owner string) {
	t.Helper()
	ctx := context.Background()
	sp := store.Spawn{
		ID: id, OwnerID: owner, AppID: "secret-app", AppVersion: "1.0.0",
		AppRef: "examples/secret-app", Model: "m", Image: "",
		Status: store.Starting, CreatedAt: 1, LastUsedAt: 1,
	}
	if err := s.st.WithTx(ctx, func(tx store.Store) error { return tx.Spawns().Create(ctx, sp, nil) }); err != nil {
		t.Fatalf("seed spawn %s: %v", id, err)
	}
	if err := s.st.Spawns().SetError(ctx, id, "", ""); err != nil {
		t.Fatalf("SetError %s: %v", id, err)
	}
}

func assertNoStartSpawn(t *testing.T, sender *capSender, spawnID string) {
	t.Helper()
	for _, ss := range sender.starts() {
		if ss.GetSpawnId() == spawnID {
			t.Fatalf("StartSpawn was sent for %s despite startup secret validation failure", spawnID)
		}
	}
}

func waitIntentSpawnStatus(t *testing.T, s *Server, spawnID string, want store.Status) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		sp, _ := s.st.Spawns().Get(context.Background(), spawnID)
		if sp.Status == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("spawn %s never reached %s (status=%v)", spawnID, want, sp.Status)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func deleteSecretAfterPendingReady(s *Server, secretID string) func(context.Context, *cpv1.PendingIntent) error {
	return func(ctx context.Context, _ *cpv1.PendingIntent) error {
		_, err := s.DeleteSecret(ctx, connect.NewRequest(&cpv1.DeleteSecretRequest{SecretId: secretID}))
		if err != nil {
			return fmt.Errorf("delete secret after pending ready: %w", err)
		}
		return nil
	}
}

// assertAuthThreaded verifies at least one StartSpawn for spawnID has a non-nil AuthEnvelope
// and a non-empty AssertedOwner [AC1 scope-3: CP-asserted owner at StartSpawn].
func assertAuthThreaded(t *testing.T, sender *capSender, spawnID string) {
	t.Helper()
	for _, ss := range sender.starts() {
		if ss.GetSpawnId() != spawnID {
			continue
		}
		env := ss.GetAuth()
		if env == nil {
			t.Fatalf("StartSpawn(%s).Auth is nil — AuthEnvelope not threaded", spawnID)
		}
		if env.GetAccessToken() == "" {
			t.Fatalf("StartSpawn(%s).Auth.AccessToken is empty", spawnID)
		}
		if env.GetIntent() == nil {
			t.Fatalf("StartSpawn(%s).Auth.Intent is nil", spawnID)
		}
		if ss.GetAssertedOwner() == "" {
			t.Fatalf("StartSpawn(%s).AssertedOwner is empty — CP-asserted owner not threaded", spawnID)
		}
		return
	}
	t.Fatalf("no StartSpawn for %q found", spawnID)
}

func assertSealedSecretsThreaded(t *testing.T, sender *capSender, spawnID string) {
	t.Helper()
	for _, ss := range sender.starts() {
		if ss.GetSpawnId() != spawnID {
			continue
		}
		if len(ss.GetSecrets()) != 1 {
			t.Fatalf("StartSpawn(%s).Secrets len=%d want 1", spawnID, len(ss.GetSecrets()))
		}
		got := ss.GetSecrets()[0]
		if got.GetSecretId() != "gh-main" || got.GetVersion() != 11 || got.GetDeliveryId() != "delivery-sp-intent-secrets-gen1-gh-main-v11" {
			t.Fatalf("StartSpawn(%s) secret identity = %+v", spawnID, got)
		}
		if got.GetType() != nodev1.SecretType_SECRET_TYPE_GITHUB_TOKEN {
			t.Fatalf("StartSpawn(%s) secret type=%v want github-token", spawnID, got.GetType())
		}
		if len(got.GetUsages()) != 2 || got.GetUsages()[0] != nodev1.SecretUsage_SECRET_USAGE_NODE_STORAGE || got.GetUsages()[1] != nodev1.SecretUsage_SECRET_USAGE_AGENT_RENDER {
			t.Fatalf("StartSpawn(%s) secret usages = %+v", spawnID, got.GetUsages())
		}
		if len(got.GetMountNames()) != 1 || got.GetMountNames()[0] != "workspace" {
			t.Fatalf("StartSpawn(%s) mount names = %+v", spawnID, got.GetMountNames())
		}
		if got.GetRender().GetCredentialHelperPath() != "github/workspace/git-credential-spawnery" {
			t.Fatalf("StartSpawn(%s) render routing = %+v", spawnID, got.GetRender())
		}
		if got.GetGithubToken().GetHost() != "github.com" || got.GetGithubToken().GetGithubUserId() != "123456" {
			t.Fatalf("StartSpawn(%s) github metadata = %+v", spawnID, got.GetGithubToken())
		}
		if string(got.GetSealed()) != "cp-sealed-refresh-tuple" {
			t.Fatalf("StartSpawn(%s) sealed bytes = %q", spawnID, string(got.GetSealed()))
		}
		return
	}
	t.Fatalf("no StartSpawn for %q found", spawnID)
}

// ---- The four handler tests -----------------------------------------------

func TestCreateSpawnAttachedSecretsMintSensitiveArtifacts(t *testing.T) {
	s, _, _ := newTestServer(t)
	createIntentCatalogSecret(t, s, "alice", "gh-main", cpv1.ArtifactTarget_ARTIFACT_TARGET_AGENT, "github/token", "GITHUB_TOKEN")

	ctx := auth.WithOwner(context.Background(), "alice")
	resp, err := s.CreateSpawn(ctx, connect.NewRequest(&cpv1.CreateSpawnRequest{
		AppId:             "secret-app",
		Model:             "m",
		AttachedSecretIds: []string{"gh-main"},
	}))
	if err != nil {
		t.Fatal(err)
	}
	arts, err := s.st.Spawns().GetArtifacts(context.Background(), resp.Msg.GetSpawnId())
	if err != nil {
		t.Fatal(err)
	}
	if len(arts) != 1 {
		t.Fatalf("artifacts len=%d want 1: %+v", len(arts), arts)
	}
	got := arts[0]
	if got.ArtifactID != "secret:gh-main" || !got.Sensitive || len(got.Inline) != 0 {
		t.Fatalf("attached artifact identity/sensitivity = %+v", got)
	}
	if got.EnvVarName != "gh-main" {
		t.Fatalf("delivery key EnvVarName=%q want secret id gh-main", got.EnvVarName)
	}
	if got.DestPath != "github/token" || got.TargetContainer != int32(cpv1.ArtifactTarget_ARTIFACT_TARGET_AGENT) {
		t.Fatalf("routing artifact = %+v", got)
	}
}

func TestCreateSpawnAttachedSecretMissingFailsClosed(t *testing.T) {
	s, _, _ := newTestServer(t)
	ctx := auth.WithOwner(context.Background(), "alice")
	_, err := s.CreateSpawn(ctx, connect.NewRequest(&cpv1.CreateSpawnRequest{
		AppId:             "secret-app",
		Model:             "m",
		AttachedSecretIds: []string{"missing"},
	}))
	if connect.CodeOf(err) != connect.CodeNotFound {
		t.Fatalf("CreateSpawn missing secret code=%v err=%v, want NotFound", connect.CodeOf(err), err)
	}
}

func TestIntentThreadedCreateSpawn(t *testing.T) {
	s, sender, stopACK := intentTestServer(t)
	defer stopACK()

	sessionKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	errCh := make(chan error, 1)

	// CreateSpawn returns immediately (provisionSpawn is async). We submit the intent
	// concurrently; goSubmitIntent polls GetPendingIntent until the goroutine registers it.
	ctx := auth.WithOwner(context.Background(), "alice")
	resp, err := s.CreateSpawn(ctx, connect.NewRequest(&cpv1.CreateSpawnRequest{
		AppId: "secret-app",
		Model: "m",
	}))
	if err != nil {
		t.Fatal(err)
	}
	spawnID := resp.Msg.SpawnId

	goSubmitIntent(context.Background(), s, spawnID, "alice", sessionKey, errCh)

	// Wait for the spawn to reach Active (provisionSpawn completed).
	deadline := time.Now().Add(5 * time.Second)
	for {
		sp, _ := s.st.Spawns().Get(context.Background(), spawnID)
		if sp.Status == store.Active {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("spawn never became active (status=%v)", sp.Status)
		}
		time.Sleep(5 * time.Millisecond)
	}

	if submitErr := <-errCh; submitErr != nil {
		t.Fatalf("SubmitIntent error: %v", submitErr)
	}
	assertAuthThreaded(t, sender, spawnID)
}

func TestIntentThreadedSealedSecretsReachStartSpawn(t *testing.T) {
	s, sender, stopACK := intentTestServer(t)
	defer stopACK()
	createIntentCatalogSecret(t, s, "alice", "gh-main", cpv1.ArtifactTarget_ARTIFACT_TARGET_AGENT, "github/token", "GITHUB_TOKEN")

	sessionKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	errCh := make(chan error, 1)

	ctx := auth.WithOwner(context.Background(), "alice")
	resp, err := s.CreateSpawn(ctx, connect.NewRequest(&cpv1.CreateSpawnRequest{
		AppId:             "secret-app",
		Model:             "m",
		AttachedSecretIds: []string{"gh-main"},
	}))
	if err != nil {
		t.Fatal(err)
	}
	spawnID := resp.Msg.SpawnId

	goSubmitIntentWithSecrets(context.Background(), s, spawnID, "alice", sessionKey, []*cpv1.SealedSecret{intentThreadingCPSecret()}, errCh)

	deadline := time.Now().Add(5 * time.Second)
	for {
		sp, _ := s.st.Spawns().Get(context.Background(), spawnID)
		if sp.Status == store.Active {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("spawn never became active (status=%v)", sp.Status)
		}
		time.Sleep(5 * time.Millisecond)
	}

	if submitErr := <-errCh; submitErr != nil {
		t.Fatalf("SubmitIntent error: %v", submitErr)
	}
	assertAuthThreaded(t, sender, spawnID)
	assertSealedSecretsThreaded(t, sender, spawnID)
}

func TestIntentThreadedCreateSpawnRejectsMissingAttachedSecretSubmission(t *testing.T) {
	s, sender, stopACK := intentTestServer(t)
	defer stopACK()
	createIntentCatalogSecret(t, s, "alice", "gh-main", cpv1.ArtifactTarget_ARTIFACT_TARGET_AGENT, "github/token", "GITHUB_TOKEN")

	sessionKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	errCh := make(chan error, 1)

	ctx := auth.WithOwner(context.Background(), "alice")
	resp, err := s.CreateSpawn(ctx, connect.NewRequest(&cpv1.CreateSpawnRequest{
		AppId:             "secret-app",
		Model:             "m",
		AttachedSecretIds: []string{"gh-main"},
	}))
	if err != nil {
		t.Fatal(err)
	}
	spawnID := resp.Msg.GetSpawnId()

	goSubmitIntentWithSecrets(context.Background(), s, spawnID, "alice", sessionKey, nil, errCh)
	if submitErr := <-errCh; submitErr != nil {
		t.Fatalf("SubmitIntent should accept the envelope and let provision validation fail: %v", submitErr)
	}

	deadline := time.Now().Add(5 * time.Second)
	for {
		sp, _ := s.st.Spawns().Get(context.Background(), spawnID)
		if sp.Status == store.Errored {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("spawn never became Errored (status=%v)", sp.Status)
		}
		time.Sleep(5 * time.Millisecond)
	}
	assertNoStartSpawn(t, sender, spawnID)
}

func TestIntentThreadedCreateProvisionRejectsDeletedAttachedSecret(t *testing.T) {
	s, sender, stopACK := intentTestServer(t)
	defer stopACK()
	createIntentCatalogSecret(t, s, "alice", "gh-main", cpv1.ArtifactTarget_ARTIFACT_TARGET_AGENT, "github/token", "GITHUB_TOKEN")

	const spawnID = "sp-create-deleted-secret"
	seedStartingSpawn(t, s, spawnID, "alice")
	addIntentStartupSecretArtifact(t, s, spawnID, "gh-main")
	deleteIntentCatalogSecret(t, s, "alice", "gh-main")

	s.provisionSpawn(context.Background(), spawnID, "alice", "examples/secret-app", "m", registry.Placement{})

	sp, err := s.st.Spawns().Get(context.Background(), spawnID)
	if err != nil {
		t.Fatal(err)
	}
	if sp.Status != store.Errored {
		t.Fatalf("status=%v want Errored after deleted attached secret before create provision", sp.Status)
	}
	assertNoStartSpawn(t, sender, spawnID)
}

func TestIntentThreadedCreateSpawnRejectsDeletedAttachedSecretAfterPendingReady(t *testing.T) {
	s, sender, stopACK := intentTestServer(t)
	defer stopACK()
	createIntentCatalogSecret(t, s, "alice", "gh-main", cpv1.ArtifactTarget_ARTIFACT_TARGET_AGENT, "github/token", "GITHUB_TOKEN")

	sessionKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	errCh := make(chan error, 1)
	ctx := auth.WithOwner(context.Background(), "alice")
	resp, err := s.CreateSpawn(ctx, connect.NewRequest(&cpv1.CreateSpawnRequest{
		AppId:             "secret-app",
		Model:             "m",
		AttachedSecretIds: []string{"gh-main"},
	}))
	if err != nil {
		t.Fatal(err)
	}
	spawnID := resp.Msg.GetSpawnId()

	goSubmitIntentWithSecretsAfterReady(context.Background(), s, spawnID, "alice", sessionKey, []*cpv1.SealedSecret{intentThreadingCPSecret()}, deleteSecretAfterPendingReady(s, "gh-main"), errCh)
	if submitErr := <-errCh; submitErr != nil {
		t.Fatalf("SubmitIntent should accept the envelope and let provision catalog recheck fail: %v", submitErr)
	}
	waitIntentSpawnStatus(t, s, spawnID, store.Errored)
	assertNoStartSpawn(t, sender, spawnID)
}

func TestIntentThreadedRestartRejectsDeletedAttachedSecret(t *testing.T) {
	for _, tc := range []struct {
		name    string
		spawnID string
		seed    func(*testing.T, *Server, string, string)
		call    func(context.Context, *Server, string) error
	}{
		{
			name:    "resume",
			spawnID: "sp-resume-deleted-secret",
			seed:    seedSuspendedSpawn,
			call: func(ctx context.Context, s *Server, spawnID string) error {
				_, err := s.ResumeSpawn(ctx, connect.NewRequest(&cpv1.ResumeSpawnRequest{SpawnId: spawnID}))
				return err
			},
		},
		{
			name:    "recreate",
			spawnID: "sp-recreate-deleted-secret",
			seed:    seedErroredSpawn,
			call: func(ctx context.Context, s *Server, spawnID string) error {
				_, err := s.RecreateSpawn(ctx, connect.NewRequest(&cpv1.RecreateSpawnRequest{SpawnId: spawnID}))
				return err
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, sender, stopACK := intentTestServer(t)
			defer stopACK()

			createIntentCatalogSecret(t, s, "alice", "gh-main", cpv1.ArtifactTarget_ARTIFACT_TARGET_AGENT, "github/token", "GITHUB_TOKEN")
			tc.seed(t, s, tc.spawnID, "alice")
			addIntentStartupSecretArtifact(t, s, tc.spawnID, "gh-main")
			deleteIntentCatalogSecret(t, s, "alice", "gh-main")

			ownerCtx := auth.WithOwner(context.Background(), "alice")
			if err := tc.call(ownerCtx, s, tc.spawnID); connect.CodeOf(err) != connect.CodeNotFound {
				t.Fatalf("%s deleted attached secret code=%v err=%v, want NotFound", tc.name, connect.CodeOf(err), err)
			}
			sp, err := s.st.Spawns().Get(context.Background(), tc.spawnID)
			if err != nil {
				t.Fatal(err)
			}
			if sp.Status != store.Errored {
				t.Fatalf("%s status=%v want Errored after deleted attached secret", tc.name, sp.Status)
			}
			assertNoStartSpawn(t, sender, tc.spawnID)
		})
	}
}

func TestIntentThreadedRestartRejectsDeletedAttachedSecretAfterPendingReady(t *testing.T) {
	for _, tc := range []struct {
		name    string
		spawnID string
		seed    func(*testing.T, *Server, string, string)
		call    func(context.Context, *Server, string) error
	}{
		{
			name:    "resume",
			spawnID: "sp-resume-delete-after-pending",
			seed:    seedSuspendedSpawn,
			call: func(ctx context.Context, s *Server, spawnID string) error {
				_, err := s.ResumeSpawn(ctx, connect.NewRequest(&cpv1.ResumeSpawnRequest{SpawnId: spawnID}))
				return err
			},
		},
		{
			name:    "recreate",
			spawnID: "sp-recreate-delete-after-pending",
			seed:    seedErroredSpawn,
			call: func(ctx context.Context, s *Server, spawnID string) error {
				_, err := s.RecreateSpawn(ctx, connect.NewRequest(&cpv1.RecreateSpawnRequest{SpawnId: spawnID}))
				return err
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, sender, stopACK := intentTestServer(t)
			defer stopACK()
			createIntentCatalogSecret(t, s, "alice", "gh-main", cpv1.ArtifactTarget_ARTIFACT_TARGET_AGENT, "github/token", "GITHUB_TOKEN")
			tc.seed(t, s, tc.spawnID, "alice")
			addIntentStartupSecretArtifact(t, s, tc.spawnID, "gh-main")

			sessionKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
			errCh := make(chan error, 1)
			goSubmitIntentWithSecretsAfterReady(context.Background(), s, tc.spawnID, "alice", sessionKey, []*cpv1.SealedSecret{intentThreadingCPSecret()}, deleteSecretAfterPendingReady(s, "gh-main"), errCh)

			ownerCtx := auth.WithOwner(context.Background(), "alice")
			if err := tc.call(ownerCtx, s, tc.spawnID); connect.CodeOf(err) != connect.CodeNotFound {
				t.Fatalf("%s deleted attached secret after pending code=%v err=%v, want NotFound", tc.name, connect.CodeOf(err), err)
			}
			if submitErr := <-errCh; submitErr != nil {
				t.Fatalf("SubmitIntent should accept the envelope and let provision catalog recheck fail: %v", submitErr)
			}
			sp, err := s.st.Spawns().Get(context.Background(), tc.spawnID)
			if err != nil {
				t.Fatal(err)
			}
			if sp.Status != store.Errored {
				t.Fatalf("%s status=%v want Errored after deleted attached secret", tc.name, sp.Status)
			}
			assertNoStartSpawn(t, sender, tc.spawnID)
		})
	}
}

func TestIntentThreadedRestartRejectsEmptyAttachedSecretPayload(t *testing.T) {
	for _, tc := range []struct {
		name    string
		spawnID string
		seed    func(*testing.T, *Server, string, string)
		call    func(context.Context, *Server, string) error
	}{
		{
			name:    "resume",
			spawnID: "sp-resume-empty-secret",
			seed:    seedSuspendedSpawn,
			call: func(ctx context.Context, s *Server, spawnID string) error {
				_, err := s.ResumeSpawn(ctx, connect.NewRequest(&cpv1.ResumeSpawnRequest{SpawnId: spawnID}))
				return err
			},
		},
		{
			name:    "recreate",
			spawnID: "sp-recreate-empty-secret",
			seed:    seedErroredSpawn,
			call: func(ctx context.Context, s *Server, spawnID string) error {
				_, err := s.RecreateSpawn(ctx, connect.NewRequest(&cpv1.RecreateSpawnRequest{SpawnId: spawnID}))
				return err
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, sender, stopACK := intentTestServer(t)
			defer stopACK()
			createIntentCatalogSecret(t, s, "alice", "gh-main", cpv1.ArtifactTarget_ARTIFACT_TARGET_AGENT, "github/token", "GITHUB_TOKEN")
			tc.seed(t, s, tc.spawnID, "alice")
			addIntentStartupSecretArtifact(t, s, tc.spawnID, "gh-main")

			sessionKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
			errCh := make(chan error, 1)
			emptyPayload := intentThreadingCPSecret()
			emptyPayload.Sealed = nil
			goSubmitIntentWithSecrets(context.Background(), s, tc.spawnID, "alice", sessionKey, []*cpv1.SealedSecret{emptyPayload}, errCh)

			ownerCtx := auth.WithOwner(context.Background(), "alice")
			if err := tc.call(ownerCtx, s, tc.spawnID); connect.CodeOf(err) != connect.CodeFailedPrecondition {
				t.Fatalf("%s empty attached secret payload code=%v err=%v, want FailedPrecondition", tc.name, connect.CodeOf(err), err)
			}
			if submitErr := <-errCh; submitErr != nil {
				t.Fatalf("SubmitIntent should accept the envelope and let provision validation fail: %v", submitErr)
			}
			sp, err := s.st.Spawns().Get(context.Background(), tc.spawnID)
			if err != nil {
				t.Fatal(err)
			}
			if sp.Status != store.Errored {
				t.Fatalf("%s status=%v want Errored after empty attached secret payload", tc.name, sp.Status)
			}
			assertNoStartSpawn(t, sender, tc.spawnID)
		})
	}
}

func TestIntentThreadedResumeSpawn(t *testing.T) {
	s, sender, stopACK := intentTestServer(t)
	defer stopACK()

	seedSuspendedSpawn(t, s, "sp-resume", "alice")

	sessionKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	errCh := make(chan error, 1)

	// ResumeSpawn BLOCKS until the intent is submitted. Start the submitter first.
	goSubmitIntent(context.Background(), s, "sp-resume", "alice", sessionKey, errCh)

	ownerCtx := auth.WithOwner(context.Background(), "alice")
	if _, err := s.ResumeSpawn(ownerCtx, connect.NewRequest(&cpv1.ResumeSpawnRequest{SpawnId: "sp-resume"})); err != nil {
		t.Fatalf("ResumeSpawn: %v", err)
	}

	if submitErr := <-errCh; submitErr != nil {
		t.Fatalf("SubmitIntent error: %v", submitErr)
	}
	assertAuthThreaded(t, sender, "sp-resume")
}

func TestIntentThreadedResumeSpawnAttachesRequestedSecrets(t *testing.T) {
	s, sender, stopACK := intentTestServer(t)
	defer stopACK()
	createIntentCatalogSecret(t, s, "alice", "gh-main", cpv1.ArtifactTarget_ARTIFACT_TARGET_AGENT, "github/token", "GITHUB_TOKEN")
	seedSuspendedSpawn(t, s, "sp-resume-attach", "alice")

	sessionKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	errCh := make(chan error, 1)
	goSubmitIntentWithSecrets(context.Background(), s, "sp-resume-attach", "alice", sessionKey, []*cpv1.SealedSecret{intentThreadingCPSecret()}, errCh)

	ownerCtx := auth.WithOwner(context.Background(), "alice")
	if _, err := s.ResumeSpawn(ownerCtx, connect.NewRequest(&cpv1.ResumeSpawnRequest{
		SpawnId:           "sp-resume-attach",
		AttachedSecretIds: []string{"gh-main"},
	})); err != nil {
		t.Fatalf("ResumeSpawn: %v", err)
	}
	if submitErr := <-errCh; submitErr != nil {
		t.Fatalf("SubmitIntent error: %v", submitErr)
	}
	arts, err := s.st.Spawns().GetArtifacts(context.Background(), "sp-resume-attach")
	if err != nil {
		t.Fatal(err)
	}
	if len(arts) != 1 || arts[0].ArtifactID != "secret:gh-main" || !arts[0].Sensitive || arts[0].EnvVarName != "gh-main" {
		t.Fatalf("resume attached artifacts = %+v", arts)
	}
	assertAuthThreaded(t, sender, "sp-resume-attach")
	assertSealedSecretsThreaded(t, sender, "sp-resume-attach")
}

func TestResumeSpawnAttachedSecretMissingFailsClosed(t *testing.T) {
	s, _, _ := newTestServer(t)
	s.SetIntentEnabled(true)
	seedSuspendedSpawn(t, s, "sp-resume-missing-secret", "alice")

	ownerCtx := auth.WithOwner(context.Background(), "alice")
	_, err := s.ResumeSpawn(ownerCtx, connect.NewRequest(&cpv1.ResumeSpawnRequest{
		SpawnId:           "sp-resume-missing-secret",
		AttachedSecretIds: []string{"missing"},
	}))
	if connect.CodeOf(err) != connect.CodeNotFound {
		t.Fatalf("ResumeSpawn missing attached secret code=%v err=%v, want NotFound", connect.CodeOf(err), err)
	}
	sp, err := s.st.Spawns().Get(context.Background(), "sp-resume-missing-secret")
	if err != nil {
		t.Fatal(err)
	}
	if sp.Status != store.Suspended {
		t.Fatalf("status=%v want Suspended after missing resume attached secret", sp.Status)
	}
}

func TestIntentThreadedRecreateSpawn(t *testing.T) {
	s, sender, stopACK := intentTestServer(t)
	defer stopACK()

	seedErroredSpawn(t, s, "sp-recreate", "alice")

	sessionKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	errCh := make(chan error, 1)

	goSubmitIntent(context.Background(), s, "sp-recreate", "alice", sessionKey, errCh)

	ownerCtx := auth.WithOwner(context.Background(), "alice")
	if _, err := s.RecreateSpawn(ownerCtx, connect.NewRequest(&cpv1.RecreateSpawnRequest{SpawnId: "sp-recreate"})); err != nil {
		t.Fatalf("RecreateSpawn: %v", err)
	}

	if submitErr := <-errCh; submitErr != nil {
		t.Fatalf("SubmitIntent error: %v", submitErr)
	}
	assertAuthThreaded(t, sender, "sp-recreate")
}

func TestIntentThreadedMigrateSpawn(t *testing.T) {
	s, sender, stopACK := intentTestServer(t)
	defer stopACK()

	// Add a second node as the migration target.
	s.reg.Add(&registry.Node{ID: "n-intent2", Sender: sender, Max: 10, Free: 10})

	seedSuspendedSpawn(t, s, "sp-migrate", "alice")

	sessionKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	errCh := make(chan error, 1)

	goSubmitIntent(context.Background(), s, "sp-migrate", "alice", sessionKey, errCh)

	ownerCtx := auth.WithOwner(context.Background(), "alice")
	if _, err := s.MigrateSpawn(ownerCtx, connect.NewRequest(&cpv1.MigrateSpawnRequest{
		SpawnId:      "sp-migrate",
		TargetNodeId: "n-intent2",
	})); err != nil {
		t.Fatalf("MigrateSpawn: %v", err)
	}

	if submitErr := <-errCh; submitErr != nil {
		t.Fatalf("SubmitIntent error: %v", submitErr)
	}
	assertAuthThreaded(t, sender, "sp-migrate")
}
