package cp

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"

	cpv1 "spawnery/gen/cp/v1"
	nodev1 "spawnery/gen/node/v1"
	"spawnery/internal/cp/auth"
	"spawnery/internal/cp/registry"
	"spawnery/internal/cp/router"
	"spawnery/internal/cp/store"
	"spawnery/internal/intent"
)

type recordingForkMaterializer struct {
	nodeID     string
	rootfsPins []store.RootfsArtifactPin
	err        error
	calls      []forkMaterializeRequest
}

func (r *recordingForkMaterializer) MaterializeFork(ctx context.Context, req forkMaterializeRequest) (forkMaterializeResult, error) {
	_ = ctx
	r.calls = append(r.calls, req)
	if r.err != nil {
		return forkMaterializeResult{}, r.err
	}
	return forkMaterializeResult{NodeID: r.nodeID, RootfsPins: r.rootfsPins}, nil
}

func (r *recordingForkMaterializer) WaitForForkTurnBoundary(context.Context, forkMaterializeRequest) error {
	return nil
}

type forkMaterializerFunc func(context.Context, forkMaterializeRequest) (forkMaterializeResult, error)

func (f forkMaterializerFunc) MaterializeFork(ctx context.Context, req forkMaterializeRequest) (forkMaterializeResult, error) {
	return f(ctx, req)
}

func (f forkMaterializerFunc) WaitForForkTurnBoundary(context.Context, forkMaterializeRequest) error {
	return nil
}

type staticForkFootprint int64

func (s staticForkFootprint) ForkFootprintBytes(context.Context, store.Spawn, store.Container) (int64, error) {
	return int64(s), nil
}

type sequenceForkFootprint struct {
	values []int64
	hook   func(call int)
	calls  int
}

func (s *sequenceForkFootprint) ForkFootprintBytes(context.Context, store.Spawn, store.Container) (int64, error) {
	s.calls++
	if s.hook != nil {
		s.hook(s.calls)
	}
	if s.calls <= len(s.values) {
		return s.values[s.calls-1], nil
	}
	return s.values[len(s.values)-1], nil
}

func seedForkSource(t *testing.T, s *Server, reg *registry.Registry, rt *router.Router, id, owner, nodeID string, sender registry.NodeSender) {
	t.Helper()
	ctx := context.Background()
	if err := s.st.Owners().Upsert(ctx, store.Owner{ID: owner, CreatedAt: 1}); err != nil {
		t.Fatalf("seed owner %s: %v", owner, err)
	}
	sp := store.Spawn{
		ID: id, OwnerID: owner, Name: "source", AppID: "secret-app", AppVersion: "1.0.0",
		AppRef: "examples/secret-app", Model: "model-a", Image: "img:agent", RunnableID: "goose-acp",
		Mode: "acp", BaseImageDigest: "sha256:base", Status: store.Starting, CreatedAt: 1, LastUsedAt: 1,
	}
	if err := s.st.WithTx(ctx, func(tx store.Store) error {
		if err := tx.Spawns().Create(ctx, sp, []store.Mount{{Name: "main", BackendURI: "scratch"}}); err != nil {
			return err
		}
		return tx.Spawns().AddArtifacts(ctx, id, []store.Artifact{{
			ArtifactID: "artifact-1", Inline: []byte("hello"), ContentType: 1,
			TargetContainer: 1, DestPath: "/tmp/artifact", Mode: 0o644,
		}})
	}); err != nil {
		t.Fatalf("seed spawn %s: %v", id, err)
	}
	if err := s.st.WithTx(ctx, func(tx store.Store) error {
		return tx.Spawns().SetActive(ctx, id, nodeID, 1)
	}); err != nil {
		t.Fatalf("SetActive: %v", err)
	}
	nodeSender := sender
	if sender != nil {
		nodeSender = autoUnpauseSender{s: s, inner: sender}
	}
	reg.Add(&registry.Node{
		ID: nodeID, Sender: nodeSender, Max: 4, Free: 4, Class: "cloud",
		Images: []string{"img:agent"}, DiskFreeBytes: 1_000_000, DiskTotalBytes: 2_000_000,
	})
	rt.Bind(id, nodeID, nodeSender)
}

type autoUnpauseSender struct {
	s     *Server
	inner registry.NodeSender
}

func (a autoUnpauseSender) Send(m *nodev1.CPMessage) error {
	if err := a.inner.Send(m); err != nil {
		return err
	}
	if cmd := m.GetUnpauseIfPaused(); cmd != nil {
		a.s.deliverUnpauseIfPausedComplete(&nodev1.UnpauseIfPausedComplete{
			SpawnId:    cmd.GetSpawnId(),
			Generation: cmd.GetGeneration(),
		})
	}
	return nil
}

type turnBoundaryHookSender struct {
	capSender
	s    *Server
	hook func(*nodev1.ForkTurnBoundary)
}

func (h *turnBoundaryHookSender) Send(m *nodev1.CPMessage) error {
	if err := h.capSender.Send(m); err != nil {
		return err
	}
	if cmd := m.GetForkTurnBoundary(); cmd != nil {
		if h.hook != nil {
			h.hook(cmd)
		}
		h.s.deliverForkTurnBoundaryComplete(&nodev1.ForkTurnBoundaryComplete{
			SourceSpawnId:    cmd.GetSourceSpawnId(),
			SourceGeneration: cmd.GetSourceGeneration(),
			TransferSetId:    cmd.GetTransferSetId(),
		})
	}
	return nil
}

type blockingStartSender struct {
	capSender
	s       *Server
	reached chan *nodev1.StartSpawn
	unblock chan struct{}
}

func (b *blockingStartSender) Send(m *nodev1.CPMessage) error {
	if err := b.capSender.Send(m); err != nil {
		return err
	}
	if st := m.GetStart(); st != nil {
		select {
		case b.reached <- st:
		default:
		}
		<-b.unblock
		b.s.sched.OnStatus(st.GetSpawnId(), nodev1.SpawnPhase_ACTIVE, "")
	}
	return nil
}

type blockingSameNodeForkSender struct {
	capSender
	s              *Server
	sourceRestored chan struct{}
	completeFork   chan struct{}
	finalDelivered chan bool
}

