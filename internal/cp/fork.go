package cp

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"connectrpc.com/connect"
	"github.com/google/uuid"

	authv1 "spawnery/gen/auth/v1"
	cpv1 "spawnery/gen/cp/v1"
	nodev1 "spawnery/gen/node/v1"
	"spawnery/internal/cp/auth"
	"spawnery/internal/cp/registry"
	"spawnery/internal/cp/store"
	"spawnery/internal/intent"
)

const forkHeadroomMultiplier = int64(3)

var errForkFootprintUnknown = errors.New("fork source disk footprint is unknown")

const defaultForkCaptureTimeout = defaultSuspendTimeout

type forkMaterializer interface {
	MaterializeFork(context.Context, forkMaterializeRequest) (forkMaterializeResult, error)
}

type forkMaterializeRequest struct {
	SourceSpawn      store.Spawn
	ForkSpawn        store.Spawn
	TransferSetID    string
	SourceGeneration uint64
	TargetGeneration uint64
	SourceNodeID     string
	TargetNodeID     string
	TargetClass      string
	Mounts           []store.Mount
	Artifacts        []store.Artifact
}

type forkMaterializeResult struct {
	NodeID     string
	MountPins  map[string]string
	RootfsPins []store.RootfsArtifactPin
}

type forkFootprintEstimator interface {
	ForkFootprintBytes(context.Context, store.Spawn, store.Container) (int64, error)
}

type unimplementedForkMaterializer struct{}

func (unimplementedForkMaterializer) MaterializeFork(context.Context, forkMaterializeRequest) (forkMaterializeResult, error) {
	return forkMaterializeResult{}, connect.NewError(connect.CodeUnimplemented, fmt.Errorf("fork materialization is not implemented yet"))
}

func (unimplementedForkMaterializer) WaitForForkTurnBoundary(context.Context, forkMaterializeRequest) error {
	return nil
}

func requiredForkHeadroomBytes(sourceFootprintBytes int64) int64 {
	if sourceFootprintBytes <= 0 {
		return 0
	}
	const maxInt64 = int64(^uint64(0) >> 1)
	if sourceFootprintBytes > maxInt64/forkHeadroomMultiplier {
		return maxInt64
	}
	return sourceFootprintBytes * forkHeadroomMultiplier
}

func (s *Server) forkHeadroomBytes(ctx context.Context, source store.Spawn, live store.Container) (int64, error) {
	if s.forkFootprintEstimator == nil {
		return 0, errForkFootprintUnknown
	}
	footprint, err := s.forkFootprintEstimator.ForkFootprintBytes(ctx, source, live)
	if err != nil {
		return 0, err
	}
	if footprint < 0 {
		return 0, fmt.Errorf("fork source disk footprint is negative: %d", footprint)
	}
	return requiredForkHeadroomBytes(footprint), nil
}

func (s *Server) checkForkDiskHeadroom(owner string, source store.Spawn, targetNodeID string, headroomBytes int64) error {
	if headroomBytes <= 0 {
		return nil
	}
	n := s.reg.PickFor(registry.Placement{
		Owner:            owner,
		Image:            source.Image,
		TargetNodeID:     targetNodeID,
		MinDiskFreeBytes: headroomBytes,
	})
	if n == nil {
		return connect.NewError(connect.CodeResourceExhausted,
			fmt.Errorf("target node %q does not have required fork disk headroom (%d bytes)", targetNodeID, headroomBytes))
	}
	return nil
}

func (s *Server) forkMaterializerOrDefault() forkMaterializer {
	if s.forkMaterializer != nil {
		return s.forkMaterializer
	}
	return unimplementedForkMaterializer{}
}

func asConnectError(code connect.Code, err error) error {
	if err == nil {
		return nil
	}
	var cerr *connect.Error
	if errors.As(err, &cerr) {
		return err
	}
	return connect.NewError(code, err)
}

func forkBucketName(forkID string) string {
	return "spawnery-spawn-" + forkID
}

