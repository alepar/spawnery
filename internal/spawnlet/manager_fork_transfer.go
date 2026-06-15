package spawnlet

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"spawnery/internal/pki"
	"spawnery/internal/secrets/seal"
	"spawnery/internal/secrets/subkey"
	"spawnery/internal/storage/journal"
)

type ForkTransferExportRequest struct {
	SourceSpawnID       string
	ForkSpawnID         string
	TransferSetID       string
	SourceGeneration    uint64
	TargetGeneration    uint64
	TargetNodeID        string
	TargetNodeClass     string
	TargetNodeOwner     string
	TargetSignedSubKey  []byte
	TargetNodeCertChain []byte
	NodeRootPEM         []byte
	SourceRestored      func() error
}

type ForkTransferExportResult struct {
	SealedTransferKey []byte
	Payload           []byte
}

type ForkTransferImportRequest struct {
	SourceSpawnID     string
	ForkSpawnID       string
	TransferSetID     string
	TargetGeneration  uint64
	SealedTransferKey []byte
	Payload           []byte
}

type ForkTransferImportResult struct {
	NodeID          string
	MountPins       map[string]string
	RootfsArtifacts []RootfsArtifact
}

type TransferKeyOpener interface {
	OpenForkTransferKey(sealed []byte, forkID string, generation uint64, transferSetID string) ([]byte, error)
}

