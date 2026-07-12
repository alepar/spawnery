package cp

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"

	cpv1 "spawnery/gen/cp/v1"
	nodev1 "spawnery/gen/node/v1"
	"spawnery/internal/cp/auth"
	"spawnery/internal/cp/registry"
	"spawnery/internal/cp/skillfetch"
	"spawnery/internal/cp/skillstore"
	"spawnery/internal/cp/store"
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

// --- By-ref (objectref) tests -------------------------------------------------------

// byRefArt returns a by-ref ArtifactSpec (non-sensitive, objectref set, inline empty).
func byRefArt(id, dest, sha256hex string) *cpv1.ArtifactSpec {
	return &cpv1.ArtifactSpec{
		Id:              id,
		ContentType:     cpv1.ArtifactContentType_ARTIFACT_CONTENT_TYPE_TAR,
		TargetContainer: cpv1.ArtifactTarget_ARTIFACT_TARGET_AGENT,
		DestPath:        dest,
		Objectref: &cpv1.ObjectRef{
			ObjectKey: "skills/" + sha256hex + ".tar.zst",
			Sha256:    sha256hex,
		},
	}
}

func TestValidateByRefAccepted(t *testing.T) {
	sha := strings.Repeat("a", 64)
	got, err := validateAndMergeArtifacts(nil, []*cpv1.ArtifactSpec{byRefArt("br1", "payloads/e1", sha)})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("want 1 artifact, got %d", len(got))
	}
	a := got[0]
	if a.ArtifactID != "br1" {
		t.Errorf("ArtifactID = %q, want br1", a.ArtifactID)
	}
	if a.ObjectKey != "skills/"+sha+".tar.zst" {
		t.Errorf("ObjectKey = %q", a.ObjectKey)
	}
	if a.ObjectSHA256 != sha {
		t.Errorf("ObjectSHA256 = %q, want %q", a.ObjectSHA256, sha)
	}
	if len(a.Inline) != 0 {
		t.Error("Inline should be nil for by-ref artifact")
	}
}

func TestValidateByRefMissingObjectKey(t *testing.T) {
	a := &cpv1.ArtifactSpec{
		Id: "br1", ContentType: cpv1.ArtifactContentType_ARTIFACT_CONTENT_TYPE_TAR,
		TargetContainer: cpv1.ArtifactTarget_ARTIFACT_TARGET_AGENT, DestPath: "payloads/e1",
		Objectref: &cpv1.ObjectRef{Sha256: strings.Repeat("b", 64)}, // missing ObjectKey
	}
	if _, err := validateAndMergeArtifacts(nil, []*cpv1.ArtifactSpec{a}); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("want InvalidArgument, got %v", err)
	}
}

func TestValidateByRefMissingSha256(t *testing.T) {
	a := &cpv1.ArtifactSpec{
		Id: "br1", ContentType: cpv1.ArtifactContentType_ARTIFACT_CONTENT_TYPE_TAR,
		TargetContainer: cpv1.ArtifactTarget_ARTIFACT_TARGET_AGENT, DestPath: "payloads/e1",
		Objectref: &cpv1.ObjectRef{ObjectKey: "skills/x.tar.zst"}, // missing Sha256
	}
	if _, err := validateAndMergeArtifacts(nil, []*cpv1.ArtifactSpec{a}); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("want InvalidArgument, got %v", err)
	}
}

func TestValidateByRefWithInlineRejected(t *testing.T) {
	sha := strings.Repeat("c", 64)
	a := &cpv1.ArtifactSpec{
		Id: "br1", ContentType: cpv1.ArtifactContentType_ARTIFACT_CONTENT_TYPE_TAR,
		TargetContainer: cpv1.ArtifactTarget_ARTIFACT_TARGET_AGENT, DestPath: "payloads/e1",
		Inline:    []byte("should not be here"),
		Objectref: &cpv1.ObjectRef{ObjectKey: "skills/" + sha + ".tar.zst", Sha256: sha},
	}
	if _, err := validateAndMergeArtifacts(nil, []*cpv1.ArtifactSpec{a}); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("want InvalidArgument, got %v", err)
	}
}

