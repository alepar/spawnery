package journal

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/sha256"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestForkTransferSetVariantSourceToForkRejournalsIntoFreshRepo(t *testing.T) {
	ctx := context.Background()
	source := newTestManager(t)
	target := newTestManager(t)

	const sourceID = "spawn-source"
	const forkID = "spawn-fork"
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
	if err := source.Restore(ctx, sourceID, "work", sourceWorkPin, sourceStageWork); err != nil {
		t.Fatalf("source rehydrate to staging: %v", err)
	}
	var stagedRootfs bytes.Buffer
	if _, err := source.GetArtifact(ctx, sourceID, sourceGeneration, sourceRootfs.ArtifactID, &stagedRootfs); err != nil {
		t.Fatalf("source rootfs artifact export: %v", err)
	}

	plain := packForkTransferPayload(t, sourceStageWork, stagedRootfs.Bytes())
	key := forkTransferTestKey("transfer-key")
	ciphertext, err := sealForkTransferPayload(key, sourceID, forkID, plain)
	if err != nil {
		t.Fatalf("seal transfer payload: %v", err)
	}
	if _, err := openForkTransferPayload(key, sourceID, "wrong-fork", ciphertext); err == nil {
		t.Fatal("transfer ciphertext must reject a fork id mismatch")
	}
	opened, err := openForkTransferPayload(key, sourceID, forkID, ciphertext)
	if err != nil {
		t.Fatalf("open transfer payload: %v", err)
	}

	targetStage := t.TempDir()
	importedRootfs := unpackForkTransferPayload(t, opened, targetStage)
	targetWork := filepath.Join(targetStage, "mounts", "work")
	if got := readFile(t, targetWork, "work.txt"); got != "source fork-point\n" {
		t.Fatalf("imported work.txt = %q", got)
	}
	if got := readFile(t, targetWork, "dir/nested.txt"); got != "nested fork-point\n" {
		t.Fatalf("imported nested.txt = %q", got)
	}

	forkPins, err := target.FinalSnapshot(ctx, forkID, forkGeneration, []Mount{{
		Name:    "work",
		HostDir: targetWork,
		Class:   OwnerSealed,
	}})
	if err != nil {
		t.Fatalf("fork gen1 re-journal: %v", err)
	}
	forkWorkPin := forkPins["work"]
	if forkWorkPin == "" || forkWorkPin == sourceWorkPin {
		t.Fatalf("fork work pin = %q, source pin = %q; want a fresh fork manifest", forkWorkPin, sourceWorkPin)
	}

	forkRootfs, err := target.PutArtifact(ctx, forkID, forkGeneration, ArtifactDescriptor{
		Type:            ArtifactRootfsDelta,
		Sequence:        sourceRootfs.Sequence,
		BaseImageDigest: sourceRootfs.BaseImageDigest,
		Format:          sourceRootfs.Format,
		ProducerNodeID:  "target-node",
		ProducerRuntime: sourceRootfs.ProducerRuntime,
		CreatedAt:       time.Unix(901, 0).UTC(),
	}, bytes.NewReader(importedRootfs))
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

func forkTransferTestKey(label string) []byte {
	sum := sha256.Sum256([]byte(label))
	return sum[:]
}

func forkTransferAAD(sourceID, forkID string) []byte {
	return []byte("spawnery/fork-transfer/v1 source=" + sourceID + " fork=" + forkID)
}

func sealForkTransferPayload(key []byte, sourceID, forkID string, plain []byte) ([]byte, error) {
	gcm, err := forkTransferGCM(key)
	if err != nil {
		return nil, err
	}
	nonceSum := sha256.Sum256([]byte("nonce:" + sourceID + ":" + forkID))
	nonce := nonceSum[:gcm.NonceSize()]
	return gcm.Seal(append([]byte{}, nonce...), nonce, plain, forkTransferAAD(sourceID, forkID)), nil
}

func openForkTransferPayload(key []byte, sourceID, forkID string, sealed []byte) ([]byte, error) {
	gcm, err := forkTransferGCM(key)
	if err != nil {
		return nil, err
	}
	if len(sealed) < gcm.NonceSize() {
		return nil, fmt.Errorf("sealed transfer payload too short")
	}
	nonce := sealed[:gcm.NonceSize()]
	ct := sealed[gcm.NonceSize():]
	return gcm.Open(nil, nonce, ct, forkTransferAAD(sourceID, forkID))
}

func forkTransferGCM(key []byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

func packForkTransferPayload(t *testing.T, workDir string, rootfs []byte) []byte {
	t.Helper()

	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	defer func() {
		if err := tw.Close(); err != nil {
			t.Fatalf("close transfer tar: %v", err)
		}
	}()

	err := filepath.WalkDir(workDir, func(path string, d os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(workDir, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}

		name := filepath.ToSlash(filepath.Join("mounts", "work", rel))
		info, err := d.Info()
		if err != nil {
			return err
		}
		hdr, err := tar.FileInfoHeader(info, "")
		if err != nil {
			return err
		}
		hdr.Name = name
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}

		f, err := os.Open(path)
		if err != nil {
			return err
		}
		defer f.Close()

		_, err = io.Copy(tw, f)
		return err
	})
	if err != nil {
		t.Fatalf("pack work dir: %v", err)
	}

	if err := tw.WriteHeader(&tar.Header{Name: "rootfs/payload", Mode: 0o600, Size: int64(len(rootfs))}); err != nil {
		t.Fatalf("write rootfs transfer header: %v", err)
	}
	if _, err := tw.Write(rootfs); err != nil {
		t.Fatalf("write rootfs transfer payload: %v", err)
	}
	return buf.Bytes()
}

func unpackForkTransferPayload(t *testing.T, payload []byte, targetRoot string) []byte {
	t.Helper()

	var rootfs []byte
	tr := tar.NewReader(bytes.NewReader(payload))
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("read transfer tar: %v", err)
		}

		rel := filepath.Clean(hdr.Name)
		if filepath.IsAbs(rel) || rel == "." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || rel == ".." {
			t.Fatalf("unsafe transfer path %q", hdr.Name)
		}
		if rel == "rootfs/payload" {
			rootfs, err = io.ReadAll(tr)
			if err != nil {
				t.Fatalf("read rootfs payload: %v", err)
			}
			continue
		}

		target := filepath.Join(targetRoot, rel)
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				t.Fatalf("mkdir %s: %v", target, err)
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				t.Fatalf("mkdir parent %s: %v", target, err)
			}
			f, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, os.FileMode(hdr.Mode).Perm())
			if err != nil {
				t.Fatalf("create %s: %v", target, err)
			}
			if _, err := io.Copy(f, tr); err != nil {
				_ = f.Close()
				t.Fatalf("copy %s: %v", target, err)
			}
			if err := f.Close(); err != nil {
				t.Fatalf("close %s: %v", target, err)
			}
		default:
			t.Fatalf("unsupported transfer tar entry %q type %d", hdr.Name, hdr.Typeflag)
		}
	}
	if len(rootfs) == 0 {
		t.Fatal("transfer payload did not include rootfs/payload")
	}
	return rootfs
}
