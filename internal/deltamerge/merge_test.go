package deltamerge_test

import (
	"archive/tar"
	"bytes"
	"io"
	"testing"
	"time"

	"spawnery/internal/deltamerge"
)

// buildTar builds a tar archive from the supplied entries.  Each entry is a
// (name, content) pair; a nil content means a directory entry.  Whiteout
// entries can be expressed as regular files with nil content.
type tarEntry struct {
	name    string
	content []byte // nil → TypeDir; non-nil → TypeReg (len 0 allowed)
	typeflag byte   // 0 → auto-detect (TypeDir if content nil else TypeReg)
}

func buildTar(t *testing.T, entries []tarEntry) io.Reader {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, e := range entries {
		tf := e.typeflag
		if tf == 0 {
			if e.content == nil {
				tf = tar.TypeDir
			} else {
				tf = tar.TypeReg
			}
		}
		hdr := &tar.Header{
			Name:     e.name,
			Typeflag: tf,
			Mode:     0o644,
			ModTime:  time.Unix(1000, 0),
			Size:     int64(len(e.content)),
		}
		if tf == tar.TypeDir {
			hdr.Mode = 0o755
			hdr.Size = 0
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatalf("buildTar WriteHeader %s: %v", e.name, err)
		}
		if len(e.content) > 0 {
			if _, err := tw.Write(e.content); err != nil {
				t.Fatalf("buildTar Write %s: %v", e.name, err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal("buildTar Close:", err)
	}
	return &buf
}

// readTar reads all entries from a tar reader and returns (name→content) map.
func readTar(t *testing.T, r io.Reader) map[string][]byte {
	t.Helper()
	out := map[string][]byte{}
	tr := tar.NewReader(r)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("readTar Next: %v", err)
		}
		var content []byte
		if hdr.Typeflag == tar.TypeReg || hdr.Typeflag == tar.TypeRegA {
			content, err = io.ReadAll(tr)
			if err != nil {
				t.Fatalf("readTar ReadAll %s: %v", hdr.Name, err)
			}
		}
		out[hdr.Name] = content
	}
	return out
}

// readTarOrdered reads all entries and returns them in order (for sorting test).
func readTarOrdered(t *testing.T, r io.Reader) []string {
	t.Helper()
	var names []string
	tr := tar.NewReader(r)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("readTarOrdered: %v", err)
		}
		names = append(names, hdr.Name)
	}
	return names
}

// T1: later-layer override wins (same path, different content).
func TestMerge_T1_LaterLayerWins(t *testing.T) {
	layerA := buildTar(t, []tarEntry{
		{name: "etc/config", content: []byte("v1")},
	})
	layerB := buildTar(t, []tarEntry{
		{name: "etc/config", content: []byte("v2")},
	})

	var out bytes.Buffer
	if err := deltamerge.Merge([]io.Reader{layerA, layerB}, deltamerge.Options{}, &out); err != nil {
		t.Fatalf("Merge: %v", err)
	}
	entries := readTar(t, &out)
	if string(entries["etc/config"]) != "v2" {
		t.Fatalf("expected v2, got %q", entries["etc/config"])
	}
}

// T2: .wh.foo cancels foo from an earlier layer — neither foo nor the whiteout appears in output.
func TestMerge_T2_WhiteoutCancelsEarlierLayer(t *testing.T) {
	layerA := buildTar(t, []tarEntry{
		{name: "foo", content: []byte("bar")},
	})
	layerB := buildTar(t, []tarEntry{
		{name: ".wh.foo", content: []byte{}},
	})

	var out bytes.Buffer
	if err := deltamerge.Merge([]io.Reader{layerA, layerB}, deltamerge.Options{}, &out); err != nil {
		t.Fatalf("Merge: %v", err)
	}
	entries := readTar(t, &out)
	if _, ok := entries["foo"]; ok {
		t.Fatal("foo should have been deleted by whiteout")
	}
	if _, ok := entries[".wh.foo"]; ok {
		t.Fatal(".wh.foo whiteout should be dropped (it already applied in-merge)")
	}
}

// T3: base-masking preservation — .wh.foo where foo is only in BasePaths → whiteout KEPT.
func TestMerge_T3_BaseMaskingPreservation(t *testing.T) {
	layerA := buildTar(t, []tarEntry{
		{name: ".wh.passwd", content: []byte{}},
	})

	opts := deltamerge.Options{
		BasePaths: map[string]struct{}{"/passwd": {}},
	}
	var out bytes.Buffer
	if err := deltamerge.Merge([]io.Reader{layerA}, opts, &out); err != nil {
		t.Fatalf("Merge: %v", err)
	}
	entries := readTar(t, &out)
	if _, ok := entries[".wh.passwd"]; !ok {
		t.Fatal(".wh.passwd should be preserved: it masks a base-image path")
	}
}

