package cp

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"connectrpc.com/connect"
	"github.com/google/uuid"

	cpv1 "spawnery/gen/cp/v1"
	"spawnery/internal/cp/auth"
	"spawnery/internal/cp/registry"
	"spawnery/internal/cp/store"
)

const forkHeadroomMultiplier = int64(3)

var errForkFootprintUnknown = errors.New("fork source disk footprint is unknown")

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
	NodeID string
}

type forkFootprintEstimator interface {
	ForkFootprintBytes(context.Context, store.Spawn, store.Container) (int64, error)
}

type unimplementedForkMaterializer struct{}

func (unimplementedForkMaterializer) MaterializeFork(context.Context, forkMaterializeRequest) (forkMaterializeResult, error) {
	return forkMaterializeResult{}, connect.NewError(connect.CodeUnimplemented, fmt.Errorf("fork materialization is not implemented yet"))
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
	_ = s.st.TransferSets().SetStatus(context.WithoutCancel(ctx), transferSetID, store.TransferSetFailed, now.UnixNano())
	if err := s.unwindFailedFork(context.WithoutCancel(ctx), failedForkUnwind{
		ForkID:        forkID,
		Generation:    gen,
		Bucket:        forkBucketName(forkID),
		NowUnixNano:   now.UnixNano(),
		DeletedAtUnix: now.Unix(),
		Resources:     s.failedForkResources,
	}); err != nil {
		return connect.NewError(connect.CodeInternal, fmt.Errorf("fork failed (%v) and unwind failed: %w", cause, err))
	}
	return cause
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

	unlock := s.locks.Lock(sourceID)
	defer unlock()

	source, err := s.st.Spawns().Get(ctx, sourceID)
	if err != nil {
		return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("unknown spawn"))
	}
	if source.OwnerID != owner {
		return nil, connect.NewError(connect.CodePermissionDenied, fmt.Errorf("not your spawn"))
	}

	var resp *connect.Response[cpv1.ForkSpawnResponse]
	if err := s.withClaim(ctx, sourceID, func(claimCtx context.Context, leaseID string) error {
		_ = leaseID
		var err error
		resp, err = s.forkSpawnClaimed(claimCtx, owner, sourceID, targetNode, targetClass, req.Msg.Name)
		return err
	}); err != nil {
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
			TransferKeyStatus: store.TransferKeyPending,
			Status:            store.TransferSetPending,
			CreatedAt:         now.UnixNano(),
			UpdatedAt:         now.UnixNano(),
		})
	}); err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("create fork rows: %w", err))
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

	result, err := s.forkMaterializerOrDefault().MaterializeFork(ctx, forkMaterializeRequest{
		SourceSpawn:      source,
		ForkSpawn:        fork,
		TransferSetID:    transferSetID,
		SourceGeneration: sourceGeneration,
		TargetGeneration: targetGeneration,
		SourceNodeID:     live.NodeID,
		TargetNodeID:     targetNode,
		TargetClass:      targetClass,
		Mounts:           mounts,
		Artifacts:        artifacts,
	})
	if err != nil {
		return nil, s.failForkAfterRow(ctx, forkID, transferSetID, targetGeneration, asConnectError(connect.CodeInternal, err))
	}
	nodeID := strings.TrimSpace(result.NodeID)
	if nodeID == "" {
		nodeID = targetNode
	}
	if err := s.st.Spawns().SetActive(ctx, forkID, nodeID, int64(targetGeneration)); err != nil {
		return nil, s.failForkAfterRow(ctx, forkID, transferSetID, targetGeneration, connect.NewError(connect.CodeInternal, fmt.Errorf("mark fork active: %w", err)))
	}
	if err := s.st.TransferSets().SetTargetNode(ctx, transferSetID, nodeID, s.now().UnixNano()); err != nil {
		return nil, s.failForkAfterRow(ctx, forkID, transferSetID, targetGeneration, connect.NewError(connect.CodeInternal, fmt.Errorf("update transfer set target node: %w", err)))
	}
	if err := s.st.TransferSets().SetStatus(ctx, transferSetID, store.TransferSetActive, s.now().UnixNano()); err != nil {
		return nil, s.failForkAfterRow(ctx, forkID, transferSetID, targetGeneration, connect.NewError(connect.CodeInternal, fmt.Errorf("mark transfer set active: %w", err)))
	}

	return connect.NewResponse(&cpv1.ForkSpawnResponse{
		ForkSpawnId:   forkID,
		TransferSetId: transferSetID,
		NodeId:        nodeID,
	}), nil
}
