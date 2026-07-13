package cp

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	nodev1 "spawnery/gen/node/v1"
	"spawnery/internal/cp/registry"
	"spawnery/internal/cp/skillstore"
	"spawnery/internal/cp/store"
)

// --- sp-mwco.4.3: re-presign over the Attach stream. These tests exercise handleRepresign
// directly (bypassing runNode's dispatch), asserting the security property (§4.3's key-scoping)
// and the ownership/generation/rate-limit gates that guard it. ---

// blockingProvisionSender lets a test hold Provision mid-flight (StartSpawn sent, ACTIVE not yet
// reported) so the scheduler's in-flight placement is observably populated.
type blockingProvisionSender struct {
	mu      sync.Mutex
	sent    []*nodev1.CPMessage
	unblock chan struct{}
	once    sync.Once
}

func newBlockingProvisionSender() *blockingProvisionSender {
	return &blockingProvisionSender{unblock: make(chan struct{})}
}

func (b *blockingProvisionSender) Send(m *nodev1.CPMessage) error {
	b.mu.Lock()
	b.sent = append(b.sent, m)
	b.mu.Unlock()
	<-b.unblock
	return nil
}

func (b *blockingProvisionSender) release() { b.once.Do(func() { close(b.unblock) }) }

// beginInFlightProvision starts s.sched.Provision for spawnID on nodeID/gen in the background and
// blocks until StartSpawn has been sent (i.e. InFlightStart is observably populated), returning a
// func that completes the provision (reports ACTIVE) and waits for Provision to return.
func beginInFlightProvision(t *testing.T, s *Server, reg *registry.Registry, spawnID, nodeID string, gen uint64) (finish func()) {
	t.Helper()
	send := newBlockingProvisionSender()
	reg.Add(&registry.Node{ID: nodeID, Sender: send, Max: 1, Free: 1})

	done := make(chan struct{})
	var provisionErr error
	go func() {
		_, provisionErr = s.sched.Provision(context.Background(), spawnID, "ref", "m", "", "", "", "", gen,
			registry.Placement{}, nil, nil, "", nil, nil, nil)
		close(done)
	}()

	deadline := time.Now().Add(2 * time.Second)
	for {
		send.mu.Lock()
		got := len(send.sent) > 0
		send.mu.Unlock()
		if got {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for Provision to send StartSpawn")
		}
		time.Sleep(time.Millisecond)
	}

	return func() {
		send.release()
		s.sched.OnStatus(spawnID, nodev1.SpawnPhase_ACTIVE, "")
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Fatal("timed out waiting for Provision to return")
		}
		if provisionErr != nil {
			t.Fatalf("Provision returned error: %v", provisionErr)
		}
	}
}

// seedByRefArtifact builds a by-ref store.Artifact whose object is present in fake (so a plain
// presign succeeds), and seeds it into fake so PresignedGet can find it.
func seedByRefArtifact(t *testing.T, fake *skillstore.FakeSkillStore, artifactID, sha string) store.Artifact {
	t.Helper()
	if err := fake.PutIfAbsent(context.Background(), sha, []byte("compressed"), nil); err != nil {
		t.Fatalf("seed fake store: %v", err)
	}
	return store.Artifact{
		ArtifactID:      artifactID,
		ContentType:     2,
		TargetContainer: 1,
		DestPath:        "payloads/" + artifactID,
		ObjectKey:       "skills/" + sha + ".tar.zst",
		ObjectSHA256:    sha,
	}
}

func presignCallCount(fake *skillstore.FakeSkillStore) int {
	n := 0
	for _, c := range fake.Calls() {
		if strings.HasPrefix(c, "presign:") {
			n++
		}
	}
	return n
}