func (s *Server) forkNeedsJournalKeyDelivery(ctx context.Context, sourceID, sourceNodeID, targetNodeID string) (bool, error) {
	if targetNodeID != "" && targetNodeID == sourceNodeID {
		return false, nil
	}
	classes, err := s.classifyMounts(ctx, sourceID)
	if err != nil {
		return false, connect.NewError(connect.CodeInternal, err)
	}
	for _, class := range classes {
		if class == mountClassOwnerSealed {
			return true, nil
		}
	}
	return false, nil
}

func (s *Server) forkName(ctx context.Context, owner string, source store.Spawn, requested string) (string, error) {
	name := strings.TrimSpace(requested)
	if name != "" && len([]rune(name)) <= maxSpawnNameRunes {
		return name, nil
	}
	existing, err := s.st.Spawns().ListByOwner(ctx, owner)
	if err != nil {
		return "", connect.NewError(connect.CodeInternal, err)
	}
	taken := make(map[string]bool, len(existing))
	for _, row := range existing {
		taken[row.Name] = true
	}
	base := strings.TrimSpace(source.Name)
	if base == "" {
		base = source.ID
	}
	return nextSpawnName(base+" fork", taken), nil
}

func (s *Server) failForkAfterRow(ctx context.Context, forkID, transferSetID string, gen uint64, cause error) error {
	now := s.now()
	s.deliveryPending.clear(forkID)
	_ = s.st.TransferSets().SetStatus(context.WithoutCancel(ctx), transferSetID, store.TransferSetFailed, now.UnixNano())
	nodeID := ""
	if ts, err := s.st.TransferSets().Get(context.WithoutCancel(ctx), transferSetID); err == nil {
		nodeID = ts.TargetNodeID
	}
	if err := s.unwindFailedFork(context.WithoutCancel(ctx), failedForkUnwind{
		ForkID:        forkID,
		Generation:    gen,
		Bucket:        forkBucketName(forkID),
		NodeID:        nodeID,
		NowUnixNano:   now.UnixNano(),
		DeletedAtUnix: now.Unix(),
		Resources:     s.failedForkResources,
	}); err != nil {
		return connect.NewError(connect.CodeInternal, fmt.Errorf("fork failed (%v) and unwind failed: %w", cause, err))
	}
	return cause
}

func (s *Server) activateForkAndTransferSet(ctx context.Context, forkID, transferSetID, nodeID string, targetGeneration uint64) error {
	return s.st.WithTx(ctx, func(tx store.Store) error {
		if err := tx.Spawns().SetActive(ctx, forkID, nodeID, int64(targetGeneration)); err != nil {
			return fmt.Errorf("mark fork active: %w", err)
		}
		if err := tx.TransferSets().SetTargetNode(ctx, transferSetID, nodeID, s.now().UnixNano()); err != nil {
			return fmt.Errorf("update transfer set target node: %w", err)
		}
		if err := tx.TransferSets().SetStatus(ctx, transferSetID, store.TransferSetActive, s.now().UnixNano()); err != nil {
			return fmt.Errorf("mark transfer set active: %w", err)
		}
		return nil
	})
}

func (s *Server) restoreForkingSource(ctx context.Context, sourceID, leaseID string, generation int64) error {
	sp, err := s.st.Spawns().Get(ctx, sourceID)
	if err != nil {
		return err
	}
	if sp.Status == store.Active && sp.ForkCaptureDeadline == nil {
		return nil
	}
	if sp.Status != store.Forking {
		return store.ErrConflict
	}
	if _, err := s.st.Spawns().TransitionForkingRecovered(ctx, sourceID, leaseID, sp.StatusSeq, generation); errors.Is(err, store.ErrConflict) {
		current, getErr := s.st.Spawns().Get(ctx, sourceID)
		if getErr == nil && current.Status == store.Active && current.ForkCaptureDeadline == nil {
			return nil
		}
		return err
	} else if err != nil {
		return err
	}
	return nil
}

func (s *Server) restoreForkingSourceAfterUnpause(ctx context.Context, sourceID, leaseID string, generation int64) error {
	if err := s.forkPauseController().UnpauseIfPaused(ctx, sourceID, generation); err != nil {
		return err
	}
	return s.restoreForkingSource(ctx, sourceID, leaseID, generation)
}

