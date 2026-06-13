package journal

import (
	"bytes"
	"context"
	"io"
	"strings"
	"testing"
	"time"
)

func TestArtifactPutGetListPinnedByGeneration(t *testing.T) {
	ctx := context.Background()
	m := newTestManager(t)
	const spawnID = "spawn-artifacts"
	payloadGen4 := []byte("uncompressed oci layer payload gen4\nfile-a\n")
	payloadGen5 := []byte("uncompressed oci layer payload gen5\nfile-b\n")

	desc4, err := m.PutArtifact(ctx, spawnID, 4, ArtifactDescriptor{
		Type:            ArtifactRootfsDelta,
		Sequence:        0,
		BaseImageDigest: "sha256:base",
		Format:          ArtifactFormatOCILayerTar,
		ProducerNodeID:  "node-a",
		ProducerRuntime: "docker",
		CreatedAt:       time.Unix(100, 0).UTC(),
	}, bytes.NewReader(payloadGen4))
	if err != nil {
		t.Fatalf("PutArtifact gen4: %v", err)
	}
	if desc4.SpawnID != spawnID || desc4.Generation != 4 {
		t.Fatalf("descriptor key = (%q,%d), want (%q,4)", desc4.SpawnID, desc4.Generation, spawnID)
	}
	if desc4.ArtifactID == "" {
		t.Fatal("PutArtifact returned empty artifact id")
	}
	if desc4.UncompressedSize != int64(len(payloadGen4)) {
		t.Fatalf("UncompressedSize = %d, want %d", desc4.UncompressedSize, len(payloadGen4))
	}
	if !strings.HasPrefix(desc4.ContentDigest, "sha256:") {
		t.Fatalf("ContentDigest = %q, want sha256 digest", desc4.ContentDigest)
	}

	desc5, err := m.PutArtifact(ctx, spawnID, 5, ArtifactDescriptor{
		Type:            ArtifactRootfsDelta,
		Sequence:        0,
		BaseImageDigest: "sha256:base",
		Format:          ArtifactFormatOCILayerTar,
		ProducerNodeID:  "node-a",
		ProducerRuntime: "docker",
		CreatedAt:       time.Unix(101, 0).UTC(),
	}, bytes.NewReader(payloadGen5))
	if err != nil {
		t.Fatalf("PutArtifact gen5: %v", err)
	}
	if desc5.ArtifactID == desc4.ArtifactID {
		t.Fatal("distinct generation artifacts must not share an artifact id")
	}

	list4, err := m.ListArtifacts(ctx, spawnID, 4, ArtifactRootfsDelta)
	if err != nil {
		t.Fatalf("ListArtifacts gen4: %v", err)
	}
	if len(list4) != 1 || list4[0].ArtifactID != desc4.ArtifactID {
		t.Fatalf("ListArtifacts gen4 = %+v, want only %s", list4, desc4.ArtifactID)
	}
	list5, err := m.ListArtifacts(ctx, spawnID, 5, ArtifactRootfsDelta)
	if err != nil {
		t.Fatalf("ListArtifacts gen5: %v", err)
	}
	if len(list5) != 1 || list5[0].ArtifactID != desc5.ArtifactID {
		t.Fatalf("ListArtifacts gen5 = %+v, want only %s", list5, desc5.ArtifactID)
	}

	var out bytes.Buffer
	gotDesc, err := m.GetArtifact(ctx, spawnID, 4, desc4.ArtifactID, &out)
	if err != nil {
		t.Fatalf("GetArtifact pinned gen4: %v", err)
	}
	if gotDesc.ArtifactID != desc4.ArtifactID {
		t.Fatalf("GetArtifact descriptor id = %q, want %q", gotDesc.ArtifactID, desc4.ArtifactID)
	}
	if !bytes.Equal(out.Bytes(), payloadGen4) {
		t.Fatalf("GetArtifact payload = %q, want %q", out.Bytes(), payloadGen4)
	}
}

func TestArtifactGetRequiresPinnedArtifactID(t *testing.T) {
	ctx := context.Background()
	m := newTestManager(t)
	var out bytes.Buffer
	if _, err := m.GetArtifact(ctx, "spawn-artifacts", 4, "", &out); err == nil {
		t.Fatal("GetArtifact with empty artifact id must fail; restore must be CP-pinned, never latest")
	}
}

func TestArtifactRejectsCompressedRootfsDeltaFormat(t *testing.T) {
	ctx := context.Background()
	m := newTestManager(t)
	_, err := m.PutArtifact(ctx, "spawn-artifacts", 4, ArtifactDescriptor{
		Type:   ArtifactRootfsDelta,
		Format: "oci_layer_tar_gzip",
	}, strings.NewReader("compressed bytes would defeat Kopia CDC"))
	if err == nil {
		t.Fatal("rootfs_delta artifacts must reject pre-gzipped input formats")
	}
}

func TestArtifactListUsesExactType(t *testing.T) {
	ctx := context.Background()
	m := newTestManager(t)
	if _, err := m.PutArtifact(ctx, "spawn-artifacts", 4, ArtifactDescriptor{
		Type:   ArtifactRootfsDelta,
		Format: ArtifactFormatOCILayerTar,
	}, strings.NewReader("rootfs")); err != nil {
		t.Fatal(err)
	}
	if _, err := m.PutArtifact(ctx, "spawn-artifacts", 4, ArtifactDescriptor{
		Type:   "debug_bundle",
		Format: "tar",
	}, strings.NewReader("debug")); err != nil {
		t.Fatal(err)
	}
	got, err := m.ListArtifacts(ctx, "spawn-artifacts", 4, ArtifactRootfsDelta)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Type != ArtifactRootfsDelta {
		t.Fatalf("ListArtifacts(rootfs_delta) = %+v, want exactly one rootfs_delta", got)
	}
}

func TestArtifactGetMissingPinnedID(t *testing.T) {
	ctx := context.Background()
	m := newTestManager(t)
	if _, err := m.GetArtifact(ctx, "spawn-artifacts", 4, "missing", io.Discard); err == nil {
		t.Fatal("GetArtifact with missing pinned id must fail")
	}
}