// TestHandleRepresign_HappyPath_InFlight: a spawn with 2 by-ref artifact rows, in-flight start on
// node A at gen g -> a response carrying a fresh URL per requested key.
func TestHandleRepresign_HappyPath_InFlight(t *testing.T) {
	s, reg, _ := newTestServer(t)
	fake := skillstore.NewFakeSkillStore()
	s.skillStore = fake
	makeSpawn(t, s, "sp1", "alice")
	a1 := seedByRefArtifact(t, fake, "a1", strings.Repeat("a", 64))
	a2 := seedByRefArtifact(t, fake, "a2", strings.Repeat("b", 64))
	if err := s.st.Spawns().AddArtifacts(context.Background(), "sp1", []store.Artifact{a1, a2}); err != nil {
		t.Fatalf("AddArtifacts: %v", err)
	}

	finish := beginInFlightProvision(t, s, reg, "sp1", "n1", 5)
	defer finish()

	sender := &capSender{}
	s.handleRepresign(context.Background(), "n1", sender, &nodev1.RepresignArtifactsRequest{
		RequestId:  "req-1",
		SpawnId:    "sp1",
		Generation: 5,
		ObjectKeys: []string{a1.ObjectKey, a2.ObjectKey},
	})

	resp := lastRepresignResponse(sender)
	if resp == nil {
		t.Fatal("no RepresignArtifactsResponse sent")
	}
	if resp.Error != "" {
		t.Fatalf("unexpected refusal: %s", resp.Error)
	}
	if resp.RequestId != "req-1" || resp.SpawnId != "sp1" {
		t.Fatalf("response envelope wrong: %+v", resp)
	}
	got := map[string]string{}
	for _, o := range resp.Objects {
		got[o.ObjectKey] = o.PresignedUrl
	}
	for _, key := range []string{a1.ObjectKey, a2.ObjectKey} {
		if got[key] == "" {
			t.Fatalf("no presigned URL for key %q in %+v", key, resp.Objects)
		}
	}
	if presignCallCount(fake) != 2 {
		t.Fatalf("presign calls = %d, want 2", presignCallCount(fake))
	}
}

// TestHandleRepresign_KeyNotInSpawnSet: the security property (§4.3). A key not bound to the
// spawn's own artifact rows refuses the WHOLE request, names the key, and issues zero presigns.
func TestHandleRepresign_KeyNotInSpawnSet(t *testing.T) {
	s, reg, _ := newTestServer(t)
	fake := skillstore.NewFakeSkillStore()
	s.skillStore = fake
	makeSpawn(t, s, "sp1", "alice")
	a1 := seedByRefArtifact(t, fake, "a1", strings.Repeat("a", 64))
	if err := s.st.Spawns().AddArtifacts(context.Background(), "sp1", []store.Artifact{a1}); err != nil {
		t.Fatalf("AddArtifacts: %v", err)
	}
	// This key is a plausible guess (same content-addressed namespace shape) but was never bound
	// to sp1's own artifact rows.
	foreignKey := "skills/" + strings.Repeat("f", 64) + ".tar.zst"
	if err := fake.PutIfAbsent(context.Background(), strings.Repeat("f", 64), []byte("other spawn's payload"), nil); err != nil {
		t.Fatalf("seed foreign object: %v", err)
	}

	finish := beginInFlightProvision(t, s, reg, "sp1", "n1", 5)
	defer finish()

	sender := &capSender{}
	s.handleRepresign(context.Background(), "n1", sender, &nodev1.RepresignArtifactsRequest{
		RequestId:  "req-1",
		SpawnId:    "sp1",
		Generation: 5,
		ObjectKeys: []string{a1.ObjectKey, foreignKey},
	})

	resp := lastRepresignResponse(sender)
	if resp == nil || resp.Error == "" {
		t.Fatalf("want refusal, got %+v", resp)
	}
	if !strings.Contains(resp.Error, foreignKey) {
		t.Errorf("error %q does not name the offending key %q", resp.Error, foreignKey)
	}
	if len(resp.Objects) != 0 {
		t.Fatalf("want zero objects on refusal, got %+v", resp.Objects)
	}
	if presignCallCount(fake) != 0 {
		t.Fatalf("presign calls = %d, want 0 (whole request must be refused, not partially granted)", presignCallCount(fake))
	}
}

