package journal

import (
	"archive/tar"
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

const forkTransferManifestName = "manifest.json"

type ForkTransferPayload struct {
	SourceSpawnID string
	ForkSpawnID   string
	Mounts        []ForkTransferMount
	Rootfs        []ForkTransferRootfs

	rawTar []byte
}

type ForkTransferMount struct {
	Name    string
	Class   DurabilityClass
	HostDir string
}

type ForkTransferRootfs struct {
	Descriptor ArtifactDescriptor
	Payload    []byte
}

type forkTransferManifest struct {
	SourceSpawnID string                       `json:"source_spawn_id"`
	ForkSpawnID   string                       `json:"fork_spawn_id"`
	Mounts        []forkTransferManifestMount  `json:"mounts"`
	Rootfs        []forkTransferManifestRootfs `json:"rootfs"`
}

type forkTransferManifestMount struct {
	Name  string `json:"name"`
	Class string `json:"class"`
}

type forkTransferManifestRootfs struct {
	Descriptor ArtifactDescriptor `json:"descriptor"`
}

func SealForkTransferPayload(key []byte, payload ForkTransferPayload) ([]byte, error) {
	if len(key) != 32 {
		return nil, fmt.Errorf("fork transfer: transfer key must be 32 bytes")
	}
	if payload.SourceSpawnID == "" || payload.ForkSpawnID == "" {
		return nil, fmt.Errorf("fork transfer: source and fork spawn ids are required")
	}
	plain, err := forkTransferPackTar(payload)
	if err != nil {
		return nil, err
	}
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
		return nil, fmt.Errorf("fork transfer: nonce: %w", err)
	}
	return gcm.Seal(append([]byte{}, nonce...), nonce, plain, forkTransferAADBytes(payload.SourceSpawnID, payload.ForkSpawnID)), nil
}

func OpenForkTransferPayload(key []byte, sourceID, forkID string, sealed []byte) (ForkTransferPayload, error) {
	if len(key) != 32 {
		return ForkTransferPayload{}, fmt.Errorf("fork transfer: transfer key must be 32 bytes")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return ForkTransferPayload{}, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return ForkTransferPayload{}, err
	}
	if len(sealed) < gcm.NonceSize() {
		return ForkTransferPayload{}, fmt.Errorf("fork transfer: sealed payload too short")
	}
	nonce := sealed[:gcm.NonceSize()]
	ct := sealed[gcm.NonceSize():]
	plain, err := gcm.Open(nil, nonce, ct, forkTransferAADBytes(sourceID, forkID))
	if err != nil {
		return ForkTransferPayload{}, err
	}
	manifest, err := readForkTransferManifest(plain)
	if err != nil {
		return ForkTransferPayload{}, err
	}
	if manifest.SourceSpawnID != sourceID || manifest.ForkSpawnID != forkID {
		return ForkTransferPayload{}, fmt.Errorf("fork transfer: manifest ids (%s,%s) do not match expected (%s,%s)", manifest.SourceSpawnID, manifest.ForkSpawnID, sourceID, forkID)
	}
	payload := ForkTransferPayload{
		SourceSpawnID: manifest.SourceSpawnID,
		ForkSpawnID:   manifest.ForkSpawnID,
		Mounts:        make([]ForkTransferMount, 0, len(manifest.Mounts)),
		Rootfs:        make([]ForkTransferRootfs, 0, len(manifest.Rootfs)),
		rawTar:        plain,
	}
	for _, mt := range manifest.Mounts {
		class, err := ParseDurability(mt.Class)
		if err != nil {
			return ForkTransferPayload{}, err
		}
		payload.Mounts = append(payload.Mounts, ForkTransferMount{Name: mt.Name, Class: class})
	}
	for _, rf := range manifest.Rootfs {
		payload.Rootfs = append(payload.Rootfs, ForkTransferRootfs{Descriptor: rf.Descriptor})
	}
	return payload, nil
}

