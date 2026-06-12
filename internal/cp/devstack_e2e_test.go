//go:build e2e

package cp_test

import (
	"bufio"
	"context"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
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
	"spawnery/internal/cp/store"
	"spawnery/internal/cp/telemetry"
	"spawnery/internal/node"
	"spawnery/internal/runtime"
	"spawnery/internal/spawnlet"

	"net/http/httptest"
)

// justfileRecipeEnv extracts the literal KEY=VALUE env assignments from a Justfile recipe's
// body. Values containing just-interpolations ({{...}}) or shell expansions ($...) are
// skipped — the test only needs the semantically load-bearing literals (NODE_CLASS,
// NODE_OWNER, EGRESS_ENFORCE, CP_DEV_TOKENS).
func justfileRecipeEnv(t *testing.T, recipe string) map[string]string {
	t.Helper()
	f, err := os.Open("../../Justfile")
	if err != nil {
		t.Fatalf("open Justfile: %v", err)
	}
	defer f.Close()

	header := regexp.MustCompile(`^` + regexp.QuoteMeta(recipe) + `(\s|:)`)
	assign := regexp.MustCompile(`(^|\s)([A-Z][A-Z0-9_]*)=(\S+)`)
	env := map[string]string{}
	in := false
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		if !in {
			if header.MatchString(line) {
				in = true
			}
			continue
		}
		// recipe body = indented lines; the next top-level line ends it.
		if line != "" && !strings.HasPrefix(line, " ") && !strings.HasPrefix(line, "\t") {
			break
		}
		for _, m := range assign.FindAllStringSubmatch(line, -1) {
			key, val := m[2], strings.TrimSuffix(m[3], `\`)
			val = strings.Trim(val, `"'`)
			if strings.Contains(val, "{{") || strings.Contains(val, "$") {
				continue
			}
			env[key] = val
		}
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("scan Justfile: %v", err)
	}
	if len(env) == 0 {
		t.Fatalf("no literal env assignments found for Justfile recipe %q — did the recipe get renamed?", recipe)
	}
	return env
}