func (m *Manager) ForkTransferExport(ctx context.Context, req ForkTransferExportRequest) (ForkTransferExportResult, error) {
	if req.SourceSpawnID == "" || req.ForkSpawnID == "" || req.TransferSetID == "" {
		return ForkTransferExportResult{}, fmt.Errorf("fork transfer export: source spawn id, fork spawn id, and transfer set id are required")
	}
	targetGen := req.TargetGeneration
	if targetGen == 0 {
		targetGen = 1
	}
	sp, ok := m.store.Get(req.SourceSpawnID)
	if !ok {
		return ForkTransferExportResult{}, fmt.Errorf("fork transfer export: unknown source spawn %s", req.SourceSpawnID)
	}
	if req.SourceGeneration != 0 && sp.Generation != req.SourceGeneration {
		return ForkTransferExportResult{}, fmt.Errorf("fork transfer export: stale source generation %d, live %d", req.SourceGeneration, sp.Generation)
	}
	if m.journal == nil {
		return ForkTransferExportResult{}, fmt.Errorf("fork transfer export: journaler is required to seed fork repo")
	}
	if err := requirePortableRootfsHistory("fork transfer export", sp.ID, sp.DeltaDepth, sp.RootfsArtifacts); err != nil {
		return ForkTransferExportResult{}, err
	}
	var signed subkey.SignedSubKey
	if err := json.Unmarshal(req.TargetSignedSubKey, &signed); err != nil {
		return ForkTransferExportResult{}, fmt.Errorf("fork transfer export: decode target SignedSubKey: %w", err)
	}
	leafPEM, chainPEM, err := splitPEMCertificateChain(req.TargetNodeCertChain)
	if err != nil {
		return ForkTransferExportResult{}, fmt.Errorf("fork transfer export: split target cert chain: %w", err)
	}
	expect, err := forkTransferExpectation(req.TargetNodeClass, req.TargetNodeOwner)
	if err != nil {
		return ForkTransferExportResult{}, err
	}

	transferKey := make([]byte, 32)
	if _, err := rand.Read(transferKey); err != nil {
		return ForkTransferExportResult{}, fmt.Errorf("fork transfer export: generate transfer key: %w", err)
	}
	defer zeroBytes(transferKey)
	sealedKey, _, err := subkey.SealTransferKeyForNode(transferKey, leafPEM, chainPEM, req.NodeRootPEM, signed, expect, subkey.AllowAll{}, seal.InFlightAAD{
		SpawnID:    req.ForkSpawnID,
		Generation: targetGen,
		DeliveryID: req.TransferSetID,
	}, time.Now())
	if err != nil {
		return ForkTransferExportResult{}, fmt.Errorf("fork transfer export: seal transfer key: %w", err)
	}

	if m.forkGenerationHold == nil {
		if m.forkGenerationHoldRequired {
			return ForkTransferExportResult{}, fmt.Errorf("fork transfer export: generation hold is required but no generation key manager is wired")
		}
	} else {
		hold := m.forkGenerationHold(sp.ID, sp.Generation, "fork-transfer "+req.TransferSetID)
		if hold == nil && m.forkGenerationHoldRequired {
			return ForkTransferExportResult{}, fmt.Errorf("fork transfer export: generation hold is required but was not acquired")
		}
		if hold != nil {
			defer hold.Release()
		}
	}

	if _, err := m.journal.WarmSnapshot(ctx, sp.ID, sp.Generation, sp.JournalMounts); err != nil {
		return ForkTransferExportResult{}, fmt.Errorf("fork transfer export: warm source snapshot: %w", err)
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
			log.Printf("fork transfer export: restore source %s: %v", sp.ID, err)
		}
	}()

	h := m.podHandleForSpawn(sp)
	h.SpawnID = sp.ID
	h.BaseImageRef = sp.LaunchImageRef
	if h.BaseImageRef == "" {
		h.BaseImageRef = sp.BaseImageDigest
	}
	if err := m.pod.Pause(ctx, h); err != nil {
		return ForkTransferExportResult{}, fmt.Errorf("fork transfer export: pause source %s: %w", sp.ID, err)
	}
	if m.forkSyncFn != nil {
		if err := m.forkSyncFn(ctx); err != nil {
			return ForkTransferExportResult{}, fmt.Errorf("fork transfer export: sync host: %w", err)
		}
	}
	sourcePins, err := m.journal.FinalSnapshot(ctx, sp.ID, sp.Generation, sp.JournalMounts)
	if err != nil {
		return ForkTransferExportResult{}, fmt.Errorf("fork transfer export: final source snapshot: %w", err)
	}
	if _, err := m.pod.CaptureDeltaAs(ctx, h, req.ForkSpawnID); err != nil {
		return ForkTransferExportResult{}, fmt.Errorf("fork transfer export: capture rootfs as fork: %w", err)
	}
	if err := restoreSource(); err != nil {
		return ForkTransferExportResult{}, fmt.Errorf("fork transfer export: restore source after capture: %w", err)
	}
	releaseSourceJournalCloseHold := func() {}
	if holder, ok := m.journal.(journalCloseHolder); ok {
		releaseSourceJournalCloseHold = holder.HoldClose(sp.ID)
	}
	defer releaseSourceJournalCloseHold()
	if req.SourceRestored != nil {
		if err := req.SourceRestored(); err != nil {
			return ForkTransferExportResult{}, fmt.Errorf("fork transfer export: report source restored: %w", err)
		}
	}

	stageRoot, err := os.MkdirTemp(m.cfg.DataRoot, "fork-transfer-"+req.ForkSpawnID+"-")
	if err != nil {
		return ForkTransferExportResult{}, fmt.Errorf("fork transfer export: create staging: %w", err)
	}
	defer func() { _ = os.RemoveAll(stageRoot) }()

	payload := journal.ForkTransferPayload{
		SourceSpawnID: req.SourceSpawnID,
		ForkSpawnID:   req.ForkSpawnID,
		Mounts:        make([]journal.ForkTransferMount, 0, len(sp.JournalMounts)),
	}
	for _, mt := range sp.JournalMounts {
		pin, ok := sourcePins[mt.Name]
		if !ok {
			continue
		}
		hostDir := filepath.Join(stageRoot, "mounts", mt.Name)
		if err := os.MkdirAll(hostDir, 0o755); err != nil {
			return ForkTransferExportResult{}, fmt.Errorf("fork transfer export: stage mount %s: %w", mt.Name, err)
		}
		if err := m.journal.RestoreGeneration(ctx, sp.ID, sp.Generation, mt.Name, pin, hostDir); err != nil {
			return ForkTransferExportResult{}, fmt.Errorf("fork transfer export: restore source mount %s: %w", mt.Name, err)
		}
		payload.Mounts = append(payload.Mounts, journal.ForkTransferMount{Name: mt.Name, Class: mt.Class, HostDir: hostDir})
	}

	inherited, err := sortedPortableRootfsArtifacts("fork transfer export", sp.ID, sp.DeltaDepth, sp.RootfsArtifacts)
	if err != nil {
		return ForkTransferExportResult{}, err
	}
	for _, art := range inherited {
		sourceGen := art.Generation
		if sourceGen == 0 {
			sourceGen = sp.Generation
		}
		var rootfs bytes.Buffer
		desc, err := m.journal.GetArtifact(ctx, sp.ID, sourceGen, art.ArtifactID, &rootfs)
		if err != nil {
			return ForkTransferExportResult{}, fmt.Errorf("fork transfer export: get inherited rootfs artifact %s: %w", art.ArtifactID, err)
		}
		desc = forkRootfsCopyDescriptor(desc, art, sp.BaseImageDigest, m.cfg.NodeID, m.rootfsProducerRuntime())
		payload.Rootfs = append(payload.Rootfs, journal.ForkTransferRootfs{Descriptor: desc, Payload: rootfs.Bytes()})
	}

	var topRootfs bytes.Buffer
	if err := m.pod.ExportDelta(ctx, req.ForkSpawnID, &topRootfs); err != nil {
		return ForkTransferExportResult{}, fmt.Errorf("fork transfer export: export fork rootfs delta: %w", err)
	}
	payload.Rootfs = append(payload.Rootfs, journal.ForkTransferRootfs{
		Descriptor: journal.ArtifactDescriptor{
			ArtifactID:       fmt.Sprintf("%s-rootfs-seq-%d", req.SourceSpawnID, nextRootfsArtifactSequence(sp.DeltaDepth, inherited)),
			Type:             journal.ArtifactRootfsDelta,
			Sequence:         nextRootfsArtifactSequence(sp.DeltaDepth, inherited),
			BaseImageDigest:  sp.BaseImageDigest,
			Format:           journal.ArtifactFormatOCILayout,
			ProducerNodeID:   m.cfg.NodeID,
			ProducerRuntime:  m.rootfsProducerRuntime(),
			UncompressedSize: int64(topRootfs.Len()),
		},
		Payload: topRootfs.Bytes(),
	})

	sealedPayload, err := journal.SealForkTransferPayload(transferKey, payload)
	if err != nil {
		return ForkTransferExportResult{}, fmt.Errorf("fork transfer export: seal payload: %w", err)
	}
	sealedTransferKeyBytes, err := json.Marshal(sealedKey)
	if err != nil {
		return ForkTransferExportResult{}, fmt.Errorf("fork transfer export: encode sealed transfer key: %w", err)
	}
	return ForkTransferExportResult{SealedTransferKey: sealedTransferKeyBytes, Payload: sealedPayload}, nil
}

