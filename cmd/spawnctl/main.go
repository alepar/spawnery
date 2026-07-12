package main

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"connectrpc.com/connect"
	"github.com/knadh/koanf/providers/confmap"
	"github.com/urfave/cli/v3"
	"golang.org/x/net/http2"

	configfiles "spawnery/config"
	cpv1 "spawnery/gen/cp/v1"
	"spawnery/gen/cp/v1/cpv1connect"
	spawnv1 "spawnery/gen/spawn/v1"
	"spawnery/gen/spawn/v1/spawnv1connect"
	"spawnery/internal/acp"
	"spawnery/internal/client"
	"spawnery/internal/config"
	"spawnery/internal/intent"
	"spawnery/internal/manifest"
)

func main() {
	cmd := &cli.Command{
		Name:  "spawnctl",
		Usage: "drive and attach to spawnery spawns",
		// Root flags + Action preserve the original CLI: create a spawn and drive it (standalone via
		// -addr, or through the CP via -cp), or register an app manifest with -register.
		// The --mount value embeds a comma (name=backend_uri,create); disable the slice-flag comma
		// separator so the ",create" option is not mis-split into a second mount binding.
		DisableSliceFlagSeparator: true,
		Flags: []cli.Flag{
			&cli.StringFlag{Name: "env", Usage: "environment dev|staging|prod (overrides SPAWNERY_ENV)", Hidden: true},
			&cli.StringFlag{Name: "addr", Value: "http://127.0.0.1:9090", Usage: "spawnlet address (standalone)"},
			&cli.StringFlag{Name: "app", Value: "examples/secret-app", Usage: "app definition dir"},
			&cli.StringFlag{Name: "model", Value: "anthropic/claude-3.5-sonnet", Usage: "OpenRouter model"},
			&cli.StringFlag{Name: "cp", Usage: "control-plane address (http://127.0.0.1:8080); overrides -addr"},
			&cli.StringFlag{Name: "app-id", Value: "spawnery/secret-app", Usage: "app id (CP mode)"},
			&cli.StringFlag{Name: "token", Value: "dev-token", Usage: "dev auth token (CP mode)"},
			&cli.BoolFlag{Name: "register", Usage: "register the -app manifest with the CP and exit"},
			&cli.StringFlag{Name: "version", Value: "1.0.0", Usage: "app version to register (with -register)"},
			&cli.StringFlag{Name: "ref", Usage: "immutable app ref creator/app@sha (with -register)"},
			&cli.StringFlag{Name: "profile", Usage: "customization profile id to apply at create (CP mode)"},
			&cli.BoolFlag{Name: "detach", Usage: "create the spawn, wait for ACTIVE, print its id, and exit WITHOUT attaching (scriptable; the spawn keeps running instead of being stopped on detach)"},
			&cli.StringSliceFlag{Name: "mount", Usage: "mount binding name=backend_uri[,create] (repeatable; e.g. repo=github:owner/repo,create) — CP mode only"},
			&cli.StringFlag{Name: "root-ca", Usage: "path to the pinned Root CA PEM for node verification"},
			&cli.StringFlag{Name: "trust-domain", Usage: "expected SPIFFE trust domain"},
			&cli.StringFlag{Name: "crl-state", Usage: "persistent certificate revocation checkpoint"},
			&cli.StringSliceFlag{Name: "crl-issuer", Usage: "trusted issuing-intermediate PEM (repeatable)"},
			&cli.StringSliceFlag{Name: "crl", Usage: "current signed CRL PEM (repeatable)"},
		},
		Action:   rootAction,
		Commands: []*cli.Command{attachCmd(), execCmd(), shellCmd(), listCmd(), statusCmd(), setModelCmd(), keyCmd(), moveCmd(), resumeCmd(), suspendCmd(), forkCmd(), loginCmd(), logoutCmd(), profileCmd(), catalogCmd(), ghCmd()},
	}
	if err := cmd.Run(context.Background(), os.Args); err != nil {
		log.Fatal(err)
	}
}