// T4: whiteout deleting nothing (not in accumulator, not in BasePaths) → dropped.
func TestMerge_T4_WhiteoutDeletingNothing(t *testing.T) {
	layerA := buildTar(t, []tarEntry{
		{name: ".wh.ghost", content: []byte{}},
	})

	var out bytes.Buffer
	if err := deltamerge.Merge([]io.Reader{layerA}, deltamerge.Options{}, &out); err != nil {
		t.Fatalf("Merge: %v", err)
	}
	entries := readTar(t, &out)
	if _, ok := entries[".wh.ghost"]; ok {
		t.Fatal(".wh.ghost should be dropped: it deletes nothing")
	}
}

// T5: .wh..wh..opq masks earlier-layer dir contents; kept only when BasePaths has entries under that dir.
func TestMerge_T5_OpaqueWhiteout_WithBase(t *testing.T) {
	layerA := buildTar(t, []tarEntry{
		{name: "etc/", content: nil},
		{name: "etc/passwd", content: []byte("root:x:0")},
		{name: "etc/hosts", content: []byte("127.0.0.1 localhost")},
	})
	layerB := buildTar(t, []tarEntry{
		{name: "etc/.wh..wh..opq", content: []byte{}},
		{name: "etc/newfile", content: []byte("fresh")},
	})

	opts := deltamerge.Options{
		BasePaths: map[string]struct{}{"/etc/fstab": {}},
	}
	var out bytes.Buffer
	if err := deltamerge.Merge([]io.Reader{layerA, layerB}, opts, &out); err != nil {
		t.Fatalf("Merge: %v", err)
	}
	entries := readTar(t, &out)

	// Earlier-layer contents must be gone.
	if _, ok := entries["etc/passwd"]; ok {
		t.Error("etc/passwd should have been removed by opaque whiteout")
	}
	if _, ok := entries["etc/hosts"]; ok {
		t.Error("etc/hosts should have been removed by opaque whiteout")
	}
	// New entry in layerB survives.
	if string(entries["etc/newfile"]) != "fresh" {
		t.Errorf("etc/newfile = %q, want fresh", entries["etc/newfile"])
	}
	// Opaque marker kept (BasePaths has /etc/fstab under etc/).
	if _, ok := entries["etc/.wh..wh..opq"]; !ok {
		t.Error("etc/.wh..wh..opq should be kept: BasePaths has entries under etc/")
	}
}

// T5b: opaque whiteout where BasePaths has NO entries under that dir → marker dropped.
func TestMerge_T5b_OpaqueWhiteout_NoBase(t *testing.T) {
	layerA := buildTar(t, []tarEntry{
		{name: "tmp/foo", content: []byte("x")},
	})
	layerB := buildTar(t, []tarEntry{
		{name: "tmp/.wh..wh..opq", content: []byte{}},
	})

	// BasePaths has nothing under tmp/.
	opts := deltamerge.Options{
		BasePaths: map[string]struct{}{"/etc/passwd": {}},
	}
	var out bytes.Buffer
	if err := deltamerge.Merge([]io.Reader{layerA, layerB}, opts, &out); err != nil {
		t.Fatalf("Merge: %v", err)
	}
	entries := readTar(t, &out)
	if _, ok := entries["tmp/.wh..wh..opq"]; ok {
		t.Error("opaque marker should be dropped: no BasePaths entries under tmp/")
	}
	if _, ok := entries["tmp/foo"]; ok {
		t.Error("tmp/foo should have been removed by opaque whiteout")
	}
}

// T6: scrub prefix /var/cache/apt removes entries even when present.
func TestMerge_T6_ScrubPrefix(t *testing.T) {
	layerA := buildTar(t, []tarEntry{
		{name: "var/cache/apt/pkgcache.bin", content: []byte("big")},
		{name: "var/cache/apt/srcpkgcache.bin", content: []byte("also big")},
		{name: "etc/hostname", content: []byte("myhost")},
	})

	opts := deltamerge.Options{
		ScrubPrefixes: []string{"/var/cache/apt"},
	}
	var out bytes.Buffer
	if err := deltamerge.Merge([]io.Reader{layerA}, opts, &out); err != nil {
		t.Fatalf("Merge: %v", err)
	}
	entries := readTar(t, &out)
	if _, ok := entries["var/cache/apt/pkgcache.bin"]; ok {
		t.Error("var/cache/apt/pkgcache.bin should be scrubbed")
	}
	if _, ok := entries["var/cache/apt/srcpkgcache.bin"]; ok {
		t.Error("var/cache/apt/srcpkgcache.bin should be scrubbed")
	}
	if string(entries["etc/hostname"]) != "myhost" {
		t.Errorf("etc/hostname = %q, want myhost", entries["etc/hostname"])
	}
}