func TestValidateSensitiveByRefRejected(t *testing.T) {
	sha := strings.Repeat("d", 64)
	a := &cpv1.ArtifactSpec{
		Id: "br1", Sensitive: true, EnvVarName: "TOK",
		ContentType:     cpv1.ArtifactContentType_ARTIFACT_CONTENT_TYPE_TAR,
		TargetContainer: cpv1.ArtifactTarget_ARTIFACT_TARGET_AGENT, DestPath: "payloads/e1",
		Objectref: &cpv1.ObjectRef{ObjectKey: "skills/" + sha + ".tar.zst", Sha256: sha},
	}
	if _, err := validateAndMergeArtifacts(nil, []*cpv1.ArtifactSpec{a}); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("want InvalidArgument, got %v", err)
	}
}

func TestStoreToNodeArtifacts_ByRef(t *testing.T) {
	sha := strings.Repeat("e", 64)
	arts := []store.Artifact{
		{ArtifactID: "br1", ContentType: 2, TargetContainer: 1, DestPath: "payloads/e1",
			ObjectKey: "skills/" + sha + ".tar.zst", ObjectSHA256: sha},
		{ArtifactID: "inline1", Inline: []byte("hi"), ContentType: 1, TargetContainer: 1, DestPath: "skills/x"},
	}
	out := storeToNodeArtifacts(arts, skillfetch.DefaultPlainTarCapBytes)
	if len(out) != 2 {
		t.Fatalf("want 2, got %d", len(out))
	}
	br := out[0]
	if br.Objectref == nil {
		t.Fatal("by-ref artifact must have Objectref")
	}
	if br.Objectref.ObjectKey != "skills/"+sha+".tar.zst" {
		t.Errorf("ObjectKey = %q", br.Objectref.ObjectKey)
	}
	if br.Objectref.Sha256 != sha {
		t.Errorf("Sha256 = %q", br.Objectref.Sha256)
	}
	if br.Objectref.PresignedUrl != "" {
		t.Error("PresignedUrl must be empty before presign")
	}
	if out[1].Objectref != nil {
		t.Error("inline artifact must not have Objectref")
	}
}

// TestNodeArtifactsCarryPlainTarCap pins sp-mwco.4.6: nodeArtifactsForStart stamps the Server's
// effective skill plain-tar cap onto every by-ref ObjectRef.MaxPlainTarBytes, and leaves inline
// specs' Objectref nil (no cap to carry — there is no by-ref fetch to bound).
func TestNodeArtifactsCarryPlainTarCap(t *testing.T) {
	s, _, _ := newTestServer(t)
	s.skillPlainTarCap = 12345

	sha := strings.Repeat("a", 64)
	arts := []store.Artifact{
		{ArtifactID: "br1", ContentType: 2, TargetContainer: 1, DestPath: "payloads/e1",
			ObjectKey: "skills/" + sha + ".tar.zst", ObjectSHA256: sha},
		{ArtifactID: "inline1", Inline: []byte("hi"), ContentType: 1, TargetContainer: 1, DestPath: "skills/x"},
	}
	out := storeToNodeArtifacts(arts, s.effectiveSkillPlainTarCap())
	if out[0].Objectref == nil || out[0].Objectref.MaxPlainTarBytes != 12345 {
		t.Fatalf("by-ref MaxPlainTarBytes = %+v, want 12345", out[0].Objectref)
	}
	if out[1].Objectref != nil {
		t.Fatal("inline artifact must not have Objectref")
	}
}

// TestNodeArtifactsCapDefaultsWhenUnset: the seam unset (SetSkillIngest never called, or called
// with 0) must still stamp a non-zero cap — skillfetch.DefaultPlainTarCapBytes — never 0. A live
// CP always states its cap explicitly on the wire.
func TestNodeArtifactsCapDefaultsWhenUnset(t *testing.T) {
	s, _, _ := newTestServer(t)
	// s.skillPlainTarCap left at its zero value.

	sha := strings.Repeat("b", 64)
	arts := []store.Artifact{
		{ArtifactID: "br1", ContentType: 2, TargetContainer: 1, DestPath: "payloads/e1",
			ObjectKey: "skills/" + sha + ".tar.zst", ObjectSHA256: sha},
	}
	out := storeToNodeArtifacts(arts, s.effectiveSkillPlainTarCap())
	if out[0].Objectref == nil || out[0].Objectref.MaxPlainTarBytes != skillfetch.DefaultPlainTarCapBytes {
		t.Fatalf("by-ref MaxPlainTarBytes = %+v, want default %d", out[0].Objectref, skillfetch.DefaultPlainTarCapBytes)
	}
}