// rootAction is the default (no-subcommand) behavior: register, CP-create, or standalone-create.
// addr and cp come from the loaded SpawnctlCfg (YAML default → explicit flag override), giving
// spawnctl.<env>.yaml the ability to change these defaults without a rebuild.
//
// Config loading is intentionally scoped to rootAction: only this action uses addr/cp from the
// config layer; the 14 subcommands (exec/shell/attach/list/…) are flag-driven and must not
// require SPAWNERY_ENV to be set.
func rootAction(ctx context.Context, c *cli.Command) error {
	// Only explicitly-set flags contribute to the flag-override layer; unset flags fall through to
	// the YAML default so spawnctl.<env>.yaml can change defaults without a rebuild.
	overrides := map[string]any{}
	if c.IsSet("addr") {
		overrides["addr"] = c.String("addr")
	}
	if c.IsSet("cp") {
		overrides["cp"] = c.String("cp")
	}
	cfg, err := config.Load[SpawnctlCfg]("spawnctl", config.Options{
		Args:         os.Args[1:],
		Embedded:     configfiles.FS,
		SecretsFS:    configfiles.FS,
		EnvAliases:   spawnctlEnvAliases,
		FlagProvider: confmap.Provider(overrides, "."),
		// spawnctl is a client CLI, not a server: default to dev when no env is set rather than
		// fail closed, so `spawnctl create`/`-register` work without SPAWNERY_ENV.
		DefaultEnv: "dev",
	})
	if err != nil {
		return fmt.Errorf("config: %w", err)
	}
	configDir, _ := defaultConfigDir()
	httpCl := connectClient()
	if c.Bool("register") {
		if cfg.CP == "" {
			return cli.Exit("-register requires -cp", 2)
		}
		src := buildTokenSource(configDir, c.String("token"), httpCl)
		runRegister(ctx, cfg.CP, c.String("app"), c.String("version"), c.String("ref"), src)
		return nil
	}
	if cfg.CP != "" {
		mounts, err := parseMountFlags(c.StringSlice("mount"))
		if err != nil {
			return cli.Exit(err.Error(), 2)
		}
		src := buildTokenSource(configDir, c.String("token"), httpCl)
		opts, err := loadMoveOptions(configDir, c.String("token"), strings.TrimSpace(c.String("root-ca")), strings.TrimSpace(c.String("trust-domain")), strings.TrimSpace(c.String("crl-state")), c.StringSlice("crl-issuer"), c.StringSlice("crl"), time.Now)
		if err != nil {
			return cli.Exit(err.Error(), 1)
		}
		if opts.CloseCertificateRevocations != nil {
			defer func() { _ = opts.CloseCertificateRevocations() }()
		}
		trust, err := targetTrustFromMoveOptions(opts)
		if err != nil {
			return cli.Exit(err.Error(), 1)
		}
		runCP(ctx, cfg.CP, c.String("app-id"), c.String("model"), c.String("profile"), mounts, src, trust, c.Bool("detach"))
		return nil
	}
	if len(c.StringSlice("mount")) > 0 {
		return cli.Exit("--mount requires -cp (standalone/register mode has no mount bindings)", 2)
	}
	runStandalone(ctx, cfg.Addr, c.String("app"), c.String("model"))
	return nil
}

// manifestToProto parses an app's spawneryapp.yml and maps it to the cp.v1
// AppManifest proto used by RegisterAppVersion.
func manifestToProto(appDir string) (*cpv1.AppManifest, error) {
	m, err := manifest.Parse(appDir)
	if err != nil {
		return nil, err
	}
	mounts := make([]*cpv1.ManifestMount, len(m.Storage.Mounts))
	for i, mt := range m.Storage.Mounts {
		mounts[i] = &cpv1.ManifestMount{Name: mt.Name, Path: mt.Path, Seed: mt.Seed, Durability: mt.Durability, Github: mt.Github}
	}
	return &cpv1.AppManifest{
		ApiVersion: m.APIVersion, Id: m.ID, Title: m.Title, Description: m.Description,
		Tags: m.Tags, Visibility: m.Visibility,
		Agents: &cpv1.ManifestAgents{Support: m.Agents.Support, Exclude: m.Agents.Exclude, RequiresAcp: m.Agents.RequiresAcp},
		Tools:  m.Tools, Persona: m.Persona, Skills: m.Skills,
		Model: &cpv1.ManifestModel{
			ToolUse: m.Model.Requires.ToolUse, MinContextTokens: m.Model.Requires.MinContextTokens,
			Vision: m.Model.Requires.Vision, RecommendedDefault: m.Model.RecommendedDefault,
		},
		RuntimeBaseVersion: m.Runtime.BaseVersion,
		Mounts:             mounts,
	}, nil
}