// TestHandleRepresign_WrongGeneration: the in-flight placement is for gen 5; a request naming a
// different generation is refused.
func TestHandleRepresign_WrongGeneration(t *testing.T) {
	s, reg, _ := newTestServer(t)
	fake := skillstore.NewFakeSkillStore()
	s.skillStore = fake
	makeSpawn(t, s, "sp1", "alice")
	a1 := seedByRefArtifact(t, fake, "a1", strings.Repeat("a", 64))
	if err := s.st.Spawns().AddArtifacts(context.Background(), "sp1", []store.Artifact{a1}); err != nil {
		t.Fatalf("AddArtifacts: %v", err)
	}

	finish := beginInFlightProvision(t, s, reg, "sp1", "n1", 5)
	defer finish()

	sender := &capSender{}
	s.handleRepresign(context.Background(), "n1", sender, &nodev1.RepresignArtifactsRequest{
		RequestId: "req-1", SpawnId: "sp1", Generation: 6, ObjectKeys: []string{a1.ObjectKey},
	})

	resp := lastRepresignResponse(sender)
	if resp == nil || resp.Error == "" {
		t.Fatalf("want refusal for a generation mismatch, got %+v", resp)
	}
	if presignCallCount(fake) != 0 {
		t.Fatalf("presign calls = %d, want 0", presignCallCount(fake))
	}
}

// TestHandleRepresign_WrongNode: the in-flight placement is on node B; node A's request is refused.
func TestHandleRepresign_WrongNode(t *testing.T) {
	s, reg, _ := newTestServer(t)
	fake := skillstore.NewFakeSkillStore()
	s.skillStore = fake
	makeSpawn(t, s, "sp1", "alice")
	a1 := seedByRefArtifact(t, fake, "a1", strings.Repeat("a", 64))
	if err := s.st.Spawns().AddArtifacts(context.Background(), "sp1", []store.Artifact{a1}); err != nil {
		t.Fatalf("AddArtifacts: %v", err)
	}

	finish := beginInFlightProvision(t, s, reg, "sp1", "n-B", 5)
	defer finish()

	sender := &capSender{}
	s.handleRepresign(context.Background(), "n-A", sender, &nodev1.RepresignArtifactsRequest{
		RequestId: "req-1", SpawnId: "sp1", Generation: 5, ObjectKeys: []string{a1.ObjectKey},
	})

	resp := lastRepresignResponse(sender)
	if resp == nil || resp.Error == "" {
		t.Fatalf("want refusal from the wrong node, got %+v", resp)
	}
	if presignCallCount(fake) != 0 {
		t.Fatalf("presign calls = %d, want 0", presignCallCount(fake))
	}
}

// TestHandleRepresign_PostActive_BoundLiveContainer: no in-flight placement, but the spawn's live
// container is bound to node A at gen g (the post-ACTIVE path) -> allowed.
func TestHandleRepresign_PostActive_BoundLiveContainer(t *testing.T) {
	s, _, _ := newTestServer(t)
	fake := skillstore.NewFakeSkillStore()
	s.skillStore = fake
	makeSpawn(t, s, "sp1", "alice")
	a1 := seedByRefArtifact(t, fake, "a1", strings.Repeat("a", 64))
	if err := s.st.Spawns().AddArtifacts(context.Background(), "sp1", []store.Artifact{a1}); err != nil {
		t.Fatalf("AddArtifacts: %v", err)
	}
	ctx := context.Background()
	// makeSpawn's Create always inserts the container at generation 1.
	if err := s.st.WithTx(ctx, func(tx store.Store) error { return tx.Spawns().SetActive(ctx, "sp1", "n1", 1) }); err != nil {
		t.Fatalf("SetActive: %v", err)
	}

	sender := &capSender{}
	s.handleRepresign(context.Background(), "n1", sender, &nodev1.RepresignArtifactsRequest{
		RequestId: "req-1", SpawnId: "sp1", Generation: 1, ObjectKeys: []string{a1.ObjectKey},
	})

	resp := lastRepresignResponse(sender)
	if resp == nil {
		t.Fatal("no response sent")
	}
	if resp.Error != "" {
		t.Fatalf("unexpected refusal: %s", resp.Error)
	}
	if len(resp.Objects) != 1 || resp.Objects[0].PresignedUrl == "" {
		t.Fatalf("want one presigned object, got %+v", resp.Objects)
	}
}

