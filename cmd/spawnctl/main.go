package main

import (
	"bufio"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"time"

	"connectrpc.com/connect"
	"github.com/urfave/cli/v3"
	"golang.org/x/net/http2"

	authv1 "spawnery/gen/auth/v1"
	cpv1 "spawnery/gen/cp/v1"
	"spawnery/gen/cp/v1/cpv1connect"
	spawnv1 "spawnery/gen/spawn/v1"
	"spawnery/gen/spawn/v1/spawnv1connect"
	"spawnery/internal/acp"
	"spawnery/internal/intent"
	"spawnery/internal/manifest"
)

// Ensure cpv1connect.SpawnServiceClient satisfies the narrow intentClient interface.
var _ intentClient = (cpv1connect.SpawnServiceClient)(nil)

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
			&cli.StringSliceFlag{Name: "mount", Usage: "mount binding name=backend_uri[,create] (repeatable; e.g. repo=github:owner/repo,create) — CP mode only"},
		},
		Action:   rootAction,
		Commands: []*cli.Command{attachCmd(), execCmd(), shellCmd(), listCmd(), setModelCmd(), keyCmd(), moveCmd(), resumeCmd(), forkCmd(), loginCmd(), logoutCmd(), profileCmd(), catalogCmd(), ghCmd()},
	}
	if err := cmd.Run(context.Background(), os.Args); err != nil {
		log.Fatal(err)
	}
}

// rootAction is the default (no-subcommand) behavior: register, CP-create, or standalone-create.
func rootAction(ctx context.Context, c *cli.Command) error {
	configDir, _ := defaultConfigDir()
	httpCl := h2cClient()
	if c.Bool("register") {
		if c.String("cp") == "" {
			return cli.Exit("-register requires -cp", 2)
		}
		src := buildTokenSource(configDir, c.String("token"), httpCl)
		runRegister(ctx, c.String("cp"), c.String("app"), c.String("version"), c.String("ref"), src)
		return nil
	}
	if c.String("cp") != "" {
		mounts, err := parseMountFlags(c.StringSlice("mount"))
		if err != nil {
			return cli.Exit(err.Error(), 2)
		}
		src := buildTokenSource(configDir, c.String("token"), httpCl)
		runCP(ctx, c.String("cp"), c.String("app-id"), c.String("model"), c.String("profile"), mounts, src)
		return nil
	}
	if len(c.StringSlice("mount")) > 0 {
		return cli.Exit("--mount requires -cp (standalone/register mode has no mount bindings)", 2)
	}
	runStandalone(ctx, c.String("addr"), c.String("app"), c.String("model"))
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
	client := cpv1connect.NewSpawnServiceClient(h2cClient(), cpAddr,
		connect.WithGRPC(), connect.WithInterceptors(tokenSourceInterceptor(src)))
	resp, err := client.RegisterAppVersion(ctx, connect.NewRequest(&cpv1.RegisterAppVersionRequest{Manifest: pm, Version: version, Ref: ref}))
	if err != nil {
		log.Fatalf("register: %v", err)
	}
	fmt.Printf("registered %s@%s tier=%s\n", resp.Msg.AppId, resp.Msg.Version, resp.Msg.Tier)
}