func (m *Manager) ForkTransferImport(ctx context.Context, req ForkTransferImportRequest, opener TransferKeyOpener) (ForkTransferImportResult, error) {
	if req.SourceSpawnID == "" || req.ForkSpawnID == "" || req.TransferSetID == "" {
		return ForkTransferImportResult{}, fmt.Errorf("fork transfer import: source spawn id, fork spawn id, and transfer set id are required")
	}
	if opener == nil {
		return ForkTransferImportResult{}, fmt.Errorf("fork transfer import: transfer key opener is required")
	}
	if m.journal == nil {
		return ForkTransferImportResult{}, fmt.Errorf("fork transfer import: journaler is required to seed fork repo")
	}
	targetGen := req.TargetGeneration
	if targetGen == 0 {
		targetGen = 1
	}
	transferKey, err := opener.OpenForkTransferKey(req.SealedTransferKey, req.ForkSpawnID, targetGen, req.TransferSetID)
	if err != nil {
		return ForkTransferImportResult{}, fmt.Errorf("fork transfer import: open transfer key: %w", err)
	}
	defer zeroBytes(transferKey)
	payload, err := journal.OpenForkTransferPayload(transferKey, req.SourceSpawnID, req.ForkSpawnID, req.Payload)
	if err != nil {
		return ForkTransferImportResult{}, fmt.Errorf("fork transfer import: open payload: %w", err)
	}
	stageRoot, err := os.MkdirTemp(m.cfg.DataRoot, "fork-import-"+req.ForkSpawnID+"-")
	if err != nil {
		return ForkTransferImportResult{}, fmt.Errorf("fork transfer import: create staging: %w", err)
	}
	defer func() { _ = os.RemoveAll(stageRoot) }()
	mounts, rootfsPayloads, err := journal.UnpackForkTransferPayload(payload, stageRoot)
	if err != nil {
		return ForkTransferImportResult{}, fmt.Errorf("fork transfer import: unpack payload: %w", err)
	}
	forkPins, err := m.journal.FinalSnapshot(ctx, req.ForkSpawnID, targetGen, mounts)
	if err != nil {
		return ForkTransferImportResult{}, fmt.Errorf("fork transfer import: final fork seed snapshot: %w", err)
	}
	if m.journalState != nil {
		if err := m.journalState.Save(req.ForkSpawnID, journalRecord{Generation: targetGen, Manifests: forkPins}); err != nil {
			log.Printf("fork transfer import: save fork journal state %s: %v", req.ForkSpawnID, err)
		}
	}
	mountPins := make(map[string]string, len(forkPins))
	for name, pin := range forkPins {
		mountPins[name] = pin.String()
	}
	outRootfs := make([]RootfsArtifact, 0, len(rootfsPayloads))
	for _, rf := range rootfsPayloads {
		stored, err := m.journal.PutArtifact(ctx, req.ForkSpawnID, targetGen, rf.Descriptor, bytes.NewReader(rf.Payload))
		if err != nil {
			return ForkTransferImportResult{}, fmt.Errorf("fork transfer import: put fork rootfs artifact: %w", err)
		}
		outRootfs = append(outRootfs, rootfsArtifactFromJournal(stored))
	}
	return ForkTransferImportResult{
		NodeID:          m.cfg.NodeID,
		MountPins:       mountPins,
		RootfsArtifacts: outRootfs,
	}, nil
}

