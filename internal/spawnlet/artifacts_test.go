package spawnlet

import (
	"archive/tar"
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"

	"spawnery/internal/runtime"
)

func tarBytes(t *testing.T, files map[string]struct {
	mode os.FileMode
	body string
}) []byte {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for name, f := range files {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: int64(f.mode), Size: int64(len(f.body)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(f.body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func newStagerPair(t *testing.T) (ArtifactStager, SecretInjector) {
	t.Helper()
	return ArtifactStager{Root: t.TempDir()}, SecretInjector{Root: t.TempDir()}
}

func TestMaterialize_BytesWritesFileAtMode(t *testing.T) {
	st, sec := newStagerPair(t)
	if err := st.Materialize("sp1", []Artifact{{
		ID: "a", Inline: []byte("hello"), ContentType: ArtifactBytes, DestPath: "manifest.json", Mode: 0o640,
	}}, sec); err != nil {
		t.Fatalf("Materialize: %v", err)
	}
	p := filepath.Join(st.DirFor("sp1"), "manifest.json")
	got, err := os.ReadFile(p)
	if err != nil || string(got) != "hello" {
		t.Fatalf("read %s: %q err=%v", p, got, err)
	}
	if fi, _ := os.Stat(p); fi.Mode().Perm() != 0o640 {
		t.Fatalf("mode = %v, want 0640", fi.Mode().Perm())
	}
}

func TestMaterialize_BytesDefaultModeWhenZero(t *testing.T) {
	st, sec := newStagerPair(t)
	if err := st.Materialize("sp1", []Artifact{{ID: "a", Inline: []byte("x"), ContentType: ArtifactBytes, DestPath: "f", Mode: 0}}, sec); err != nil {
		t.Fatal(err)
	}
	fi, _ := os.Stat(filepath.Join(st.DirFor("sp1"), "f"))
	if fi.Mode().Perm() != 0o644 {
		t.Fatalf("default mode = %v, want 0644", fi.Mode().Perm())
	}
}

func TestMaterialize_TarUnpacksPreservingPerFileModes(t *testing.T) {
	st, sec := newStagerPair(t)
	blob := tarBytes(t, map[string]struct {
		mode os.FileMode
		body string
	}{
		"SKILL.md":   {0o644, "# skill"},
		"bin/run.sh": {0o755, "#!/bin/sh"},
	})
	if err := st.Materialize("sp1", []Artifact{{ID: "skill", Inline: blob, ContentType: ArtifactTar, DestPath: "payloads/skill"}}, sec); err != nil {
		t.Fatalf("Materialize tar: %v", err)
	}
	base := filepath.Join(st.DirFor("sp1"), "payloads", "skill")
	if fi, _ := os.Stat(filepath.Join(base, "SKILL.md")); fi == nil || fi.Mode().Perm() != 0o644 {
		t.Fatalf("SKILL.md mode wrong: %v", fi)
	}
	fi, err := os.Stat(filepath.Join(base, "bin", "run.sh"))
	if err != nil || fi.Mode().Perm() != 0o755 {
		t.Fatalf("run.sh mode = %v err=%v, want 0755", fi, err)
	}
}

func TestMaterialize_SensitiveRoutesToSecretsNotStaging(t *testing.T) {
	st, sec := newStagerPair(t)
	if err := st.Materialize("sp1", []Artifact{{
		ID: "tok", Inline: []byte("s3cr3t"), ContentType: ArtifactBytes, Sensitive: true, EnvVarName: "GH_TOKEN", DestPath: "ignored",
	}}, sec); err != nil {
		t.Fatalf("Materialize: %v", err)
	}
	// Lands in secrets root @0600, keyed by env var name.
	secp := filepath.Join(sec.DirFor("sp1"), "GH_TOKEN")
	got, err := os.ReadFile(secp)
	if err != nil || string(got) != "s3cr3t" {
		t.Fatalf("secret read %s: %q err=%v", secp, got, err)
	}
	if fi, _ := os.Stat(secp); fi.Mode().Perm() != 0o600 {
		t.Fatalf("secret mode = %v, want 0600", fi.Mode().Perm())
	}
	// NOT in the staging dir.
	if _, err := os.Stat(filepath.Join(st.DirFor("sp1"), "ignored")); !os.IsNotExist(err) {
		t.Fatalf("sensitive artifact leaked into staging dir: err=%v", err)
	}
}

func TestMaterialize_SensitiveEmptyInlineSkipped(t *testing.T) {
	st, sec := newStagerPair(t)
	// Async-delivered secret (no inline in StartSpawn): no-op, no error.
	if err := st.Materialize("sp1", []Artifact{{ID: "tok", Sensitive: true, EnvVarName: "GH_TOKEN"}}, sec); err != nil {
		t.Fatalf("Materialize: %v", err)
	}
	if _, err := os.Stat(filepath.Join(sec.DirFor("sp1"), "GH_TOKEN")); !os.IsNotExist(err) {
		t.Fatalf("empty-inline sensitive should not write a secret file: err=%v", err)
	}
}

func TestMaterialize_RejectsDestPathTraversal(t *testing.T) {
	st, sec := newStagerPair(t)
	err := st.Materialize("sp1", []Artifact{{ID: "evil", Inline: []byte("x"), ContentType: ArtifactBytes, DestPath: "../escape"}}, sec)
	if err == nil {
		t.Fatal("expected traversal rejection for dest_path '../escape'")
	}
}

func TestMaterialize_RejectsTarEntryTraversal(t *testing.T) {
	st, sec := newStagerPair(t)
	blob := tarBytes(t, map[string]struct {
		mode os.FileMode
		body string
	}{"../escape": {0o644, "x"}})
	if err := st.Materialize("sp1", []Artifact{{ID: "evil", Inline: blob, ContentType: ArtifactTar, DestPath: "payloads/skill"}}, sec); err == nil {
		t.Fatal("expected traversal rejection for tar entry '../escape'")
	}
}

func TestMaterialize_AbsoluteDestTreatedAsMountRelative(t *testing.T) {
	st, sec := newStagerPair(t)
	if err := st.Materialize("sp1", []Artifact{{ID: "a", Inline: []byte("x"), ContentType: ArtifactBytes, DestPath: "/etc/passwd"}}, sec); err != nil {
		t.Fatalf("Materialize: %v", err)
	}
	// "/etc/passwd" is confined under the staging dir, not the host root.
	if _, err := os.Stat(filepath.Join(st.DirFor("sp1"), "etc", "passwd")); err != nil {
		t.Fatalf("absolute dest not confined under staging dir: %v", err)
	}
}

func TestMaterialize_IdempotentReapplyWipesStaging(t *testing.T) {
	st, sec := newStagerPair(t)
	if err := st.Materialize("sp1", []Artifact{{ID: "a", Inline: []byte("v1"), ContentType: ArtifactBytes, DestPath: "f"}}, sec); err != nil {
		t.Fatal(err)
	}
	// Stale file from a prior apply must not survive re-threading on resume.
	stale := filepath.Join(st.DirFor("sp1"), "stale")
	if err := os.WriteFile(stale, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := st.Materialize("sp1", []Artifact{{ID: "a", Inline: []byte("v2"), ContentType: ArtifactBytes, DestPath: "f"}}, sec); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Fatalf("stale file survived re-apply: err=%v", err)
	}
	got, _ := os.ReadFile(filepath.Join(st.DirFor("sp1"), "f"))
	if string(got) != "v2" {
		t.Fatalf("re-apply content = %q, want v2", got)
	}
}

func TestMaterialize_EmptyListNoop(t *testing.T) {
	st, sec := newStagerPair(t)
	if err := st.Materialize("sp1", nil, sec); err != nil {
		t.Fatalf("empty Materialize: %v", err)
	}
}

// TestManagerArtifactsMaterialized exercises the Manager-level wiring: CreateWithSelection with
// AgentSelection.Artifacts materializes staging files and threads the bind-mount into AgentSpec.
func TestManagerArtifactsMaterialized(t *testing.T) {
	dataRoot := t.TempDir()
	m := NewManager(runtime.NewFake(), ManagerConfig{
		AgentImage: "a", SidecarImage: "s", DataRoot: dataRoot,
	})
	fb := &fakePodBackend{}
	m.pod = fb

	spawnID := "sp-art-test"
	_, err := m.CreateWithSelection(context.Background(), spawnID, "../../examples/secret-app", "model", "", "", 0,
		AgentSelection{
			Artifacts: []Artifact{
				{ID: "pub", Inline: []byte("payload"), ContentType: ArtifactBytes, DestPath: "manifest.json", Mode: 0o644},
				{ID: "tok", Inline: []byte("secret-val"), ContentType: ArtifactBytes, Sensitive: true, EnvVarName: "MY_TOKEN"},
			},
		})
	if err != nil {
		t.Fatalf("CreateWithSelection: %v", err)
	}

	// (a) staging file exists under ArtifactsRoot/<spawnID>
	artifactsRoot := filepath.Join(dataRoot, "artifacts")
	stagingFile := filepath.Join(artifactsRoot, spawnID, "manifest.json")
	got, err := os.ReadFile(stagingFile)
	if err != nil || string(got) != "payload" {
		t.Fatalf("staging file %s: %q err=%v", stagingFile, got, err)
	}

	// (b) AgentSpec.Mounts includes the artifacts bind-mount at ArtifactsMountPath
	var found bool
	for _, mt := range fb.agentSpec.Mounts {
		if mt.ContainerPath == ArtifactsMountPath {
			wantHost := filepath.Join(artifactsRoot, spawnID)
			if mt.HostPath != wantHost {
				t.Fatalf("artifacts mount HostPath = %q, want %q", mt.HostPath, wantHost)
			}
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("ArtifactsMountPath %q not found in agent mounts: %+v", ArtifactsMountPath, fb.agentSpec.Mounts)
	}

	// (c) sensitive artifact landed under secrets root, not staging
	secretsRoot := filepath.Join(dataRoot, "secrets")
	secretFile := filepath.Join(secretsRoot, spawnID, "MY_TOKEN")
	secGot, err := os.ReadFile(secretFile)
	if err != nil || string(secGot) != "secret-val" {
		t.Fatalf("secret file %s: %q err=%v", secretFile, secGot, err)
	}
	// sensitive artifact must NOT appear in staging dir
	if _, err := os.Stat(filepath.Join(artifactsRoot, spawnID, "MY_TOKEN")); !os.IsNotExist(err) {
		t.Fatalf("sensitive artifact leaked into staging dir: err=%v", err)
	}
}