func (s *Server) startFork(ctx context.Context, owner, sourceID string, fork store.Spawn, nodeID string, targetGeneration uint64, rootfsPins []store.RootfsArtifactPin, rootfsLocalOnly bool) (string, error) {
	placement := registry.Placement{Owner: owner, Image: fork.Image, TargetNodeID: nodeID}
	artifacts, err := s.st.Spawns().GetArtifacts(ctx, fork.ID)
	if err != nil {
		return "", connect.NewError(connect.CodeInternal, err)
	}
	mounts, err := s.st.Spawns().GetMounts(ctx, fork.ID)
	if err != nil {
		return "", connect.NewError(connect.CodeInternal, err)
	}
	requiredSecretIDs := startupSecretIDsForSpawn(artifacts, mounts)
	var env *authv1.AuthEnvelope
	var secrets []*nodev1.SealedSecret
	if s.intentEnabled {
		pi := buildPendingIntent(intent.OpForkSpawn, fork.ID, targetGeneration, nodeID, fork.Image, fork.AppRef, fork.Model, "", mounts, requiredSecretIDs)
		ch := s.pendingIntents.register(sourceID, owner, pi)
		defer s.pendingIntents.cleanup(sourceID)
		submission, err := s.pendingIntents.await(ctx, ch)
		if err != nil {
			return "", connect.NewError(connect.CodeDeadlineExceeded, fmt.Errorf("await fork SignedIntent: %w", err))
		}
		if err := validateSubmittedStartupSecrets(requiredSecretIDs, submission.Secrets); err != nil {
			return "", connect.NewError(connect.CodeFailedPrecondition, err)
		}
		if err := s.ensureStartupSecretsExist(ctx, owner, requiredSecretIDs); err != nil {
			return "", err
		}
		env = submission.Env
		secrets = submission.Secrets
	}
	rootfs := &rootfsRestorePins{SourceGeneration: targetGeneration, Pins: rootfsPins, LocalOnly: rootfsLocalOnly}
	return s.sched.Provision(ctx, fork.ID, fork.AppRef, fork.Model, fork.Name, fork.AppID, fork.RunnableID, fork.Mode,
		targetGeneration, placement, env, storeToNodeMounts(mounts), fork.BaseImageDigest, schedulerRootfsRestore(rootfs), storeToNodeArtifacts(artifacts), secrets)
}

func (s *Server) ForkSpawn(ctx context.Context, req *connect.Request[cpv1.ForkSpawnRequest]) (*connect.Response[cpv1.ForkSpawnResponse], error) {
	owner, ok := auth.OwnerFromContext(ctx)
	if !ok {
		return nil, connect.NewError(connect.CodeUnauthenticated, fmt.Errorf("no owner"))
	}
	sourceID := req.Msg.SpawnId
	targetNode := strings.TrimSpace(req.Msg.TargetNodeId)
	targetClass := strings.TrimSpace(req.Msg.TargetClass)
	if targetNode != "" && targetClass != "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("specify a target node id OR a class, not both"))
	}

	source, err := s.st.Spawns().Get(ctx, sourceID)
	if err != nil {
		return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("unknown spawn"))
	}
	if source.OwnerID != owner {
		return nil, connect.NewError(connect.CodePermissionDenied, fmt.Errorf("not your spawn"))
	}

	var resp *connect.Response[cpv1.ForkSpawnResponse]
	resp, err = s.forkSpawnClaimed(ctx, owner, sourceID, targetNode, targetClass, req.Msg.Name)
	if err != nil {
		return nil, err
	}
	return resp, nil
}

