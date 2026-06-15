package cp

import (
	"context"
	"testing"
	"time"

	"connectrpc.com/connect"

	cpv1 "spawnery/gen/cp/v1"
	nodev1 "spawnery/gen/node/v1"
	"spawnery/internal/cp/auth"
	"spawnery/internal/cp/registry"
	"spawnery/internal/cp/store"
)

func TestForkMaterializerSendsForkSameNodeAndReturnsPins(t *testing.T) {
	s, reg, _ := newTestServer(t)
	s.forks = newForkWaiters()
	sender := &capSender{}
	reg.Add(registryNode("node-1", sender))
	mat := newSameNodeForkMaterializer(s, time.Second)

	done := make(chan struct{})
	var (
		got forkMaterializeResult
		err error
	)
	go func() {
		got, err = mat.MaterializeFork(context.Background(), forkMaterializeRequest{
			SourceSpawn:      store.Spawn{ID: "sp-source", BaseImageDigest: "agent@sha256:base"},
			ForkSpawn:        store.Spawn{ID: "sp-fork"},
			TransferSetID:    "ts-1",
			SourceGeneration: 9,
			TargetGeneration: 1,
			SourceNodeID:     "node-1",
			TargetNodeID:     "node-1",
		})
		close(done)
	}()

	waitForForkCPMessage(t, sender)
	s.deliverForkSourceRestored(&nodev1.ForkSourceRestored{
		SourceSpawnId:    "sp-source",
		SourceGeneration: 9,
		TransferSetId:    "ts-1",
	})
	s.deliverForkSameNodeComplete(&nodev1.ForkSameNodeComplete{
		SourceSpawnId: "sp-source",
		ForkSpawnId:   "sp-fork",
		TransferSetId: "ts-1",
		NodeId:        "node-1",
		Mounts:        []*nodev1.MountMarker{{Name: "work", Marker: "fork-manifest"}},
		RootfsArtifacts: []*nodev1.RootfsArtifact{{
			ArtifactId: "rootfs-fork-gen1", Generation: 1, Sequence: 1, BaseImageDigest: "agent@sha256:base",
			Format: "oci_layout",
		}},
	})
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("materializer did not finish")
	}
	if err != nil {
		t.Fatalf("MaterializeFork: %v", err)
	}
	if got.NodeID != "node-1" || got.MountPins["work"] != "fork-manifest" {
		t.Fatalf("materializer result = %+v", got)
	}
	if len(got.RootfsPins) != 1 || got.RootfsPins[0].ArtifactID != "rootfs-fork-gen1" || got.RootfsPins[0].Generation != 1 {
		t.Fatalf("rootfs pins = %+v", got.RootfsPins)
	}
	msg := sender.sent[len(sender.sent)-1].GetForkSameNode()
	if msg.GetSourceSpawnId() != "sp-source" || msg.GetForkSpawnId() != "sp-fork" ||
		msg.GetSourceGeneration() != 9 || msg.GetTargetGeneration() != 1 || msg.GetTransferSetId() != "ts-1" {
		t.Fatalf("ForkSameNode message = %+v", msg)
	}
}