func UnpackForkTransferPayload(payload ForkTransferPayload, targetRoot string) ([]Mount, []ForkTransferRootfs, error) {
	if payload.SourceSpawnID == "" || payload.ForkSpawnID == "" {
		return nil, nil, fmt.Errorf("fork transfer: source and fork ids are required")
	}
	if len(payload.rawTar) == 0 {
		return nil, nil, fmt.Errorf("fork transfer: raw payload tar is missing")
	}
	mountsByName := make(map[string]ForkTransferMount, len(payload.Mounts))
	outMounts := make([]Mount, 0, len(payload.Mounts))
	for _, mt := range payload.Mounts {
		if err := validateForkTransferMountName(mt.Name); err != nil {
			return nil, nil, err
		}
		if _, exists := mountsByName[mt.Name]; exists {
			return nil, nil, fmt.Errorf("fork transfer: duplicate mount %q", mt.Name)
		}
		hostDir := filepath.Join(targetRoot, "mounts", mt.Name)
		if err := os.MkdirAll(hostDir, 0o755); err != nil {
			return nil, nil, fmt.Errorf("fork transfer: create mount dir %s: %w", mt.Name, err)
		}
		mt.HostDir = hostDir
		mountsByName[mt.Name] = mt
		outMounts = append(outMounts, Mount{Name: mt.Name, HostDir: hostDir, Class: mt.Class})
	}
	sort.Slice(outMounts, func(i, j int) bool { return outMounts[i].Name < outMounts[j].Name })

	rootfsByPath := make(map[string]int, len(payload.Rootfs))
	outRootfs := make([]ForkTransferRootfs, len(payload.Rootfs))
	seenRootfsSeq := map[int]bool{}
	for i, rf := range payload.Rootfs {
		if seenRootfsSeq[rf.Descriptor.Sequence] {
			return nil, nil, fmt.Errorf("fork transfer: duplicate rootfs sequence %d", rf.Descriptor.Sequence)
		}
		seenRootfsSeq[rf.Descriptor.Sequence] = true
		if rf.Descriptor.ArtifactID == "" {
			return nil, nil, fmt.Errorf("fork transfer: rootfs artifact id is required")
		}
		path := forkTransferRootfsEntryName(rf.Descriptor)
		rootfsByPath[path] = i
		outRootfs[i] = ForkTransferRootfs{Descriptor: rf.Descriptor}
	}

	tr := tar.NewReader(bytes.NewReader(payload.rawTar))
	seenManifest := false
	seenPaths := make(map[string]struct{})
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, nil, fmt.Errorf("fork transfer: read tar: %w", err)
		}
		clean, err := validateForkTransferTarPath(hdr.Name)
		if err != nil {
			return nil, nil, err
		}
		if _, exists := seenPaths[clean]; exists {
			return nil, nil, fmt.Errorf("fork transfer: duplicate tar entry %q", clean)
		}
		seenPaths[clean] = struct{}{}
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(filepath.Join(targetRoot, filepath.FromSlash(clean)), 0o755); err != nil {
				return nil, nil, fmt.Errorf("fork transfer: mkdir %s: %w", clean, err)
			}
			continue
		case tar.TypeReg, tar.TypeRegA:
		default:
			return nil, nil, fmt.Errorf("fork transfer: unsupported tar entry %q type %d", clean, hdr.Typeflag)
		}
		switch {
		case clean == forkTransferManifestName:
			manifestBytes, err := io.ReadAll(tr)
			if err != nil {
				return nil, nil, fmt.Errorf("fork transfer: read manifest: %w", err)
			}
			manifest := forkTransferManifest{}
			if err := json.Unmarshal(manifestBytes, &manifest); err != nil {
				return nil, nil, fmt.Errorf("fork transfer: decode manifest: %w", err)
			}
			if manifest.SourceSpawnID != payload.SourceSpawnID || manifest.ForkSpawnID != payload.ForkSpawnID {
				return nil, nil, fmt.Errorf("fork transfer: manifest ids (%s,%s) do not match payload (%s,%s)", manifest.SourceSpawnID, manifest.ForkSpawnID, payload.SourceSpawnID, payload.ForkSpawnID)
			}
			seenManifest = true
		case strings.HasPrefix(clean, "mounts/"):
			parts := strings.Split(clean, "/")
			if len(parts) < 3 {
				return nil, nil, fmt.Errorf("fork transfer: malformed mount entry %q", clean)
			}
			mt, ok := mountsByName[parts[1]]
			if !ok {
				return nil, nil, fmt.Errorf("fork transfer: mount entry %q has unknown mount %q", clean, parts[1])
			}
			target := filepath.Join(mt.HostDir, filepath.FromSlash(strings.Join(parts[2:], "/")))
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return nil, nil, fmt.Errorf("fork transfer: mkdir for mount entry %q: %w", clean, err)
			}
			if err := writeRegularFile(target, tr, hdr.FileInfo().Mode().Perm()); err != nil {
				return nil, nil, fmt.Errorf("fork transfer: write mount entry %q: %w", clean, err)
			}
		case strings.HasPrefix(clean, "rootfs/"):
			idx, ok := rootfsByPath[clean]
			if !ok {
				return nil, nil, fmt.Errorf("fork transfer: unexpected rootfs entry %q", clean)
			}
			payloadBytes, err := io.ReadAll(tr)
			if err != nil {
				return nil, nil, fmt.Errorf("fork transfer: read rootfs entry %q: %w", clean, err)
			}
			outRootfs[idx].Payload = payloadBytes
			outRootfs[idx].Descriptor.ArtifactID = ""
		default:
			return nil, nil, fmt.Errorf("fork transfer: unexpected tar entry %q", clean)
		}
	}
	if !seenManifest {
		return nil, nil, fmt.Errorf("fork transfer: missing manifest")
	}
	for _, rf := range outRootfs {
		if rf.Payload == nil {
			return nil, nil, fmt.Errorf("fork transfer: missing rootfs payload for artifact %s", rf.Descriptor.ArtifactID)
		}
	}
	return outMounts, outRootfs, nil
}