func (s *Server) forkSpawnClaimed(ctx context.Context, owner, sourceID, targetNode, targetClass, requestedName string) (*connect.Response[cpv1.ForkSpawnResponse], error) {
	source, err := s.st.Spawns().Get(ctx, sourceID)
	if err != nil {
		return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("unknown spawn"))
	}
	if source.OwnerID != owner {
		return nil, connect.NewError(connect.CodePermissionDenied, fmt.Errorf("not your spawn"))
	}
	if source.Status != store.Active {
		return nil, connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("spawn must be active to fork"))
	}
	if source.ClaimHolder != nil && source.ClaimDeadline != nil && *source.ClaimDeadline > time.Now().UnixNano() {
		return nil, connect.NewError(connect.CodeAborted, fmt.Errorf("spawn busy — transition in progress"))
	}
	live, ok, err := s.st.Spawns().LiveContainer(ctx, sourceID)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	if !ok || live.NodeID == "" {
		return nil, connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("spawn has no live source container"))
	}
	if err := s.checkSpawnQuota(ctx, owner); err != nil {
		return nil, err
	}

	if targetNode == "" && targetClass == "" {
		targetNode = live.NodeID
	}
	if targetNode != "" {
		exists, eligible := s.reg.TargetEligible(targetNode, owner)
		if !exists {
			return nil, connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("target node %q is not connected", targetNode))
		}
		if !eligible {
			return nil, connect.NewError(connect.CodePermissionDenied, fmt.Errorf("target node %q belongs to another owner", targetNode))
		}
	}

	headroomBytes, err := s.forkHeadroomBytes(ctx, source, live)
	if err != nil {
		return nil, connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("cannot check fork disk headroom: %w", err))
	}
	if targetClass != "" {
		ver, err := s.st.Apps().GetVersion(ctx, source.AppID, source.AppVersion)
		if err != nil {
			return nil, connect.NewError(connect.CodeInternal, err)
		}
		placement, err := s.placementFor(ctx, owner, source.AppID, ver)
		if err != nil {
			return nil, err
		}
		placement.Image = source.Image
		placement.RequireClass = targetClass
		placement.MinDiskFreeBytes = headroomBytes
		pickedNode, err := s.sched.PickNodeID(placement)
		if err != nil {
			return nil, err
		}
		targetNode = pickedNode
	}
	if err := s.guardCrossNodeDurability(ctx, sourceID, live.NodeID, targetNode, false); err != nil {
		return nil, err
	}
	if err := s.checkForkDiskHeadroom(owner, source, targetNode, headroomBytes); err != nil {
		return nil, err
	}
	crossNode := targetNode != "" && targetNode != live.NodeID
	needsJournalKeyDelivery, err := s.forkNeedsJournalKeyDelivery(ctx, sourceID, live.NodeID, targetNode)
	if err != nil {
		return nil, err
	}
	transferKeyStatus := store.TransferKeySourceReady
	if crossNode {
		transferKeyStatus = store.TransferKeyTargetReady
	}

	forkUUID, err := uuid.NewV7()
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	forkID := forkUUID.String()
	transferSetID := uuid.NewString()
	name, err := s.forkName(ctx, owner, source, requestedName)
	if err != nil {
		return nil, err
	}
	mounts, err := s.st.Spawns().GetMounts(ctx, sourceID)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	artifacts, err := s.st.Spawns().GetArtifacts(ctx, sourceID)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}

	now := s.now()
	forkedAt := now.Unix()
	parentID := sourceID
	fork := store.Spawn{
		ID: forkID, OwnerID: owner, Name: name, AppID: source.AppID, AppVersion: source.AppVersion,
		AppRef: source.AppRef, Pinned: source.Pinned, Model: source.Model, Image: source.Image,
		RunnableID: source.RunnableID, Mode: source.Mode, BaseImageDigest: source.BaseImageDigest,
		Status: store.Starting, CreatedAt: forkedAt, LastUsedAt: forkedAt,
		ParentSpawnID: &parentID, ForkedAt: &forkedAt,
	}
	sourceGeneration := uint64(live.Generation)
	targetGeneration := uint64(1)
	if err := s.st.WithTx(ctx, func(tx store.Store) error {
		if err := tx.Spawns().Create(ctx, fork, mounts); err != nil {
			return err
		}
		if err := tx.Spawns().AddArtifacts(ctx, forkID, artifacts); err != nil {
			return err
		}
		return tx.TransferSets().Create(ctx, store.TransferSet{
			ID:                transferSetID,
			Kind:              store.TransferSetFork,
			SpawnID:           forkID,
			SourceSpawnID:     sourceID,
			ForkSpawnID:       forkID,
			SourceGeneration:  sourceGeneration,
			TargetGeneration:  targetGeneration,
			SourceNodeID:      live.NodeID,
			TargetNodeID:      targetNode,
			BaseImageDigest:   source.BaseImageDigest,
			TransferKeyStatus: transferKeyStatus,
			Status:            store.TransferSetPending,
			CreatedAt:         now.UnixNano(),
			UpdatedAt:         now.UnixNano(),
		})
	}); err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("create fork rows: %w", err))
	}
	if needsJournalKeyDelivery {
		s.deliveryPending.mark(forkID)
	}

	if err := s.st.TransferSets().SetStatus(ctx, transferSetID, store.TransferSetCapturing, s.now().UnixNano()); err != nil {
		return nil, s.failForkAfterRow(ctx, forkID, transferSetID, targetGeneration, connect.NewError(connect.CodeInternal, fmt.Errorf("mark transfer set capturing: %w", err)))
	}
	headroomBytes, err = s.forkHeadroomBytes(ctx, source, live)
	if err != nil {
		return nil, s.failForkAfterRow(ctx, forkID, transferSetID, targetGeneration,
			connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("cannot check fork disk headroom: %w", err)))
	}
	if err := s.checkForkDiskHeadroom(owner, source, targetNode, headroomBytes); err != nil {
		return nil, s.failForkAfterRow(ctx, forkID, transferSetID, targetGeneration, err)
	}
	if err := s.st.TransferSets().SetStatus(ctx, transferSetID, store.TransferSetRestoring, s.now().UnixNano()); err != nil {
		return nil, s.failForkAfterRow(ctx, forkID, transferSetID, targetGeneration, connect.NewError(connect.CodeInternal, fmt.Errorf("mark transfer set restoring: %w", err)))
	}

	barrierReq := forkMaterializeRequest{
		SourceSpawn:      source,
		ForkSpawn:        fork,
		TransferSetID:    transferSetID,
		SourceGeneration: sourceGeneration,
		TargetGeneration: targetGeneration,
		SourceNodeID:     live.NodeID,
		TargetNodeID:     targetNode,
		TargetClass:      targetClass,
	}
	barrierAcquired := false
	defer func() {
		if barrierAcquired {
			s.releaseForkTurnBoundary(context.WithoutCancel(ctx), barrierReq)
		}
	}()
	if err := s.waitForkTurnBoundary(ctx, barrierReq); err != nil {
		return nil, s.failForkAfterRow(ctx, forkID, transferSetID, targetGeneration, asConnectError(connect.CodeInternal, err))
	}
	barrierAcquired = true

	unlockSource := s.locks.Lock(sourceID)
	sourceLockHeld := true
	claim, claimErr := s.acquireClaim(ctx, sourceID)
	releaseSource := func() {
		if claim != nil {
			claim.Release()
			claim = nil
		}
		if sourceLockHeld {
			unlockSource()
			sourceLockHeld = false
		}
	}
	defer releaseSource()
	if claimErr != nil {
		return nil, s.failForkAfterRow(ctx, forkID, transferSetID, targetGeneration, claimErr)
	}
	claimCtx := claim.ctx
	leaseID := claim.leaseID
	currentSource, err := s.st.Spawns().Get(claimCtx, sourceID)
	if err != nil {
		return nil, s.failForkAfterRow(ctx, forkID, transferSetID, targetGeneration, connect.NewError(connect.CodeNotFound, fmt.Errorf("unknown spawn")))
	}
	if currentSource.Status != store.Active {
		return nil, s.failForkAfterRow(ctx, forkID, transferSetID, targetGeneration,
			connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("spawn must still be active to fork")))
	}
	currentLive, ok, err := s.st.Spawns().LiveContainer(claimCtx, sourceID)
	if err != nil {
		return nil, s.failForkAfterRow(ctx, forkID, transferSetID, targetGeneration, connect.NewError(connect.CodeInternal, err))
	}
	if !ok || currentLive.NodeID == "" {
		return nil, s.failForkAfterRow(ctx, forkID, transferSetID, targetGeneration,
			connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("spawn has no live source container")))
	}
	if currentLive.Generation != int64(sourceGeneration) || currentLive.NodeID != live.NodeID {
		return nil, s.failForkAfterRow(ctx, forkID, transferSetID, targetGeneration,
			connect.NewError(connect.CodeAborted, fmt.Errorf("source changed while fork was waiting for turn boundary")))
	}

	captureDeadline := s.now().Add(defaultForkCaptureTimeout).UnixNano()
	if err := s.st.Spawns().SetForking(claimCtx, sourceID, currentLive.Generation, captureDeadline); err != nil {
		return nil, s.failForkAfterRow(ctx, forkID, transferSetID, targetGeneration, connect.NewError(connect.CodeInternal, fmt.Errorf("mark source forking: %w", err)))
	}
	restoreSource := func() error {
		return s.restoreForkingSourceAfterUnpause(context.WithoutCancel(ctx), sourceID, leaseID, currentLive.Generation)
	}
	restoreOnFailure := true
	defer func() {
		if restoreOnFailure {
			if err := restoreSource(); err != nil {
				log.Printf("restore source %s after failed fork: %v", sourceID, err)
			}
		}
	}()

	materializeReq := barrierReq
	materializeReq.Mounts = mounts
	materializeReq.Artifacts = artifacts
	materializeCtx, cancelMaterialize := context.WithCancel(ctx)
	materializeDone := make(chan struct{})
	sourceReleased := make(chan struct{})
	var sourceReleasedOnce sync.Once
	go func() {
		select {
		case <-claimCtx.Done():
			cancelMaterialize()
		case <-sourceReleased:
		case <-materializeDone:
		}
	}()
	defer func() {
		close(materializeDone)
		cancelMaterialize()
	}()
	result, err := s.materializeForkWithSourceRestored(materializeCtx, materializeReq, func() error {
		if err := s.restoreForkingSource(context.WithoutCancel(ctx), sourceID, leaseID, currentLive.Generation); err != nil {
			return err
		}
		restoreOnFailure = false
		sourceReleasedOnce.Do(func() { close(sourceReleased) })
		releaseSource()
		if barrierAcquired {
			s.releaseForkTurnBoundary(context.WithoutCancel(ctx), barrierReq)
			barrierAcquired = false
		}
		return nil
	})
	if err != nil {
		return nil, s.failForkAfterRow(ctx, forkID, transferSetID, targetGeneration, asConnectError(connect.CodeInternal, err))
	}
	if err := s.st.TransferSets().SetPins(ctx, transferSetID, sourceGeneration, result.MountPins, result.RootfsPins, s.now().UnixNano()); err != nil {
		return nil, s.failForkAfterRow(ctx, forkID, transferSetID, targetGeneration, connect.NewError(connect.CodeInternal, fmt.Errorf("record fork transfer pins: %w", err)))
	}
	nodeID := strings.TrimSpace(result.NodeID)
	if nodeID == "" {
		nodeID = targetNode
	}
	nodeID, err = s.startFork(ctx, owner, sourceID, fork, nodeID, targetGeneration, result.RootfsPins, nodeID == live.NodeID)
	if err != nil {
		s.rt.StopOnNode(forkID)
		s.rt.Drop(forkID)
		return nil, s.failForkAfterRow(ctx, forkID, transferSetID, targetGeneration, asConnectError(connect.CodeInternal, err))
	}
	if err := s.activateForkAndTransferSet(ctx, forkID, transferSetID, nodeID, targetGeneration); err != nil {
		s.rt.StopOnNode(forkID)
		s.rt.Drop(forkID)
		return nil, s.failForkAfterRow(ctx, forkID, transferSetID, targetGeneration, connect.NewError(connect.CodeInternal, err))
	}

	return connect.NewResponse(&cpv1.ForkSpawnResponse{
		ForkSpawnId:   forkID,
		TransferSetId: transferSetID,
		NodeId:        nodeID,
	}), nil
}