// runRegister is the reference CI client: it maps the local manifest to the
// AppManifest proto and calls RegisterAppVersion on the control plane.
func runRegister(ctx context.Context, cpAddr, appDir, version, ref string, src *cpTokenSource) {
	pm, err := manifestToProto(appDir)
	if err != nil {
		log.Fatalf("manifest: %v", err)
	}
	client := cpv1connect.NewSpawnServiceClient(connectClient(), cpAddr,
		connect.WithGRPC(), connect.WithInterceptors(tokenSourceInterceptor(src)))
	resp, err := client.RegisterAppVersion(ctx, connect.NewRequest(&cpv1.RegisterAppVersionRequest{Manifest: pm, Version: version, Ref: ref}))
	if err != nil {
		log.Fatalf("register: %v", err)
	}
	fmt.Printf("registered %s@%s tier=%s\n", resp.Msg.AppId, resp.Msg.Version, resp.Msg.Tier)
}

// runStandalone drives a spawnlet directly via the spawn.v1 service (CP-less).
func runStandalone(ctx context.Context, addr, appPath, model string) {
	client := spawnv1connect.NewSpawnServiceClient(connectClient(), addr, connect.WithGRPC())

	cs, err := client.CreateSpawn(ctx, connect.NewRequest(&spawnv1.CreateSpawnRequest{
		AppPath: appPath,
		Model:   model,
	}))
	if err != nil {
		log.Fatalf("createSpawn: %v", err)
	}
	id := cs.Msg.SpawnId
	fmt.Println("spawn:", id)

	stream := client.Session(ctx)

	// Adapt the Connect bidi stream to io.Reader/io.Writer for acp.Client.
	// pr/pw is the agent->client pipe: a goroutine receives frames from the
	// stream and writes their Data into pw; acp.Client reads from pr.
	pr, pw := io.Pipe()
	go func() {
		for {
			f, err := stream.Receive()
			if err != nil {
				pw.CloseWithError(err)
				return
			}
			if _, werr := pw.Write(f.Data); werr != nil {
				return
			}
		}
	}()

	// sendW is the client->agent writer: every Write call encodes the bytes
	// as a Frame and sends it on the stream.
	sendW := writerFunc(func(b []byte) (int, error) {
		if err := stream.Send(&spawnv1.Frame{SpawnId: id, Data: b}); err != nil {
			return 0, err
		}
		return len(b), nil
	})

	driveACP(pr, sendW)

	_ = stream.CloseRequest()
	_, _ = client.StopSpawn(ctx, connect.NewRequest(&spawnv1.StopSpawnRequest{SpawnId: id}))
}