func TestForkMaterializerCrossNodeRelaysTargetKeyMaterialAndReturnsTargetPins(t *testing.T) {
	s, reg, _ := newTestServer(t)
	s.nodeKeys.put("node-2", []byte("signed-subkey"), []byte("leaf-chain"))
	sourceSender := &capSender{}
	targetSender := &capSender{}
	reg.Add(registryNode("node-1", sourceSender))
	reg.Add(registryNode("node-2", targetSender))
	mat := newSameNodeForkMaterializer(s, time.Second).(forkSourceRestoredMaterializer)

	restored := make(chan struct{}, 1)
	done := make(chan struct{})
	var (
		got forkMaterializeResult
		err error
	)
	go func() {
		got, err = mat.MaterializeForkWithSourceRestored(context.Background(), forkMaterializeRequest{
			SourceSpawn:      store.Spawn{ID: "sp-source", BaseImageDigest: "agent@sha256:base"},
			ForkSpawn:        store.Spawn{ID: "sp-fork"},
			TransferSetID:    "ts-cross-node",
			SourceGeneration: 9,
			TargetGeneration: 1,
			SourceNodeID:     "node-1",
			TargetNodeID:     "node-2",
			TargetClass:      "cloud",
		}, func() error {
			restored <- struct{}{}
			return nil
		})
		close(done)
	}()

	waitForForkAnyCPMessage(t, "ForkTransferExport", sourceSender)
	export := sourceSender.lastCPMessage().GetForkTransferExport()
	if export == nil {
		t.Fatalf("source message = %+v, want ForkTransferExport", sourceSender.lastCPMessage())
	}
	if export.GetTargetNodeId() != "node-2" || string(export.GetTargetSignedSubkey()) != "signed-subkey" || string(export.GetTargetNodeCertChain()) != "leaf-chain" {
		t.Fatalf("ForkTransferExport = %+v", export)
	}
	s.deliverForkSourceRestored(&nodev1.ForkSourceRestored{
		SourceSpawnId:    "sp-source",
		SourceGeneration: 9,
		TransferSetId:    "ts-cross-node",
	})
	select {
	case <-restored:
	case <-time.After(time.Second):
		t.Fatal("source restored callback did not fire")
	}
	s.deliverForkTransferExported(&nodev1.ForkTransferExported{
		SourceSpawnId:     "sp-source",
		ForkSpawnId:       "sp-fork",
		TransferSetId:     "ts-cross-node",
		SealedTransferKey: []byte("sealed-key"),
		Payload:           []byte("sealed-payload"),
	})
	waitForForkAnyCPMessage(t, "ForkTransferImport", targetSender)
	importReq := targetSender.lastCPMessage().GetForkTransferImport()
	if importReq == nil {
		t.Fatalf("target message = %+v, want ForkTransferImport", targetSender.lastCPMessage())
	}
	if string(importReq.GetSealedTransferKey()) != "sealed-key" || string(importReq.GetPayload()) != "sealed-payload" {
		t.Fatalf("ForkTransferImport = %+v", importReq)
	}
	s.deliverForkTransferImported(&nodev1.ForkTransferImported{
		SourceSpawnId: "sp-source",
		ForkSpawnId:   "sp-fork",
		TransferSetId: "ts-cross-node",
		NodeId:        "node-2",
		Mounts:        []*nodev1.MountMarker{{Name: "work", Marker: "fork-manifest"}},
		RootfsArtifacts: []*nodev1.RootfsArtifact{{
			ArtifactId: "rootfs-fork-gen1", Generation: 1, Sequence: 1, BaseImageDigest: "agent@sha256:base",
			Format: "oci_layout",
		}},
	})
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("materializer did not finish")
	}
	if err != nil {
		t.Fatalf("MaterializeForkWithSourceRestored: %v", err)
	}
	if got.NodeID != "node-2" || got.MountPins["work"] != "fork-manifest" {
		t.Fatalf("materializer result = %+v", got)
	}
	if len(got.RootfsPins) != 1 || got.RootfsPins[0].ArtifactID != "rootfs-fork-gen1" || got.RootfsPins[0].Generation != 1 {
		t.Fatalf("rootfs pins = %+v", got.RootfsPins)
	}
}

func TestForkMaterializerCrossNodeFailsClosedWithoutTargetSubkey(t *testing.T) {
	s, reg, _ := newTestServer(t)
	sourceSender := &capSender{}
	targetSender := &capSender{}
	reg.Add(registryNode("node-1", sourceSender))
	reg.Add(registryNode("node-2", targetSender))
	mat := newSameNodeForkMaterializer(s, time.Second)

	_, err := mat.MaterializeFork(context.Background(), forkMaterializeRequest{
		SourceSpawn:      store.Spawn{ID: "sp-source", BaseImageDigest: "agent@sha256:base"},
		ForkSpawn:        store.Spawn{ID: "sp-fork"},
		TransferSetID:    "ts-cross-node",
		SourceGeneration: 9,
		TargetGeneration: 1,
		SourceNodeID:     "node-1",
		TargetNodeID:     "node-2",
		TargetClass:      "cloud",
	})
	if connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Fatalf("cross-node MaterializeFork error = %v, want FailedPrecondition", err)
	}
	if sourceSender.lastCPMessage() != nil || targetSender.lastCPMessage() != nil {
		t.Fatalf("cross-node materializer must fail before dispatch, source=%+v target=%+v",
			sourceSender.lastCPMessage(), targetSender.lastCPMessage())
	}
}