func (b *blockingSameNodeForkSender) Send(m *nodev1.CPMessage) error {
	if err := b.capSender.Send(m); err != nil {
		return err
	}
	if cmd := m.GetForkTurnBoundary(); cmd != nil {
		b.s.deliverForkTurnBoundaryComplete(&nodev1.ForkTurnBoundaryComplete{
			SourceSpawnId:    cmd.GetSourceSpawnId(),
			SourceGeneration: cmd.GetSourceGeneration(),
			TransferSetId:    cmd.GetTransferSetId(),
		})
	}
	if cmd := m.GetForkSameNode(); cmd != nil {
		b.s.deliverForkSourceRestored(&nodev1.ForkSourceRestored{
			SourceSpawnId:    cmd.GetSourceSpawnId(),
			SourceGeneration: cmd.GetSourceGeneration(),
			TransferSetId:    cmd.GetTransferSetId(),
		})
		close(b.sourceRestored)
		go func() {
			<-b.completeFork
			delivered := b.s.deliverForkSameNodeComplete(&nodev1.ForkSameNodeComplete{
				SourceSpawnId: cmd.GetSourceSpawnId(),
				ForkSpawnId:   cmd.GetForkSpawnId(),
				TransferSetId: cmd.GetTransferSetId(),
				NodeId:        "node-1",
				Mounts:        []*nodev1.MountMarker{{Name: "main", Marker: "fork-main"}},
			})
			select {
			case b.finalDelivered <- delivered:
			default:
			}
		}()
	}
	return nil
}

type blockingTurnBoundarySender struct {
	capSender
	s       *Server
	reached chan *nodev1.ForkTurnBoundary
	unblock chan struct{}
}

func (b *blockingTurnBoundarySender) Send(m *nodev1.CPMessage) error {
	if err := b.capSender.Send(m); err != nil {
		return err
	}
	if cmd := m.GetForkTurnBoundary(); cmd != nil {
		select {
		case b.reached <- cmd:
		default:
		}
		<-b.unblock
		b.s.deliverForkTurnBoundaryComplete(&nodev1.ForkTurnBoundaryComplete{
			SourceSpawnId:    cmd.GetSourceSpawnId(),
			SourceGeneration: cmd.GetSourceGeneration(),
			TransferSetId:    cmd.GetTransferSetId(),
		})
	}
	return nil
}

type blockingBoundaryMaterializer struct {
	nodeID  string
	reached chan forkMaterializeRequest
	unblock chan struct{}
}

func (b *blockingBoundaryMaterializer) WaitForForkTurnBoundary(ctx context.Context, req forkMaterializeRequest) error {
	select {
	case b.reached <- req:
	default:
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-b.unblock:
		return nil
	}
}

func (b *blockingBoundaryMaterializer) MaterializeFork(context.Context, forkMaterializeRequest) (forkMaterializeResult, error) {
	return forkMaterializeResult{NodeID: b.nodeID}, nil
}

func sawUnpause(sender *capSender, spawnID string) bool {
	sender.mu.Lock()
	defer sender.mu.Unlock()
	for _, msg := range sender.sent {
		if cmd := msg.GetUnpauseIfPaused(); cmd != nil && cmd.GetSpawnId() == spawnID {
			return true
		}
	}
	return false
}

func sawForkSameNode(sender *capSender) bool {
	sender.mu.Lock()
	defer sender.mu.Unlock()
	for _, msg := range sender.sent {
		if msg.GetForkSameNode() != nil {
			return true
		}
	}
	return false
}

