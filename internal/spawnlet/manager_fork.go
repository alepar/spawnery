package spawnlet

import (
	"bytes"
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"

	"spawnery/internal/runtime"
	"spawnery/internal/storage/journal"
)

type ForkSameNodeRequest struct {
	SourceSpawnID    string
	ForkSpawnID      string
	TransferSetID    string
	SourceGeneration uint64
	TargetGeneration uint64
	SourceRestored   func() error
}

type ForkSameNodeResult struct {
	NodeID          string
	MountPins       map[string]string
	RootfsArtifacts []RootfsArtifact
}

type generationHold interface {
	Release()
}

func (m *Manager) SetGenerationKeyManager(g *journal.GenerationKeyManager) {
	m.generationKeys = g
	if g == nil {
		m.forkGenerationHold = nil
		m.forkGenerationHoldRequired = false
		return
	}
	m.forkGenerationHold = func(spawnID string, gen uint64, reason string) generationHold {
		h := g.HoldGeneration(spawnID, gen, reason)
		if h == nil {
			return nil
		}
		return h
	}
	m.forkGenerationHoldRequired = true
}

func (m *Manager) RevokeForkGeneration(ctx context.Context, forkID string, gen uint64) error {
	if m.generationKeys == nil {
		return fmt.Errorf("failed fork cleanup: generation key manager is not wired")
	}
	return m.generationKeys.RevokeGeneration(context.WithoutCancel(ctx), forkID, gen)
}

func (m *Manager) EmptyForkBucket(ctx context.Context, forkID, bucket string) error {
	if m.generationKeys == nil {
		return fmt.Errorf("failed fork cleanup: generation key manager is not wired")
	}
	if want := m.generationKeys.BucketFor(forkID); bucket != want {
		return fmt.Errorf("failed fork cleanup: bucket %q does not match fork %s bucket %q", bucket, forkID, want)
	}
	return m.generationKeys.EmptyBucket(context.WithoutCancel(ctx), bucket)
}

func (m *Manager) DropForkBucket(ctx context.Context, forkID, bucket string) error {
	if m.generationKeys == nil {
		return fmt.Errorf("failed fork cleanup: generation key manager is not wired")
	}
	if want := m.generationKeys.BucketFor(forkID); bucket != want {
		return fmt.Errorf("failed fork cleanup: bucket %q does not match fork %s bucket %q", bucket, forkID, want)
	}
	return m.generationKeys.DropBucket(context.WithoutCancel(ctx), bucket)
}

func (m *Manager) RequireForkGenerationHold(required bool) {
	m.forkGenerationHoldRequired = required
}