// TestHandleRepresign_DeniedSha: a denylisted sha is refused with zero presign calls (sp-mwco.3.2
// real revocation, consulted for free via presignNodeArtifacts's denylist gate).
func TestHandleRepresign_DeniedSha(t *testing.T) {
	s, reg, _ := newTestServer(t)
	fake := skillstore.NewFakeSkillStore()
	s.skillStore = fake
	makeSpawn(t, s, "sp1", "alice")
	sha := strings.Repeat("a", 64)
	a1 := seedByRefArtifact(t, fake, "a1", sha)
	if err := s.st.Spawns().AddArtifacts(context.Background(), "sp1", []store.Artifact{a1}); err != nil {
		t.Fatalf("AddArtifacts: %v", err)
	}
	if err := s.st.SkillDenylist().Deny(context.Background(), store.SkillObjectDenial{
		SHA256: sha, Reason: "malicious payload", DeniedBy: "admin", CreatedAt: 1000,
	}); err != nil {
		t.Fatalf("Deny: %v", err)
	}

	finish := beginInFlightProvision(t, s, reg, "sp1", "n1", 5)
	defer finish()

	sender := &capSender{}
	s.handleRepresign(context.Background(), "n1", sender, &nodev1.RepresignArtifactsRequest{
		RequestId: "req-1", SpawnId: "sp1", Generation: 5, ObjectKeys: []string{a1.ObjectKey},
	})

	resp := lastRepresignResponse(sender)
	if resp == nil || resp.Error == "" {
		t.Fatalf("want refusal for a denylisted sha, got %+v", resp)
	}
	if presignCallCount(fake) != 0 {
		t.Fatalf("presign calls = %d, want 0", presignCallCount(fake))
	}
}

// TestHandleRepresign_TooManyKeys: len(object_keys) > maxRepresignKeys is refused up front.
func TestHandleRepresign_TooManyKeys(t *testing.T) {
	s, reg, _ := newTestServer(t)
	fake := skillstore.NewFakeSkillStore()
	s.skillStore = fake
	makeSpawn(t, s, "sp1", "alice")

	finish := beginInFlightProvision(t, s, reg, "sp1", "n1", 5)
	defer finish()

	keys := make([]string, maxRepresignKeys+1)
	for i := range keys {
		keys[i] = "skills/" + strings.Repeat("a", 64) + ".tar.zst"
	}

	sender := &capSender{}
	s.handleRepresign(context.Background(), "n1", sender, &nodev1.RepresignArtifactsRequest{
		RequestId: "req-1", SpawnId: "sp1", Generation: 5, ObjectKeys: keys,
	})

	resp := lastRepresignResponse(sender)
	if resp == nil || resp.Error == "" {
		t.Fatalf("want refusal for too many object keys, got %+v", resp)
	}
}