func TestForkSpawnMintsChildWithLineageAndSourceStaysActive(t *testing.T) {
	s, reg, rt := newTestServer(t)
	s.now = func() time.Time { return time.Unix(1234, 0) }
	seedForkSource(t, s, reg, rt, "sp-source", "alice", "node-1", &capSender{})
	targetSender := &capSender{}
	stopAck := goAckStarts(s, targetSender)
	defer stopAck()
	reg.Add(&registry.Node{
		ID: "node-2", Sender: targetSender, Max: 4, Free: 4, Class: "cloud",
		Images: []string{"img:agent"}, DiskFreeBytes: 1_000_000, DiskTotalBytes: 2_000_000,
	})
	mat := &recordingForkMaterializer{nodeID: "node-2", rootfsPins: []store.RootfsArtifactPin{{
		ArtifactID:      "rootfs-fork-gen1",
		Generation:      1,
		BaseImageDigest: "sha256:base",
		Format:          "oci_layout",
	}}}
	s.forkMaterializer = mat
	s.forkFootprintEstimator = staticForkFootprint(100)

	ctx := auth.WithOwner(context.Background(), "alice")
	resp, err := s.ForkSpawn(ctx, connect.NewRequest(&cpv1.ForkSpawnRequest{
		SpawnId: "sp-source", TargetNodeId: "node-2", Name: " child ",
	}))
	if err != nil {
		t.Fatalf("ForkSpawn: %v", err)
	}
	if resp.Msg.ForkSpawnId == "" || resp.Msg.TransferSetId == "" || resp.Msg.NodeId != "node-2" {
		t.Fatalf("response = %+v", resp.Msg)
	}

	source, err := s.st.Spawns().Get(ctx, "sp-source")
	if err != nil {
		t.Fatalf("Get source: %v", err)
	}
	if source.Status != store.Active {
		t.Fatalf("source status=%s want active", source.Status)
	}
	sourceLive, ok, err := s.st.Spawns().LiveContainer(ctx, "sp-source")
	if err != nil || !ok || sourceLive.NodeID != "node-1" || sourceLive.Generation != 1 {
		t.Fatalf("source live = %+v ok=%v err=%v", sourceLive, ok, err)
	}

	fork, err := s.st.Spawns().Get(ctx, resp.Msg.ForkSpawnId)
	if err != nil {
		t.Fatalf("Get fork: %v", err)
	}
	if fork.Status != store.Active {
		t.Fatalf("fork status=%s want active", fork.Status)
	}
	if fork.ParentSpawnID == nil || *fork.ParentSpawnID != "sp-source" {
		t.Fatalf("fork parent = %v", fork.ParentSpawnID)
	}
	if fork.ForkedAt == nil || *fork.ForkedAt != 1234 {
		t.Fatalf("forked_at = %v want 1234", fork.ForkedAt)
	}
	if fork.Name != "child" || fork.AppID != source.AppID || fork.Model != source.Model ||
		fork.Image != source.Image || fork.RunnableID != source.RunnableID || fork.Mode != source.Mode {
		t.Fatalf("fork row was not copied from source: source=%+v fork=%+v", source, fork)
	}
	forkLive, ok, err := s.st.Spawns().LiveContainer(ctx, resp.Msg.ForkSpawnId)
	if err != nil || !ok || forkLive.NodeID != "node-2" || forkLive.Generation != 1 {
		t.Fatalf("fork live = %+v ok=%v err=%v", forkLive, ok, err)
	}
	start := targetSender.firstStart()
	if start == nil {
		t.Fatal("fork materialization must start the fork pod")
	}
	if start.GetSpawnId() != resp.Msg.ForkSpawnId || start.GetGeneration() != 1 {
		t.Fatalf("fork StartSpawn id/gen = %s/%d, want %s/1", start.GetSpawnId(), start.GetGeneration(), resp.Msg.ForkSpawnId)
	}
	if start.GetRootfsSourceGeneration() != 1 || len(start.GetRootfsArtifacts()) != 1 ||
		start.GetRootfsArtifacts()[0].GetArtifactId() != "rootfs-fork-gen1" {
		t.Fatalf("fork StartSpawn rootfs restore = gen %d artifacts %+v", start.GetRootfsSourceGeneration(), start.GetRootfsArtifacts())
	}
	mounts, err := s.st.Spawns().GetMounts(ctx, resp.Msg.ForkSpawnId)
	if err != nil || len(mounts) != 1 || mounts[0].Name != "main" || mounts[0].SpawnID != resp.Msg.ForkSpawnId {
		t.Fatalf("fork mounts = %+v err=%v", mounts, err)
	}
	artifacts, err := s.st.Spawns().GetArtifacts(ctx, resp.Msg.ForkSpawnId)
	if err != nil || len(artifacts) != 1 || artifacts[0].ArtifactID != "artifact-1" || artifacts[0].SpawnID != resp.Msg.ForkSpawnId {
		t.Fatalf("fork artifacts = %+v err=%v", artifacts, err)
	}

	ts, err := s.st.TransferSets().Get(ctx, resp.Msg.TransferSetId)
	if err != nil {
		t.Fatalf("Get transfer set: %v", err)
	}
	if ts.Kind != store.TransferSetFork || ts.SpawnID != resp.Msg.ForkSpawnId ||
		ts.SourceSpawnID != "sp-source" || ts.ForkSpawnID != resp.Msg.ForkSpawnId {
		t.Fatalf("fork transfer set = %+v", ts)
	}
	if ts.SourceGeneration != 1 || ts.TargetGeneration != 1 || ts.SourceNodeID != "node-1" || ts.TargetNodeID != "node-2" {
		t.Fatalf("fork transfer set route/generation = %+v", ts)
	}
	if ts.Status != store.TransferSetActive {
		t.Fatalf("transfer set status=%s want active", ts.Status)
	}
	if len(mat.calls) != 1 {
		t.Fatalf("materializer calls=%d want 1", len(mat.calls))
	}
	call := mat.calls[0]
	if call.SourceSpawn.ID != "sp-source" || call.ForkSpawn.ID != resp.Msg.ForkSpawnId ||
		call.TransferSetID != resp.Msg.TransferSetId || call.SourceGeneration != 1 ||
		call.TargetGeneration != 1 || call.SourceNodeID != "node-1" || call.TargetNodeID != "node-2" {
		t.Fatalf("materializer request = %+v", call)
	}

	list, err := s.ListSpawns(ctx, connect.NewRequest(&cpv1.ListSpawnsRequest{}))
	if err != nil {
		t.Fatalf("ListSpawns: %v", err)
	}
	var summary *cpv1.SpawnSummary
	for _, row := range list.Msg.Spawns {
		if row.SpawnId == resp.Msg.ForkSpawnId {
			summary = row
			break
		}
	}
	if summary == nil || summary.ParentSpawnId != "sp-source" || summary.ForkedAt != 1234 {
		t.Fatalf("fork summary = %+v", summary)
	}
}

func TestForkSpawnReleasesTurnBoundaryWhenSetForkingFails(t *testing.T) {
	s, reg, rt := newTestServer(t)
	s.forkFootprintEstimator = staticForkFootprint(100)
	sourceSender := &turnBoundaryHookSender{s: s}
	sourceSender.hook = func(cmd *nodev1.ForkTurnBoundary) {
		if err := s.st.Spawns().SetSuspending(context.Background(), cmd.GetSourceSpawnId(), int64(cmd.GetSourceGeneration())); err != nil {
			t.Errorf("force source out of Active after turn-boundary: %v", err)
		}
	}
	seedForkSource(t, s, reg, rt, "sp-source", "alice", "node-1", sourceSender)

	_, err := s.ForkSpawn(auth.WithOwner(context.Background(), "alice"), connect.NewRequest(&cpv1.ForkSpawnRequest{
		SpawnId: "sp-source",
	}))
	if err == nil {
		t.Fatal("ForkSpawn should fail after source leaves Active before SetForking")
	}
	if sawForkSameNode(&sourceSender.capSender) {
		t.Fatal("ForkSameNode must not be sent after SetForking fails")
	}
	if msg := waitForReleaseForkTurnBoundaryCPMessage(t, &sourceSender.capSender); msg.GetSourceSpawnId() != "sp-source" ||
		msg.GetSourceGeneration() != 1 || msg.GetTransferSetId() == "" {
		t.Fatalf("ReleaseForkTurnBoundary = %+v", msg)
	}
}

func TestForkSpawnDefaultsToSameNode(t *testing.T) {
	s, reg, rt := newTestServer(t)
	sender := &capSender{}
	seedForkSource(t, s, reg, rt, "sp-source", "alice", "node-1", sender)
	stopAck := goAckStarts(s, sender)
	defer stopAck()
	mat := &recordingForkMaterializer{nodeID: "node-1"}
	s.forkMaterializer = mat
	s.forkFootprintEstimator = staticForkFootprint(100)

	resp, err := s.ForkSpawn(auth.WithOwner(context.Background(), "alice"), connect.NewRequest(&cpv1.ForkSpawnRequest{
		SpawnId: "sp-source",
	}))
	if err != nil {
		t.Fatalf("ForkSpawn: %v", err)
	}
	if resp.Msg.NodeId != "node-1" || mat.calls[0].TargetNodeID != "node-1" {
		t.Fatalf("default target response=%+v call=%+v", resp.Msg, mat.calls[0])
	}
}

