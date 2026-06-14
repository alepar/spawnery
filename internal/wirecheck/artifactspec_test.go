package wirecheck

import (
	"testing"

	"google.golang.org/protobuf/proto"

	cpv1 "spawnery/gen/cp/v1"
	nodev1 "spawnery/gen/node/v1"
)

func TestCPArtifactSpecRoundTrip(t *testing.T) {
	in := &cpv1.ArtifactSpec{
		Id:              "skill-foo",
		Inline:          []byte("payload"),
		ContentType:     cpv1.ArtifactContentType_ARTIFACT_CONTENT_TYPE_TAR,
		TargetContainer: cpv1.ArtifactTarget_ARTIFACT_TARGET_AGENT,
		DestPath:        "skills/foo",
		Mode:            0o600,
		Sensitive:       true,
		EnvVarName:      "FOO_TOKEN",
	}
	b, err := proto.Marshal(in)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var out cpv1.ArtifactSpec
	if err := proto.Unmarshal(b, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !proto.Equal(in, &out) {
		t.Fatalf("round-trip mismatch: %v != %v", in, &out)
	}
}

func TestCPParentsCarryArtifacts(t *testing.T) {
	req := &cpv1.CreateSpawnRequest{Artifacts: []*cpv1.ArtifactSpec{{Id: "a"}}}
	man := &cpv1.AppManifest{Artifacts: []*cpv1.ArtifactSpec{{Id: "b"}}}
	if len(req.GetArtifacts()) != 1 || len(man.GetArtifacts()) != 1 {
		t.Fatal("CreateSpawnRequest and AppManifest must carry artifacts")
	}
}

func TestNodeArtifactSpecRoundTripAndStart(t *testing.T) {
	in := &nodev1.ArtifactSpec{
		Id:              "cfg",
		Inline:          []byte("x"),
		ContentType:     nodev1.ArtifactContentType_ARTIFACT_CONTENT_TYPE_BYTES,
		TargetContainer: nodev1.ArtifactTarget_ARTIFACT_TARGET_SIDECAR,
		DestPath:        "mcp.json",
		Mode:            0o644,
	}
	b, err := proto.Marshal(in)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var out nodev1.ArtifactSpec
	if err := proto.Unmarshal(b, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !proto.Equal(in, &out) {
		t.Fatalf("round-trip mismatch")
	}
	start := &nodev1.StartSpawn{Artifacts: []*nodev1.ArtifactSpec{in}}
	if len(start.GetArtifacts()) != 1 {
		t.Fatal("StartSpawn must carry artifacts")
	}
}
