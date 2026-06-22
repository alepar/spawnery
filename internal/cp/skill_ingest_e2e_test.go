//go:build e2e

package cp_test

// TestCPSkillIngestE2E is the §4.12 acceptance e2e for the GitHub-URL skill ingest path.
// It proves the full chain end-to-end:
//
//	IngestSkillFromURL → catalog row → profile entry (CATALOG_REF, SKILL kind) →
//	CreateSpawn (claude-tui, spawnery/agent:dev) → ACTIVE →
//	SKILL.md lands in /root/.claude/skills/<name>/ inside the agent container
//
// S2 probe (presign reachability): NodeEndpoint is the Docker bridge gateway (172.17.0.1:3900),
// not the loopback. If the spawn reaches ACTIVE with SKILL.md present, the spawnlet fetched the
// presigned URL successfully at that node-reachable host, proving SigV4 validity + routability (S2-A).
// S2-B runs an explicit curl inside the agent container (separate netns) as a best-effort cross-check.
//
// S4 probe (resume Garage dependency): suspend the spawn, deny the journal key on the skills
// bucket (isolated — journal remains available), attempt resume. Whether the spawn comes back
// ACTIVE with SKILL.md present determines whether first-create-only gating is safe.
//
// Requires:
//   - Docker + spawnery/agent:dev + spawnery/sidecar:dev (built with agentinstall + apply-artifacts.sh)
//   - Garage running (just garage), dev-creds.env present at deploy/garage/dev-creds.env
//   - OPENROUTER_API_KEY in env or .env at repo root
//   - Network access to github.com / api.github.com / codeload.github.com
//
// FAILS loudly (no skips) per the build-tag-is-opt-in rule when any dep is down.
// The test is intentionally slow (~120s per spawn boot); budget ~8 minutes.

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
	"golang.org/x/net/http2"
	"golang.org/x/net/http2/h2c"

	cpv1 "spawnery/gen/cp/v1"
	"spawnery/gen/cp/v1/cpv1connect"
	"spawnery/gen/node/v1/nodev1connect"
	"spawnery/internal/cp"
	"spawnery/internal/cp/auth"
	"spawnery/internal/cp/registry"
	"spawnery/internal/cp/router"
	"spawnery/internal/cp/scheduler"
	"spawnery/internal/cp/skillfetch"
	"spawnery/internal/cp/skillstore"
	"spawnery/internal/cp/store"
	"spawnery/internal/cp/telemetry"
	"spawnery/internal/node"
	"spawnery/internal/runtime"
	"spawnery/internal/spawnlet"
	"spawnery/internal/storage/journal"
)

// skillIngestStack bundles the artefacts returned by setupSkillIngestStack.
type skillIngestStack struct {
	client     cpv1connect.SpawnServiceClient
	ctx        context.Context
	appID      string
	srv        *cp.Server
	skillStore skillstore.SkillStore
	s3cfg      journal.S3Config // shared JOURNAL_S3_* creds, for listing bucket in S2-B
	bucketID   string           // garage bucket id for spawnery-skills (S4 DenyKeyOnBucket)
}

// garageAdminCredsFromEnv reads JOURNAL_GARAGE_ADMIN_ENDPOINT and JOURNAL_GARAGE_ADMIN_TOKEN
// from the environment (set by deploy/garage/dev-creds.env). Returns ("","") if absent.
func garageAdminCredsFromEnv() (endpoint, token string) {
	endpoint = os.Getenv("JOURNAL_GARAGE_ADMIN_ENDPOINT")
	token = os.Getenv("JOURNAL_GARAGE_ADMIN_TOKEN")
	return
}

// garageAdminCredsFromFile reads the admin creds from deploy/garage/dev-creds.env (repo-relative;
// test CWD is internal/cp).
func garageAdminCredsFromFile() (endpoint, token string) {
	credsFile := filepath.Join("..", "..", "deploy", "garage", "dev-creds.env")
	data, err := os.ReadFile(credsFile)
	if err != nil {
		return
	}
	kv := map[string]string{}
	for _, line := range strings.Split(string(data), "\n") {
		k, v, ok := strings.Cut(strings.TrimSpace(line), "=")
		if !ok || strings.HasPrefix(k, "#") {
			continue
		}
		kv[k] = v
	}
	return kv["JOURNAL_GARAGE_ADMIN_ENDPOINT"], kv["JOURNAL_GARAGE_ADMIN_TOKEN"]
}

// loadGarageAdminCreds returns the Garage admin endpoint + token, failing loudly if absent.
func loadGarageAdminCreds(t *testing.T) (endpoint, token string) {
	t.Helper()
	endpoint, token = garageAdminCredsFromEnv()
	if endpoint != "" && token != "" {
		return
	}
	endpoint, token = garageAdminCredsFromFile()
	if endpoint == "" || token == "" {
		t.Fatalf("Garage admin credentials not available — start the dev Garage (`just garage`) and ensure JOURNAL_GARAGE_ADMIN_ENDPOINT + JOURNAL_GARAGE_ADMIN_TOKEN are in deploy/garage/dev-creds.env")
	}
	return
}