func TestForkSpawnIntentEnabledSignsForkStartViaSourcePendingIntent(t *testing.T) {
	s, reg, rt := newTestServer(t)
	s.SetIntentEnabled(true)
	s.pendingIntents.ttl = 5 * time.Second
	sender := &capSender{}
	seedForkSource(t, s, reg, rt, "sp-source", "alice", "node-1", sender)
	stopAck := goAckStarts(s, sender)
	defer stopAck()
	s.forkMaterializer = &recordingForkMaterializer{nodeID: "node-1"}
	s.forkFootprintEstimator = staticForkFootprint(100)

	errCh := make(chan error, 1)
	sessionKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	goSubmitIntent(context.Background(), s, "sp-source", "alice", sessionKey, errCh)

	resp, err := s.ForkSpawn(auth.WithOwner(context.Background(), "alice"), connect.NewRequest(&cpv1.ForkSpawnRequest{
		SpawnId: "sp-source",
	}))
	if err != nil {
		t.Fatalf("ForkSpawn: %v", err)
	}
	select {
	case submitErr := <-errCh:
		if submitErr != nil {
			t.Fatalf("SubmitIntent: %v", submitErr)
		}
	case <-time.After(time.Second):
		t.Fatal("SubmitIntent did not complete")
	}

	start := sender.firstStart()
	if start == nil || start.GetSpawnId() != resp.Msg.ForkSpawnId {
		t.Fatalf("fork StartSpawn = %+v, response=%+v", start, resp.Msg)
	}
	if start.GetAuth() == nil || start.GetAuth().GetIntent() == nil {
		t.Fatalf("fork StartSpawn auth must be non-nil in intent mode: %+v", start)
	}
	body, err := intent.ParseBody(start.GetAuth().GetIntent().GetBody())
	if err != nil {
		t.Fatalf("parse signed fork intent: %v", err)
	}
	if body.GetSpawnId() != resp.Msg.ForkSpawnId || body.GetOp() != string(intent.OpForkSpawn) {
		t.Fatalf("signed fork intent body spawn/op = %q/%q, want %q/%q",
			body.GetSpawnId(), body.GetOp(), resp.Msg.ForkSpawnId, intent.OpForkSpawn)
	}
	if _, ready := s.pendingIntents.get("sp-source"); ready {
		t.Fatal("source-keyed fork pending intent must be cleaned up after start")
	}
}

func TestForkSpawnMarksSourceForkingOnlyDuringMaterialization(t *testing.T) {
	s, reg, rt := newTestServer(t)
	sender := &capSender{}
	seedForkSource(t, s, reg, rt, "sp-source", "alice", "node-1", sender)
	stopAck := goAckStarts(s, sender)
	defer stopAck()
	s.forkFootprintEstimator = staticForkFootprint(100)

	seenForking := false
	s.forkMaterializer = forkMaterializerFunc(func(ctx context.Context, req forkMaterializeRequest) (forkMaterializeResult, error) {
		sp, err := s.st.Spawns().Get(ctx, req.SourceSpawn.ID)
		if err != nil {
			return forkMaterializeResult{}, err
		}
		if sp.Status != store.Forking {
			return forkMaterializeResult{}, fmt.Errorf("source status during materialization = %s, want forking", sp.Status)
		}
		if sp.ForkCaptureDeadline == nil {
			return forkMaterializeResult{}, fmt.Errorf("source fork_capture_deadline must be set")
		}
		seenForking = true
		return forkMaterializeResult{NodeID: req.TargetNodeID}, nil
	})

	resp, err := s.ForkSpawn(auth.WithOwner(context.Background(), "alice"), connect.NewRequest(&cpv1.ForkSpawnRequest{SpawnId: "sp-source"}))
	if err != nil {
		t.Fatalf("ForkSpawn: %v", err)
	}
	if resp.Msg.ForkSpawnId == "" || !seenForking {
		t.Fatalf("fork response=%+v seenForking=%v", resp.Msg, seenForking)
	}
	source, err := s.st.Spawns().Get(context.Background(), "sp-source")
	if err != nil {
		t.Fatalf("Get source: %v", err)
	}
	if source.Status != store.Active || source.ForkCaptureDeadline != nil {
		t.Fatalf("source after fork = status %s deadline %v, want active nil", source.Status, source.ForkCaptureDeadline)
	}
}

func TestForkSpawnClassTargetPicksEligibleNode(t *testing.T) {
	s, reg, rt := newTestServer(t)
	seedForkSource(t, s, reg, rt, "sp-source", "alice", "node-1", &capSender{})
	targetSender := &capSender{}
	stopAck := goAckStarts(s, targetSender)
	defer stopAck()
	reg.Add(&registry.Node{
		ID: "cloud-1", Sender: targetSender, Max: 9, Free: 9, Class: "cloud",
		Images: []string{"img:agent"}, DiskFreeBytes: 1_000_000,
	})
	mat := &recordingForkMaterializer{nodeID: "cloud-1"}
	s.forkMaterializer = mat
	s.forkFootprintEstimator = staticForkFootprint(100)

	resp, err := s.ForkSpawn(auth.WithOwner(context.Background(), "alice"), connect.NewRequest(&cpv1.ForkSpawnRequest{
		SpawnId: "sp-source", TargetClass: "cloud",
	}))
	if err != nil {
		t.Fatalf("ForkSpawn(class): %v", err)
	}
	if resp.Msg.NodeId != "cloud-1" || mat.calls[0].TargetNodeID != "cloud-1" || mat.calls[0].TargetClass != "cloud" {
		t.Fatalf("class target response=%+v call=%+v", resp.Msg, mat.calls[0])
	}
}