func splitPEMCertificateChain(chain []byte) (leafPEM, intermediatesPEM []byte, err error) {
	var certs [][]byte
	rest := chain
	for len(rest) > 0 {
		block, rem := pem.Decode(rest)
		if block == nil {
			break
		}
		if block.Type == "CERTIFICATE" {
			certs = append(certs, pem.EncodeToMemory(block))
		}
		rest = rem
	}
	if len(certs) == 0 {
		return nil, nil, fmt.Errorf("missing PEM certificates")
	}
	leafPEM = certs[0]
	for _, cert := range certs[1:] {
		intermediatesPEM = append(intermediatesPEM, cert...)
	}
	return leafPEM, intermediatesPEM, nil
}

func forkTransferExpectation(targetClass, targetOwner string) (subkey.Expectation, error) {
	switch strings.TrimSpace(targetClass) {
	case "", pki.ClassCloud:
		return subkey.Expectation{Tenancy: pki.ClassCloud}, nil
	case pki.ClassSelfHosted:
		return subkey.Expectation{Tenancy: pki.ClassSelfHosted, AccountID: targetOwner}, nil
	default:
		return subkey.Expectation{}, fmt.Errorf("fork transfer export: unsupported target node class %q", targetClass)
	}
}

func zeroBytes(buf []byte) {
	for i := range buf {
		buf[i] = 0
	}
}
