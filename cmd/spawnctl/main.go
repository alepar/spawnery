package main

import (
	"bufio"
	"context"
	"crypto/tls"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"

	"connectrpc.com/connect"
	"golang.org/x/net/http2"

	cpv1 "spawnery/gen/cp/v1"
	"spawnery/gen/cp/v1/cpv1connect"
	spawnv1 "spawnery/gen/spawn/v1"
	"spawnery/gen/spawn/v1/spawnv1connect"
	"spawnery/internal/acp"
	"spawnery/internal/manifest"
)

func main() {
	addr := flag.String("addr", "http://127.0.0.1:9090", "spawnlet address")
	appPath := flag.String("app", "examples/secret-app", "app definition dir")
	model := flag.String("model", "anthropic/claude-3.5-sonnet", "OpenRouter model")
	cpAddr := flag.String("cp", "", "control-plane address (http://127.0.0.1:8080); overrides -addr")
	appID := flag.String("app-id", "spawnery/secret-app", "app id (CP mode)")
	token := flag.String("token", "dev-token", "dev auth token (CP mode)")
	register := flag.Bool("register", false, "register the -app manifest with the CP and exit (CP mode)")
	version := flag.String("version", "1.0.0", "app version to register (with -register)")
	ref := flag.String("ref", "", "immutable app ref creator/app@sha (with -register)")
	flag.Parse()

	ctx := context.Background()
	if *register {
		if *cpAddr == "" {
			log.Fatal("-register requires -cp")
		}
		runRegister(ctx, *cpAddr, *appPath, *version, *ref, *token)
		return
	}
	if *cpAddr != "" {
		runCP(ctx, *cpAddr, *appID, *model, *token)
		return
	}
	runStandalone(ctx, *addr, *appPath, *model)
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
		mounts[i] = &cpv1.ManifestMount{Name: mt.Name, Path: mt.Path, Seed: mt.Seed}
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
func runRegister(ctx context.Context, cpAddr, appDir, version, ref, token string) {
	pm, err := manifestToProto(appDir)
	if err != nil {
		log.Fatalf("manifest: %v", err)
	}
	client := cpv1connect.NewSpawnServiceClient(h2cClient(), cpAddr,
		connect.WithGRPC(), connect.WithInterceptors(cpBearer(token)))
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

	stream.CloseRequest()
	_, _ = client.StopSpawn(ctx, connect.NewRequest(&spawnv1.StopSpawnRequest{SpawnId: id}))
}

// runCP drives the agent through the control plane via the cp.v1 service.
func runCP(ctx context.Context, addr, appID, model, token string) {
	client := cpv1connect.NewSpawnServiceClient(h2cClient(), addr,
		connect.WithGRPC(), connect.WithInterceptors(cpBearer(token)))

	cs, err := client.CreateSpawn(ctx, connect.NewRequest(&cpv1.CreateSpawnRequest{
		AppId: appID,
		Model: model,
	}))
	if err != nil {
		log.Fatalf("createSpawn: %v", err)
	}
	id := cs.Msg.SpawnId
	fmt.Println("spawn:", id)

	stream := client.Session(ctx)

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

	driveACP(pr, sendW)

	stream.CloseRequest()
	_, _ = client.StopSpawn(ctx, connect.NewRequest(&cpv1.StopSpawnRequest{SpawnId: id}))
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

// cpBearer is a client-side interceptor that sets "Authorization: Bearer <token>"
// on unary requests and on the streaming-client connection, mirroring the CP's
// server-side auth interceptor (internal/cp/auth).
func cpBearer(token string) connect.Interceptor { return bearerInterceptor{token: token} }

type bearerInterceptor struct{ token string }

func (b bearerInterceptor) WrapUnary(next connect.UnaryFunc) connect.UnaryFunc {
	return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
		req.Header().Set("Authorization", "Bearer "+b.token)
		return next(ctx, req)
	}
}

func (b bearerInterceptor) WrapStreamingClient(next connect.StreamingClientFunc) connect.StreamingClientFunc {
	return func(ctx context.Context, spec connect.Spec) connect.StreamingClientConn {
		conn := next(ctx, spec)
		conn.RequestHeader().Set("Authorization", "Bearer "+b.token)
		return conn
	}
}

func (b bearerInterceptor) WrapStreamingHandler(next connect.StreamingHandlerFunc) connect.StreamingHandlerFunc {
	return next // server-side: no-op
}

// writerFunc adapts a func([]byte)(int,error) to the io.Writer interface.
type writerFunc func([]byte) (int, error)

func (f writerFunc) Write(b []byte) (int, error) { return f(b) }