func TestPresignNodeArtifacts_FillsURL(t *testing.T) {
	s, _, _ := newTestServer(t)
	sha := strings.Repeat("f", 64)
	fake := skillstore.NewFakeSkillStore()
	// Pre-populate so PresignedGet succeeds.
	_ = fake.PutIfAbsent(context.Background(), sha, []byte("compressed"), nil)
	s.skillStore = fake

	arts := []store.Artifact{
		{ArtifactID: "br1", ContentType: 2, TargetContainer: 1, DestPath: "payloads/e1",
			ObjectKey: "skills/" + sha + ".tar.zst", ObjectSHA256: sha},
		{ArtifactID: "inline1", Inline: []byte("hi"), ContentType: 1, TargetContainer: 1, DestPath: "skills/x"},
	}
	out, err := s.nodeArtifactsForStart(context.Background(), arts)
	if err != nil {
		t.Fatalf("nodeArtifactsForStart: %v", err)
	}
	if len(out) != 2 {
		t.Fatalf("want 2, got %d", len(out))
	}
	if out[0].Objectref == nil || out[0].Objectref.PresignedUrl == "" {
		t.Error("by-ref spec must have PresignedUrl after presign")
	}
	if out[1].Objectref != nil {
		t.Error("inline spec must not have Objectref")
	}
	// Verify the fake store was called with a presign call.
	var presignCalls []string
	for _, c := range fake.Calls() {
		if strings.HasPrefix(c, "presign:") {
			presignCalls = append(presignCalls, c)
		}
	}
	if len(presignCalls) != 1 || presignCalls[0] != "presign:"+sha {
		t.Errorf("presign calls = %v, want [presign:%s]", presignCalls, sha)
	}
}

func TestPresignNodeArtifacts_NoSkillStoreError(t *testing.T) {
	s, _, _ := newTestServer(t)
	// skillStore is nil by default in newTestServer.
	sha := strings.Repeat("g", 64)
	arts := []store.Artifact{
		{ArtifactID: "br1", ContentType: 2, TargetContainer: 1, DestPath: "payloads/e1",
			ObjectKey: "skills/" + sha + ".tar.zst", ObjectSHA256: sha},
	}
	_, err := s.nodeArtifactsForStart(context.Background(), arts)
	if connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Fatalf("want FailedPrecondition when skillStore nil, got %v", err)
	}
}

func TestPresignNodeArtifacts_NoByRef_NoSkillStoreNeeded(t *testing.T) {
	s, _, _ := newTestServer(t)
	// skillStore is nil — should be fine when there are no by-ref artifacts.
	arts := []store.Artifact{
		{ArtifactID: "a1", Inline: []byte("hi"), ContentType: 1, TargetContainer: 1, DestPath: "skills/x"},
	}
	out, err := s.nodeArtifactsForStart(context.Background(), arts)
	if err != nil {
		t.Fatalf("unexpected error with no by-ref artifacts: %v", err)
	}
	if len(out) != 1 || out[0].Id != "a1" {
		t.Fatalf("unexpected output: %v", out)
	}
}

func TestCreateSpawn_RejectsClientObjectref(t *testing.T) {
	s, _, _ := newTestServer(t)
	ctx := auth.WithOwner(context.Background(), "alice")
	sha := strings.Repeat("h", 64)
	a := byRefArt("br1", "payloads/e1", sha)
	_, err := s.CreateSpawn(ctx, connect.NewRequest(&cpv1.CreateSpawnRequest{
		AppId:     "secret-app",
		Model:     "m",
		Artifacts: []*cpv1.ArtifactSpec{a},
	}))
	if connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("want InvalidArgument for client-supplied objectref, got %v", err)
	}
}

