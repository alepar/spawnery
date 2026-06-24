//go:build e2e

package spawnlet_test

import (
	"context"
	"crypto/tls"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	"golang.org/x/net/http2"
	"golang.org/x/net/http2/h2c"

	spawnv1 "spawnery/gen/spawn/v1"
	"spawnery/gen/spawn/v1/spawnv1connect"
	"spawnery/internal/acp"
	"spawnery/internal/runtime"
	"spawnery/internal/spawnlet"
)

// TestEndToEndPiACP exercises the pi rich-web path (pi-acp runnable) end to end
// through the spawnlet: the in-pod pi-adapter spawns `pi --mode rpc` and
// translates its JSONL stream to ACP. It drives an ACP round-trip
// (initialize -> session/new -> prompt) where pi reads an unguessable secret out
// of the scratch mount and recites it, a multi-turn follow-up in the SAME
// session, and a suspend/resume-from-delta cycle proving the spawn relaunches
// from its captured rootfs delta and still answers.
//
// Requires Docker, the spawnery/agent:dev image (make images), and a live
// OPENROUTER_API_KEY. If any is missing it FAILS loudly (no skips) so a broken
// env is detected (project lane-test rule).
func TestEndToEndPiACP(t *testing.T) {
	key := os.Getenv("OPENROUTER_API_KEY")
	if key == "" {
		t.Fatal("OPENROUTER_API_KEY is required for the pi-acp e2e test")
	}

	rt, err := runtime.NewDocker()
	if err != nil {
		t.Fatalf("docker unavailable (need Docker for the pi-acp e2e): %v", err)
	}
	if err := rt.Ping(context.Background()); err != nil {
		t.Fatalf("docker not pingable: %v", err)
	}

	mgr := spawnlet.NewManager(rt, spawnlet.ManagerConfig{
		AgentImage:    "spawnery/agent:dev", // unified image; build with: make images
		SidecarImage:  "spawnery/sidecar:dev",
		OpenRouterKey: key,
		DataRoot:      t.TempDir(),
		DeltaCapture:  true, // Suspend captures spawnery/delta:<id> for the resume leg
	})

	srv := spawnlet.NewServer(mgr)
	mux := http.NewServeMux()
	mux.Handle(spawnv1connect.NewSpawnServiceHandler(srv))
	hs := httptest.NewUnstartedServer(h2c.NewHandler(mux, &http2.Server{}))
	hs.Start()
	defer hs.Close()

	// h2c (cleartext HTTP/2) client so the gRPC bidi Session stream works.
	hc := &http.Client{
		Transport: &http2.Transport{
			AllowHTTP: true,
			DialTLSContext: func(ctx context.Context, network, addr string, _ *tls.Config) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, network, addr)
			},
		},
	}
	cl := spawnv1connect.NewSpawnServiceClient(hc, hs.URL, connect.WithGRPC())

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Second)
	defer cancel()

	const (
		spawnID = "sp-pi-acp-e2e"
		model   = "openai/gpt-oss-120b:free"
		secret  = "QUOKKA-4417"
	)
	appPath := mustAbs(t, "../../examples/secret-app")

	// Create the pi spawn directly on the manager: CreateSpawn has no runnable
	// field and would default to the image's opencode runnable.
	if _, err := mgr.CreateWithSelection(ctx, spawnID, appPath, model, "", "", 0,
		spawnlet.AgentSelection{RunnableID: "pi-acp"}); err != nil {
		t.Fatalf("create (pi-acp): %v", err)
	}
	defer func() {
		_, _ = cl.StopSpawn(context.Background(), connect.NewRequest(&spawnv1.StopSpawnRequest{SpawnId: spawnID}))
	}()

	// --- Phase 1+2: ACP round-trip + multi-turn over one Session stream ---
	func() {
		streamCtx, streamCancel := context.WithCancel(ctx)
		defer streamCancel()
		stream := cl.Session(streamCtx)
		pr, pw := io.Pipe()
		go func() {
			for {
				f, err := stream.Receive()
				if err != nil {
					pw.CloseWithError(err)
					return
				}
				pw.Write(f.Data)
			}
		}()

		c := acp.NewClient(pr, writerTo(stream, spawnID))
		if err := c.Initialize(); err != nil {
			t.Fatalf("phase1 init: %v", err)
		}
		if err := c.NewSession("/app"); err != nil {
			t.Fatalf("phase1 session/new: %v", err)
		}

		// Phase 1: ACP round-trip — read the seeded secret via pi's file-read tool.
		var t1 strings.Builder
		if err := c.Prompt("What is the secret word?", func(s string) { t1.WriteString(s) }); err != nil {
			t.Fatalf("phase1 prompt: %v", err)
		}
		t.Logf("pi turn1 reply: %q", t1.String())
		if !strings.Contains(t1.String(), secret) {
			t.Fatalf("phase1: agent did not recite the secret; got %q", t1.String())
		}

		// Phase 2: multi-turn — a follow-up in the SAME session must still answer.
		var t2 strings.Builder
		if err := c.Prompt("Repeat ONLY the secret word, nothing else.", func(s string) { t2.WriteString(s) }); err != nil {
			t.Fatalf("phase2 prompt: %v", err)
		}
		t.Logf("pi turn2 reply: %q", t2.String())
		if !strings.Contains(t2.String(), secret) {
			t.Fatalf("phase2 (multi-turn): follow-up lost the secret; got %q", t2.String())
		}
	}()

	// --- Phase 3: suspend (captures rootfs delta) then resume-from-delta ---
	if _, err := mgr.Suspend(ctx, spawnID); err != nil {
		t.Fatalf("suspend: %v", err)
	}
	// Re-create the SAME spawn id: EnsureImage returns spawnery/delta:<id>, so the
	// pod relaunches from the captured delta rather than the base image.
	if _, err := mgr.CreateWithSelection(ctx, spawnID, appPath, model, "", "", 0,
		spawnlet.AgentSelection{RunnableID: "pi-acp"}); err != nil {
		t.Fatalf("resume (re-create from delta): %v", err)
	}

	func() {
		streamCtx, streamCancel := context.WithCancel(ctx)
		defer streamCancel()
		stream := cl.Session(streamCtx)
		pr, pw := io.Pipe()
		go func() {
			for {
				f, err := stream.Receive()
				if err != nil {
					pw.CloseWithError(err)
					return
				}
				pw.Write(f.Data)
			}
		}()

		c := acp.NewClient(pr, writerTo(stream, spawnID))
		if err := c.Initialize(); err != nil {
			t.Fatalf("phase3 init (post-resume): %v", err)
		}
		if err := c.NewSession("/app"); err != nil {
			t.Fatalf("phase3 session/new (post-resume): %v", err)
		}
		var t3 strings.Builder
		if err := c.Prompt("What is the secret word?", func(s string) { t3.WriteString(s) }); err != nil {
			t.Fatalf("phase3 prompt (post-resume): %v", err)
		}
		t.Logf("pi post-resume reply: %q", t3.String())
		if !strings.Contains(t3.String(), secret) {
			t.Fatalf("phase3 (resume): agent failed after resume-from-delta; got %q", t3.String())
		}
	}()
}