// runStandalone drives a spawnlet directly via the spawn.v1 service (CP-less).
func runStandalone(ctx context.Context, addr, appPath, model string) {
	client := spawnv1connect.NewSpawnServiceClient(h2cClient(), addr, connect.WithGRPC())

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
func runCP(ctx context.Context, addr, appID, model, profileID string, mounts []*cpv1.MountBinding, src *cpTokenSource) {
	client := cpv1connect.NewSpawnServiceClient(h2cClient(), addr,
		connect.WithGRPC(), connect.WithInterceptors(tokenSourceInterceptor(src)))

	cs, err := client.CreateSpawn(ctx, connect.NewRequest(&cpv1.CreateSpawnRequest{
		AppId:     appID,
		Model:     model,
		ProfileId: profileID,
		Mounts:    mounts,
	}))
	if err != nil {
		log.Fatalf("createSpawn: %v", err)
	}
	id := cs.Msg.SpawnId
	fmt.Println("spawn:", id)

	// A4 two-phase sign-after-resolve [AC1][AM12]: start the poll-and-sign loop concurrently with
	// waitActiveCP. The CP blocks the spawn in 'starting' until the client submits a signed intent;
	// pollAndSign polls until the CP registers the pending intent, then builds and submits it.
	// If the CP does not have the intent flow enabled (old CP or intentEnabled=false), pollAndSign
	// polls until its context is cancelled when waitActiveCP returns — the spawn becomes active
	// without it and the context.Canceled error is suppressed.
	pollCtx, cancelPoll := context.WithCancel(ctx)
	defer cancelPoll()
	go func() {
		// AppRef is intentionally left empty: in CP create mode the user supplies an app *id*
		// (--app-id), not the immutable app_ref the CP resolves it to (id != ref for catalog/seed
		// apps). The client cannot validate a ref it never specified, so the AM1 app_ref gate is
		// skipped; the model correspondence check still runs, and the signed intent carries the
		// CP-resolved app_ref verbatim.
		if err := pollAndSign(pollCtx, client, id, intentParams{Model: model}); err != nil && !errors.Is(err, context.Canceled) {
			log.Printf("pollAndSign: %v (spawn may still become active if CP intent flow is disabled)", err)
		}
	}()

	// CreateSpawn is async: the CP binds the spawn to its node only once the node reports ACTIVE.
	// Wait for that before attaching, else the session races provisioning and gets "unknown spawn".
	spawnGen := waitActiveCP(ctx, client, id)
	cancelPoll()

	// A4 session-open signing [AC1][AM12]: build a signed intent with the live episode generation
	// so the node can verify correspondence. A fresh ephemeral key is used; the CP mints the
	// aud=node token in dev mode when access_token is empty.
	bindFrame := &cpv1.Frame{SpawnId: id}
	if sessionKey, skErr := ecdsa.GenerateKey(elliptic.P256(), rand.Reader); skErr == nil {
		var jtiBytes [16]byte
		if _, rErr := rand.Read(jtiBytes[:]); rErr == nil {
			body := &authv1.IntentBody{
				Jti:        fmt.Sprintf("%x", jtiBytes),
				IssuedAt:   time.Now().Unix(),
				SpawnId:    id,
				Generation: spawnGen,
				SessionId:  "0",
				Op:         string(intent.OpSessionOpen),
			}
			if si, bErr := intent.Build(intent.OpSessionOpen, body, sessionKey); bErr == nil {
				bindFrame.SessionAuth = &authv1.AuthEnvelope{Intent: si}
			}
		}
	}

	stream := client.Session(ctx)
	if err := stream.Send(bindFrame); err != nil { // bind frame (carries the spawn id + session-open auth)
		log.Fatalf("bind: %v", err)
	}

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

	sendW := writerFunc(func(b []byte) (int, error) {
		if err := stream.Send(&cpv1.Frame{SpawnId: id, Data: b}); err != nil {
			return 0, err
		}
		return len(b), nil
	})

	// The CP relays the frame protocol (not raw ACP): the node's pump does the ACP handshake and
	// exposes {"kind":"prompt"} in / user|agent|turn frames out. Drive it like the web client.
	driveFrames(pr, sendW)

	_ = stream.CloseRequest()
	_, _ = client.StopSpawn(ctx, connect.NewRequest(&cpv1.StopSpawnRequest{SpawnId: id}))
}

// waitActiveCP polls ListSpawns until the spawn is ACTIVE (router-bound), failing fast on a terminal
// status. Returns the spawn's live episode generation for use in A4 session-open signing [AM11].
// CreateSpawn returns in 'starting' and provisions asynchronously on the node.
func waitActiveCP(ctx context.Context, client cpv1connect.SpawnServiceClient, id string) uint64 {
	deadline := time.Now().Add(60 * time.Second)
	for {
		ls, err := client.ListSpawns(ctx, connect.NewRequest(&cpv1.ListSpawnsRequest{}))
		if err != nil {
			log.Fatalf("listSpawns: %v", err)
		}
		for _, sp := range ls.Msg.Spawns {
			if sp.SpawnId != id {
				continue
			}
			switch sp.Status {
			case cpv1.SpawnStatus_SPAWN_STATUS_ACTIVE:
				return sp.Generation
			case cpv1.SpawnStatus_SPAWN_STATUS_ERROR, cpv1.SpawnStatus_SPAWN_STATUS_DELETED,
				cpv1.SpawnStatus_SPAWN_STATUS_UNREACHABLE:
				log.Fatalf("spawn %s reached terminal status %v before active", id, sp.Status)
			}
		}
		if time.Now().After(deadline) {
			log.Fatalf("spawn %s did not reach ACTIVE within 60s", id)
		}
		time.Sleep(200 * time.Millisecond)
	}
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

// h2cClient returns an *http.Client configured for cleartext HTTP/2 (h2c).
// This is required for Connect bidi streaming without TLS.
func h2cClient() *http.Client {
	return &http.Client{
		Transport: &http2.Transport{
			AllowHTTP: true,
			DialTLSContext: func(ctx context.Context, network, addr string, _ *tls.Config) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, network, addr)
			},
		},
	}
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