func TestForkMaterializerReportsSourceRestoredBeforeFinalCompletion(t *testing.T) {
	s, reg, _ := newTestServer(t)
	s.forks = newForkWaiters()
	s.forkSourceRestored = newForkSourceRestoredWaiters()
	sender := &capSender{}
	reg.Add(registryNode("node-1", sender))
	mat := newSameNodeForkMaterializer(s, time.Second).(forkSourceRestoredMaterializer)

	restored := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		_, err := mat.MaterializeForkWithSourceRestored(context.Background(), forkMaterializeRequest{
			SourceSpawn:      store.Spawn{ID: "sp-source", BaseImageDigest: "agent@sha256:base"},
			ForkSpawn:        store.Spawn{ID: "sp-fork"},
			TransferSetID:    "ts-1",
			SourceGeneration: 9,
			TargetGeneration: 1,
			SourceNodeID:     "node-1",
			TargetNodeID:     "node-1",
		}, func() error {
			close(restored)
			return nil
		})
		done <- err
	}()

	waitForForkCPMessage(t, sender)
	s.deliverForkSourceRestored(&nodev1.ForkSourceRestored{
		SourceSpawnId:    "sp-source",
		SourceGeneration: 9,
		TransferSetId:    "ts-1",
	})
	select {
	case <-restored:
	case <-time.After(time.Second):
		t.Fatal("source restored callback was not invoked")
	}
	select {
	case err := <-done:
		t.Fatalf("materializer completed before final ForkSameNodeComplete: %v", err)
	default:
	}

	s.deliverForkSameNodeComplete(&nodev1.ForkSameNodeComplete{
		SourceSpawnId: "sp-source",
		ForkSpawnId:   "sp-fork",
		TransferSetId: "ts-1",
		NodeId:        "node-1",
		Mounts:        []*nodev1.MountMarker{{Name: "work", Marker: "fork-manifest"}},
		RootfsArtifacts: []*nodev1.RootfsArtifact{{
			ArtifactId: "rootfs-fork-gen1", Generation: 1, Sequence: 1, BaseImageDigest: "agent@sha256:base",
			Format: "oci_layout",
		}},
	})
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("MaterializeForkWithSourceRestored: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("materializer did not finish after final completion")
	}
}

func TestForkMaterializerCancelsDispatchedSameNodeForkOnContextCancel(t *testing.T) {
	s, reg, _ := newTestServer(t)
	s.forks = newForkWaiters()
	s.forkSourceRestored = newForkSourceRestoredWaiters()
	sender := &capSender{}
	reg.Add(registryNode("node-1", sender))
	mat := newSameNodeForkMaterializer(s, time.Second)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := mat.MaterializeFork(ctx, forkMaterializeRequest{
			SourceSpawn:      store.Spawn{ID: "sp-source", BaseImageDigest: "agent@sha256:base"},
			ForkSpawn:        store.Spawn{ID: "sp-fork"},
			TransferSetID:    "ts-1",
			SourceGeneration: 9,
			TargetGeneration: 1,
			SourceNodeID:     "node-1",
			TargetNodeID:     "node-1",
		})
		done <- err
	}()

	waitForForkCPMessage(t, sender)
	cancel()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("MaterializeFork error = nil, want cancellation")
		}
	case <-time.After(time.Second):
		t.Fatal("materializer did not return after context cancellation")
	}
	waitForForkCancelCPMessage(t, sender)
	cancelMsg := sender.lastCPMessage().GetCancelForkSameNode()
	if cancelMsg.GetSourceSpawnId() != "sp-source" || cancelMsg.GetForkSpawnId() != "sp-fork" || cancelMsg.GetTransferSetId() != "ts-1" {
		t.Fatalf("CancelForkSameNode = %+v", cancelMsg)
	}
}

func TestForkMaterializerWaitsForTurnBoundaryPreflight(t *testing.T) {
	s, reg, _ := newTestServer(t)
	s.forks = newForkWaiters()
	s.forkTurnBoundaries = newForkTurnBoundaryWaiters()
	sender := &capSender{}
	reg.Add(registryNode("node-1", sender))
	mat := newSameNodeForkMaterializer(s, time.Second).(forkTurnBoundaryWaiter)

	done := make(chan error, 1)
	go func() {
		err := mat.WaitForForkTurnBoundary(context.Background(), forkMaterializeRequest{
			SourceSpawn:      store.Spawn{ID: "sp-source"},
			ForkSpawn:        store.Spawn{ID: "sp-fork"},
			TransferSetID:    "ts-1",
			SourceGeneration: 9,
			TargetGeneration: 1,
			SourceNodeID:     "node-1",
			TargetNodeID:     "node-1",
		})
		done <- err
	}()

	waitForForkTurnBoundaryCPMessage(t, sender)
	if msg := sender.lastCPMessage(); msg.GetForkSameNode() != nil {
		t.Fatalf("turn-boundary preflight must not send ForkSameNode: %+v", msg)
	}
	s.deliverForkTurnBoundaryComplete(&nodev1.ForkTurnBoundaryComplete{
		SourceSpawnId: "sp-source",
		TransferSetId: "ts-1",
	})
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("WaitForForkTurnBoundary: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("turn-boundary preflight did not finish")
	}
}