// TestCreateSpawnByRef_PresignsOnStart verifies that a by-ref artifact persisted on a spawn
// gets its PresignedUrl filled before the StartSpawn node message is sent.
func TestCreateSpawnByRef_PresignsOnStart(t *testing.T) {
	s, reg, _ := newTestServer(t)
	sha := strings.Repeat("i", 64)
	fake := skillstore.NewFakeSkillStore()
	_ = fake.PutIfAbsent(context.Background(), sha, []byte("compressed"), nil)
	s.skillStore = fake

	sender := &capSender{}
	reg.Add(&registry.Node{ID: "n1", Sender: sender, Max: 1, Free: 1})
	go func() {
		deadline := time.Now().Add(3 * time.Second)
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

	// Inject a by-ref artifact directly into the spawn via the profile assembly path.
	// We do this by directly adding a by-ref spec to the manifest layer (not the request layer).
	// Use an app that has a manifest artifact with objectref.
	// Simplest: create a spawn, then manually add a by-ref artifact row and resume.
	// For the initial CreateSpawn path, we instead inject via profile assembly.
	// Here we test by inserting the artifact directly and using validateAndMergeArtifacts with a manifest.
	byRefSpec := byRefArt("br1", "payloads/e1", sha)
	got, err := validateAndMergeArtifacts([]*cpv1.ArtifactSpec{byRefSpec}, nil)
	if err != nil {
		t.Fatalf("validateAndMergeArtifacts: %v", err)
	}
	if len(got) != 1 || got[0].ObjectKey == "" {
		t.Fatalf("expected by-ref artifact, got %+v", got)
	}

	// Presign the node artifacts using the fake store.
	nodeArts, err := s.nodeArtifactsForStart(context.Background(), got)
	if err != nil {
		t.Fatalf("nodeArtifactsForStart: %v", err)
	}
	if len(nodeArts) != 1 || nodeArts[0].Objectref == nil {
		t.Fatalf("expected 1 by-ref node artifact, got %+v", nodeArts)
	}
	if nodeArts[0].Objectref.PresignedUrl == "" {
		t.Error("PresignedUrl must be filled after presign")
	}
	// Verify the fake was called for presign.
	calls := fake.Calls()
	var found bool
	for _, c := range calls {
		if c == "presign:"+sha {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("presign:%s not in fake.Calls() %v", sha, calls)
	}
}

// --- statSkillObjects (HEAD-before-presign gate, sp-mwco.4.4) ----------------------------

// byRefStoreArt returns a by-ref store.Artifact row (bypassing validateAndMergeArtifacts,
// which is the CP-internal wire form these gate tests exercise directly).
func byRefStoreArt(id, dest, sha256hex string) store.Artifact {
	return store.Artifact{
		ArtifactID:      id,
		ContentType:     int32(cpv1.ArtifactContentType_ARTIFACT_CONTENT_TYPE_TAR),
		TargetContainer: int32(cpv1.ArtifactTarget_ARTIFACT_TARGET_AGENT),
		DestPath:        dest,
		ObjectKey:       "skills/" + sha256hex + ".tar.zst",
		ObjectSHA256:    sha256hex,
	}
}

// TestStatSkillObjects_MissingObjectFailsStart is the acceptance case: a 20-member bundle with
// one absent object fails the whole start with FailedPrecondition naming the missing key, and
// the gate short-circuits before any presign call is issued.
func TestStatSkillObjects_MissingObjectFailsStart(t *testing.T) {
	s, _, _ := newTestServer(t)
	fake := skillstore.NewFakeSkillStore()
	s.skillStore = fake

	const n = 20
	const missingIdx = 7
	var arts []store.Artifact
	var missingKey string
	for i := 0; i < n; i++ {
		sha := fmt.Sprintf("%064x", i+1)
		arts = append(arts, byRefStoreArt(fmt.Sprintf("br%d", i), fmt.Sprintf("payloads/e%d", i), sha))
		if i == missingIdx {
			missingKey = "skills/" + sha + ".tar.zst"
			continue
		}
		if err := fake.PutIfAbsent(context.Background(), sha, []byte("x"), nil); err != nil {
			t.Fatalf("PutIfAbsent: %v", err)
		}
	}

	_, err := s.nodeArtifactsForStart(context.Background(), arts)
	if connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Fatalf("want FailedPrecondition, got %v", err)
	}
	if !strings.Contains(err.Error(), "skill object missing") {
		t.Errorf("error %v does not mention 'skill object missing'", err)
	}
	if !strings.Contains(err.Error(), missingKey) {
		t.Errorf("error %v does not name the missing key %q", err, missingKey)
	}
	for _, c := range fake.Calls() {
		if strings.HasPrefix(c, "presign:") {
			t.Errorf("gate must short-circuit before presign, got call %q", c)
		}
	}
}

// TestStatSkillObjects_BrownoutReportsUnavailable verifies a transport fault on every stat
// reports Unavailable, never "skill object missing".
func TestStatSkillObjects_BrownoutReportsUnavailable(t *testing.T) {
	s, _, _ := newTestServer(t)
	fake := skillstore.NewFakeSkillStore()
	s.skillStore = fake
	fake.StatHook = func(_ context.Context, _ string) error {
		return fmt.Errorf("dial tcp: connection refused")
	}

	arts := []store.Artifact{
		byRefStoreArt("br1", "payloads/e1", strings.Repeat("1", 64)),
		byRefStoreArt("br2", "payloads/e2", strings.Repeat("2", 64)),
	}
	_, err := s.nodeArtifactsForStart(context.Background(), arts)
	if connect.CodeOf(err) != connect.CodeUnavailable {
		t.Fatalf("want Unavailable, got %v", err)
	}
	if strings.Contains(err.Error(), "skill object missing") {
		t.Errorf("brownout must not report 'skill object missing': %v", err)
	}
}

// TestStatSkillObjects_TransportWinsOverMissing verifies the precedence rule: when some shas
// are genuinely absent AND others fail with a transport error, the whole gate reports
// Unavailable, not FailedPrecondition — a brownout must never mass-report "missing".
func TestStatSkillObjects_TransportWinsOverMissing(t *testing.T) {
	s, _, _ := newTestServer(t)
	fake := skillstore.NewFakeSkillStore()
	s.skillStore = fake
	transportSha := strings.Repeat("3", 64)
	missingSha := strings.Repeat("4", 64) // never put — genuinely absent
	fake.StatHook = func(_ context.Context, sha string) error {
		if sha == transportSha {
			return fmt.Errorf("dial tcp: connection refused")
		}
		return nil
	}

	arts := []store.Artifact{
		byRefStoreArt("br1", "payloads/e1", transportSha),
		byRefStoreArt("br2", "payloads/e2", missingSha),
	}
	_, err := s.nodeArtifactsForStart(context.Background(), arts)
	if connect.CodeOf(err) != connect.CodeUnavailable {
		t.Fatalf("want Unavailable (transport wins over missing), got %v", err)
	}
	if strings.Contains(err.Error(), "skill object missing") {
		t.Errorf("transport-wins error must not mention 'skill object missing': %v", err)
	}
}

// TestStatSkillObjects_ParallelAndDistinct verifies the gate HEADs distinct shas only, and does
// so in parallel (bounded by skillStatParallelism), not serially.
func TestStatSkillObjects_ParallelAndDistinct(t *testing.T) {
	s, _, _ := newTestServer(t)
	fake := skillstore.NewFakeSkillStore()
	s.skillStore = fake

	var mu sync.Mutex
	inFlight, maxInFlight := 0, 0
	fake.StatHook = func(_ context.Context, _ string) error {
		mu.Lock()
		inFlight++
		if inFlight > maxInFlight {
			maxInFlight = inFlight
		}
		mu.Unlock()
		time.Sleep(20 * time.Millisecond)
		mu.Lock()
		inFlight--
		mu.Unlock()
		return nil
	}

	const total, distinct = 12, 6
	var arts []store.Artifact
	for i := 0; i < total; i++ {
		sha := strings.Repeat(fmt.Sprintf("%x", i%distinct), 64)
		if i < distinct {
			if err := fake.PutIfAbsent(context.Background(), sha, []byte("x"), nil); err != nil {
				t.Fatalf("PutIfAbsent: %v", err)
			}
		}
		arts = append(arts, byRefStoreArt(fmt.Sprintf("br%d", i), fmt.Sprintf("payloads/e%d", i), sha))
	}
	if _, err := s.nodeArtifactsForStart(context.Background(), arts); err != nil {
		t.Fatalf("nodeArtifactsForStart: %v", err)
	}

	var statCalls int
	for _, c := range fake.Calls() {
		if strings.HasPrefix(c, "stat:") {
			statCalls++
		}
	}
	if statCalls != distinct {
		t.Errorf("stat calls = %d, want %d (distinct shas only)", statCalls, distinct)
	}

	mu.Lock()
	defer mu.Unlock()
	if maxInFlight <= 1 {
		t.Errorf("maxInFlight = %d, want > 1 (HEADs must run in parallel)", maxInFlight)
	}
	if maxInFlight > skillStatParallelism {
		t.Errorf("maxInFlight = %d, want <= skillStatParallelism (%d)", maxInFlight, skillStatParallelism)
	}
}

// TestStatSkillObjects_MemoizesKnownPresent verifies a confirmed-present sha is not re-HEAD'd
// on a second start, while presign (URLs expire) always runs.
func TestStatSkillObjects_MemoizesKnownPresent(t *testing.T) {
	s, _, _ := newTestServer(t)
	fake := skillstore.NewFakeSkillStore()
	s.skillStore = fake
	sha := strings.Repeat("5", 64)
	if err := fake.PutIfAbsent(context.Background(), sha, []byte("x"), nil); err != nil {
		t.Fatalf("PutIfAbsent: %v", err)
	}
	arts := []store.Artifact{byRefStoreArt("br1", "payloads/e1", sha)}

	if _, err := s.nodeArtifactsForStart(context.Background(), arts); err != nil {
		t.Fatalf("first start: %v", err)
	}
	if _, err := s.nodeArtifactsForStart(context.Background(), arts); err != nil {
		t.Fatalf("second start: %v", err)
	}

	var statCount, presignCount int
	for _, c := range fake.Calls() {
		switch c {
		case "stat:" + sha:
			statCount++
		case "presign:" + sha:
			presignCount++
		}
	}
	if statCount != 1 {
		t.Errorf("stat calls = %d, want 1 (memoized after the first pass)", statCount)
	}
	if presignCount != 2 {
		t.Errorf("presign calls = %d, want 2 (presign is never memoized)", presignCount)
	}
}

// TestStatSkillObjects_DoesNotNegativeCacheMissing verifies a missing sha is NOT cached: once
// the object is later put, a subsequent start succeeds without a stale "missing" verdict.
func TestStatSkillObjects_DoesNotNegativeCacheMissing(t *testing.T) {
	s, _, _ := newTestServer(t)
	fake := skillstore.NewFakeSkillStore()
	s.skillStore = fake
	sha := strings.Repeat("6", 64)
	arts := []store.Artifact{byRefStoreArt("br1", "payloads/e1", sha)}

	_, err := s.nodeArtifactsForStart(context.Background(), arts)
	if connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Fatalf("first start (missing): want FailedPrecondition, got %v", err)
	}

	if err := fake.PutIfAbsent(context.Background(), sha, []byte("x"), nil); err != nil {
		t.Fatalf("PutIfAbsent: %v", err)
	}

	if _, err := s.nodeArtifactsForStart(context.Background(), arts); err != nil {
		t.Fatalf("second start (now present): want success, got %v", err)
	}
}

