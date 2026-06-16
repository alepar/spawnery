package journal

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"io"
	"path/filepath"
	"testing"
	"time"
)

func TestForkTransferPayloadRejectsSourceAndForkAADMismatches(t *testing.T) {
	key := bytes.Repeat([]byte{7}, 32)
	stage := t.TempDir()
	writeFile(t, stage, "work.txt", "fork payload\n")

	sealed, err := SealForkTransferPayload(key, ForkTransferPayload{
		SourceSpawnID: "sp-source",
		ForkSpawnID:   "sp-fork",
		Mounts: []ForkTransferMount{{
			Name:    "work",
			Class:   OwnerSealed,
			HostDir: stage,
		}},
		Rootfs: []ForkTransferRootfs{{
			Descriptor: ArtifactDescriptor{
				Type:            ArtifactRootfsDelta,
				ArtifactID:      "rootfs-source-0",
				Sequence:        0,
				BaseImageDigest: "sha256:base",
				Format:          ArtifactFormatOCILayerTar,
				ProducerNodeID:  "node-1",
				ProducerRuntime: "docker",
				CreatedAt:       time.Unix(1_700_000_000, 0).UTC(),
			},
			Payload: []byte("rootfs payload"),
		}},
	})
	if err != nil {
		t.Fatalf("SealForkTransferPayload: %v", err)
	}
	if _, err := OpenForkTransferPayload(key, "wrong-source", "sp-fork", sealed); err == nil {
		t.Fatal("wrong source id must reject")
	}
	if _, err := OpenForkTransferPayload(key, "sp-source", "wrong-fork", sealed); err == nil {
		t.Fatal("wrong fork id must reject")
	}
	opened, err := OpenForkTransferPayload(key, "sp-source", "sp-fork", sealed)
	if err != nil {
		t.Fatalf("OpenForkTransferPayload: %v", err)
	}
	if opened.SourceSpawnID != "sp-source" || opened.ForkSpawnID != "sp-fork" {
		t.Fatalf("opened ids = (%q,%q)", opened.SourceSpawnID, opened.ForkSpawnID)
	}
}