// provisionSkillsBucket idempotently creates the spawnery-skills bucket (if absent) and
// grants the journal access key read/write/owner on it. Returns the bucket id.
// This is the out-of-band provisioning step required because the journal key is Forbidden
// for MakeBucket (spike S1 finding, §4.3).
func provisionSkillsBucket(t *testing.T, adminEndpoint, adminToken, accessKeyID string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	admin, err := journal.NewGarageAdmin(adminEndpoint, adminToken, nil)
	if err != nil {
		t.Fatalf("garage admin client: %v", err)
	}

	bucketID, err := admin.EnsureBucket(ctx, skillstore.DefaultBucket)
	if err != nil {
		t.Fatalf("EnsureBucket %q: %v — ensure Garage is running (`just garage`)", skillstore.DefaultBucket, err)
	}
	t.Logf("skills bucket id: %s", bucketID)

	if err := admin.AllowKeyOnBucket(ctx, bucketID, accessKeyID); err != nil {
		t.Fatalf("AllowKeyOnBucket skills→journal key: %v", err)
	}
	t.Log("skills bucket provisioned (idempotent)")
	return bucketID
}

// dockerBridgeGatewayIP attempts to resolve the Docker bridge gateway IP from the
// docker0 interface. Falls back to 172.17.0.1 (Docker default) if unavailable.
// Used as the NodeEndpoint for the skills S3 store so presigned URLs use a
// non-loopback address routable from Docker containers (S2-A faithfulness, §4.6).
func dockerBridgeGatewayIP(t *testing.T) string {
	t.Helper()
	// Try `docker network inspect bridge` for the authoritative gateway address.
	out, err := exec.Command("docker", "network", "inspect", "bridge",
		"--format", "{{range .IPAM.Config}}{{.Gateway}}{{end}}",
	).Output()
	if err == nil {
		ip := strings.TrimSpace(string(out))
		if ip != "" {
			t.Logf("docker bridge gateway: %s (from network inspect)", ip)
			return ip
		}
	}
	// Fallback to the well-known Docker default.
	t.Log("docker bridge gateway: 172.17.0.1 (fallback)")
	return "172.17.0.1"
}

