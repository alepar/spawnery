package spawnlet

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"spawnery/internal/pki"
	"spawnery/internal/secrets/seal"
	"spawnery/internal/secrets/subkey"
	"spawnery/internal/storage/journal"
)

type transferNodeFixture struct {
	leaf, chain, root []byte
	key               *ecdsa.PrivateKey
}

func issueTransferNode(t *testing.T, nodeID, account, class string) transferNodeFixture {
	t.Helper()
	r, err := pki.NewRootCA("transfer-root")
	if err != nil {
		t.Fatalf("NewRootCA: %v", err)
	}
	inter, err := r.NewIntermediate(class)
	if err != nil {
		t.Fatalf("NewIntermediate: %v", err)
	}
	n, err := inter.IssueNode(nodeID, account, class, time.Now().Add(365*24*time.Hour))
	if err != nil {
		t.Fatalf("IssueNode: %v", err)
	}
	return transferNodeFixture{
		leaf:  pki.MarshalCertPEM(n.Cert),
		chain: pki.MarshalCertPEM(inter.Cert),
		root:  pki.MarshalCertPEM(r.Cert),
		key:   n.Key,
	}
}

type staticTransferKeyOpener struct {
	key []byte
	err error
}

func (o staticTransferKeyOpener) OpenForkTransferKey(_ []byte, _ string, _ uint64, _ string) ([]byte, error) {
	if o.err != nil {
		return nil, o.err
	}
	return append([]byte(nil), o.key...), nil
}

type trackingTransferKeyOpener struct {
	key    []byte
	called bool
}

func (o *trackingTransferKeyOpener) OpenForkTransferKey(_ []byte, _ string, _ uint64, _ string) ([]byte, error) {
	o.called = true
	return append([]byte(nil), o.key...), nil
}