func TestForkTransferPayloadRejournalsAsForkGenerationOne(t *testing.T) {
	ctx := context.Background()
	source := newTestManager(t)
	target := newTestManager(t)

	const sourceID = "sp-source"
	const forkID = "sp-fork"
	const sourceGeneration uint64 = 9
	const forkGeneration uint64 = 1
	const baseDigest = "sha256:base-image"

	sourceWork := t.TempDir()
	writeFile(t, sourceWork, "work.txt", "source fork-point\n")
	writeFile(t, sourceWork, "dir/nested.txt", "nested fork-point\n")

	sourcePins, err := source.FinalSnapshot(ctx, sourceID, sourceGeneration, []Mount{{
		Name:    "work",
		HostDir: sourceWork,
		Class:   OwnerSealed,
	}})
	if err != nil {
		t.Fatalf("source final snapshot: %v", err)
	}
	sourceWorkPin := sourcePins["work"]
	if sourceWorkPin == "" {
		t.Fatal("source work manifest pin is empty")
	}

	sourceRootfsPayload := []byte("rootfs delta bytes from source generation 9\n")
	sourceRootfs, err := source.PutArtifact(ctx, sourceID, sourceGeneration, ArtifactDescriptor{
		Type:            ArtifactRootfsDelta,
		Sequence:        0,
		BaseImageDigest: baseDigest,
		Format:          ArtifactFormatOCILayerTar,
		ProducerNodeID:  "source-node",
		ProducerRuntime: "docker",
		CreatedAt:       time.Unix(900, 0).UTC(),
	}, bytes.NewReader(sourceRootfsPayload))
	if err != nil {
		t.Fatalf("source put rootfs artifact: %v", err)
	}

	sourceStage := t.TempDir()
	sourceStageWork := filepath.Join(sourceStage, "mounts", "work")
	if err := source.RestoreGeneration(ctx, sourceID, sourceGeneration, "work", sourceWorkPin, sourceStageWork); err != nil {
		t.Fatalf("source rehydrate to staging: %v", err)
	}
	var stagedRootfs bytes.Buffer
	if _, err := source.GetArtifact(ctx, sourceID, sourceGeneration, sourceRootfs.ArtifactID, &stagedRootfs); err != nil {
		t.Fatalf("source rootfs artifact export: %v", err)
	}

	key := bytes.Repeat([]byte{9}, 32)
	sealed, err := SealForkTransferPayload(key, ForkTransferPayload{
		SourceSpawnID: sourceID,
		ForkSpawnID:   forkID,
		Mounts: []ForkTransferMount{{
			Name:    "work",
			Class:   OwnerSealed,
			HostDir: sourceStageWork,
		}},
		Rootfs: []ForkTransferRootfs{{
			Descriptor: sourceRootfs,
			Payload:    stagedRootfs.Bytes(),
		}},
	})
	if err != nil {
		t.Fatalf("SealForkTransferPayload: %v", err)
	}

	opened, err := OpenForkTransferPayload(key, sourceID, forkID, sealed)
	if err != nil {
		t.Fatalf("OpenForkTransferPayload: %v", err)
	}
	targetStage := t.TempDir()
	importedMounts, importedRootfs, err := UnpackForkTransferPayload(opened, targetStage)
	if err != nil {
		t.Fatalf("UnpackForkTransferPayload: %v", err)
	}
	if len(importedMounts) != 1 || importedMounts[0].Name != "work" {
		t.Fatalf("imported mounts = %+v", importedMounts)
	}
	targetWork := importedMounts[0].HostDir
	if got := readFile(t, targetWork, "work.txt"); got != "source fork-point\n" {
		t.Fatalf("imported work.txt = %q", got)
	}
	if got := readFile(t, targetWork, "dir/nested.txt"); got != "nested fork-point\n" {
		t.Fatalf("imported nested.txt = %q", got)
	}

	forkPins, err := target.FinalSnapshot(ctx, forkID, forkGeneration, importedMounts)
	if err != nil {
		t.Fatalf("fork gen1 re-journal: %v", err)
	}
	forkWorkPin := forkPins["work"]
	if forkWorkPin == "" || forkWorkPin == sourceWorkPin {
		t.Fatalf("fork work pin = %q, source pin = %q; want a fresh fork manifest", forkWorkPin, sourceWorkPin)
	}

	if len(importedRootfs) != 1 {
		t.Fatalf("imported rootfs = %+v", importedRootfs)
	}
	forkRootfs, err := target.PutArtifact(ctx, forkID, forkGeneration, importedRootfs[0].Descriptor, bytes.NewReader(importedRootfs[0].Payload))
	if err != nil {
		t.Fatalf("fork rootfs re-journal: %v", err)
	}
	if forkRootfs.SpawnID != forkID || forkRootfs.Generation != forkGeneration {
		t.Fatalf("fork rootfs key = (%q,%d), want (%q,%d)", forkRootfs.SpawnID, forkRootfs.Generation, forkID, forkGeneration)
	}
	if forkRootfs.ArtifactID == "" || forkRootfs.ArtifactID == sourceRootfs.ArtifactID {
		t.Fatalf("fork rootfs artifact id = %q, source id = %q; want a fresh artifact", forkRootfs.ArtifactID, sourceRootfs.ArtifactID)
	}

	forkRestore := t.TempDir()
	if err := target.Restore(ctx, forkID, "work", forkWorkPin, forkRestore); err != nil {
		t.Fatalf("restore fork gen1 work: %v", err)
	}
	if got := readFile(t, forkRestore, "work.txt"); got != "source fork-point\n" {
		t.Fatalf("fork restore work.txt = %q", got)
	}
	var forkRootfsOut bytes.Buffer
	if _, err := target.GetArtifact(ctx, forkID, forkGeneration, forkRootfs.ArtifactID, &forkRootfsOut); err != nil {
		t.Fatalf("restore fork rootfs artifact: %v", err)
	}
	if !bytes.Equal(forkRootfsOut.Bytes(), sourceRootfsPayload) {
		t.Fatalf("fork rootfs payload = %q, want %q", forkRootfsOut.Bytes(), sourceRootfsPayload)
	}

	if err := target.Restore(ctx, sourceID, "work", sourceWorkPin, t.TempDir()); err == nil {
		t.Fatal("target fork repo must not be able to restore the source manifest pin by source id")
	}
	if _, err := target.GetArtifact(ctx, sourceID, sourceGeneration, sourceRootfs.ArtifactID, io.Discard); err == nil {
		t.Fatal("target fork repo must not expose the source rootfs artifact by source id")
	}
}