// runCP drives the agent through the control plane via the cp.v1 service.
func runCP(ctx context.Context, addr, appID, model, profileID string, mounts []*cpv1.MountBinding, src *cpTokenSource, trust client.TargetTrust, detach bool) {
	cli := client.New(addr, src, nil, client.WithNodeAuthorization(src, trust), client.WithWarnHandler(func(err error) {
		log.Printf("%v", err)
	}))

	id, err := cli.CreateSpawn(ctx, &cpv1.CreateSpawnRequest{
		AppId:     appID,
		Model:     model,
		ProfileId: profileID,
		Mounts:    mounts,
	})
	if err != nil {
		log.Fatalf("createSpawn: %v", err)
	}
	fmt.Println("spawn:", id)

	// A4 two-phase sign-after-resolve [AC1][AM12]: run authorization concurrently with WaitActive.
	// A signing or target-verification failure is fatal immediately rather than degrading into a
	// warning while WaitActive sits behind the CP's pending intent.
	pollCtx, cancelPoll := context.WithCancel(ctx)
	defer cancelPoll()
	authorize := func(authCtx context.Context) error {
		// AppRef is intentionally left empty: in CP create mode the user supplies an app *id*
		// (--app-id), not the immutable app_ref the CP resolves it to (id != ref for catalog/seed
		// apps). The client cannot validate a ref it never specified, so the AM1 app_ref gate is
		// skipped; the model correspondence check still runs, and the signed intent carries the
		// CP-resolved app_ref verbatim.
		return cli.SignProvision(authCtx, id, client.IntentParams{Op: intent.OpCreateSpawn, Model: model, Mounts: mounts, AttachedSecretIDs: []string{}})
	}

	// CreateSpawn is async: the CP binds the spawn to its node only once the node reports ACTIVE.
	// Wait for that before attaching, else the session races provisioning and gets "unknown spawn".
	var lastLine string
	onPoll := func(sp *cpv1.SpawnSummary) {
		if line, changed := nextProgressLine(lastLine, sp); changed {
			fmt.Fprintln(os.Stdout, line)
			lastLine = line
		}
	}
	spawnGen, err := awaitCreateAuthorization(pollCtx, authorize, func(waitCtx context.Context) (uint64, error) {
		return cli.WaitActive(waitCtx, id, onPoll)
	})
	cancelPoll()
	if err != nil {
		var te *client.TerminalError
		if errors.As(err, &te) {
			log.Fatal(provisionFailure(te.Summary))
		}
		log.Fatalf("authorize create: %v", err)
	}

	// -detach: the spawn is ACTIVE and (if the CP runs the intent flow) its create intent is signed.
	// Return WITHOUT opening an interactive session — so we skip the StopSpawn that a normal detach
	// (stdin EOF) issues, and the spawn keeps running. This is the scriptable create path (CI, the
	// acceptance suite): `spawnctl -cp ... -detach` prints `spawn: <id>` and exits 0, spawn persists.
	if detach {
		fmt.Println("detached; spawn active:", id)
		return
	}

	env, err := cli.BuildSessionOpenIntent(ctx, id, spawnGen, "0")
	if err != nil {
		log.Fatalf("bind: session-open authorization: %v", err)
	}
	bindFrame := &cpv1.Frame{SpawnId: id, SessionAuth: env}
	stream := cli.Session(ctx)
	if err := stream.Send(bindFrame); err != nil { // bind frame (carries the spawn id + session-open auth)
		log.Fatalf("bind: %v", err)
	}

	pr, sendW := cli.SessionStream(stream, id)

	// The CP relays the frame protocol (not raw ACP): the node's pump does the ACP handshake and
	// exposes {"kind":"prompt"} in / user|agent|turn frames out. Drive it like the web client.
	driveFrames(pr, sendW)

	_ = stream.CloseRequest()
	_ = cli.Stop(ctx, id)
}

func awaitCreateAuthorization(ctx context.Context, authorize func(context.Context) error, waitActive func(context.Context) (uint64, error)) (uint64, error) {
	operationCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	authCh := make(chan error, 1)
	type waitResult struct {
		generation uint64
		err        error
	}
	waitCh := make(chan waitResult, 1)
	go func() { authCh <- authorize(operationCtx) }()
	go func() {
		generation, err := waitActive(operationCtx)
		waitCh <- waitResult{generation: generation, err: err}
	}()
	authDone, waitDone := false, false
	var generation uint64
	for !authDone || !waitDone {
		select {
		case err := <-authCh:
			if err != nil {
				return 0, err
			}
			authDone = true
			authCh = nil
		case result := <-waitCh:
			if result.err != nil {
				return 0, result.err
			}
			generation = result.generation
			waitDone = true
			waitCh = nil
		case <-ctx.Done():
			return 0, ctx.Err()
		}
	}
	return generation, nil
}

// driveFrames is the CP-lane interactive loop over the frame protocol: it sends each stdin line as a
// {"kind":"prompt"} frame and prints agent frames until the turn goes idle, then reads the next line.
func driveFrames(pr io.Reader, sendW io.Writer) {
	fmt.Println("ready. type prompts:")
	turnIdle := make(chan struct{}, 1)
	go func() {
		sc := bufio.NewScanner(pr)
		sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
		for sc.Scan() {
			var f struct {
				Kind  string `json:"kind"`
				Text  string `json:"text"`
				State string `json:"state"`
			}
			if json.Unmarshal(sc.Bytes(), &f) != nil {
				continue
			}
			switch f.Kind {
			case "agent":
				fmt.Print(f.Text)
			case "turn":
				if f.State == "idle" {
					fmt.Println()
					select {
					case turnIdle <- struct{}{}:
					default:
					}
				}
			}
		}
	}()
	in := bufio.NewScanner(os.Stdin)
	for in.Scan() {
		line := in.Text()
		if line == "" {
			continue
		}
		b, _ := json.Marshal(map[string]string{"kind": "prompt", "text": line})
		if _, err := sendW.Write(append(b, '\n')); err != nil {
			log.Fatal(err)
		}
		<-turnIdle
	}
}