// setupSkillIngestStack builds a full CP+node+Garage-backed-skillstore stack.
// It is modelled on setupTmuxStack with two additions:
//   - It returns *cp.Server (so the test can call srv.SetSkillIngest).
//   - It provisions the spawnery-skills bucket and wires the real skillstore + fetcher into the server.
//
// NodeEndpoint is set to the Docker bridge gateway (not 127.0.0.1) to make presigned GET URLs
// routable from Docker containers — this is the S2-A faithfulness measure (§4.6).
func setupSkillIngestStack(t *testing.T, opts ...func(*spawnlet.ManagerConfig)) skillIngestStack {
	t.Helper()

	// --- OPENROUTER key (required for claude-tui) ---
	key := os.Getenv("OPENROUTER_API_KEY")
	if key == "" {
		envPath, _ := filepath.Abs("../../.env")
		if raw, err := os.ReadFile(envPath); err == nil {
			for _, line := range splitLines(string(raw)) {
				const prefix = "OPENROUTER_API_KEY="
				if len(line) > len(prefix) && line[:len(prefix)] == prefix {
					key = line[len(prefix):]
				}
			}
		}
	}
	if key == "" {
		t.Fatal("OPENROUTER_API_KEY is required for the skill ingest e2e (set env or put in .env)")
	}

	// --- GITHUB_TOKEN (optional; raises api.github.com rate-limit from ~60/hr to 5000/hr) ---
	githubToken := os.Getenv("GITHUB_TOKEN")

	// --- Docker ---
	rt, err := runtime.NewDocker()
	if err != nil {
		t.Fatalf("docker unavailable: %v", err)
	}
	if err := rt.Ping(context.Background()); err != nil {
		t.Fatalf("docker not pingable: %v", err)
	}

	// --- Garage S3 credentials ---
	s3cfg, ok := garageS3Config(t)
	if !ok {
		t.Fatalf("Garage S3 credentials not available — start the dev Garage (`just garage`) " +
			"or set JOURNAL_S3_ENDPOINT + JOURNAL_S3_ACCESS_KEY + JOURNAL_S3_SECRET_KEY")
	}

	// --- Garage admin credentials ---
	adminEndpoint, adminToken := loadGarageAdminCreds(t)

	// --- Provision the skills bucket (idempotent; fails loud if Garage is down) ---
	bucketID := provisionSkillsBucket(t, adminEndpoint, adminToken, s3cfg.AccessKeyID)

	// --- NodeEndpoint: docker bridge gateway for S2-A faithfulness ---
	bridgeIP := dockerBridgeGatewayIP(t)
	// NodeEndpoint uses the same port as JOURNAL_S3_ENDPOINT but with the bridge IP
	// so presigned GET URLs are routable from containers on the bridge network.
	nodeEndpoint := bridgeIP + ":3900"
	t.Logf("S2-A: skillstore NodeEndpoint=%s (SigV4 will bind to this host)", nodeEndpoint)

	// --- Skill store backed by Garage ---
	ss, err := skillstore.New(skillstore.Config{
		Endpoint:        s3cfg.Endpoint,
		NodeEndpoint:    nodeEndpoint,
		AccessKeyID:     s3cfg.AccessKeyID,
		SecretAccessKey: s3cfg.SecretAccessKey,
		Region:          s3cfg.Region,
		DisableTLS:      s3cfg.DisableTLS,
		Bucket:          skillstore.DefaultBucket,
	})
	if err != nil {
		t.Fatalf("skillstore.New: %v", err)
	}

	// --- Skill fetcher (real GitHub HTTPS fetch) ---
	fetcher := skillfetch.New(skillfetch.Config{
		GitHubToken: githubToken,
		ZstdLevel:   3,
	})

	// --- CP ---
	reg := registry.New()
	rtr := router.New()
	sched := scheduler.New(reg, rtr, 60*time.Second)
	tel, err := telemetry.NewJSONLSink(filepath.Join(t.TempDir(), "events.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	authn := auth.NewVerifier(auth.VerifierConfig{DevTokens: map[string]string{"dev-token": "alice"}, DevMode: true})
	appRef, err := filepath.Abs("../../examples/secret-app")
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(context.Background(), store.Config{
		Driver: "sqlite",
		DSN:    "file:cpskille2e_" + t.Name() + "?mode=memory&cache=shared&_pragma=foreign_keys(1)",
	})
	if err != nil {
		t.Fatal(err)
	}
	appID := "secret-app"
	if err := cp.Seed(context.Background(), st, map[string]string{"dev-token": "alice"},
		[]cp.AppSeed{{ID: appID, Ref: appRef, Version: "1.0.0", Mounts: []string{"main"}}}); err != nil {
		t.Fatal(err)
	}
	srv := cp.NewServer(reg, rtr, sched, st, tel)

	// Wire the skill fetcher + store into the CP (the seam at ingest_skill.go:179).
	// Without this, IngestSkillFromURL returns FailedPrecondition.
	srv.SetSkillIngest(fetcher, ss)

	mux := http.NewServeMux()
	mux.Handle(nodev1connect.NewNodeServiceHandler(srv))
	mux.Handle(cpv1connect.NewSpawnServiceHandler(srv, connect.WithInterceptors(authn.Interceptor())))
	cpSrv := httptest.NewUnstartedServer(h2c.NewHandler(mux, &http2.Server{}))
	cpSrv.Start()

	t.Cleanup(func() { _ = tel.Close() })
	t.Cleanup(func() { _ = st.Close() })
	t.Cleanup(cpSrv.Close)

	// --- node (attached) with real Docker + agent image ---
	mgrCfg := spawnlet.ManagerConfig{
		AgentImage:    "spawnery/agent:dev",
		SidecarImage:  "spawnery/sidecar:dev",
		OpenRouterKey: key,
		DataRoot:      t.TempDir(),
	}
	for _, opt := range opts {
		opt(&mgrCfg)
	}
	mgr := spawnlet.NewManager(rt, mgrCfg)
	nodeCtx, stopNode := context.WithCancel(context.Background())
	t.Cleanup(stopNode)
	go node.Run(nodeCtx, mgr, h2cClient(), node.Config{
		NodeID:    "n-skillingest",
		CPURL:     cpSrv.URL,
		MaxSpawns: 2,
		AgentImage: "spawnery/agent:dev",
		// Advertise claude-code so the CP can select claude-tui.
		AgentBinaries: []string{"opencode", "goose", "claude-code"},
	})

	// Wait for node to register.
	deadline := time.Now().Add(15 * time.Second)
	for {
		if _, ok := reg.Get("n-skillingest"); ok {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("node never registered with CP")
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Log("node registered")

	// --- client ---
	cl := cpv1connect.NewSpawnServiceClient(h2cClient(), cpSrv.URL, connect.WithGRPC(),
		connect.WithInterceptors(bearer("dev-token")))

	// Context with 8-minute budget: 2 spawns × 120s boot + S4 overhead.
	var cancel context.CancelFunc
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	t.Cleanup(cancel)

	return skillIngestStack{
		client:     cl,
		ctx:        ctx,
		appID:      appID,
		srv:        srv,
		skillStore: ss,
		s3cfg:      s3cfg,
		bucketID:   bucketID,
	}
}

// sha256FromSkillsBucket lists the spawnery-skills bucket and returns the sha256hex of the
// first skills/<sha256>.tar.zst object found. Used for S2-B curl probe.
// Fails the test if no objects are found.
func sha256FromSkillsBucket(t *testing.T, s3cfg journal.S3Config) string {
	t.Helper()
	endpoint := s3cfg.Endpoint
	endpoint = strings.TrimPrefix(endpoint, "https://")
	endpoint = strings.TrimPrefix(endpoint, "http://")
	mc, err := minio.New(endpoint, &minio.Options{
		Creds:  credentials.NewStaticV4(s3cfg.AccessKeyID, s3cfg.SecretAccessKey, ""),
		Secure: !s3cfg.DisableTLS,
		Region: s3cfg.Region,
	})
	if err != nil {
		t.Fatalf("sha256FromSkillsBucket: minio client: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	var sha256hex string
	for obj := range mc.ListObjects(ctx, skillstore.DefaultBucket, minio.ListObjectsOptions{Prefix: "skills/"}) {
		if obj.Err != nil {
			t.Fatalf("sha256FromSkillsBucket: list objects: %v", obj.Err)
		}
		// Key format: skills/<sha256hex>.tar.zst
		key := obj.Key
		key = strings.TrimPrefix(key, "skills/")
		key = strings.TrimSuffix(key, ".tar.zst")
		if key != "" {
			sha256hex = key
			break
		}
	}
	if sha256hex == "" {
		t.Fatal("sha256FromSkillsBucket: no skill objects found in bucket (ingest may have failed)")
	}
	return sha256hex
}

// TestCPSkillIngestE2E is the §4.12 acceptance chain for GitHub-URL skill ingest.
// Runs the full path: ingest → profile → spawn → SKILL.md present in agent container.
// Also runs the S2 (presign cross-netns) and S4 (resume Garage dependency) probes.
func TestCPSkillIngestE2E(t *testing.T) {
	// Pre-flight: validate Docker and agent image.
	if out, err := exec.Command("docker", "info").CombinedOutput(); err != nil {
		t.Fatalf("Docker is required: %v\n%s", err, out)
	}
	if out, err := exec.Command("docker", "run", "--rm", "--entrypoint", "which",
		"spawnery/agent:dev", "agentinstall").CombinedOutput(); err != nil {
		t.Fatalf("spawnery/agent:dev must include agentinstall (run `make images`): %v\n%s", err, out)
	}

	stk := setupSkillIngestStack(t)
	cl, ctx, appID := stk.client, stk.ctx, stk.appID

	// ── Phase 1: Ingest a real GitHub skill ───────────────────────────────────────────────────
	// Uses 199-biotechnologies/claude-deep-research-skill — a real, publicly accessible skill
	// with a top-level SKILL.md + frontmatter name. Unauthenticated api.github.com is ~60/hr;
	// set GITHUB_TOKEN to raise the budget to 5000/hr.
	// FAILS (not skips) when GitHub is unreachable — a missing network is an env error.
	const skillURL = "https://github.com/199-biotechnologies/claude-deep-research-skill"
	t.Logf("phase 1: IngestSkillFromURL %s", skillURL)

	ingestResp, err := cl.IngestSkillFromURL(ctx, connect.NewRequest(&cpv1.IngestSkillFromURLRequest{
		Url: skillURL,
	}))
	if err != nil {
		// Map connect error codes to actionable messages.
		switch connect.CodeOf(err) {
		case connect.CodeUnavailable:
			t.Fatalf("IngestSkillFromURL: GitHub is unreachable — check network + GITHUB_TOKEN: %v", err)
		case connect.CodeResourceExhausted:
			t.Fatalf("IngestSkillFromURL: GitHub rate limit — set GITHUB_TOKEN: %v", err)
		case connect.CodeFailedPrecondition:
			t.Fatalf("IngestSkillFromURL: skill ingest seam not wired (SetSkillIngest not called?): %v", err)
		default:
			t.Fatalf("IngestSkillFromURL: %v", err)
		}
	}
	catalogID := ingestResp.Msg.CatalogId
	t.Logf("phase 1: ingested catalog_id=%s", catalogID)

	// ── Phase 2: Read the installed name (must not hardcode the repo leaf) ────────────────────
	// The SKILL.md frontmatter may rename the skill (§4.10). Read the actual name from the
	// catalog entry so the SKILL.md path assertion uses the correct directory.
	t.Log("phase 2: GetCatalogEntry → installed name")
	ceResp, err := cl.GetCatalogEntry(ctx, connect.NewRequest(&cpv1.GetCatalogEntryRequest{
		CatalogId: catalogID,
	}))
	if err != nil {
		t.Fatalf("GetCatalogEntry: %v", err)
	}
	skillName := ceResp.Msg.Entry.GetName()
	if skillName == "" {
		t.Fatal("catalog entry has empty name — ingest did not resolve a name from frontmatter or repo leaf")
	}
	t.Logf("phase 2: skill installed name = %q (catalog_id=%s)", skillName, catalogID)

	// ── Phase 3: Create a profile with the URL-ingested skill as a CATALOG_REF entry ─────────
	t.Log("phase 3: CreateProfile + AddProfileEntry(SKILL, CATALOG_REF)")
	profResp, err := cl.CreateProfile(ctx, connect.NewRequest(&cpv1.CreateProfileRequest{
		Name: "skill-ingest-e2e",
	}))
	if err != nil {
		t.Fatalf("CreateProfile: %v", err)
	}
	profileID := profResp.Msg.ProfileId

	_, err = cl.AddProfileEntry(ctx, connect.NewRequest(&cpv1.AddProfileEntryRequest{
		ProfileId:       profileID,
		ExpectedVersion: 1,
		Entry: &cpv1.ProfileEntry{
			Kind:      cpv1.ProfileEntryKind_PROFILE_ENTRY_KIND_SKILL,
			Name:      skillName,
			Source:    cpv1.ProfileEntrySource_PROFILE_ENTRY_SOURCE_CATALOG_REF,
			CatalogId: catalogID,
		},
	}))
	if err != nil {
		t.Fatalf("AddProfileEntry: %v", err)
	}
	t.Logf("phase 3: profile %s with skill entry %q", profileID, skillName)

	// ── Phase 4: CreateSpawn with claude-tui and the skill profile ────────────────────────────
	// Uses spawnery/agent:dev (real agent image with agentinstall). The sidecar handles
	// model key; OPENROUTER_API_KEY is required for the sidecar to reach ACTIVE.
	t.Log("phase 4: CreateSpawn (claude-tui, spawnery/agent:dev, profile attached)")
	cs, err := cl.CreateSpawn(ctx, connect.NewRequest(&cpv1.CreateSpawnRequest{
		AppId:      appID,
		Model:      "openai/gpt-4o-mini",
		Image:      "spawnery/agent:dev",
		RunnableId: "claude-tui",
		ProfileId:  profileID,
	}))
	if err != nil {
		t.Fatalf("CreateSpawn: %v", err)
	}
	spawnID := cs.Msg.SpawnId
	t.Logf("phase 4: spawn created %s", spawnID)
	t.Cleanup(func() {
		stopCtx, c := context.WithTimeout(context.Background(), 10*time.Second)
		defer c()
		_, _ = cl.StopSpawn(stopCtx, connect.NewRequest(&cpv1.StopSpawnRequest{SpawnId: spawnID}))
		time.Sleep(2 * time.Second)
	})

	// ── Phase 5: Wait for ACTIVE ──────────────────────────────────────────────────────────────
	// Container boot + tmux startup + artifact materialization (presigned tarball fetch from
	// Garage + apply-artifacts.sh): allow 120s. The skill path is strictly slower than plain
	// tmux; 60s (waitActiveTmux's budget) is not sufficient here.
	t.Log("phase 5: waiting for ACTIVE (120s budget, skill artifact path)")
	waitActiveWithTimeout(ctx, t, cl, spawnID, 120*time.Second)
	t.Logf("phase 5: spawn %s is ACTIVE", spawnID)

	// ── Phase 6: Assert SKILL.md is present in the agent container ────────────────────────────
	// Primary acceptance assertion (§4.12) AND the implicit S2-A proof:
	// the spawn could only reach ACTIVE with SKILL.md present if the spawnlet successfully
	// fetched the CP-presigned URL — which was signed against the Docker bridge gateway
	// (NodeEndpoint=172.17.0.1:3900), not the CP-internal loopback. If the SigV4 binding
	// or host routability was wrong, the fetch would 403/fail and the spawn would ERROR.
	gen := findSpawnGeneration(ctx, t, cl, spawnID)
	agentContainer := findAgentContainer(ctx, t, spawnID, gen)
	t.Logf("phase 6: agent container %s (gen %d)", agentContainer, gen)

	skillMDPath := fmt.Sprintf("/root/.claude/skills/%s/SKILL.md", skillName)
	t.Logf("phase 6: asserting SKILL.md at %s", skillMDPath)

	// Diagnostics: dump the artifacts staging dir and apply-report.json to understand what agentinstall saw.
	manifestContent, _ := exec.CommandContext(ctx, "docker", "exec", agentContainer,
		"cat", "/run/spawnery/artifacts/manifest.json").CombinedOutput()
	t.Logf("phase 6: /run/spawnery/artifacts/manifest.json: %s", strings.TrimSpace(string(manifestContent)))

	payloadsTree, _ := exec.CommandContext(ctx, "docker", "exec", agentContainer,
		"find", "/run/spawnery/artifacts/payloads", "-maxdepth", "3").CombinedOutput()
	t.Logf("phase 6: payloads tree: %s", strings.TrimSpace(string(payloadsTree)))

	reportContent, _ := exec.CommandContext(ctx, "docker", "exec", agentContainer,
		"cat", "/run/spawnery/artifacts/apply-report.json").CombinedOutput()
	t.Logf("phase 6: apply-report.json: %s", strings.TrimSpace(string(reportContent)))

	skillContent := dockerOutputNoFail(ctx, t, agentContainer, skillMDPath)
	if skillContent == "" {
		// Diagnostics: list ~/.claude/skills to see what got installed
		installed, _ := exec.CommandContext(ctx, "docker", "exec", agentContainer,
			"find", "/root/.claude/skills", "-name", "SKILL.md", "-o", "-type", "d").CombinedOutput()
		t.Fatalf("SKILL.md not found at %s in agent container %s\nInstalled tree:\n%s",
			skillMDPath, agentContainer, string(installed))
	}
	t.Logf("phase 6: SKILL.md present (%d bytes) — §4.12 acceptance chain PASS", len(skillContent))
	t.Logf("phase 6: S2-A IMPLICIT PROOF: spawn reached ACTIVE+SKILL.md via presign at NodeEndpoint=%s:3900 (not loopback)", dockerBridgeGatewayIP(t))

	// ── Phase 7: S2-B explicit cross-netns curl probe (best-effort) ──────────────────────────
	// The agent container has its own network namespace. This probe presigns the skill object
	// URL directly and then runs `curl` from inside the container — a genuinely separate netns
	// fetch that validates both URL routability and SigV4 signature from a container perspective.
	// Caveats: per-pod egress floor may block S3 traffic; treat as diagnostic, not a hard gate.
	t.Log("phase 7: S2-B — explicit curl probe from agent container netns (best-effort)")
	runS2BProbe(t, ctx, stk, agentContainer)

	// ── Phase 8: S4 — resume-with-skill-unavailable ───────────────────────────────────────────
	// Suspend the spawn, then make the skill object inaccessible (isolated: deny the journal key
	// on just the spawnery-skills bucket; journal remains available so journal-restore is NOT
	// confounded). Attempt resume; observe whether it succeeds and whether SKILL.md is present.
	//
	// CONFOUND NOTE: if Garage were fully stopped (docker stop garage), the journal backend would
	// also fail — masking the skill-specific signal. Using DenyKeyOnBucket isolates skill access.
	// `just garage-down` is also off-limits (it wipes volumes via `docker compose down -v`).
	t.Log("phase 8: S4 — suspend → deny skills bucket → resume → observe")
	s4Finding := runS4Probe(t, ctx, stk, cl, appID, spawnID, skillName, profileID, false /* DeltaCapture=false */)
	t.Logf("phase 8: S4 finding (DeltaCapture=false): %s", s4Finding)

	// S4 with DeltaCapture=true: the delta image bakes the agent rootfs (including
	// ~/.claude/skills/) on suspend. If the skill survives resume without Garage,
	// first-create-only gating is safe for the delta path.
	t.Log("phase 8b: S4 — DeltaCapture=true variant")
	s4FindingDelta := runS4DeltaCapture(t, ctx, stk, appID, skillName, profileID)
	t.Logf("phase 8b: S4 finding (DeltaCapture=true): %s", s4FindingDelta)

	// Summary log for the structured output field.
	t.Logf("S4 SUMMARY: DeltaCapture=false → %s | DeltaCapture=true → %s", s4Finding, s4FindingDelta)
}

// dockerOutputNoFail runs `docker exec <container> cat <path>` and returns the output.
// Returns empty string (with a t.Log) if the command fails; does NOT fatalf.
func dockerOutputNoFail(ctx context.Context, t *testing.T, containerID, path string) string {
	t.Helper()
	out, err := exec.CommandContext(ctx, "docker", "exec", containerID, "cat", path).CombinedOutput()
	if err != nil {
		t.Logf("docker exec cat %s: %v (output: %q)", path, err, strings.TrimSpace(string(out)))
		return ""
	}
	return strings.TrimSpace(string(out))
}

// runS2BProbe runs the explicit S2-B cross-netns curl probe from inside the agent container.
// Presigns the skill object URL and runs `curl` from the container's netns (a separate netns
// from the test host). Failures are logged but do not fatalf — S2-B is best-effort (see §S2 caveats).
func runS2BProbe(t *testing.T, ctx context.Context, stk skillIngestStack, agentContainer string) {
	t.Helper()

	// Get the sha256 of the skill object by listing the bucket (no extra GitHub fetch needed).
	sha256hex := sha256FromSkillsBucket(t, stk.s3cfg)
	t.Logf("S2-B: skill object sha256=%s", sha256hex)

	// Presign against the node-reachable endpoint (same as what the CP used at StartSpawn).
	presignCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	presignedURL, err := stk.skillStore.PresignedGet(presignCtx, sha256hex)
	if err != nil {
		t.Logf("S2-B: PresignedGet failed (non-fatal): %v", err)
		return
	}
	t.Logf("S2-B: presigned URL host=%s", extractURLHost(presignedURL))

	// Run curl inside the agent container to fetch the object.
	// The agent container is on the Docker bridge → 172.17.0.1:3900 is reachable.
	// The egress floor may still block it; treat failure as informational.
	curlCtx, curlCancel := context.WithTimeout(ctx, 30*time.Second)
	defer curlCancel()
	out, err := exec.CommandContext(curlCtx, "docker", "exec", agentContainer,
		"curl", "--silent", "--fail", "--max-time", "20",
		"--output", "/tmp/skill-s2b-probe.tzst",
		presignedURL,
	).CombinedOutput()
	if err != nil {
		t.Logf("S2-B: curl from container failed (non-fatal — egress floor may block or URL host unreachable): %v\n%s", err, string(out))
		t.Log("S2-B: INCONCLUSIVE (not a hard gate; S2-A implicit proof remains)")
		return
	}
	t.Log("S2-B: curl succeeded — cross-netns presigned GET confirmed from agent container")

	// Verify the downloaded object is non-empty (sha256 check would need the container to compute it).
	sizeOut, _ := exec.CommandContext(curlCtx, "docker", "exec", agentContainer,
		"sh", "-c", "wc -c < /tmp/skill-s2b-probe.tzst",
	).Output()
	t.Logf("S2-B: downloaded object size: %s bytes", strings.TrimSpace(string(sizeOut)))
	t.Log("S2-B: PASS — URL is routable from a separate (pod) network namespace")
}

// extractURLHost returns the host:port from a URL string for logging.
func extractURLHost(rawURL string) string {
	// Simple extraction: find between "://" and "/"
	s := rawURL
	if idx := strings.Index(s, "://"); idx >= 0 {
		s = s[idx+3:]
	}
	if idx := strings.Index(s, "/"); idx >= 0 {
		s = s[:idx]
	}
	return s
}

// runS4Probe suspends the given spawnID, denies the journal key on the skills bucket (isolated),
// attempts to resume the spawn, and returns a description of the outcome.
// It always restores the key permissions before returning.
func runS4Probe(t *testing.T, ctx context.Context, stk skillIngestStack,
	cl cpv1connect.SpawnServiceClient, appID, spawnID, skillName, profileID string,
	deltaCapture bool) string {
	t.Helper()

	// Suspend the spawn.
	t.Log("S4: SuspendSpawn")
	if _, err := cl.SuspendSpawn(ctx, connect.NewRequest(&cpv1.SuspendSpawnRequest{SpawnId: spawnID})); err != nil {
		t.Fatalf("S4: SuspendSpawn: %v", err)
	}
	waitStatus(ctx, t, cl, spawnID, cpv1.SpawnStatus_SPAWN_STATUS_SUSPENDED, 30*time.Second)
	t.Log("S4: spawn is SUSPENDED")

	// Deny the journal key on the skills bucket (isolated: journal is NOT affected).
	t.Log("S4: denying journal key on spawnery-skills bucket (isolated — journal unaffected)")
	adminEndpoint, adminToken := loadGarageAdminCreds(t)
	adminCtx, adminCancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer adminCancel()
	admin, err := journal.NewGarageAdmin(adminEndpoint, adminToken, nil)
	if err != nil {
		t.Fatalf("S4: garage admin: %v", err)
	}
	if err := admin.DenyKeyOnBucket(adminCtx, stk.bucketID, stk.s3cfg.AccessKeyID); err != nil {
		t.Fatalf("S4: DenyKeyOnBucket: %v", err)
	}
	t.Log("S4: skills bucket access denied (key still valid for journal bucket)")

	// Always restore key permissions to leave the bucket usable for subsequent tests/reruns.
	t.Cleanup(func() {
		restoreCtx, c := context.WithTimeout(context.Background(), 15*time.Second)
		defer c()
		restoreAdmin, _ := journal.NewGarageAdmin(adminEndpoint, adminToken, nil)
		if restoreAdmin != nil {
			if err := restoreAdmin.AllowKeyOnBucket(restoreCtx, stk.bucketID, stk.s3cfg.AccessKeyID); err != nil {
				t.Logf("S4: WARNING — failed to restore skills bucket key permissions: %v", err)
			} else {
				t.Log("S4: skills bucket key permissions restored")
			}
		}
	})

	// Attempt ResumeSpawn.
	t.Log("S4: ResumeSpawn (skills bucket access denied)")
	if _, err := cl.ResumeSpawn(ctx, connect.NewRequest(&cpv1.ResumeSpawnRequest{SpawnId: spawnID})); err != nil {
		// ResumeSpawn may fail synchronously if the presign itself fails (FailedPrecondition/Unavailable).
		t.Logf("S4: ResumeSpawn RPC failed synchronously: %v", err)
		return fmt.Sprintf("RESUME_RPC_ERROR (skills bucket denied): %v → by-ref materialize is REQUIRED at resume; gating to first-create-only is NOT safe (DeltaCapture=%v)", err, deltaCapture)
	}

	// Poll for terminal status (ACTIVE or ERROR) with a 2-minute budget.
	deadline := time.Now().Add(2 * time.Minute)
	for time.Now().Before(deadline) {
		ls, err := cl.ListSpawns(ctx, connect.NewRequest(&cpv1.ListSpawnsRequest{}))
		if err != nil {
			t.Logf("S4: ListSpawns error: %v (continuing)", err)
			time.Sleep(500 * time.Millisecond)
			continue
		}
		for _, sp := range ls.Msg.Spawns {
			if sp.SpawnId != spawnID {
				continue
			}
			switch sp.Status {
			case cpv1.SpawnStatus_SPAWN_STATUS_ACTIVE:
				// Spawn came up — check if SKILL.md is still present.
				gen := findSpawnGeneration(ctx, t, cl, spawnID)
				ac := findAgentContainer(ctx, t, spawnID, gen)
				skillMDPath := fmt.Sprintf("/root/.claude/skills/%s/SKILL.md", skillName)
				content := dockerOutputNoFail(ctx, t, ac, skillMDPath)
				if content != "" {
					return fmt.Sprintf("ACTIVE+SKILL_PRESENT → skill survived resume with skills bucket denied (DeltaCapture=%v): by-ref materialize is NOT needed at resume; gating to first-create-only is SAFE", deltaCapture)
				}
				return fmt.Sprintf("ACTIVE+SKILL_ABSENT → spawn came up but SKILL.md is missing (DeltaCapture=%v): skill was not re-fetched; CHECK apply-artifacts failure logs", deltaCapture)
			case cpv1.SpawnStatus_SPAWN_STATUS_ERROR:
				return fmt.Sprintf("ERROR → resume failed with skills bucket denied (DeltaCapture=%v): by-ref materialize IS required at resume; §5 resume-Garage-dependency is confirmed; first-create-only gating NOT safe without further changes", deltaCapture)
			case cpv1.SpawnStatus_SPAWN_STATUS_DELETED:
				return fmt.Sprintf("DELETED → spawn deleted during resume (DeltaCapture=%v): unexpected terminal state", deltaCapture)
			}
		}
		time.Sleep(500 * time.Millisecond)
	}
	return fmt.Sprintf("TIMEOUT → spawn did not reach terminal status within 2m with skills denied (DeltaCapture=%v)", deltaCapture)
}

// runS4DeltaCapture runs the S4 probe for the DeltaCapture=true case by creating a
// separate spawn stack with DeltaCapture enabled. This tests whether the delta image
// (which bakes the agent rootfs including ~/.claude/skills/ on suspend) allows resume
// without re-fetching the skill from Garage.
func runS4DeltaCapture(t *testing.T, parentCtx context.Context, stk skillIngestStack,
	appID, skillName, profileID string) string {
	t.Helper()

	// We need a fresh spawn on a DeltaCapture-enabled stack.
	// Create a child sub-stack with DeltaCapture=true.
	// NOTE: this spawns a second full stack; the parent stack's context is reused.
	deltaStk := setupSkillIngestStack(t, func(cfg *spawnlet.ManagerConfig) {
		cfg.DeltaCapture = true
	})
	cl := deltaStk.client
	ctx := deltaStk.ctx

	// Ingest the skill again (idempotent — returns the same catalog_id due to (owner,sha256) constraint).
	const skillURL = "https://github.com/199-biotechnologies/claude-deep-research-skill"
	ingestResp, err := cl.IngestSkillFromURL(ctx, connect.NewRequest(&cpv1.IngestSkillFromURLRequest{
		Url: skillURL,
	}))
	if err != nil {
		t.Fatalf("S4-delta: IngestSkillFromURL: %v", err)
	}
	catalogID := ingestResp.Msg.CatalogId

	// Create a fresh profile for the delta-stack spawn.
	profResp, err := cl.CreateProfile(ctx, connect.NewRequest(&cpv1.CreateProfileRequest{
		Name: "skill-s4-delta",
	}))
	if err != nil {
		t.Fatalf("S4-delta: CreateProfile: %v", err)
	}
	deltaProfileID := profResp.Msg.ProfileId

	_, err = cl.AddProfileEntry(ctx, connect.NewRequest(&cpv1.AddProfileEntryRequest{
		ProfileId:       deltaProfileID,
		ExpectedVersion: 1,
		Entry: &cpv1.ProfileEntry{
			Kind:      cpv1.ProfileEntryKind_PROFILE_ENTRY_KIND_SKILL,
			Name:      skillName,
			Source:    cpv1.ProfileEntrySource_PROFILE_ENTRY_SOURCE_CATALOG_REF,
			CatalogId: catalogID,
		},
	}))
	if err != nil {
		t.Fatalf("S4-delta: AddProfileEntry: %v", err)
	}

	cs, err := cl.CreateSpawn(ctx, connect.NewRequest(&cpv1.CreateSpawnRequest{
		AppId:      deltaStk.appID,
		Model:      "openai/gpt-4o-mini",
		Image:      "spawnery/agent:dev",
		RunnableId: "claude-tui",
		ProfileId:  deltaProfileID,
	}))
	if err != nil {
		t.Fatalf("S4-delta: CreateSpawn: %v", err)
	}
	deltaSpawnID := cs.Msg.SpawnId
	t.Logf("S4-delta: spawn created %s (DeltaCapture=true)", deltaSpawnID)
	t.Cleanup(func() {
		stopCtx, c := context.WithTimeout(context.Background(), 10*time.Second)
		defer c()
		_, _ = cl.StopSpawn(stopCtx, connect.NewRequest(&cpv1.StopSpawnRequest{SpawnId: deltaSpawnID}))
		time.Sleep(2 * time.Second)
	})

	waitActiveWithTimeout(ctx, t, cl, deltaSpawnID, 120*time.Second)
	t.Logf("S4-delta: spawn %s is ACTIVE", deltaSpawnID)

	gen := findSpawnGeneration(ctx, t, cl, deltaSpawnID)
	ac := findAgentContainer(ctx, t, deltaSpawnID, gen)
	skillMDPath := fmt.Sprintf("/root/.claude/skills/%s/SKILL.md", skillName)
	content := dockerOutputNoFail(ctx, t, ac, skillMDPath)
	if content == "" {
		installed, _ := exec.CommandContext(ctx, "docker", "exec", ac,
			"find", "/root/.claude/skills", "-name", "SKILL.md", "-o", "-type", "d").CombinedOutput()
		t.Fatalf("S4-delta: SKILL.md not found at %s in agent container %s before suspend — "+
			"artifact materialization failed on the delta path (delta capture cannot bake what isn't there)\n"+
			"Installed skills tree:\n%s",
			skillMDPath, ac, string(installed))
	}
	t.Logf("S4-delta: SKILL.md present before suspend (%d bytes)", len(content))

	// Now run the S4 probe using the delta stack's bucket ID (same bucket — same creds).
	return runS4Probe(t, ctx, deltaStk, cl, deltaStk.appID, deltaSpawnID, skillName, deltaProfileID, true /* DeltaCapture=true */)
}