func forkTransferPackTar(payload ForkTransferPayload) ([]byte, error) {
	manifest := forkTransferManifest{
		SourceSpawnID: payload.SourceSpawnID,
		ForkSpawnID:   payload.ForkSpawnID,
		Mounts:        make([]forkTransferManifestMount, 0, len(payload.Mounts)),
		Rootfs:        make([]forkTransferManifestRootfs, 0, len(payload.Rootfs)),
	}
	for _, mt := range payload.Mounts {
		if mt.Name == "" || mt.HostDir == "" {
			return nil, fmt.Errorf("fork transfer: mount name and host dir are required")
		}
		manifest.Mounts = append(manifest.Mounts, forkTransferManifestMount{Name: mt.Name, Class: mt.Class.String()})
	}
	for _, rf := range payload.Rootfs {
		if rf.Descriptor.ArtifactID == "" {
			return nil, fmt.Errorf("fork transfer: rootfs artifact id is required")
		}
		manifest.Rootfs = append(manifest.Rootfs, forkTransferManifestRootfs{Descriptor: rf.Descriptor})
	}
	manifestBytes, err := json.Marshal(manifest)
	if err != nil {
		return nil, fmt.Errorf("fork transfer: encode manifest: %w", err)
	}

	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	if err := writeForkTransferTarEntry(tw, forkTransferManifestName, manifestBytes, 0o600); err != nil {
		return nil, err
	}

	mounts := append([]ForkTransferMount(nil), payload.Mounts...)
	sort.Slice(mounts, func(i, j int) bool { return mounts[i].Name < mounts[j].Name })
	for _, mt := range mounts {
		if err := addForkTransferMount(tw, mt); err != nil {
			return nil, err
		}
	}
	rootfs := append([]ForkTransferRootfs(nil), payload.Rootfs...)
	sort.Slice(rootfs, func(i, j int) bool { return rootfs[i].Descriptor.Sequence < rootfs[j].Descriptor.Sequence })
	for _, rf := range rootfs {
		if err := writeForkTransferTarEntry(tw, forkTransferRootfsEntryName(rf.Descriptor), rf.Payload, 0o600); err != nil {
			return nil, err
		}
	}
	if err := tw.Close(); err != nil {
		return nil, fmt.Errorf("fork transfer: close tar: %w", err)
	}
	return buf.Bytes(), nil
}