func TestForkSpawnFencesSourceDuringMaterialization(t *testing.T) {
	s, reg, rt := newTestServer(t)
	sender := &capSender{}
	seedForkSource(t, s, reg, rt, "sp-source", "alice", "node-1", sender)
	stopAck := goAckStarts(s, sender)
	defer stopAck()
	s.forkFootprintEstimator = staticForkFootprint(100)

	keyedLockAttempting := make(chan struct{})
	keyedLockAcquired := make(chan func(), 1)
	s.forkMaterializer = forkMaterializerFunc(func(ctx context.Context, req forkMaterializeRequest) (forkMaterializeResult, error) {
		source, err := s.st.Spawns().Get(ctx, req.SourceSpawn.ID)
		if err != nil {
			return forkMaterializeResult{}, err
		}
		leaseID := "competing-source-lease"
		seq, claimErr := s.st.Spawns().Acquire(ctx, req.SourceSpawn.ID, "competitor", leaseID,
			time.Now().UnixNano(), time.Now().Add(time.Minute).UnixNano(), source.StatusSeq)
		if claimErr == nil {
			_ = s.st.Spawns().Release(ctx, req.SourceSpawn.ID, leaseID)
			return forkMaterializeResult{}, fmt.Errorf("source DB claim was not held; competitor acquired seq %d", seq)
		}
		if !errors.Is(claimErr, store.ErrConflict) {
			return forkMaterializeResult{}, fmt.Errorf("source claim probe: %w", claimErr)
		}

		go func() {
			close(keyedLockAttempting)
			unlock := s.locks.Lock(req.SourceSpawn.ID)
			keyedLockAcquired <- unlock
		}()
		select {
		case <-keyedLockAttempting:
		case <-time.After(time.Second):
			return forkMaterializeResult{}, fmt.Errorf("source keyed lock probe did not start")
		}
		select {
		case unlock := <-keyedLockAcquired:
			unlock()
			return forkMaterializeResult{}, fmt.Errorf("source keyed lock was not held during materialization")
		case <-time.After(20 * time.Millisecond):
			return forkMaterializeResult{NodeID: req.TargetNodeID}, nil
		}
	})

	resp, err := s.ForkSpawn(auth.WithOwner(context.Background(), "alice"), connect.NewRequest(&cpv1.ForkSpawnRequest{
		SpawnId: "sp-source",
	}))
	if err != nil {
		t.Fatalf("ForkSpawn: %v", err)
	}
	if resp.Msg.ForkSpawnId == "" {
		t.Fatalf("empty fork id in response: %+v", resp.Msg)
	}
	select {
	case unlock := <-keyedLockAcquired:
		unlock()
	case <-time.After(time.Second):
		t.Fatal("source keyed lock probe did not acquire after ForkSpawn returned")
	}
}

func TestForkSpawnDoesNotClaimSourceWhileWaitingForTurnBoundary(t *testing.T) {
	s, reg, rt := newTestServer(t)
	sourceSender := &capSender{}
	seedForkSource(t, s, reg, rt, "sp-source", "alice", "node-1", sourceSender)
	stopAck := goAckStarts(s, sourceSender)
	defer stopAck()
	s.forkFootprintEstimator = staticForkFootprint(100)
	mat := &blockingBoundaryMaterializer{nodeID: "node-1", reached: make(chan forkMaterializeRequest, 1), unblock: make(chan struct{})}
	s.forkMaterializer = mat

	ctx := auth.WithOwner(context.Background(), "alice")
	done := make(chan error, 1)
	go func() {
		_, err := s.ForkSpawn(ctx, connect.NewRequest(&cpv1.ForkSpawnRequest{SpawnId: "sp-source"}))
		done <- err
	}()
	select {
	case <-mat.reached:
	case <-time.After(time.Second):
		t.Fatal("fork did not reach turn-boundary wait")
	}
	defer func() {
		select {
		case <-mat.unblock:
		default:
			close(mat.unblock)
		}
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("ForkSpawn after turn-boundary release: %v", err)
			}
		case <-time.After(time.Second):
			t.Fatal("ForkSpawn did not finish after releasing turn boundary")
		}
	}()

	source, err := s.st.Spawns().Get(context.Background(), "sp-source")
	if err != nil {
		t.Fatalf("Get source: %v", err)
	}
	leaseID := "source-turn-boundary-probe"
	if _, err := s.st.Spawns().Acquire(context.Background(), "sp-source", "competitor", leaseID,
		time.Now().UnixNano(), time.Now().Add(time.Minute).UnixNano(), source.StatusSeq); err != nil {
		t.Fatalf("source DB claim must not be held while fork waits for turn boundary: %v", err)
	}
	if err := s.st.Spawns().Release(context.Background(), "sp-source", leaseID); err != nil {
		t.Fatalf("release source turn-boundary probe claim: %v", err)
	}

	lockAcquired := make(chan func(), 1)
	go func() {
		lockAcquired <- s.locks.Lock("sp-source")
	}()
	select {
	case unlock := <-lockAcquired:
		unlock()
	case <-time.After(50 * time.Millisecond):
		t.Fatal("source keyed lock must not be held while fork waits for turn boundary")
	}
}

func TestForkSpawnReleasesSourceClaimAndLockBeforeForkStandup(t *testing.T) {
	s, reg, rt := newTestServer(t)
	sourceSender := &capSender{}
	seedForkSource(t, s, reg, rt, "sp-source", "alice", "node-1", sourceSender)
	targetSender := &blockingStartSender{s: s, reached: make(chan *nodev1.StartSpawn, 1), unblock: make(chan struct{})}
	reg.Add(&registry.Node{
		ID: "node-2", Sender: targetSender, Max: 4, Free: 4, Class: "cloud",
		Images: []string{"img:agent"}, DiskFreeBytes: 1_000_000, DiskTotalBytes: 2_000_000,
	})
	s.forkFootprintEstimator = staticForkFootprint(100)
	s.forkMaterializer = &recordingForkMaterializer{nodeID: "node-2"}

	ctx := auth.WithOwner(context.Background(), "alice")
	done := make(chan error, 1)
	go func() {
		_, err := s.ForkSpawn(ctx, connect.NewRequest(&cpv1.ForkSpawnRequest{SpawnId: "sp-source", TargetNodeId: "node-2"}))
		done <- err
	}()
	var start *nodev1.StartSpawn
	select {
	case start = <-targetSender.reached:
	case <-time.After(time.Second):
		t.Fatal("fork standup did not reach StartSpawn")
	}
	defer func() {
		select {
		case <-targetSender.unblock:
		default:
			close(targetSender.unblock)
		}
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("ForkSpawn after unblock: %v", err)
			}
		case <-time.After(time.Second):
			t.Fatal("ForkSpawn did not finish after unblocking StartSpawn")
		}
	}()

	source, err := s.st.Spawns().Get(context.Background(), "sp-source")
	if err != nil {
		t.Fatalf("Get source: %v", err)
	}
	if source.Status != store.Active {
		t.Fatalf("source status while fork standup is blocked = %s, want Active", source.Status)
	}
	leaseID := "source-after-capture-probe"
	if _, err := s.st.Spawns().Acquire(context.Background(), "sp-source", "competitor", leaseID,
		time.Now().UnixNano(), time.Now().Add(time.Minute).UnixNano(), source.StatusSeq); err != nil {
		t.Fatalf("source DB claim must be released before fork standup completes; StartSpawn=%s err=%v", start.GetSpawnId(), err)
	}
	defer s.st.Spawns().Release(context.Background(), "sp-source", leaseID)

	lockAcquired := make(chan func(), 1)
	go func() {
		lockAcquired <- s.locks.Lock("sp-source")
	}()
	select {
	case unlock := <-lockAcquired:
		unlock()
	case <-time.After(50 * time.Millisecond):
		t.Fatalf("source keyed lock must be released before fork standup completes; StartSpawn=%s", start.GetSpawnId())
	}
}