// TestHandleRepresign_RateLimit: the 9th request within the window is refused (limit is 8/min).
func TestHandleRepresign_RateLimit(t *testing.T) {
	s, reg, _ := newTestServer(t)
	fake := skillstore.NewFakeSkillStore()
	s.skillStore = fake
	makeSpawn(t, s, "sp1", "alice")
	a1 := seedByRefArtifact(t, fake, "a1", strings.Repeat("a", 64))
	if err := s.st.Spawns().AddArtifacts(context.Background(), "sp1", []store.Artifact{a1}); err != nil {
		t.Fatalf("AddArtifacts: %v", err)
	}

	finish := beginInFlightProvision(t, s, reg, "sp1", "n1", 5)
	defer finish()

	now := time.Unix(1_700_000_000, 0)
	s.represigns.now = func() time.Time { return now }

	for i := 0; i < maxRepresignPerWindow; i++ {
		sender := &capSender{}
		s.handleRepresign(context.Background(), "n1", sender, &nodev1.RepresignArtifactsRequest{
			RequestId: "req", SpawnId: "sp1", Generation: 5, ObjectKeys: []string{a1.ObjectKey},
		})
		resp := lastRepresignResponse(sender)
		if resp == nil || resp.Error != "" {
			t.Fatalf("request %d: want success, got %+v", i+1, resp)
		}
	}

	sender := &capSender{}
	s.handleRepresign(context.Background(), "n1", sender, &nodev1.RepresignArtifactsRequest{
		RequestId: "req", SpawnId: "sp1", Generation: 5, ObjectKeys: []string{a1.ObjectKey},
	})
	resp := lastRepresignResponse(sender)
	if resp == nil || resp.Error == "" {
		t.Fatalf("request %d: want rate-limit refusal, got %+v", maxRepresignPerWindow+1, resp)
	}
}

// TestHandleRepresign_NoResponseLeaksAURL: across every refusal path, resp.Error never contains a
// presigned URL/signature — refusal must never leak a bearer capability.
func TestHandleRepresign_NoResponseLeaksAURL(t *testing.T) {
	s, reg, _ := newTestServer(t)
	fake := skillstore.NewFakeSkillStore()
	s.skillStore = fake
	makeSpawn(t, s, "sp1", "alice")
	a1 := seedByRefArtifact(t, fake, "a1", strings.Repeat("a", 64))
	if err := s.st.Spawns().AddArtifacts(context.Background(), "sp1", []store.Artifact{a1}); err != nil {
		t.Fatalf("AddArtifacts: %v", err)
	}

	finish := beginInFlightProvision(t, s, reg, "sp1", "n1", 5)
	defer finish()

	foreignKey := "skills/" + strings.Repeat("f", 64) + ".tar.zst"
	requests := []*nodev1.RepresignArtifactsRequest{
		{RequestId: "", SpawnId: "sp1", Generation: 5, ObjectKeys: []string{a1.ObjectKey}},  // invalid: no request_id
		{RequestId: "r", SpawnId: "sp1", Generation: 6, ObjectKeys: []string{a1.ObjectKey}}, // wrong gen
		{RequestId: "r", SpawnId: "sp1", Generation: 5, ObjectKeys: []string{foreignKey}},   // key not bound
	}
	for i, req := range requests {
		sender := &capSender{}
		s.handleRepresign(context.Background(), "n1", sender, req)
		resp := lastRepresignResponse(sender)
		if resp == nil || resp.Error == "" {
			t.Fatalf("request %d: want a refusal, got %+v", i, resp)
		}
		if strings.Contains(resp.Error, "X-Amz-Signature") || strings.Contains(resp.Error, "https://") || strings.Contains(resp.Error, "?sig=") {
			t.Fatalf("request %d: refusal error leaks a URL/signature: %q", i, resp.Error)
		}
		if len(resp.Objects) != 0 {
			t.Fatalf("request %d: refusal must carry zero objects, got %+v", i, resp.Objects)
		}
	}
}

func lastRepresignResponse(c *capSender) *nodev1.RepresignArtifactsResponse {
	c.mu.Lock()
	defer c.mu.Unlock()
	for i := len(c.sent) - 1; i >= 0; i-- {
		if r := c.sent[i].GetRepresignArtifactsResponse(); r != nil {
			return r
		}
	}
	return nil
}
