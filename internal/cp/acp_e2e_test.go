//go:build e2e

package cp_test

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"

	"spawnery/internal/acp"
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

// TestAcpmuxTwoClientsShareSession proves the shared-session property end-to-end
// against the REAL image's in-container acpmux + real goose: it starts a
// sidecar+agent pod (goose-acp) directly via docker, connects TWO raw canonical
// ACP clients to acpmux on :7000, has client A prompt, and asserts:
//   - Fanout: client B (connected before the prompt) sees the SAME agent
//     session/update chunks A's turn produces.
//   - Late-join replay: a third client C connecting AFTER the turn and doing
//     session/new receives the buffered history (the earlier session/updates).
//
// This is the live counterpart to the internal/acpmux unit tests' fanout +
// late-join cases, now through real goose + the real model. Requires Docker +
// spawnery/agent:dev + spawnery/sidecar:dev + OPENROUTER_API_KEY (env or
// repo-root .env). FAILS loudly (no skips).
func TestAcpmuxTwoClientsShareSession(t *testing.T) {
	key := loadOpenRouterKey(t)

	// Phase 1: sidecar (owns the pod netns, injects the real key). Mirrors
	// spawnlet.Manager's SidecarEnv (OPENROUTER_API_KEY + SIDECAR_ADDR).
	const sidecarAddr = "127.0.0.1:8080"
	sidecar := dockerRunD(t, []string{
		"-e", "OPENROUTER_API_KEY=" + key,
		"-e", "SIDECAR_ADDR=" + sidecarAddr,
		"spawnery/sidecar:dev",
	})
	t.Cleanup(func() { _ = exec.Command("docker", "rm", "-f", sidecar).Run() })

	sidecarIP := dockerIP(t, sidecar)
	if sidecarIP == "" {
		t.Fatal("sidecar has no bridge IP (rootless-without-bridge unsupported for this test)")
	}

	// Phase 2: agent (goose-acp) in the sidecar's netns, ACP on :7000. Mirrors
	// Manager's AgentSpec env (OPENAI_BASE_URL via the sidecar, SPAWN_MODEL).
	agent := dockerRunD(t, []string{
		"--network", "container:" + sidecar,
		"-e", "ACP_LISTEN=tcp://0.0.0.0:7000",
		"-e", "OPENAI_BASE_URL=http://" + sidecarAddr + "/v1",
		"-e", "SPAWN_MODEL=openai/gpt-4o-mini",
		"--entrypoint", "/usr/bin/tini",
		"spawnery/agent:dev", "--", "/entrypoint.sh", "goose-acp",
	})
	t.Cleanup(func() { _ = exec.Command("docker", "rm", "-f", agent).Run() })

	// Wait for acpmux to be listening + session ready (the log line from main).
	endpoint := net.JoinHostPort(sidecarIP, "7000")
	waitAcpmuxReady(t, agent, endpoint)

	// Two raw ACP clients connected up front.
	a := dialRawACP(t, endpoint)
	defer a.Close()
	b := dialRawACP(t, endpoint)
	defer b.Close()
	rawHandshake(t, a)
	rawHandshake(t, b)

	// A prompts; collect A's streamed agent chunks until its prompt result.
	aReqID := rawPrompt(t, a, "Reply with the single word: banana")
	aText, aSession := rawCollectTurn(t, a, aReqID, 90*time.Second)
	if strings.TrimSpace(aText) == "" {
		t.Fatal("client A got empty agent text; expected real model output via acpmux")
	}
	t.Logf("client A agent text: %q", aText)

	// Fanout: B (also attached) must have seen the SAME agent chunks. Read B's
	// buffered updates (it never prompted, so it only receives fanned updates).
	bText := rawCollectFanned(t, b, len(aSession), 30*time.Second)
	if strings.TrimSpace(bText) == "" {
		t.Fatal("client B saw no fanned session/updates; fanout broken across clients")
	}
	t.Logf("client B fanned text: %q", bText)
	if strings.TrimSpace(bText) != strings.TrimSpace(aText) {
		t.Fatalf("fanout mismatch: A=%q B=%q (both clients must see the same updates)", aText, bText)
	}

	// Late-join replay: C connects AFTER the turn; session/new must replay the
	// buffered history (the same agent chunks).
	c := dialRawACP(t, endpoint)
	defer c.Close()
	rawHandshake(t, c) // session/new triggers replay
	cText := rawCollectFanned(t, c, len(aSession), 30*time.Second)
	if strings.TrimSpace(cText) != strings.TrimSpace(aText) {
		t.Fatalf("late-join replay mismatch: A=%q C(replay)=%q", aText, cText)
	}
	t.Logf("late-join client C replayed text: %q", cText)
	t.Log("acpmux two-client shared-session verified (fanout + late-join replay through real goose)")
}

// ---- raw ACP client helpers for the two-client e2e --------------------------

func loadOpenRouterKey(t *testing.T) string {
	t.Helper()
	if k := os.Getenv("OPENROUTER_API_KEY"); k != "" {
		return k
	}
	envPath, _ := filepath.Abs("../../.env")
	raw, err := os.ReadFile(envPath)
	if err == nil {
		for _, line := range strings.Split(string(raw), "\n") {
			if strings.HasPrefix(line, "OPENROUTER_API_KEY=") {
				return strings.TrimSpace(strings.TrimPrefix(line, "OPENROUTER_API_KEY="))
			}
		}
	}
	t.Fatal("OPENROUTER_API_KEY required (env or repo-root .env)")
	return ""
}

