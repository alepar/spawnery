package scheduler

import (
	"context"
	"sync"
	"testing"
	"time"

	nodev1 "spawnery/gen/node/v1"
	"spawnery/internal/cp/registry"
	"spawnery/internal/cp/router"
)

type fakeSender struct {
	mu   sync.Mutex
	sent []*nodev1.CPMessage
}

func (f *fakeSender) Send(m *nodev1.CPMessage) error {
	f.mu.Lock()
	f.sent = append(f.sent, m)
	f.mu.Unlock()
	return nil
}

func (f *fakeSender) first() *nodev1.CPMessage {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.sent) == 0 {
		return nil
	}
	return f.sent[0]
}

func TestProvisionRoutesAndAwaitsActive(t *testing.T) {
	reg := registry.New()
	rt := router.New()
	s := New(reg, rt, 2*time.Second)

	send := &fakeSender{}
	reg.Add(&registry.Node{ID: "n1", Sender: send, Max: 1, Free: 1})

	go func() {
		for {
			if m := send.first(); m != nil {
				s.OnStatus(m.GetStart().GetSpawnId(), nodev1.SpawnPhase_ACTIVE, "")
				return
			}
			time.Sleep(time.Millisecond)
		}
	}()

	nodeID, err := s.Provision(context.Background(), "sp-test", "examples/secret-app", "m", "", "", "", "", 3, registry.Placement{}, nil, "", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if nodeID != "n1" {
		t.Fatalf("provision node=%q", nodeID)
	}
	got := send.first().GetStart()
	if got.GetSpawnId() != "sp-test" || got.GetAppRef() != "examples/secret-app" || got.GetModel() != "m" {
		t.Fatalf("StartSpawn payload wrong: %+v", got)
	}
	if got.GetGeneration() != 3 {
		t.Fatalf("StartSpawn generation=%d want 3 (the node labels + reports its pod with it)", got.GetGeneration())
	}
}

func TestProvisionNoCapacity(t *testing.T) {
	s := New(registry.New(), router.New(), time.Second)
	if _, err := s.Provision(context.Background(), "sp-x", "ref", "m", "", "", "", "", 1, registry.Placement{}, nil, "", nil, nil); err == nil {
		t.Fatal("expected ResourceExhausted when no node")
	}
}

func TestProvisionThreadsSelection(t *testing.T) {
	reg := registry.New()
	rt := router.New()
	s := New(reg, rt, 2*time.Second)

	send := &fakeSender{}
	reg.Add(&registry.Node{ID: "n1", Sender: send, Max: 1, Free: 1, Images: []string{"img:1"}})

	go func() {
		for {
			if m := send.first(); m != nil {
				s.OnStatus(m.GetStart().GetSpawnId(), nodev1.SpawnPhase_ACTIVE, "")
				return
			}
			time.Sleep(time.Millisecond)
		}
	}()

	_, err := s.Provision(context.Background(), "sp-sel", "ref", "m", "nm", "app", "goose-acp", "acp",
		1, registry.Placement{Image: "img:1"}, nil, "", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	got := send.first().GetStart()
	if got.GetImage() != "img:1" || got.GetRunnableId() != "goose-acp" || got.GetMode() != "acp" {
		t.Fatalf("StartSpawn selection not threaded: image=%q runnable=%q mode=%q",
			got.GetImage(), got.GetRunnableId(), got.GetMode())
	}
}

// G: Provision threads base_image_digest into StartSpawn (sp-ei4.1.10 pinning).
func TestProvisionThreadsBaseImageDigest(t *testing.T) {
	reg := registry.New()
	rt := router.New()
	s := New(reg, rt, 2*time.Second)

	send := &fakeSender{}
	reg.Add(&registry.Node{ID: "n1", Sender: send, Max: 1, Free: 1})

	go func() {
		for {
			if m := send.first(); m != nil {
				s.OnStatus(m.GetStart().GetSpawnId(), nodev1.SpawnPhase_ACTIVE, "")
				return
			}
			time.Sleep(time.Millisecond)
		}
	}()

	const digest = "spawnery/agent@sha256:deadbeef"
	_, err := s.Provision(context.Background(), "sp-digest", "ref", "m", "", "", "", "", 1,
		registry.Placement{}, nil, digest, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	got := send.first().GetStart()
	if got.GetBaseImageDigest() != digest {
		t.Fatalf("StartSpawn.BaseImageDigest = %q, want %q (base image digest not threaded)", got.GetBaseImageDigest(), digest)
	}
}

// G2: On fresh create (empty digest), Provision sends an empty base_image_digest in StartSpawn.
func TestProvisionFreshCreateSendsEmptyDigest(t *testing.T) {
	reg := registry.New()
	rt := router.New()
	s := New(reg, rt, 2*time.Second)

	send := &fakeSender{}
	reg.Add(&registry.Node{ID: "n1", Sender: send, Max: 1, Free: 1})

	go func() {
		for {
			if m := send.first(); m != nil {
				s.OnStatus(m.GetStart().GetSpawnId(), nodev1.SpawnPhase_ACTIVE, "")
				return
			}
			time.Sleep(time.Millisecond)
		}
	}()

	_, err := s.Provision(context.Background(), "sp-fresh", "ref", "m", "", "", "", "", 1,
		registry.Placement{}, nil, "", nil, nil) // empty = fresh create
	if err != nil {
		t.Fatal(err)
	}
	got := send.first().GetStart()
	if got.GetBaseImageDigest() != "" {
		t.Fatalf("StartSpawn.BaseImageDigest = %q, want empty on fresh create", got.GetBaseImageDigest())
	}
}

// TestProvisionThreadsArtifacts verifies that non-nil artifacts are relayed to StartSpawn and
// that a sensitive artifact's inline bytes are empty (metadata-only relay per CP-blindness rule).
func TestProvisionThreadsArtifacts(t *testing.T) {
	reg := registry.New()
	rt := router.New()
	s := New(reg, rt, 2*time.Second)

	send := &fakeSender{}
	reg.Add(&registry.Node{ID: "n1", Sender: send, Max: 1, Free: 1})

	go func() {
		for {
			if m := send.first(); m != nil {
				s.OnStatus(m.GetStart().GetSpawnId(), nodev1.SpawnPhase_ACTIVE, "")
				return
			}
			time.Sleep(time.Millisecond)
		}
	}()

	arts := []*nodev1.ArtifactSpec{
		{Id: "a1", Inline: []byte("content"), ContentType: nodev1.ArtifactContentType_ARTIFACT_CONTENT_TYPE_BYTES, TargetContainer: nodev1.ArtifactTarget_ARTIFACT_TARGET_AGENT, DestPath: "skills/x"},
		{Id: "s1", Sensitive: true, EnvVarName: "TOK", ContentType: nodev1.ArtifactContentType_ARTIFACT_CONTENT_TYPE_BYTES, TargetContainer: nodev1.ArtifactTarget_ARTIFACT_TARGET_AGENT, DestPath: "mcp/y"},
	}
	_, err := s.Provision(context.Background(), "sp-arts", "ref", "m", "", "", "", "", 1,
		registry.Placement{}, nil, "", nil, arts)
	if err != nil {
		t.Fatal(err)
	}
	got := send.first().GetStart()
	if len(got.Artifacts) != 2 {
		t.Fatalf("StartSpawn.Artifacts len=%d, want 2", len(got.Artifacts))
	}
	if got.Artifacts[0].Id != "a1" || string(got.Artifacts[0].Inline) != "content" {
		t.Fatalf("a1 wrong: %+v", got.Artifacts[0])
	}
	if got.Artifacts[1].Id != "s1" || !got.Artifacts[1].Sensitive || got.Artifacts[1].EnvVarName != "TOK" {
		t.Fatalf("s1 wrong: %+v", got.Artifacts[1])
	}
}

func TestProvisionThreadsRootfsRestorePins(t *testing.T) {
	reg := registry.New()
	rt := router.New()
	s := New(reg, rt, 2*time.Second)

	send := &fakeSender{}
	reg.Add(&registry.Node{ID: "n1", Sender: send, Max: 1, Free: 1})

	go func() {
		for {
			if m := send.first(); m != nil {
				s.OnStatus(m.GetStart().GetSpawnId(), nodev1.SpawnPhase_ACTIVE, "")
				return
			}
			time.Sleep(time.Millisecond)
		}
	}()

	rootfs := &RootfsRestore{
		SourceGeneration: 9,
		Artifacts: []*nodev1.RootfsArtifact{{
			ArtifactId: "rootfs-gen9", Generation: 9, BaseImageDigest: "agent@sha256:base", Format: "oci_layout",
		}},
	}
	_, err := s.Provision(context.Background(), "sp-rootfs", "ref", "m", "", "", "", "", 10,
		registry.Placement{}, nil, "agent@sha256:base", rootfs, nil)
	if err != nil {
		t.Fatal(err)
	}
	got := send.first().GetStart()
	if got.GetRootfsSourceGeneration() != 9 || len(got.GetRootfsArtifacts()) != 1 ||
		got.GetRootfsArtifacts()[0].GetArtifactId() != "rootfs-gen9" {
		t.Fatalf("StartSpawn rootfs restore = gen %d artifacts %+v", got.GetRootfsSourceGeneration(), got.GetRootfsArtifacts())
	}
}
