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
				s.sched.OnStatus(starts[seen].GetSpawnId(), nodev1.SpawnPhase_ACTIVE)
				seen++
			}
		}
	}()
	return s, sender, func() { close(stopACK) }
}

// goSubmitIntent launches a goroutine that polls GetPendingIntent until ready, then builds
// and submits a SignedIntent. The submitter uses the given session key. Errors land in errCh.
func goSubmitIntent(ctx context.Context, s *Server, spawnID, owner string, sessionKey *ecdsa.PrivateKey, errCh chan<- error) {
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
		}))
		errCh <- err
	}()
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
	if err := s.st.Spawns().SetError(ctx, id); err != nil {
		t.Fatalf("SetError %s: %v", id, err)
	}
}

// assertAuthThreaded verifies at least one StartSpawn for spawnID has a non-nil AuthEnvelope.
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
		return
	}
	t.Fatalf("no StartSpawn for %q found", spawnID)
}

// ---- The four handler tests -----------------------------------------------

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