func dockerRunD(t *testing.T, args []string) string {
	t.Helper()
	out, err := exec.Command("docker", append([]string{"run", "-d"}, args...)...).CombinedOutput()
	if err != nil {
		t.Fatalf("docker run -d %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func dockerIP(t *testing.T, id string) string {
	t.Helper()
	out, err := exec.Command("docker", "inspect", "-f",
		"{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}", id).Output()
	if err != nil {
		t.Fatalf("docker inspect ip %s: %v", id, err)
	}
	return strings.TrimSpace(string(out))
}

// waitAcpmuxReady waits for acpmux to log "session ... ready" AND for the TCP
// endpoint to accept a connection.
func waitAcpmuxReady(t *testing.T, agentID, endpoint string) {
	t.Helper()
	deadline := time.Now().Add(120 * time.Second)
	for time.Now().Before(deadline) {
		logs, _ := exec.Command("docker", "logs", agentID).CombinedOutput()
		if strings.Contains(string(logs), "session") && strings.Contains(string(logs), "ready") {
			if conn, err := net.DialTimeout("tcp", endpoint, 2*time.Second); err == nil {
				_ = conn.Close()
				return
			}
		}
		time.Sleep(1 * time.Second)
	}
	logs, _ := exec.Command("docker", "logs", agentID).CombinedOutput()
	t.Fatalf("acpmux not ready within 120s; agent logs:\n%s", logs)
}

type rawClient struct {
	conn net.Conn
	rd   *acp.Reader
	nid  int
}

func dialRawACP(t *testing.T, endpoint string) *rawClient {
	t.Helper()
	conn, err := net.Dial("tcp", endpoint)
	if err != nil {
		t.Fatalf("dial acpmux %s: %v", endpoint, err)
	}
	return &rawClient{conn: conn, rd: acp.NewReader(conn)}
}

func (rc *rawClient) Close() { _ = rc.conn.Close() }

func (rc *rawClient) send(m acp.Message) { _ = acp.WriteMessage(rc.conn, m) }

func (rc *rawClient) callRaw(t *testing.T, method string, params json.RawMessage) {
	t.Helper()
	rc.nid++
	id := rc.nid
	rc.send(acp.Message{ID: &id, Method: method, Params: params})
	_ = rc.conn.SetReadDeadline(time.Now().Add(30 * time.Second))
	for {
		m, err := rc.rd.ReadMessage()
		if err != nil {
			t.Fatalf("callRaw %s read: %v", method, err)
		}
		if m.ID != nil && *m.ID == id && (m.Result != nil || m.Error != nil) {
			return
		}
	}
}

func rawHandshake(t *testing.T, rc *rawClient) {
	t.Helper()
	rc.callRaw(t, "initialize", json.RawMessage(`{"protocolVersion":1,"clientCapabilities":{}}`))
	rc.callRaw(t, "session/new", json.RawMessage(`{"cwd":"/app","mcpServers":[]}`))
}

func rawPrompt(t *testing.T, rc *rawClient, text string) int {
	t.Helper()
	rc.nid++
	id := rc.nid
	p, _ := json.Marshal(map[string]any{
		"sessionId": "ignored",
		"prompt":    []any{map[string]string{"type": "text", "text": text}},
	})
	rc.send(acp.Message{ID: &id, Method: "session/prompt", Params: p})
	return id
}

// rawUpdateText returns the agent_message_chunk text from a session/update, "" otherwise.
func rawUpdateText(m acp.Message) string {
	if m.Method != "session/update" {
		return ""
	}
	var u struct {
		Update struct {
			SessionUpdate string `json:"sessionUpdate"`
			Content       struct {
				Text string `json:"text"`
			} `json:"content"`
		} `json:"update"`
	}
	if json.Unmarshal(m.Params, &u) != nil || u.Update.SessionUpdate != "agent_message_chunk" {
		return ""
	}
	return u.Update.Content.Text
}

// rawCollectTurn reads from rc until rc's session/prompt result (reqID) arrives,
// accumulating agent_message_chunk text. Returns the text and the count of
// session/update notifications observed (for the fanout count baseline).
func rawCollectTurn(t *testing.T, rc *rawClient, reqID int, timeout time.Duration) (string, []string) {
	t.Helper()
	var text strings.Builder
	var updates []string
	deadline := time.Now().Add(timeout)
	for {
		_ = rc.conn.SetReadDeadline(deadline)
		m, err := rc.rd.ReadMessage()
		if err != nil {
			t.Fatalf("rawCollectTurn read: %v (text so far %q)", err, text.String())
		}
		if txt := rawUpdateText(m); txt != "" {
			text.WriteString(txt)
			updates = append(updates, txt)
			continue
		}
		if m.ID != nil && *m.ID == reqID && (m.Result != nil || m.Error != nil) {
			return text.String(), updates
		}
	}
}

// rawCollectFanned reads agent_message_chunk text from a non-prompting client
// until it has accumulated at least wantChunks chunks (or timeout). Used to
// verify a second/late-joining client sees the same fanned/replayed updates.
func rawCollectFanned(t *testing.T, rc *rawClient, wantChunks int, timeout time.Duration) string {
	t.Helper()
	var text strings.Builder
	got := 0
	deadline := time.Now().Add(timeout)
	for got < wantChunks && time.Now().Before(deadline) {
		_ = rc.conn.SetReadDeadline(time.Now().Add(5 * time.Second))
		m, err := rc.rd.ReadMessage()
		if err != nil {
			break // deadline or close; return what we have
		}
		if txt := rawUpdateText(m); txt != "" {
			text.WriteString(txt)
			got++
		}
	}
	return text.String()
}