func TestForkTransferExportGeneratesKeySealsToVerifiedTargetAndRestoresSource(t *testing.T) {
	ctx := context.Background()
	rec := &forkOpRecorder{}
	j := &recordingForkJournal{
		rec: rec,
		artifactPayloads: map[string][]byte{
			forkArtifactKey("sp-source", 9, "source-rootfs-gen9-seq1"): []byte("source-rootfs-layer-1"),
		},
		artifactDescs: map[string]journal.ArtifactDescriptor{
			forkArtifactKey("sp-source", 9, "source-rootfs-gen9-seq1"): {
				ArtifactID:      "source-rootfs-gen9-seq1",
				Type:            journal.ArtifactRootfsDelta,
				Generation:      9,
				Sequence:        1,
				BaseImageDigest: "agent@sha256:base",
				Format:          journal.ArtifactFormatOCILayout,
			},
		},
	}
	m, _ := newForkTestManager(t, rec, j)
	putForkSource(t, m, "sp-source", 9)
	src, ok := m.store.Get("sp-source")
	if !ok {
		t.Fatal("missing source")
	}
	src.RootfsArtifacts = []RootfsArtifact{{
		ArtifactID:       "source-rootfs-gen9-seq1",
		Generation:       9,
		Sequence:         1,
		BaseImageDigest:  "agent@sha256:base",
		Format:           journal.ArtifactFormatOCILayout,
		ProducerNodeID:   "source-node",
		ProducerRuntime:  "docker",
		UncompressedSize: int64(len("source-rootfs-layer-1")),
	}}
	src.DeltaDepth = 1

	targetFx := issueTransferNode(t, "node-2", "alice", pki.ClassSelfHosted)
	targetHolder := subkey.NewNode(targetFx.key, "node-2", time.Hour)
	published, err := targetHolder.Rotate(time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	targetSignedSubkey, err := jsonMarshal(published)
	if err != nil {
		t.Fatal(err)
	}

	restored := false
	res, err := m.ForkTransferExport(ctx, ForkTransferExportRequest{
		SourceSpawnID:       "sp-source",
		ForkSpawnID:         "sp-fork",
		TransferSetID:       "ts-1",
		SourceGeneration:    9,
		TargetGeneration:    1,
		TargetNodeID:        "node-2",
		TargetNodeClass:     "self-hosted",
		TargetNodeOwner:     "alice",
		TargetSignedSubKey:  targetSignedSubkey,
		TargetNodeCertChain: append(append([]byte{}, targetFx.leaf...), targetFx.chain...),
		NodeRootPEM:         targetFx.root,
		SourceRestored: func() error {
			restored = true
			return nil
		},
	})
	if err != nil {
		t.Fatalf("ForkTransferExport: %v", err)
	}
	if !restored {
		t.Fatal("source restore callback did not fire")
	}
	if len(res.SealedTransferKey) == 0 || len(res.Payload) == 0 {
		t.Fatalf("export result = %+v", res)
	}

	var sealedKey seal.NodeSealed
	if err := jsonUnmarshal(res.SealedTransferKey, &sealedKey); err != nil {
		t.Fatalf("unmarshal sealed transfer key: %v", err)
	}
	openedKey, err := targetHolder.OpenDelivered(&sealedKey, seal.InFlightAAD{
		SpawnID:    "sp-fork",
		Generation: 1,
		DeliveryID: "ts-1",
	}, time.Now())
	if err != nil {
		t.Fatalf("OpenDelivered: %v", err)
	}
	if len(openedKey) != 32 {
		t.Fatalf("transfer key len = %d, want 32", len(openedKey))
	}
	if _, err := journal.OpenForkTransferPayload(openedKey, "sp-source", "sp-fork", res.Payload); err != nil {
		t.Fatalf("OpenForkTransferPayload: %v", err)
	}
}

func TestForkTransferExportFailsClosedWithoutPinnedRoot(t *testing.T) {
	ctx := context.Background()
	rec := &forkOpRecorder{}
	j := &recordingForkJournal{rec: rec}
	m, _ := newForkTestManager(t, rec, j)
	putForkSource(t, m, "sp-source", 9)

	targetFx := issueTransferNode(t, "node-2", "alice", pki.ClassSelfHosted)
	targetHolder := subkey.NewNode(targetFx.key, "node-2", time.Hour)
	published, err := targetHolder.Rotate(time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	targetSignedSubkey, err := jsonMarshal(published)
	if err != nil {
		t.Fatal(err)
	}

	_, err = m.ForkTransferExport(ctx, ForkTransferExportRequest{
		SourceSpawnID:       "sp-source",
		ForkSpawnID:         "sp-fork",
		TransferSetID:       "ts-1",
		SourceGeneration:    9,
		TargetGeneration:    1,
		TargetNodeID:        "node-2",
		TargetNodeClass:     "self-hosted",
		TargetNodeOwner:     "alice",
		TargetSignedSubKey:  targetSignedSubkey,
		TargetNodeCertChain: append(append([]byte{}, targetFx.leaf...), targetFx.chain...),
	})
	if err == nil {
		t.Fatal("missing pinned root must fail closed")
	}
}

func TestForkTransferImportOpensSealedKeyAndReturnsForkOwnedGenerationOnePins(t *testing.T) {
	ctx := context.Background()
	rec := &forkOpRecorder{}
	j := &recordingForkJournal{rec: rec}
	m, _ := newForkTestManager(t, rec, j)

	key := bytes.Repeat([]byte{5}, 32)
	stage := t.TempDir()
	writeFile := func(name, content string) {
		t.Helper()
		path := filepath.Join(stage, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	writeFile("work.txt", "fork payload\n")
	sealedPayload, err := journal.SealForkTransferPayload(key, journal.ForkTransferPayload{
		SourceSpawnID: "sp-source",
		ForkSpawnID:   "sp-fork",
		Mounts: []journal.ForkTransferMount{{
			Name:    "work",
			Class:   journal.NodeLocal,
			HostDir: stage,
		}},
		Rootfs: []journal.ForkTransferRootfs{{
			Descriptor: journal.ArtifactDescriptor{
				Type:            journal.ArtifactRootfsDelta,
				ArtifactID:      "source-rootfs-seq1",
				Sequence:        1,
				BaseImageDigest: "agent@sha256:base",
				Format:          journal.ArtifactFormatOCILayout,
			},
			Payload: []byte("rootfs"),
		}},
	})
	if err != nil {
		t.Fatalf("SealForkTransferPayload: %v", err)
	}

	res, err := m.ForkTransferImport(ctx, ForkTransferImportRequest{
		SourceSpawnID:     "sp-source",
		ForkSpawnID:       "sp-fork",
		TransferSetID:     "ts-1",
		TargetGeneration:  1,
		SealedTransferKey: []byte("opaque-sealed-key"),
		Payload:           sealedPayload,
	}, staticTransferKeyOpener{key: key})
	if err != nil {
		t.Fatalf("ForkTransferImport: %v", err)
	}
	if res.NodeID != "node-1" || res.MountPins["work"] != "sp-fork-work-gen1" {
		t.Fatalf("import result = %+v", res)
	}
	if len(res.RootfsArtifacts) != 1 || res.RootfsArtifacts[0].Generation != 1 {
		t.Fatalf("rootfs artifacts = %+v", res.RootfsArtifacts)
	}
}

func TestForkTransferImportRejectsSourceForkAADMismatch(t *testing.T) {
	ctx := context.Background()
	rec := &forkOpRecorder{}
	j := &recordingForkJournal{rec: rec}
	m, _ := newForkTestManager(t, rec, j)

	key := bytes.Repeat([]byte{6}, 32)
	stage := t.TempDir()
	if err := os.WriteFile(filepath.Join(stage, "work.txt"), []byte("fork payload\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	sealedPayload, err := journal.SealForkTransferPayload(key, journal.ForkTransferPayload{
		SourceSpawnID: "sp-source",
		ForkSpawnID:   "sp-fork",
		Mounts: []journal.ForkTransferMount{{
			Name:    "work",
			Class:   journal.NodeLocal,
			HostDir: stage,
		}},
	})
	if err != nil {
		t.Fatalf("SealForkTransferPayload: %v", err)
	}

	_, err = m.ForkTransferImport(ctx, ForkTransferImportRequest{
		SourceSpawnID:     "sp-other",
		ForkSpawnID:       "sp-fork",
		TransferSetID:     "ts-1",
		TargetGeneration:  1,
		SealedTransferKey: []byte("opaque-sealed-key"),
		Payload:           sealedPayload,
	}, staticTransferKeyOpener{key: key})
	if err == nil {
		t.Fatal("AAD mismatch must reject import")
	}
}

func TestForkTransferImportRejectsNonGenerationOneTarget(t *testing.T) {
	ctx := context.Background()
	rec := &forkOpRecorder{}
	j := &recordingForkJournal{rec: rec}
	m, _ := newForkTestManager(t, rec, j)

	opener := &trackingTransferKeyOpener{key: bytes.Repeat([]byte{2}, 32)}
	_, err := m.ForkTransferImport(ctx, ForkTransferImportRequest{
		SourceSpawnID:     "sp-source",
		ForkSpawnID:       "sp-fork",
		TransferSetID:     "ts-1",
		TargetGeneration:  2,
		SealedTransferKey: []byte("opaque-sealed-key"),
		Payload:           []byte("sealed-payload"),
	}, opener)
	if err == nil {
		t.Fatal("non-generation-1 target must reject")
	}
	if opener.called {
		t.Fatal("generation guard must reject before opening transfer key")
	}
	if ops := rec.snapshot(); len(ops) != 0 {
		t.Fatalf("journal should not run on rejected generation, ops=%v", ops)
	}
}

func jsonMarshal(v any) ([]byte, error)      { return json.Marshal(v) }
func jsonUnmarshal(data []byte, v any) error { return json.Unmarshal(data, v) }