func TestForkMaterializerReleasesTurnBoundaryOnTimeout(t *testing.T) {
	s, reg, _ := newTestServer(t)
	s.forkTurnBoundaries = newForkTurnBoundaryWaiters()
	sender := &capSender{}
	reg.Add(registryNode("node-1", sender))
	mat := newSameNodeForkMaterializer(s, time.Millisecond).(forkTurnBoundaryWaiter)

	err := mat.WaitForForkTurnBoundary(context.Background(), forkMaterializeRequest{
		SourceSpawn:      store.Spawn{ID: "sp-source"},
		ForkSpawn:        store.Spawn{ID: "sp-fork"},
		TransferSetID:    "ts-timeout",
		SourceGeneration: 9,
		TargetGeneration: 1,
		SourceNodeID:     "node-1",
		TargetNodeID:     "node-1",
	})
	if connect.CodeOf(err) != connect.CodeDeadlineExceeded {
		t.Fatalf("WaitForForkTurnBoundary error = %v, want DeadlineExceeded", err)
	}
	if msg := waitForReleaseForkTurnBoundaryCPMessage(t, sender); msg.GetSourceSpawnId() != "sp-source" ||
		msg.GetSourceGeneration() != 9 || msg.GetTransferSetId() != "ts-timeout" {
		t.Fatalf("ReleaseForkTurnBoundary = %+v", msg)
	}
}

func TestForkSpawnRecordsMaterializerPinsOnTransferSet(t *testing.T) {
	s, reg, rt := newTestServer(t)
	sender := &capSender{}
	seedForkSource(t, s, reg, rt, "sp-source", "alice", "node-1", sender)
	stopAck := goAckStarts(s, sender)
	defer stopAck()
	s.forkFootprintEstimator = staticForkFootprint(100)
	s.forkMaterializer = forkMaterializerFunc(func(context.Context, forkMaterializeRequest) (forkMaterializeResult, error) {
		return forkMaterializeResult{
			NodeID:    "node-1",
			MountPins: map[string]string{"work": "fork-manifest"},
			RootfsPins: []store.RootfsArtifactPin{{
				ArtifactID: "rootfs-fork-gen1",
				Generation: 1,
				Sequence:   1,
				Format:     "oci_layout",
			}},
		}, nil
	})

	resp, err := s.ForkSpawn(auth.WithOwner(context.Background(), "alice"), connect.NewRequest(&cpv1.ForkSpawnRequest{SpawnId: "sp-source"}))
	if err != nil {
		t.Fatalf("ForkSpawn: %v", err)
	}
	ts, err := s.st.TransferSets().Get(context.Background(), resp.Msg.TransferSetId)
	if err != nil {
		t.Fatalf("Get transfer set: %v", err)
	}
	if ts.MountManifestPins["work"] != "fork-manifest" {
		t.Fatalf("mount pins = %+v", ts.MountManifestPins)
	}
	if len(ts.RootfsArtifactPins) != 1 || ts.RootfsArtifactPins[0].ArtifactID != "rootfs-fork-gen1" {
		t.Fatalf("rootfs pins = %+v", ts.RootfsArtifactPins)
	}
}

func registryNode(id string, sender *capSender) *registry.Node {
	return &registry.Node{ID: id, Sender: sender, Max: 1, Free: 1, Class: "cloud", Images: []string{"img:agent"}, DiskFreeBytes: 1_000_000}
}

func waitForForkCPMessage(t *testing.T, sender *capSender) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		sender.mu.Lock()
		for _, msg := range sender.sent {
			if msg.GetForkSameNode() != nil {
				sender.mu.Unlock()
				return
			}
		}
		sender.mu.Unlock()
		time.Sleep(time.Millisecond)
	}
	t.Fatal("timed out waiting for ForkSameNode CP message")
}

func waitForForkAnyCPMessage(t *testing.T, name string, sender *capSender) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if sender.lastCPMessage() != nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s CP message", name)
}

func waitForForkCancelCPMessage(t *testing.T, sender *capSender) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		sender.mu.Lock()
		for _, msg := range sender.sent {
			if msg.GetCancelForkSameNode() != nil {
				sender.mu.Unlock()
				return
			}
		}
		sender.mu.Unlock()
		time.Sleep(time.Millisecond)
	}
	t.Fatal("timed out waiting for CancelForkSameNode CP message")
}

func waitForForkTurnBoundaryCPMessage(t *testing.T, sender *capSender) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		sender.mu.Lock()
		for _, msg := range sender.sent {
			if msg.GetForkTurnBoundary() != nil {
				sender.mu.Unlock()
				return
			}
		}
		sender.mu.Unlock()
		time.Sleep(time.Millisecond)
	}
	t.Fatal("timed out waiting for ForkTurnBoundary CP message")
}

func waitForReleaseForkTurnBoundaryCPMessage(t *testing.T, sender *capSender) *nodev1.ReleaseForkTurnBoundary {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		sender.mu.Lock()
		for _, msg := range sender.sent {
			if cmd := msg.GetReleaseForkTurnBoundary(); cmd != nil {
				sender.mu.Unlock()
				return cmd
			}
		}
		sender.mu.Unlock()
		time.Sleep(time.Millisecond)
	}
	t.Fatal("timed out waiting for ReleaseForkTurnBoundary CP message")
	return nil
}
