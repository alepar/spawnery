//go:build e2e

package cp_test

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"

	cpv1 "spawnery/gen/cp/v1"
	"spawnery/gen/cp/v1/cpv1connect"
)

// TestCPGooseAcpEndToEnd drives the full goose-acp ACP path:
//   - CP + node run in-process with a real Docker backend (spawnery/agent:dev),
//     advertising "goose" so the catalog offers goose-acp.
//   - CreateSpawn selects goose-acp (ModeACP). The image dispatcher runs
//     `acpexec goose acp`: acpexec listens on ACP_LISTEN (tcp://0.0.0.0:7000),
//     the node dials it and the Pump performs ACP initialize + session/new.
//   - Reaching ACTIVE already proves goose answered the ACP handshake over the
//     stdio<->TCP bridge (the node only marks ACTIVE after the Pump handshake
//     succeeds).
//   - The client then drives the frame protocol (like TestCPEndToEndStub) — sends
//     a {"kind":"prompt"} frame and asserts a non-empty assistant ("agent") frame
//     arrives, i.e. real model output through the ACP fanout (goose -> sidecar ->
//     OpenRouter -> back through the Pump fanout).
//
// goose reaches the model via the OpenAI sidecar (GOOSE_PROVIDER=openai,
// OPENAI_BASE_URL set by the node). Requires Docker + spawnery/agent:dev +
// spawnery/sidecar:dev + OPENROUTER_API_KEY (env or repo-root .env).
// FAILS loudly (no skips) when the environment is broken.
func TestCPGooseAcpEndToEnd(t *testing.T) {
	cl, ctx, appID := setupTmuxStack(t)

	// Create a goose-acp spawn via the dispatcher (ModeACP — no tmux).
	cs, err := cl.CreateSpawn(ctx, connect.NewRequest(&cpv1.CreateSpawnRequest{
		AppId:      appID,
		Model:      "openai/gpt-4o-mini",
		Image:      "spawnery/agent:dev",
		RunnableId: "goose-acp",
	}))
	if err != nil {
		t.Fatalf("CreateSpawn goose-acp: %v", err)
	}
	id := cs.Msg.SpawnId
	t.Logf("goose-acp spawn created: %s", id)
	t.Cleanup(func() {
		stopCtx, c := context.WithTimeout(context.Background(), 10*time.Second)
		defer c()
		_, _ = cl.StopSpawn(stopCtx, connect.NewRequest(&cpv1.StopSpawnRequest{SpawnId: id}))
		time.Sleep(2 * time.Second) // allow the node to receive Stop + destroy containers
	})

	// Wait for ACTIVE — image boot + goose init over the ACP handshake. Reaching
	// ACTIVE proves the Pump's initialize + session/new succeeded against goose
	// through the acpexec bridge.
	waitActiveGooseAcp(ctx, t, cl, id)
	t.Log("goose-acp spawn is ACTIVE (ACP handshake proven), opening Session")

	// Drive the frame protocol: bind, send a prompt, read assistant frames.
	stream := cl.Session(ctx)
	if err := stream.Send(&cpv1.Frame{SpawnId: id}); err != nil { // bind frame
		t.Fatalf("Session bind frame: %v", err)
	}
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

	sendFrame := func(f map[string]any) {
		b, _ := json.Marshal(f)
		if err := stream.Send(&cpv1.Frame{SpawnId: id, Data: append(b, '\n')}); err != nil {
			t.Fatalf("send frame: %v", err)
		}
	}
	sendFrame(map[string]any{"kind": "prompt", "text": "Reply with the single word: banana"})

	// Collect assistant ("agent") text until the turn goes idle (or timeout).
	type frame struct {
		Kind  string `json:"kind"`
		Text  string `json:"text"`
		State string `json:"state"`
	}
	var got strings.Builder
	done := make(chan struct{})
	go func() {
		defer close(done)
		sc := bufio.NewScanner(pr)
		sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
		for sc.Scan() {
			var fr frame
			if json.Unmarshal(sc.Bytes(), &fr) != nil {
				continue
			}
			if fr.Kind == "agent" {
				got.WriteString(fr.Text)
			}
			if fr.Kind == "turn" && fr.State == "idle" {
				return // turn complete
			}
		}
	}()

	// Real inference over the bridge can take a while (cold model + sidecar).
	select {
	case <-done:
	case <-time.After(90 * time.Second):
		t.Fatalf("timed out (90s) waiting for goose assistant response; got so far: %q", got.String())
	}

	resp := strings.TrimSpace(got.String())
	t.Logf("goose-acp assistant response: %q", resp)
	if resp == "" {
		t.Fatal("goose-acp returned an empty assistant response; expected real model output via the ACP fanout")
	}

	stream.CloseRequest()
	time.Sleep(300 * time.Millisecond) // let session_end flush
	t.Log("goose-acp end-to-end verified (handshake + real model output via acpexec bridge + Pump fanout)")
}

// waitActiveGooseAcp polls ListSpawns until the spawn reaches ACTIVE, allowing a
// generous timeout for image boot + goose's ACP init over the bridge.
func waitActiveGooseAcp(ctx context.Context, t *testing.T, cl cpv1connect.SpawnServiceClient, id string) {
	t.Helper()
	deadline := time.Now().Add(90 * time.Second)
	for {
		ls, err := cl.ListSpawns(ctx, connect.NewRequest(&cpv1.ListSpawnsRequest{}))
		if err != nil {
			t.Fatalf("listSpawns: %v", err)
		}
		for _, sp := range ls.Msg.Spawns {
			if sp.SpawnId != id {
				continue
			}
			switch sp.Status {
			case cpv1.SpawnStatus_SPAWN_STATUS_ACTIVE:
				return
			case cpv1.SpawnStatus_SPAWN_STATUS_ERROR, cpv1.SpawnStatus_SPAWN_STATUS_DELETED,
				cpv1.SpawnStatus_SPAWN_STATUS_UNREACHABLE:
				t.Fatalf("spawn %s reached terminal status %v before active", id, sp.Status)
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("spawn %s did not reach ACTIVE within 90s", id)
		}
		time.Sleep(200 * time.Millisecond)
	}
}