func addForkTransferMount(tw *tar.Writer, mt ForkTransferMount) error {
	return filepath.WalkDir(mt.HostDir, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(mt.HostDir, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		name := filepath.ToSlash(filepath.Join("mounts", mt.Name, rel))
		info, err := d.Info()
		if err != nil {
			return err
		}
		switch {
		case info.Mode().IsDir():
			return tw.WriteHeader(&tar.Header{Name: name, Mode: 0o755, Typeflag: tar.TypeDir})
		case info.Mode().IsRegular():
			hdr, err := tar.FileInfoHeader(info, "")
			if err != nil {
				return err
			}
			hdr.Name = name
			if err := tw.WriteHeader(hdr); err != nil {
				return err
			}
			f, err := os.Open(path)
			if err != nil {
				return err
			}
			defer f.Close()
			_, err = io.Copy(tw, f)
			return err
		default:
			return fmt.Errorf("fork transfer: unsupported mount entry %q mode %s", path, info.Mode())
		}
	})
}

func readForkTransferManifest(raw []byte) (forkTransferManifest, error) {
	tr := tar.NewReader(bytes.NewReader(raw))
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return forkTransferManifest{}, fmt.Errorf("fork transfer: missing manifest")
		}
		if err != nil {
			return forkTransferManifest{}, fmt.Errorf("fork transfer: read tar: %w", err)
		}
		clean, err := validateForkTransferTarPath(hdr.Name)
		if err != nil {
			return forkTransferManifest{}, err
		}
		if clean != forkTransferManifestName {
			continue
		}
		if hdr.Typeflag != tar.TypeReg && hdr.Typeflag != tar.TypeRegA {
			return forkTransferManifest{}, fmt.Errorf("fork transfer: manifest is not a regular file")
		}
		manifestBytes, err := io.ReadAll(tr)
		if err != nil {
			return forkTransferManifest{}, fmt.Errorf("fork transfer: read manifest: %w", err)
		}
		var manifest forkTransferManifest
		if err := json.Unmarshal(manifestBytes, &manifest); err != nil {
			return forkTransferManifest{}, fmt.Errorf("fork transfer: decode manifest: %w", err)
		}
		return manifest, nil
	}
}

func validateForkTransferTarPath(name string) (string, error) {
	clean := filepath.Clean(name)
	if filepath.IsAbs(clean) || clean == "." || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("fork transfer: unsafe tar path %q", name)
	}
	if clean != name {
		return "", fmt.Errorf("fork transfer: non-canonical tar path %q", name)
	}
	return clean, nil
}

func validateForkTransferMountName(name string) error {
	if name == "" {
		return fmt.Errorf("fork transfer: mount name is required")
	}
	if filepath.IsAbs(name) || name == "." || name == ".." {
		return fmt.Errorf("fork transfer: unsafe mount name %q", name)
	}
	if filepath.Clean(name) != name {
		return fmt.Errorf("fork transfer: non-canonical mount name %q", name)
	}
	if filepath.Base(name) != name {
		return fmt.Errorf("fork transfer: mount name must be a single path component %q", name)
	}
	return nil
}

func writeRegularFile(path string, r io.Reader, mode fs.FileMode) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, mode)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = io.Copy(f, r)
	return err
}

func writeForkTransferTarEntry(tw *tar.Writer, name string, payload []byte, mode int64) error {
	if err := tw.WriteHeader(&tar.Header{Name: name, Mode: mode, Size: int64(len(payload))}); err != nil {
		return fmt.Errorf("fork transfer: write tar header %q: %w", name, err)
	}
	if _, err := tw.Write(payload); err != nil {
		return fmt.Errorf("fork transfer: write tar payload %q: %w", name, err)
	}
	return nil
}

func forkTransferRootfsEntryName(desc ArtifactDescriptor) string {
	return fmt.Sprintf("rootfs/%06d-%s.payload", desc.Sequence, desc.ArtifactID)
}

func forkTransferAADBytes(sourceID, forkID string) []byte {
	return []byte("spawnery/fork-transfer/v1 source=" + sourceID + " fork=" + forkID)
}