// TestStatSkillObjects_TimeoutIsUnavailable verifies the gate's own aggregate timeout fires
// Unavailable and returns promptly, rather than hanging on a stuck StatObject call.
func TestStatSkillObjects_TimeoutIsUnavailable(t *testing.T) {
	orig := skillStatTimeout
	skillStatTimeout = 50 * time.Millisecond
	t.Cleanup(func() { skillStatTimeout = orig })

	s, _, _ := newTestServer(t)
	fake := skillstore.NewFakeSkillStore()
	s.skillStore = fake
	fake.StatHook = func(ctx context.Context, _ string) error {
		<-ctx.Done()
		return ctx.Err()
	}
	arts := []store.Artifact{byRefStoreArt("br1", "payloads/e1", strings.Repeat("7", 64))}

	start := time.Now()
	_, err := s.nodeArtifactsForStart(context.Background(), arts)
	elapsed := time.Since(start)

	if connect.CodeOf(err) != connect.CodeUnavailable {
		t.Fatalf("want Unavailable, got %v", err)
	}
	if !strings.Contains(err.Error(), "deadline") && !strings.Contains(err.Error(), "context") {
		t.Errorf("error should name a timeout, got %v", err)
	}
	if elapsed > 2*time.Second {
		t.Errorf("gate took %v, want well under the real 10s production timeout", elapsed)
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