func (m *Manager) ForkSameNode(ctx context.Context, req ForkSameNodeRequest) (ForkSameNodeResult, error) {
	if req.SourceSpawnID == "" || req.ForkSpawnID == "" {
		return ForkSameNodeResult{}, fmt.Errorf("fork same-node: source and fork spawn ids are required")
	}
	targetGen := req.TargetGeneration
	if targetGen == 0 {
		targetGen = 1
	}
	sp, ok := m.store.Get(req.SourceSpawnID)
	if !ok {
		return ForkSameNodeResult{}, fmt.Errorf("fork same-node: unknown source spawn %s", req.SourceSpawnID)
	}
	if req.SourceGeneration != 0 && sp.Generation != req.SourceGeneration {
		return ForkSameNodeResult{}, fmt.Errorf("fork same-node: stale source generation %d, live %d", req.SourceGeneration, sp.Generation)
	}
	if m.journal == nil {
		return ForkSameNodeResult{}, fmt.Errorf("fork same-node: journaler is required to seed fork repo")
	}
	if m.forkGenerationHold == nil {
		if m.forkGenerationHoldRequired {
			return ForkSameNodeResult{}, fmt.Errorf("fork same-node: generation hold is required but no generation key manager is wired")
		}
	} else {
		hold := m.forkGenerationHold(sp.ID, sp.Generation, "fork "+req.TransferSetID)
		if hold == nil && m.forkGenerationHoldRequired {
			return ForkSameNodeResult{}, fmt.Errorf("fork same-node: generation hold is required but was not acquired")
		}
		if hold != nil {
			defer hold.Release()
		}
	}

	if _, err := m.journal.WarmSnapshot(ctx, sp.ID, sp.Generation, sp.JournalMounts); err != nil {
		return ForkSameNodeResult{}, fmt.Errorf("fork same-node: warm source snapshot: %w", err)
	}
	for _, w := range m.takeWatchers(sp) {
		w.Stop()
	}
	sourceRestored := false
	restoreSource := func() error {
		if sourceRestored {
			return nil
		}
		cleanupCtx := context.WithoutCancel(ctx)
		if err := m.UnpauseIfPaused(cleanupCtx, sp.ID, int64(sp.Generation)); err != nil {
			return fmt.Errorf("unpause source %s: %w", sp.ID, err)
		}
		if err := m.journal.Close(cleanupCtx, sp.ID); err != nil {
			return fmt.Errorf("close source journal %s before watcher restart: %w", sp.ID, err)
		}
		m.setWatchers(sp, m.startJournalWatchers(sp.ID, sp.Generation, sp.JournalMounts))
		sourceRestored = true
		return nil
	}
	defer func() {
		if err := restoreSource(); err != nil {
			log.Printf("fork same-node: restore source %s: %v", sp.ID, err)
		}
	}()

	h := m.podHandleForSpawn(sp)
	h.SpawnID = sp.ID
	h.BaseImageRef = sp.LaunchImageRef
	if h.BaseImageRef == "" {
		h.BaseImageRef = sp.BaseImageDigest
	}
	if err := m.pod.Pause(ctx, h); err != nil {
		return ForkSameNodeResult{}, fmt.Errorf("fork same-node: pause source %s: %w", sp.ID, err)
	}
	if m.forkSyncFn != nil {
		if err := m.forkSyncFn(ctx); err != nil {
			return ForkSameNodeResult{}, fmt.Errorf("fork same-node: sync host: %w", err)
		}
	}

	sourcePins, err := m.journal.FinalSnapshot(ctx, sp.ID, sp.Generation, sp.JournalMounts)
	if err != nil {
		return ForkSameNodeResult{}, fmt.Errorf("fork same-node: final source snapshot: %w", err)
	}
	if _, err := m.pod.CaptureDeltaAs(ctx, h, req.ForkSpawnID); err != nil {
		return ForkSameNodeResult{}, fmt.Errorf("fork same-node: capture rootfs as fork: %w", err)
	}
	if err := restoreSource(); err != nil {
		return ForkSameNodeResult{}, fmt.Errorf("fork same-node: restore source after capture: %w", err)
	}
	if req.SourceRestored != nil {
		if err := req.SourceRestored(); err != nil {
			return ForkSameNodeResult{}, fmt.Errorf("fork same-node: report source restored: %w", err)
		}
	}

	var rootfsPayload bytes.Buffer
	if err := m.pod.ExportDelta(ctx, req.ForkSpawnID, &rootfsPayload); err != nil {
		return ForkSameNodeResult{}, fmt.Errorf("fork same-node: export fork rootfs delta: %w", err)
	}
	rootfsArtifacts, err := m.copyForkRootfsArtifacts(ctx, sp, req.ForkSpawnID, targetGen)
	if err != nil {
		return ForkSameNodeResult{}, err
	}

	stageRoot, err := os.MkdirTemp(m.cfg.DataRoot, "fork-seed-"+req.ForkSpawnID+"-")
	if err != nil {
		return ForkSameNodeResult{}, fmt.Errorf("fork same-node: create fork seed staging: %w", err)
	}
	defer func() { _ = os.RemoveAll(stageRoot) }()

	forkMounts := make([]journal.Mount, 0, len(sp.JournalMounts))
	for _, mt := range sp.JournalMounts {
		pin, ok := sourcePins[mt.Name]
		if !ok {
			continue
		}
		hostDir := filepath.Join(stageRoot, mt.Name)
		if err := os.MkdirAll(hostDir, 0o755); err != nil {
			return ForkSameNodeResult{}, fmt.Errorf("fork same-node: stage mount %s: %w", mt.Name, err)
		}
		if err := m.journal.RestoreGeneration(ctx, sp.ID, sp.Generation, mt.Name, pin, hostDir); err != nil {
			return ForkSameNodeResult{}, fmt.Errorf("fork same-node: restore source mount %s: %w", mt.Name, err)
		}
		forkMounts = append(forkMounts, journal.Mount{Name: mt.Name, HostDir: hostDir, Class: mt.Class})
	}

	rootfsDesc := journal.ArtifactDescriptor{
		Type:            journal.ArtifactRootfsDelta,
		Sequence:        nextRootfsArtifactSequence(sp.DeltaDepth, rootfsArtifacts),
		BaseImageDigest: sp.BaseImageDigest,
		Format:          journal.ArtifactFormatOCILayout,
		ProducerNodeID:  m.cfg.NodeID,
		ProducerRuntime: m.rootfsProducerRuntime(),
	}
	storedRootfs, err := m.journal.PutArtifact(ctx, req.ForkSpawnID, targetGen, rootfsDesc, bytes.NewReader(rootfsPayload.Bytes()))
	if err != nil {
		return ForkSameNodeResult{}, fmt.Errorf("fork same-node: put fork rootfs artifact: %w", err)
	}

	forkPins, err := m.journal.FinalSnapshot(ctx, req.ForkSpawnID, targetGen, forkMounts)
	if err != nil {
		return ForkSameNodeResult{}, fmt.Errorf("fork same-node: final fork seed snapshot: %w", err)
	}
	if m.journalState != nil {
		if err := m.journalState.Save(req.ForkSpawnID, journalRecord{Generation: targetGen, Manifests: forkPins}); err != nil {
			log.Printf("fork same-node: save fork journal state %s: %v", req.ForkSpawnID, err)
		}
	}
	mountPins := make(map[string]string, len(forkPins))
	for name, pin := range forkPins {
		mountPins[name] = pin.String()
	}
	rootfsArtifacts = append(rootfsArtifacts, rootfsArtifactFromJournal(storedRootfs))
	return ForkSameNodeResult{
		NodeID:          m.cfg.NodeID,
		MountPins:       mountPins,
		RootfsArtifacts: rootfsArtifacts,
	}, nil
}