// TestDevStackSpawnE2E guards the seam that hermetic tests structurally cannot: the env shape
// of the `just cp` + `just node` dev recipes vs the CP's placement/tenancy/auth rules
// (sp-5v03). It boots the CP and a node configured FROM THE JUSTFILE'S OWN VALUES
// (NODE_CLASS/NODE_OWNER/EGRESS_ENFORCE, CP_DEV_TOKENS) and asserts CreateSpawn reaches
// ACTIVE through the CP API. When a recipe and a placement rule drift apart — e.g. the
// sp-uo4m regression, where the tenancy gate made an owner-less self-hosted node ineligible
// for every dev spawn — this test fails while every hermetic suite stays green.
// Requires Docker + the stub/sidecar images; FAILS (no skip) if the env is broken.
func TestDevStackSpawnE2E(t *testing.T) {
	nodeEnv := justfileRecipeEnv(t, "node")
	cpEnv := justfileRecipeEnv(t, "cp")

	// The recipe values this test exists to keep honest. If a key disappears from the
	// Justfile, fail loudly rather than silently testing a different config.
	for _, k := range []string{"NODE_CLASS", "NODE_OWNER", "EGRESS_ENFORCE"} {
		if _, ok := nodeEnv[k]; !ok {
			t.Fatalf("Justfile `node` recipe no longer sets %s — update this test to match the new dev-stack shape", k)
		}
	}
	devTokens := map[string]string{}
	for _, pair := range strings.Split(cpEnv["CP_DEV_TOKENS"], ",") {
		tok, owner, ok := strings.Cut(pair, "=")
		if !ok {
			t.Fatalf("Justfile `cp` recipe CP_DEV_TOKENS %q is not token=owner", cpEnv["CP_DEV_TOKENS"])
		}
		devTokens[tok] = owner
	}
	var token, owner string
	for tok, ow := range devTokens {
		token, owner = tok, ow
		break
	}

	rt, err := runtime.NewDocker()
	if err != nil {
		t.Fatalf("docker unavailable: %v", err)
	}
	if err := rt.Ping(context.Background()); err != nil {
		t.Fatalf("docker not pingable: %v", err)
	}

	// --- CP, seeded + authed exactly as `just cp` shapes it ---
	reg := registry.New()
	rtr := router.New()
	sched := scheduler.New(reg, rtr, 60*time.Second)
	telPath := filepath.Join(t.TempDir(), "events.jsonl")
	tel, err := telemetry.NewJSONLSink(telPath)
	if err != nil {
		t.Fatal(err)
	}
	defer tel.Close()
	authn := auth.NewVerifier(auth.VerifierConfig{DevTokens: devTokens, DevMode: true})
	appRef, err := filepath.Abs("../../examples/secret-app")
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(context.Background(), store.Config{Driver: "sqlite", DSN: "file:devstacke2e?mode=memory&cache=shared&_pragma=foreign_keys(1)"})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := cp.Seed(context.Background(), st, devTokens,
		[]cp.AppSeed{{ID: "secret-app", Ref: appRef, Version: "1.0.0", Mounts: []string{"main"}}}); err != nil {
		t.Fatal(err)
	}
	srv := cp.NewServer(reg, rtr, sched, st, tel)

	mux := http.NewServeMux()
	mux.Handle(nodev1connect.NewNodeServiceHandler(srv))
	mux.Handle(cpv1connect.NewSpawnServiceHandler(srv, connect.WithInterceptors(authn.Interceptor())))
	cpSrv := httptest.NewUnstartedServer(h2c.NewHandler(mux, &http2.Server{}))
	cpSrv.Start()
	defer cpSrv.Close()

	// --- node, configured from the `just node` recipe's literals ---
	mgr := spawnlet.NewManager(rt, spawnlet.ManagerConfig{
		AgentImage: "spawnery/stubagent:dev", SidecarImage: "spawnery/sidecar:dev",
		OpenRouterKey: "unused", DataRoot: t.TempDir(),
		NodeClass:     nodeEnv["NODE_CLASS"],
		EgressEnforce: nodeEnv["EGRESS_ENFORCE"] == "true",
	})
	nodeCtx, stopNode := context.WithCancel(context.Background())
	defer stopNode()
	go node.Run(nodeCtx, mgr, h2cClient(), node.Config{
		NodeID: "dev-node", CPURL: cpSrv.URL, MaxSpawns: 2, AgentImage: "spawnery/stubagent:dev",
		NodeClass: nodeEnv["NODE_CLASS"],
		NodeOwner: nodeEnv["NODE_OWNER"],
	})
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, ok := reg.Get("dev-node"); ok {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("node never registered with CP")
		}
		time.Sleep(50 * time.Millisecond)
	}

	// --- the assertion: a dev-token spawn places onto the dev-shaped node ---
	cl := cpv1connect.NewSpawnServiceClient(h2cClient(), cpSrv.URL, connect.WithGRPC(),
		connect.WithInterceptors(bearer(token)))
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	cs, err := cl.CreateSpawn(ctx, connect.NewRequest(&cpv1.CreateSpawnRequest{AppId: "secret-app", Model: "x"}))
	if err != nil {
		t.Fatalf("CreateSpawn as %s (owner %s) against the Justfile-shaped node (class=%s owner=%s): %v\n"+
			"resource_exhausted here usually means a placement/tenancy rule and the dev recipes drifted apart (sp-uo4m class)",
			token, owner, nodeEnv["NODE_CLASS"], nodeEnv["NODE_OWNER"], err)
	}
	id := cs.Msg.SpawnId
	defer cl.StopSpawn(context.Background(), connect.NewRequest(&cpv1.StopSpawnRequest{SpawnId: id}))

	waitActive(ctx, t, cl, id)
}