func TestForkTransferPayloadRejectsUnsafePaths(t *testing.T) {
	key := bytes.Repeat([]byte{4}, 32)

	var plain bytes.Buffer
	tw := tar.NewWriter(&plain)
	writeTarEntry(t, tw, "manifest.json", []byte(`{"source_spawn_id":"sp-source","fork_spawn_id":"sp-fork","mounts":[{"name":"work","class":"owner-sealed"}]}`), 0o600)
	writeTarEntry(t, tw, "../escape", []byte("nope"), 0o600)
	if err := tw.Close(); err != nil {
		t.Fatalf("close tar: %v", err)
	}

	sealed, err := sealForkTransferBytesForTest(key, "sp-source", "sp-fork", plain.Bytes())
	if err != nil {
		t.Fatalf("seal test payload: %v", err)
	}
	opened, err := OpenForkTransferPayload(key, "sp-source", "sp-fork", sealed)
	if err != nil {
		t.Fatalf("OpenForkTransferPayload: %v", err)
	}
	if _, _, err := UnpackForkTransferPayload(opened, t.TempDir()); err == nil {
		t.Fatal("unsafe paths must reject")
	}
}

func TestForkTransferPayloadRejectsUnsafeManifestMountNames(t *testing.T) {
	key := bytes.Repeat([]byte{8}, 32)
	for _, name := range []string{"", ".", "..", "/abs", "work/subdir", "./work"} {
		t.Run(name, func(t *testing.T) {
			var plain bytes.Buffer
			tw := tar.NewWriter(&plain)
			writeTarEntry(t, tw, "manifest.json", []byte(`{"source_spawn_id":"sp-source","fork_spawn_id":"sp-fork","mounts":[{"name":"`+name+`","class":"owner-sealed"}]}`), 0o600)
			if err := tw.Close(); err != nil {
				t.Fatalf("close tar: %v", err)
			}

			sealed, err := sealForkTransferBytesForTest(key, "sp-source", "sp-fork", plain.Bytes())
			if err != nil {
				t.Fatalf("seal test payload: %v", err)
			}
			opened, err := OpenForkTransferPayload(key, "sp-source", "sp-fork", sealed)
			if err != nil {
				t.Fatalf("OpenForkTransferPayload: %v", err)
			}
			if _, _, err := UnpackForkTransferPayload(opened, t.TempDir()); err == nil {
				t.Fatalf("mount name %q must reject", name)
			}
		})
	}
}

func TestForkTransferPayloadRejectsDuplicateTarEntries(t *testing.T) {
	key := bytes.Repeat([]byte{3}, 32)

	var plain bytes.Buffer
	tw := tar.NewWriter(&plain)
	writeTarEntry(t, tw, "manifest.json", []byte(`{"source_spawn_id":"sp-source","fork_spawn_id":"sp-fork","mounts":[{"name":"work","class":"owner-sealed"}]}`), 0o600)
	writeTarEntry(t, tw, "mounts/work/file.txt", []byte("first"), 0o600)
	writeTarEntry(t, tw, "mounts/work/file.txt", []byte("second"), 0o600)
	if err := tw.Close(); err != nil {
		t.Fatalf("close tar: %v", err)
	}

	sealed, err := sealForkTransferBytesForTest(key, "sp-source", "sp-fork", plain.Bytes())
	if err != nil {
		t.Fatalf("seal test payload: %v", err)
	}
	opened, err := OpenForkTransferPayload(key, "sp-source", "sp-fork", sealed)
	if err != nil {
		t.Fatalf("OpenForkTransferPayload: %v", err)
	}
	if _, _, err := UnpackForkTransferPayload(opened, t.TempDir()); err == nil {
		t.Fatal("duplicate tar entries must reject")
	}
}

func writeTarEntry(t *testing.T, tw *tar.Writer, name string, payload []byte, mode int64) {
	t.Helper()
	if err := tw.WriteHeader(&tar.Header{Name: name, Mode: mode, Size: int64(len(payload))}); err != nil {
		t.Fatalf("write tar header %q: %v", name, err)
	}
	if _, err := tw.Write(payload); err != nil {
		t.Fatalf("write tar payload %q: %v", name, err)
	}
}

func sealForkTransferBytesForTest(key []byte, sourceID, forkID string, plain []byte) ([]byte, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	return gcm.Seal(append([]byte{}, nonce...), nonce, plain, []byte("spawnery/fork-transfer/v1 source="+sourceID+" fork="+forkID)), nil
}