// T6b: scrub also drops whiteouts whose targets are scrubbed paths.
func TestMerge_T6b_ScrubDropsTargetWhiteout(t *testing.T) {
	// A delta that whiteouts /var/cache/apt as a whole (from base).
	layerA := buildTar(t, []tarEntry{
		{name: "var/cache/.wh.apt", content: []byte{}},
	})
	opts := deltamerge.Options{
		ScrubPrefixes: []string{"/var/cache/apt"},
		BasePaths:     map[string]struct{}{"/var/cache/apt/lists": {}},
	}
	var out bytes.Buffer
	if err := deltamerge.Merge([]io.Reader{layerA}, opts, &out); err != nil {
		t.Fatalf("Merge: %v", err)
	}
	entries := readTar(t, &out)
	// The whiteout targets a scrubbed path → should be dropped.
	if _, ok := entries["var/cache/.wh.apt"]; ok {
		t.Error("whiteout targeting scrubbed path should be dropped")
	}
}

// T7: determinism — two Merge calls over the same input produce byte-identical output;
// output entries are in sorted path order; mtime is fixed.
func TestMerge_T7_Determinism(t *testing.T) {
	entries := []tarEntry{
		{name: "z/b", content: []byte("zb")},
		{name: "a/c", content: []byte("ac")},
		{name: "a/a", content: []byte("aa")},
	}

	buildAndMerge := func() []byte {
		layer := buildTar(t, entries)
		var out bytes.Buffer
		if err := deltamerge.Merge([]io.Reader{layer}, deltamerge.Options{}, &out); err != nil {
			t.Fatalf("Merge: %v", err)
		}
		return out.Bytes()
	}

	first := buildAndMerge()
	second := buildAndMerge()
	if !bytes.Equal(first, second) {
		t.Fatal("Merge output is not deterministic across two calls")
	}

	// Verify sorted order.
	names := readTarOrdered(t, bytes.NewReader(first))
	want := []string{"a/a", "a/c", "z/b"}
	if len(names) != len(want) {
		t.Fatalf("output names = %v, want %v", names, want)
	}
	for i, n := range names {
		if n != want[i] {
			t.Errorf("output[%d] = %q, want %q", i, n, want[i])
		}
	}

	// Verify mtime is the fixed epoch.
	tr := tar.NewReader(bytes.NewReader(first))
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if !hdr.ModTime.IsZero() && hdr.ModTime.Unix() != 0 {
			t.Errorf("entry %s: ModTime = %v, want epoch (0)", hdr.Name, hdr.ModTime)
		}
	}
}

// TestParseWhiteout exercises the exported ParseWhiteout helper.
func TestParseWhiteout(t *testing.T) {
	cases := []struct {
		name             string
		wantDir          string
		wantTarget       string
		wantOpaque       bool
		wantOK           bool
	}{
		{"etc/.wh.passwd", "etc", "etc/passwd", false, true},
		{".wh.foo", ".", "foo", false, true},
		{"etc/.wh..wh..opq", "etc", "", true, true},
		{"etc/passwd", "", "", false, false},
		{"etc/", "", "", false, false},
	}
	for _, tc := range cases {
		dir, target, opaque, ok := deltamerge.ParseWhiteout(tc.name)
		if ok != tc.wantOK {
			t.Errorf("ParseWhiteout(%q) ok=%v, want %v", tc.name, ok, tc.wantOK)
			continue
		}
		if !ok {
			continue
		}
		if dir != tc.wantDir {
			t.Errorf("ParseWhiteout(%q) dir=%q, want %q", tc.name, dir, tc.wantDir)
		}
		if target != tc.wantTarget {
			t.Errorf("ParseWhiteout(%q) target=%q, want %q", tc.name, target, tc.wantTarget)
		}
		if opaque != tc.wantOpaque {
			t.Errorf("ParseWhiteout(%q) opaque=%v, want %v", tc.name, opaque, tc.wantOpaque)
		}
	}
}