// driveACP runs the ACP client over the given agent->client reader and
// client->agent writer: initialize, new session, then a stdin prompt loop.
func driveACP(pr io.Reader, sendW io.Writer) {
	c := acp.NewClient(pr, sendW)
	if err := c.Initialize(); err != nil {
		log.Fatal(err)
	}
	if err := c.NewSession("/app"); err != nil {
		log.Fatal(err)
	}

	fmt.Println("ready. type prompts:")
	in := bufio.NewScanner(os.Stdin)
	for in.Scan() {
		line := in.Text()
		if line == "" {
			continue
		}
		if err := c.Prompt(line, func(chunk string) { fmt.Print(chunk) }); err != nil {
			log.Fatal(err)
		}
		fmt.Println()
	}
}

// connectClient returns an *http.Client for Connect RPCs (and the token-refresh POST)
// whose transport is chosen per-request by URL scheme: https -> real TLS + HTTP/2
// handshake, http -> cleartext HTTP/2 (h2c). This lets -cp target an https:// CP while
// http:// keeps working for local/dev. Both sub-transports speak HTTP/2, as required by
// connect.WithGRPC().
func connectClient() *http.Client {
	return &http.Client{Transport: newSchemeTransport(nil)}
}

// newSchemeTransport builds the scheme-dispatching RoundTripper. tlsConf is nil in
// production (system roots / standard verification); tests inject a cert pool.
func newSchemeTransport(tlsConf *tls.Config) *schemeTransport {
	return &schemeTransport{
		h2c: &http2.Transport{
			AllowHTTP: true,
			DialTLSContext: func(ctx context.Context, network, addr string, _ *tls.Config) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, network, addr)
			},
		},
		tls: &http2.Transport{TLSClientConfig: tlsConf},
	}
}

// schemeTransport routes each request to the h2c or TLS HTTP/2 transport by URL scheme.
type schemeTransport struct {
	h2c *http2.Transport
	tls *http2.Transport
}

func (t *schemeTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.URL.Scheme == "https" {
		return t.tls.RoundTrip(req)
	}
	return t.h2c.RoundTrip(req)
}

// tokenSourceInterceptor builds a Connect interceptor backed by a cpTokenSource.
// Unary: sets bearer token; on CodeUnauthenticated, forces refresh and retries once.
// Streaming: proactively refreshes before opening the connection (mid-stream 401 needs reconnect — out of scope).
func tokenSourceInterceptor(src *cpTokenSource) connect.Interceptor {
	return &tsInterceptor{src: src}
}

type tsInterceptor struct{ src *cpTokenSource }

func (t *tsInterceptor) WrapUnary(next connect.UnaryFunc) connect.UnaryFunc {
	return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
		tok, err := t.src.Token(ctx)
		if err != nil {
			return nil, connect.NewError(connect.CodeUnauthenticated, err)
		}
		req.Header().Set("Authorization", "Bearer "+tok)
		resp, err := next(ctx, req)
		if err != nil {
			var connErr *connect.Error
			if errors.As(err, &connErr) && connErr.Code() == connect.CodeUnauthenticated {
				// Force refresh and retry once.
				if refreshErr := t.src.OnUnauthenticated(ctx); refreshErr == nil {
					if newTok, tokErr := t.src.Token(ctx); tokErr == nil {
						req.Header().Set("Authorization", "Bearer "+newTok)
						return next(ctx, req)
					}
				}
			}
		}
		return resp, err
	}
}

func (t *tsInterceptor) WrapStreamingClient(next connect.StreamingClientFunc) connect.StreamingClientFunc {
	return func(ctx context.Context, spec connect.Spec) connect.StreamingClientConn {
		conn := next(ctx, spec)
		tok, err := t.src.Token(ctx)
		if err == nil {
			conn.RequestHeader().Set("Authorization", "Bearer "+tok)
		}
		return conn
	}
}

func (t *tsInterceptor) WrapStreamingHandler(next connect.StreamingHandlerFunc) connect.StreamingHandlerFunc {
	return next // server-side: no-op
}

// writerFunc adapts a func([]byte)(int,error) to the io.Writer interface.
type writerFunc func([]byte) (int, error)

func (f writerFunc) Write(b []byte) (int, error) { return f(b) }
