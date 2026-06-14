package cp

import (
	"context"
	"strconv"
	"testing"
	"time"

	"connectrpc.com/connect"

	cpv1 "spawnery/gen/cp/v1"
	nodev1 "spawnery/gen/node/v1"
	"spawnery/internal/cp/auth"
	"spawnery/internal/cp/registry"
)

func bytesArt(id, dest string, b []byte) *cpv1.ArtifactSpec {
	return &cpv1.ArtifactSpec{
		Id: id, Inline: b,
		ContentType:     cpv1.ArtifactContentType_ARTIFACT_CONTENT_TYPE_BYTES,
		TargetContainer: cpv1.ArtifactTarget_ARTIFACT_TARGET_AGENT,
		DestPath:        dest, Mode: 0o600,
	}
}

func TestValidateAndMergeArtifacts_OK(t *testing.T) {
	got, err := validateAndMergeArtifacts(nil, []*cpv1.ArtifactSpec{bytesArt("a1", "skills/x", []byte("hi"))})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if len(got) != 1 || got[0].ArtifactID != "a1" || string(got[0].Inline) != "hi" {
		t.Fatalf("got %+v", got)
	}
}

func TestValidateRejectsSensitiveWithInline(t *testing.T) {
	a := bytesArt("s1", "mcp/y", []byte("secret"))
	a.Sensitive = true
	a.EnvVarName = "TOK"
	_, err := validateAndMergeArtifacts(nil, []*cpv1.ArtifactSpec{a})
	if connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("want InvalidArgument, got %v", err)
	}
}

func TestValidateSensitiveMetadataOnlyOK(t *testing.T) {
	a := &cpv1.ArtifactSpec{
		Id: "s1", Sensitive: true, EnvVarName: "TOK",
		ContentType:     cpv1.ArtifactContentType_ARTIFACT_CONTENT_TYPE_BYTES,
		TargetContainer: cpv1.ArtifactTarget_ARTIFACT_TARGET_AGENT,
		DestPath:        "mcp/y",
	}
	got, err := validateAndMergeArtifacts(nil, []*cpv1.ArtifactSpec{a})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if len(got) != 1 || !got[0].Sensitive || len(got[0].Inline) != 0 {
		t.Fatalf("got %+v", got)
	}
}

func TestValidateSensitiveRequiresEnvVar(t *testing.T) {
	a := &cpv1.ArtifactSpec{
		Id: "s1", Sensitive: true,
		ContentType:     cpv1.ArtifactContentType_ARTIFACT_CONTENT_TYPE_BYTES,
		TargetContainer: cpv1.ArtifactTarget_ARTIFACT_TARGET_AGENT,
		DestPath:        "mcp/y",
	}
	if _, err := validateAndMergeArtifacts(nil, []*cpv1.ArtifactSpec{a}); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("want InvalidArgument, got %v", err)
	}
}

func TestValidateRejectsNonSensitiveEmpty(t *testing.T) {
	if _, err := validateAndMergeArtifacts(nil, []*cpv1.ArtifactSpec{bytesArt("a1", "x", nil)}); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("want InvalidArgument, got %v", err)
	}
}

func TestValidateRejectsPathEscape(t *testing.T) {
	for _, p := range []string{"../escape", "/abs", "a/../../b"} {
		if _, err := validateAndMergeArtifacts(nil, []*cpv1.ArtifactSpec{bytesArt("a1", p, []byte("x"))}); connect.CodeOf(err) != connect.CodeInvalidArgument {
			t.Fatalf("path %q: want InvalidArgument, got %v", p, err)
		}
	}
}

func TestValidateRejectsOversizeInline(t *testing.T) {
	big := make([]byte, maxArtifactInlineBytes+1)
	if _, err := validateAndMergeArtifacts(nil, []*cpv1.ArtifactSpec{bytesArt("a1", "x", big)}); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("want InvalidArgument, got %v", err)
	}
}

func TestValidateRejectsTooMany(t *testing.T) {
	many := make([]*cpv1.ArtifactSpec, maxArtifactsPerSpawn+1)
	for i := range many {
		many[i] = bytesArt("a"+strconv.Itoa(i), "x"+strconv.Itoa(i), []byte("x"))
	}
	if _, err := validateAndMergeArtifacts(nil, many); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("count: want InvalidArgument, got %v", err)
	}
}