func TestForkSpawnRestoresSourceBeforeSameNodeSeedCompletes(t *testing.T) {
	s, reg, rt := newTestServer(t)
	sourceSender := &blockingSameNodeForkSender{
		s:              s,
		sourceRestored: make(chan struct{}),
		completeFork:   make(chan struct{}),
		finalDelivered: make(chan bool, 1),
	}
	seedForkSource(t, s, reg, rt, "sp-source", "alice", "node-1", sourceSender)
	stopAck := goAckStarts(s, &sourceSender.capSender)
	defer stopAck()
	s.forkFootprintEstimator = staticForkFootprint(100)

	ctx := auth.WithOwner(context.Background(), "alice")
	done := make(chan error, 1)
	go func() {
		_, err := s.ForkSpawn(ctx, connect.NewRequest(&cpv1.ForkSpawnRequest{SpawnId: "sp-source"}))
		done <- err
	}()
	select {
	case <-sourceSender.sourceRestored:
	case <-time.After(time.Second):
		t.Fatal("same-node fork did not report source restored")
	}
	defer func() {
		select {
		case <-sourceSender.completeFork:
		default:
			close(sourceSender.completeFork)
		}
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("ForkSpawn after same-node seed completion: %v", err)
			}
		case <-time.After(time.Second):
			var delivered any = "missing"
			select {
			case delivered = <-sourceSender.finalDelivered:
			default:
			}
			t.Fatalf("ForkSpawn did not finish after same-node seed completion; starts=%d finalDelivered=%v", len(sourceSender.starts()), delivered)
		}
	}()

	var source store.Spawn
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		var err error
		source, err = s.st.Spawns().Get(context.Background(), "sp-source")
		if err == nil && source.Status == store.Active {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if source.Status != store.Active {
		t.Fatalf("source status while same-node seed is still blocked = %s, want Active", source.Status)
	}
	leaseID := "source-restored-before-seed-probe"
	if _, err := s.st.Spawns().Acquire(context.Background(), "sp-source", "competitor", leaseID,
		time.Now().UnixNano(), time.Now().Add(time.Minute).UnixNano(), source.StatusSeq); err != nil {
		t.Fatalf("source DB claim must be released before same-node seed completes: %v", err)
	}
	if err := s.st.Spawns().Release(context.Background(), "sp-source", leaseID); err != nil {
		t.Fatalf("release source seed probe claim: %v", err)
	}

	lockAcquired := make(chan func(), 1)
	go func() {
		lockAcquired <- s.locks.Lock("sp-source")
	}()
	select {
	case unlock := <-lockAcquired:
		unlock()
	case <-time.After(50 * time.Millisecond):
		t.Fatal("source keyed lock must be released before same-node seed completes")
	}
}

func TestForkSpawnGuards(t *testing.T) {
	t.Run("unauthenticated", func(t *testing.T) {
		s, reg, rt := newTestServer(t)
		seedForkSource(t, s, reg, rt, "sp-source", "alice", "node-1", &capSender{})
		_, err := s.ForkSpawn(context.Background(), connect.NewRequest(&cpv1.ForkSpawnRequest{SpawnId: "sp-source"}))
		if connect.CodeOf(err) != connect.CodeUnauthenticated {
			t.Fatalf("want Unauthenticated, got %v", err)
		}
	})
	t.Run("unknown source", func(t *testing.T) {
		s, _, _ := newTestServer(t)
		_, err := s.ForkSpawn(auth.WithOwner(context.Background(), "alice"), connect.NewRequest(&cpv1.ForkSpawnRequest{SpawnId: "missing"}))
		if connect.CodeOf(err) != connect.CodeNotFound {
			t.Fatalf("want NotFound, got %v", err)
		}
	})
	t.Run("foreign owner", func(t *testing.T) {
		s, reg, rt := newTestServer(t)
		seedForkSource(t, s, reg, rt, "sp-source", "alice", "node-1", &capSender{})
		_, err := s.ForkSpawn(auth.WithOwner(context.Background(), "bob"), connect.NewRequest(&cpv1.ForkSpawnRequest{SpawnId: "sp-source"}))
		if connect.CodeOf(err) != connect.CodePermissionDenied {
			t.Fatalf("want PermissionDenied, got %v", err)
		}
	})
	t.Run("source not active", func(t *testing.T) {
		s, reg, rt := newTestServer(t)
		seedForkSource(t, s, reg, rt, "sp-source", "alice", "node-1", &capSender{})
		if err := s.st.WithTx(context.Background(), func(tx store.Store) error {
			return tx.Spawns().SetSuspending(context.Background(), "sp-source", 1)
		}); err != nil {
			t.Fatalf("SetSuspending: %v", err)
		}
		_, err := s.ForkSpawn(auth.WithOwner(context.Background(), "alice"), connect.NewRequest(&cpv1.ForkSpawnRequest{SpawnId: "sp-source"}))
		if connect.CodeOf(err) != connect.CodeFailedPrecondition {
			t.Fatalf("want FailedPrecondition, got %v", err)
		}
	})
	t.Run("both target fields", func(t *testing.T) {
		s, reg, rt := newTestServer(t)
		seedForkSource(t, s, reg, rt, "sp-source", "alice", "node-1", &capSender{})
		_, err := s.ForkSpawn(auth.WithOwner(context.Background(), "alice"), connect.NewRequest(&cpv1.ForkSpawnRequest{
			SpawnId: "sp-source", TargetNodeId: "node-1", TargetClass: "cloud",
		}))
		if connect.CodeOf(err) != connect.CodeInvalidArgument {
			t.Fatalf("want InvalidArgument, got %v", err)
		}
	})
	t.Run("unknown target", func(t *testing.T) {
		s, reg, rt := newTestServer(t)
		seedForkSource(t, s, reg, rt, "sp-source", "alice", "node-1", &capSender{})
		_, err := s.ForkSpawn(auth.WithOwner(context.Background(), "alice"), connect.NewRequest(&cpv1.ForkSpawnRequest{
			SpawnId: "sp-source", TargetNodeId: "ghost",
		}))
		if connect.CodeOf(err) != connect.CodeFailedPrecondition {
			t.Fatalf("want FailedPrecondition, got %v", err)
		}
	})
	t.Run("foreign self-hosted target", func(t *testing.T) {
		s, reg, rt := newTestServer(t)
		seedForkSource(t, s, reg, rt, "sp-source", "alice", "node-1", &capSender{})
		reg.Add(&registry.Node{ID: "bob-box", Sender: &capSender{}, Max: 1, Free: 1, Class: "self-hosted", Owner: "bob", DiskFreeBytes: 1_000_000})
		_, err := s.ForkSpawn(auth.WithOwner(context.Background(), "alice"), connect.NewRequest(&cpv1.ForkSpawnRequest{
			SpawnId: "sp-source", TargetNodeId: "bob-box",
		}))
		if connect.CodeOf(err) != connect.CodePermissionDenied {
			t.Fatalf("want PermissionDenied, got %v", err)
		}
	})
	t.Run("quota exceeded", func(t *testing.T) {
		s, reg, rt := newTestServer(t)
		seedForkSource(t, s, reg, rt, "sp-source", "alice", "node-1", &capSender{})
		s.SetMaxSpawnsPerOwner(1)
		_, err := s.ForkSpawn(auth.WithOwner(context.Background(), "alice"), connect.NewRequest(&cpv1.ForkSpawnRequest{SpawnId: "sp-source"}))
		if connect.CodeOf(err) != connect.CodeResourceExhausted {
			t.Fatalf("want ResourceExhausted, got %v", err)
		}
	})
	t.Run("source claim busy", func(t *testing.T) {
		s, reg, rt := newTestServer(t)
		seedForkSource(t, s, reg, rt, "sp-source", "alice", "node-1", &capSender{})
		s.forkMaterializer = &recordingForkMaterializer{nodeID: "node-1"}
		s.forkFootprintEstimator = staticForkFootprint(100)

		source, err := s.st.Spawns().Get(context.Background(), "sp-source")
		if err != nil {
			t.Fatalf("Get source: %v", err)
		}
		leaseID := "existing-source-claim"
		_, err = s.st.Spawns().Acquire(context.Background(), "sp-source", "other-driver", leaseID,
			time.Now().UnixNano(), time.Now().Add(time.Minute).UnixNano(), source.StatusSeq)
		if err != nil {
			t.Fatalf("Acquire source claim: %v", err)
		}
		defer func() {
			_ = s.st.Spawns().Release(context.Background(), "sp-source", leaseID)
		}()

		_, err = s.ForkSpawn(auth.WithOwner(context.Background(), "alice"), connect.NewRequest(&cpv1.ForkSpawnRequest{SpawnId: "sp-source"}))
		if connect.CodeOf(err) != connect.CodeAborted {
			t.Fatalf("want Aborted, got %v", err)
		}
	})
}