func (m *Manager) copyForkRootfsArtifacts(ctx context.Context, sp *Spawn, forkID string, targetGen uint64) ([]RootfsArtifact, error) {
	if len(sp.RootfsArtifacts) == 0 {
		return nil, nil
	}
	out := make([]RootfsArtifact, 0, len(sp.RootfsArtifacts))
	for _, art := range sp.RootfsArtifacts {
		if art.ArtifactID == "" {
			return nil, fmt.Errorf("fork same-node: source rootfs artifact has empty artifact id")
		}
		sourceGen := art.Generation
		if sourceGen == 0 {
			sourceGen = sp.Generation
		}
		var payload bytes.Buffer
		desc, err := m.journal.GetArtifact(ctx, sp.ID, sourceGen, art.ArtifactID, &payload)
		if err != nil {
			return nil, fmt.Errorf("fork same-node: get inherited rootfs artifact %s: %w", art.ArtifactID, err)
		}
		desc = forkRootfsCopyDescriptor(desc, art, sp.BaseImageDigest, m.cfg.NodeID, m.rootfsProducerRuntime())
		stored, err := m.journal.PutArtifact(ctx, forkID, targetGen, desc, bytes.NewReader(payload.Bytes()))
		if err != nil {
			return nil, fmt.Errorf("fork same-node: put inherited rootfs artifact %s: %w", art.ArtifactID, err)
		}
		out = append(out, rootfsArtifactFromJournal(stored))
	}
	return out, nil
}

func forkRootfsCopyDescriptor(desc journal.ArtifactDescriptor, art RootfsArtifact, baseImageDigest, nodeID, producerRuntime string) journal.ArtifactDescriptor {
	if desc.ArtifactID == "" {
		desc.ArtifactID = art.ArtifactID
	}
	if desc.Type == "" {
		desc.Type = journal.ArtifactRootfsDelta
	}
	if desc.Sequence == 0 {
		desc.Sequence = art.Sequence
	}
	if desc.BaseImageDigest == "" {
		desc.BaseImageDigest = art.BaseImageDigest
	}
	if desc.BaseImageDigest == "" {
		desc.BaseImageDigest = baseImageDigest
	}
	if desc.Format == "" {
		desc.Format = art.Format
	}
	if desc.Format == "" {
		desc.Format = journal.ArtifactFormatOCILayout
	}
	if desc.ContentDigest == "" {
		desc.ContentDigest = art.ContentDigest
	}
	if desc.UncompressedSize == 0 {
		desc.UncompressedSize = art.UncompressedSize
	}
	if desc.ProducerNodeID == "" {
		desc.ProducerNodeID = art.ProducerNodeID
	}
	if desc.ProducerNodeID == "" {
		desc.ProducerNodeID = nodeID
	}
	if desc.ProducerRuntime == "" {
		desc.ProducerRuntime = art.ProducerRuntime
	}
	if desc.ProducerRuntime == "" {
		desc.ProducerRuntime = producerRuntime
	}
	return desc
}

func nextRootfsArtifactSequence(deltaDepth int, artifacts []RootfsArtifact) int {
	next := deltaDepth + 1
	for _, art := range artifacts {
		if art.Sequence >= next {
			next = art.Sequence + 1
		}
	}
	return next
}

func (m *Manager) UnpauseIfPaused(ctx context.Context, spawnID string, generation int64) error {
	sp, ok := m.store.Get(spawnID)
	if !ok {
		return fmt.Errorf("unpause if paused: source spawn %s is not tracked on this node", spawnID)
	}
	if generation != 0 && sp.Generation != uint64(generation) {
		return nil
	}
	if err := m.pod.Unpause(context.WithoutCancel(ctx), m.podHandleForSpawn(sp)); err != nil {
		msg := strings.ToLower(err.Error())
		if strings.Contains(msg, "not paused") || strings.Contains(msg, "is not paused") ||
			strings.Contains(msg, "already running") {
			return nil
		}
		return err
	}
	return nil
}

func (m *Manager) podHandleForSpawn(sp *Spawn) *runtime.PodHandle {
	return &runtime.PodHandle{
		SpawnID:   sp.ID,
		PodIP:     sp.PodIP,
		AgentID:   sp.AgentID,
		NetnsPath: sp.NetnsPath,
		SidecarID: sp.SidecarID,
		SandboxID: sp.SandboxID,
	}
}