func TestCreateSpawnPersistsArtifacts(t *testing.T) {
	s, reg, _ := newTestServer(t)
	sender := &capSender{}
	reg.Add(&registry.Node{ID: "n1", Sender: sender, Max: 1, Free: 1})
	go func() {
		deadline := time.Now().Add(2 * time.Second)
		for {
			if st := sender.firstStart(); st != nil {
				s.sched.OnStatus(st.GetSpawnId(), nodev1.SpawnPhase_ACTIVE, "")
				return
			}
			if time.Now().After(deadline) {
				return
			}
			time.Sleep(time.Millisecond)
		}
	}()
	ctx := auth.WithOwner(context.Background(), "alice")
	resp, err := s.CreateSpawn(ctx, connect.NewRequest(&cpv1.CreateSpawnRequest{
		AppId:     "secret-app",
		Model:     "m",
		Artifacts: []*cpv1.ArtifactSpec{bytesArt("a1", "skills/x", []byte("hi"))},
	}))
	if err != nil {
		t.Fatalf("CreateSpawn: %v", err)
	}
	got, err := s.st.Spawns().GetArtifacts(ctx, resp.Msg.SpawnId)
	if err != nil || len(got) != 1 || got[0].ArtifactID != "a1" {
		t.Fatalf("artifacts = %+v, err %v", got, err)
	}
}

func TestCreateSpawnRejectsSensitiveInline(t *testing.T) {
	s, _, _ := newTestServer(t)
	ctx := auth.WithOwner(context.Background(), "alice")
	a := bytesArt("s1", "mcp/y", []byte("secret"))
	a.Sensitive, a.EnvVarName = true, "TOK"
	_, err := s.CreateSpawn(ctx, connect.NewRequest(&cpv1.CreateSpawnRequest{
		AppId:     "secret-app",
		Model:     "m",
		Artifacts: []*cpv1.ArtifactSpec{a},
	}))
	if connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("want InvalidArgument, got %v", err)
	}
}

func TestCreateSpawnRelaysArtifactsOnStartSpawn(t *testing.T) {
	s, reg, _ := newTestServer(t)
	sender := &capSender{}
	reg.Add(&registry.Node{ID: "n1", Sender: sender, Max: 1, Free: 1})
	go func() {
		deadline := time.Now().Add(2 * time.Second)
		for {
			if st := sender.firstStart(); st != nil {
				s.sched.OnStatus(st.GetSpawnId(), nodev1.SpawnPhase_ACTIVE, "")
				return
			}
			if time.Now().After(deadline) {
				return
			}
			time.Sleep(time.Millisecond)
		}
	}()
	ctx := auth.WithOwner(context.Background(), "alice")
	resp, err := s.CreateSpawn(ctx, connect.NewRequest(&cpv1.CreateSpawnRequest{
		AppId:     "secret-app",
		Model:     "m",
		Artifacts: []*cpv1.ArtifactSpec{bytesArt("a1", "skills/x", []byte("payload"))},
	}))
	if err != nil {
		t.Fatalf("CreateSpawn: %v", err)
	}
	waitActive(t, s, resp.Msg.SpawnId)
	st := sender.firstStart()
	if st == nil {
		t.Fatal("no StartSpawn sent")
	}
	if len(st.Artifacts) != 1 || st.Artifacts[0].Id != "a1" || string(st.Artifacts[0].Inline) != "payload" {
		t.Fatalf("StartSpawn.Artifacts = %+v", st.Artifacts)
	}
}

func TestMergeOwnerOverridesManifestById(t *testing.T) {
	m := []*cpv1.ArtifactSpec{bytesArt("dup", "from/manifest", []byte("M")), bytesArt("monly", "m/only", []byte("X"))}
	o := []*cpv1.ArtifactSpec{bytesArt("dup", "from/owner", []byte("O"))}
	got, err := validateAndMergeArtifacts(m, o)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	byID := map[string]string{}
	for _, a := range got {
		byID[a.ArtifactID] = string(a.Inline)
	}
	if byID["dup"] != "O" || byID["monly"] != "X" || len(got) != 2 {
		t.Fatalf("merge wrong: %+v", got)
	}
}