func TestForkSpawnRejectsInsufficientDiskBeforeCreatingFork(t *testing.T) {
	s, reg, rt := newTestServer(t)
	seedForkSource(t, s, reg, rt, "sp-source", "alice", "node-1", &capSender{})
	reg.Add(&registry.Node{
		ID: "node-2", Sender: &capSender{}, Max: 4, Free: 4, Class: "cloud",
		Images: []string{"img:agent"}, DiskFreeBytes: 299,
	})
	mat := &recordingForkMaterializer{nodeID: "node-2"}
	s.forkMaterializer = mat
	s.forkFootprintEstimator = staticForkFootprint(100)

	ctx := auth.WithOwner(context.Background(), "alice")
	_, err := s.ForkSpawn(ctx, connect.NewRequest(&cpv1.ForkSpawnRequest{SpawnId: "sp-source", TargetNodeId: "node-2"}))
	if code := connect.CodeOf(err); code != connect.CodeResourceExhausted && code != connect.CodeFailedPrecondition {
		t.Fatalf("want disk gate error, got %v", err)
	}
	rows, err := s.st.Spawns().ListByOwner(ctx, "alice")
	if err != nil {
		t.Fatalf("ListByOwner: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("fork row should not be created on first disk gate failure: %+v", rows)
	}
	if len(mat.calls) != 0 {
		t.Fatalf("materializer calls=%d want 0", len(mat.calls))
	}
}

func TestForkSpawnRechecksDiskBeforeMaterialization(t *testing.T) {
	s, reg, rt := newTestServer(t)
	seedForkSource(t, s, reg, rt, "sp-source", "alice", "node-1", &capSender{})
	reg.Add(&registry.Node{
		ID: "node-2", Sender: &capSender{}, Max: 4, Free: 4, Class: "cloud",
		Images: []string{"img:agent"}, DiskFreeBytes: 1_000_000,
	})
	mat := &recordingForkMaterializer{nodeID: "node-2"}
	res := &recordingForkResources{}
	s.forkMaterializer = mat
	s.failedForkResources = res
	s.forkFootprintEstimator = &sequenceForkFootprint{
		values: []int64{100, 100},
		hook: func(call int) {
			if call == 2 {
				reg.Add(&registry.Node{
					ID: "node-2", Sender: &capSender{}, Max: 4, Free: 4, Class: "cloud",
					Images: []string{"img:agent"}, DiskFreeBytes: 1,
				})
			}
		},
	}

	ctx := auth.WithOwner(context.Background(), "alice")
	_, err := s.ForkSpawn(ctx, connect.NewRequest(&cpv1.ForkSpawnRequest{SpawnId: "sp-source", TargetNodeId: "node-2"}))
	if code := connect.CodeOf(err); code != connect.CodeResourceExhausted && code != connect.CodeFailedPrecondition {
		t.Fatalf("want second disk gate error, got %v", err)
	}
	rows, err := s.st.Spawns().ListByOwner(ctx, "alice")
	if err != nil {
		t.Fatalf("ListByOwner: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("fork row should be unwound after second disk gate failure: %+v", rows)
	}
	if len(mat.calls) != 0 {
		t.Fatalf("materializer calls=%d want 0", len(mat.calls))
	}
	if len(res.ops) == 0 || !strings.HasPrefix(res.ops[len(res.ops)-1], "delete-row:") {
		t.Fatalf("disk recheck must use ordered fork unwind, ops=%v", res.ops)
	}
}

func TestForkSpawnMaterializerFailureUnwindsFork(t *testing.T) {
	s, reg, rt := newTestServer(t)
	sourceSender := &capSender{}
	seedForkSource(t, s, reg, rt, "sp-source", "alice", "node-1", sourceSender)
	reg.Add(&registry.Node{
		ID: "node-2", Sender: &capSender{}, Max: 4, Free: 4, Class: "cloud",
		Images: []string{"img:agent"}, DiskFreeBytes: 1_000_000,
	})
	res := &recordingForkResources{}
	s.failedForkResources = res
	s.forkFootprintEstimator = staticForkFootprint(100)
	s.forkMaterializer = unimplementedForkMaterializer{}

	ctx := auth.WithOwner(context.Background(), "alice")
	_, err := s.ForkSpawn(ctx, connect.NewRequest(&cpv1.ForkSpawnRequest{SpawnId: "sp-source", TargetNodeId: "node-2"}))
	if connect.CodeOf(err) != connect.CodeUnimplemented {
		t.Fatalf("default materializer should fail unimplemented, got %v", err)
	}
	rows, err := s.st.Spawns().ListByOwner(ctx, "alice")
	if err != nil {
		t.Fatalf("ListByOwner: %v", err)
	}
	if len(rows) != 1 || rows[0].ID != "sp-source" {
		t.Fatalf("fork row should be hidden after materializer failure: %+v", rows)
	}
	source, err := s.st.Spawns().Get(ctx, "sp-source")
	if err != nil || source.Status != store.Active {
		t.Fatalf("source after failed fork = %+v err=%v", source, err)
	}
	if !sawUnpause(sourceSender, "sp-source") {
		t.Fatal("failed fork cleanup must unpause source before restoring it active")
	}
	wantPrefixes := []string{"revoke-key:", "empty-bucket:spawnery-spawn-", "drop-bucket:spawnery-spawn-", "delete-row:"}
	if len(res.ops) != len(wantPrefixes) {
		t.Fatalf("unwind ops=%v want prefixes %v", res.ops, wantPrefixes)
	}
	for i, prefix := range wantPrefixes {
		if !strings.HasPrefix(res.ops[i], prefix) {
			t.Fatalf("unwind ops=%v want prefix %q at %d", res.ops, prefix, i)
		}
	}
}

func TestForkSpawnMaterializerFailureLeavesSourceForkingWhenUnpauseUnavailable(t *testing.T) {
	s, reg, rt := newTestServer(t)
	seedForkSource(t, s, reg, rt, "sp-source", "alice", "node-1", nil)
	reg.Add(&registry.Node{
		ID: "node-2", Sender: &capSender{}, Max: 4, Free: 4, Class: "cloud",
		Images: []string{"img:agent"}, DiskFreeBytes: 1_000_000,
	})
	res := &recordingForkResources{}
	s.failedForkResources = res
	s.forkFootprintEstimator = staticForkFootprint(100)
	s.forkMaterializer = unimplementedForkMaterializer{}

	ctx := auth.WithOwner(context.Background(), "alice")
	_, err := s.ForkSpawn(ctx, connect.NewRequest(&cpv1.ForkSpawnRequest{SpawnId: "sp-source", TargetNodeId: "node-2"}))
	if connect.CodeOf(err) != connect.CodeUnimplemented {
		t.Fatalf("default materializer should fail unimplemented, got %v", err)
	}
	source, err := s.st.Spawns().Get(ctx, "sp-source")
	if err != nil {
		t.Fatalf("Get source: %v", err)
	}
	if source.Status != store.Forking {
		t.Fatalf("source status=%s want Forking after failed unpause", source.Status)
	}
	if source.ForkCaptureDeadline == nil {
		t.Fatal("fork_capture_deadline must remain set for recovery")
	}
}

func TestForkSpawnMaterializerFailureWithoutResourcesLeavesFailedForkForRetry(t *testing.T) {
	s, reg, rt := newTestServer(t)
	s.failedForkResources = nil
	seedForkSource(t, s, reg, rt, "sp-source", "alice", "node-1", &capSender{})
	reg.Add(&registry.Node{
		ID: "node-2", Sender: &capSender{}, Max: 4, Free: 4, Class: "cloud",
		Images: []string{"img:agent"}, DiskFreeBytes: 1_000_000,
	})
	s.forkFootprintEstimator = staticForkFootprint(100)
	s.forkMaterializer = unimplementedForkMaterializer{}

	ctx := auth.WithOwner(context.Background(), "alice")
	_, err := s.ForkSpawn(ctx, connect.NewRequest(&cpv1.ForkSpawnRequest{SpawnId: "sp-source", TargetNodeId: "node-2"}))
	if connect.CodeOf(err) != connect.CodeUnimplemented {
		t.Fatalf("default materializer should fail unimplemented, got %v", err)
	}

	rows, err := s.st.Spawns().ListByOwner(ctx, "alice")
	if err != nil {
		t.Fatalf("ListByOwner: %v", err)
	}
	var fork store.Spawn
	for _, row := range rows {
		if row.ID != "sp-source" {
			fork = row
			break
		}
	}
	if fork.ID == "" {
		t.Fatalf("failed fork row should remain visible for retry when resources are nil: %+v", rows)
	}
	if _, ok, err := s.st.Spawns().LiveContainer(ctx, fork.ID); err != nil || !ok {
		t.Fatalf("failed fork live row should remain for retry: ok=%v err=%v", ok, err)
	}
	failed, err := s.st.TransferSets().ListFailedForks(ctx)
	if err != nil {
		t.Fatalf("ListFailedForks: %v", err)
	}
	if len(failed) != 1 || failed[0].ForkSpawnID != fork.ID || failed[0].Status != store.TransferSetFailed {
		t.Fatalf("failed fork transfer sets = %+v, fork=%s", failed, fork.ID)
	}
}

func TestForkSpawnRejectsUnknownFootprintBeforeCreatingFork(t *testing.T) {
	s, reg, rt := newTestServer(t)
	seedForkSource(t, s, reg, rt, "sp-source", "alice", "node-1", &capSender{})
	mat := &recordingForkMaterializer{nodeID: "node-1", err: errors.New("must not be called")}
	s.forkMaterializer = mat

	ctx := auth.WithOwner(context.Background(), "alice")
	_, err := s.ForkSpawn(ctx, connect.NewRequest(&cpv1.ForkSpawnRequest{SpawnId: "sp-source"}))
	if connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Fatalf("want unknown-footprint FailedPrecondition, got %v", err)
	}
	rows, err := s.st.Spawns().ListByOwner(ctx, "alice")
	if err != nil {
		t.Fatalf("ListByOwner: %v", err)
	}
	if len(rows) != 1 || len(mat.calls) != 0 {
		t.Fatalf("unknown footprint should fail before fork creation/materialization: rows=%+v calls=%d", rows, len(mat.calls))
	}
}

func TestRequiredForkHeadroomBytesUsesTripleFootprint(t *testing.T) {
	if got := requiredForkHeadroomBytes(101); got != 303 {
		t.Fatalf("requiredForkHeadroomBytes(101)=%d want 303", got)
	}
}
