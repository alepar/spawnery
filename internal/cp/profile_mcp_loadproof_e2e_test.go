//go:build e2e

package cp_test

// TestCPProfileMCPLoadProof is the live-agent MCP load-proof e2e (design §11,
// roast E7). It injects a custom MCP via a profile into a real agent pod, prompts
// the agent to call the tool, and asserts the tool call actually executed — proving
// the inject→load→tool-call path end-to-end.
//
// The fixture MCP server (deploy/agent/testmcp/main.go, baked as spawnery-test-mcp
// into the agent image at /usr/local/bin/spawnery-test-mcp) exposes a single tool
// "record_proof" that writes $SPAWNERY_TEST_MCP_TOKEN to $SPAWNERY_MCP_PROOF_FILE.
// The test polls that file via `docker exec` to confirm the call happened.
//
// Requires:
//   - Docker + spawnery/agent:dev (built with the testmcp binary via `make images`)
//   - OPENROUTER_API_KEY in env or .env at repo root
//
// Runs per-agent (claude-tui primary; codex-tui; opencode-tui) as subtests.
// FAILS loudly (no skips) when the environment is broken per the build-tag-is-opt-in rule.

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"

	cpv1 "spawnery/gen/cp/v1"
	"spawnery/internal/agentinstall/spec"
	"spawnery/internal/runtime"
)

func TestCPProfileMCPLoadProof(t *testing.T) {
	// Pre-flight: validate Docker and agent image (fail loudly, no skips).
	if out, err := exec.Command("docker", "info").CombinedOutput(); err != nil {
		t.Fatalf("Docker is required for this e2e test but is not available: %v\n%s", err, out)
	}
	if out, err := exec.Command("docker", "run", "--rm", "--entrypoint", "which",
		"spawnery/agent:dev", "spawnery-test-mcp").CombinedOutput(); err != nil {
		t.Fatalf("spawnery/agent:dev must be built with spawnery-test-mcp (run `make images`): %v\n%s", err, out)
	}

	type agentCase struct {
		runnableID string
		proofPath  string
	}
	cases := []agentCase{
		{runnableID: "claude-tui", proofPath: "/tmp/spawnery-proof-claude"},
		{runnableID: "codex-tui", proofPath: "/tmp/spawnery-proof-codex"},
		{runnableID: "opencode-tui", proofPath: "/tmp/spawnery-proof-opencode"},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.runnableID, func(t *testing.T) {
			// Each subtest gets its own CP+node stack and 150s context so that sequential
			// subtests don't share a timeout (each agent spawn can take ≈120s on its own).
			cl, ctx, appID := setupTmuxStack(t)

			// Unique token per subtest so cross-subtest noise is detectable.
			token := fmt.Sprintf("QUOKKA-%s-%d", tc.runnableID, time.Now().UnixNano())

			// 1. Create a profile with a single custom MCP entry pointing to spawnery-test-mcp.
			profResp, err := cl.CreateProfile(ctx, connect.NewRequest(&cpv1.CreateProfileRequest{
				Name: "mcp-load-proof-" + tc.runnableID,
			}))
			if err != nil {
				t.Fatalf("CreateProfile: %v", err)
			}
			profileID := profResp.Msg.ProfileId

			mcpPayload := spec.MCPPayload{
				Stdio: &spec.MCPTransportStdio{
					Command: "spawnery-test-mcp",
					Env: map[string]string{
						"SPAWNERY_TEST_MCP_TOKEN": token,
						"SPAWNERY_MCP_PROOF_FILE": tc.proofPath,
					},
				},
			}
			mcpBytes, err := json.Marshal(mcpPayload)
			if err != nil {
				t.Fatalf("marshal MCP payload: %v", err)
			}

			_, err = cl.AddProfileEntry(ctx, connect.NewRequest(&cpv1.AddProfileEntryRequest{
				ProfileId:       profileID,
				ExpectedVersion: 1,
				Entry: &cpv1.ProfileEntry{
					Kind:         cpv1.ProfileEntryKind_PROFILE_ENTRY_KIND_MCP,
					Name:         "record-proof-mcp",
					Source:       cpv1.ProfileEntrySource_PROFILE_ENTRY_SOURCE_CUSTOM,
					CustomInline: mcpBytes,
				},
			}))
			if err != nil {
				t.Fatalf("AddProfileEntry: %v", err)
			}

			// 2. Create a spawn with the profile attached.
			cs, err := cl.CreateSpawn(ctx, connect.NewRequest(&cpv1.CreateSpawnRequest{
				AppId:      appID,
				Model:      "openai/gpt-4o-mini",
				Image:      "spawnery/agent:dev",
				RunnableId: tc.runnableID,
				ProfileId:  profileID,
			}))
			if err != nil {
				t.Fatalf("CreateSpawn %s: %v", tc.runnableID, err)
			}
			spawnID := cs.Msg.SpawnId
			t.Logf("%s spawn created: %s (profile: %s)", tc.runnableID, spawnID, profileID)
			t.Cleanup(func() {
				stopCtx, c := context.WithTimeout(context.Background(), 10*time.Second)
				defer c()
				_, _ = cl.StopSpawn(stopCtx, connect.NewRequest(&cpv1.StopSpawnRequest{SpawnId: spawnID}))
				time.Sleep(2 * time.Second)
			})

			// 3. Wait for the spawn to become ACTIVE (container boot + agent startup).
			waitActiveTmux(ctx, t, cl, spawnID)
			t.Logf("%s spawn is ACTIVE", tc.runnableID)

			// 4. Find the agent container ID (generation 1 for a fresh spawn).
			gen := findSpawnGeneration(ctx, t, cl, spawnID)
			agentContainer := findAgentContainer(ctx, t, spawnID, gen)
			t.Logf("%s agent container: %s (gen %d)", tc.runnableID, agentContainer, gen)

			// 5. Drive a prompt instructing the agent to call record_proof.
			// Open a Session, send the binding frame + the prompt keystrokes.
			prompt := "Call the record_proof tool with no arguments, then reply 'done'."
			driveTUIPrompt(t, ctx, cl, spawnID, prompt)
			t.Logf("%s: prompt sent, polling proof file %s", tc.runnableID, tc.proofPath)

			// 6. Poll the proof file inside the container (≤60s for LLM round-trip + tool call).
			assertProofFile(ctx, t, agentContainer, tc.proofPath, token, 60*time.Second)
			t.Logf("%s: proof file confirmed — MCP inject→load→tool-call verified", tc.runnableID)
		})
	}
}

// driveTUIPrompt sends a plain-text prompt to a tmux-based TUI spawn via the Session API.
// It opens a Session stream, binds to the spawn, waits briefly for the TUI to render,
// then sends the prompt string followed by carriage-return.
func driveTUIPrompt(t *testing.T, ctx context.Context, cl interface {
	Session(context.Context) *connect.BidiStreamForClient[cpv1.Frame, cpv1.Frame]
}, spawnID, prompt string) {
	t.Helper()
	stream := cl.Session(ctx)
	// Binding frame: SpawnId only, no data.
	if err := stream.Send(&cpv1.Frame{SpawnId: spawnID}); err != nil {
		t.Fatalf("driveTUIPrompt: Session bind: %v", err)
	}

	// Drain a small amount of initial terminal output (≤5s) to confirm the TUI is
	// rendering before we type. We don't need to assert specific bytes here.
	drainCh := make(chan struct{}, 1)
	go func() {
		for {
			f, err := stream.Receive()
			if err != nil {
				return
			}
			if len(f.Data) > 0 {
				select {
				case drainCh <- struct{}{}:
				default:
				}
				return
			}
		}
	}()
	select {
	case <-drainCh:
		t.Log("driveTUIPrompt: TUI rendered initial output")
	case <-time.After(15 * time.Second):
		t.Log("driveTUIPrompt: no initial output within 15s, sending prompt anyway")
	}

	// Send the prompt + Enter.
	data := []byte(prompt + "\r")
	if err := stream.Send(&cpv1.Frame{SpawnId: spawnID, Data: data}); err != nil {
		t.Logf("driveTUIPrompt: send prompt frame: %v (non-fatal)", err)
	}
	stream.CloseRequest()
}

// assertProofFile polls `docker exec <container> cat <path>` until the output contains
// token, or fails after timeout. This verifies that the MCP tool was actually called.
func assertProofFile(ctx context.Context, t *testing.T, containerID, path, token string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		out, err := exec.CommandContext(ctx, "docker", "exec", containerID, "cat", path).CombinedOutput()
		content := strings.TrimSpace(string(out))
		if err == nil && strings.Contains(content, token) {
			t.Logf("proof file %s contains token (content: %q)", path, content)
			return
		}
		if time.Now().After(deadline) {
			// Provide diagnostic output: what's on the screen?
			pane, _ := exec.CommandContext(ctx,
				"docker", "exec", containerID,
				"tmux", "capture-pane", "-p", "-t", "spawn",
			).CombinedOutput()
			t.Fatalf(
				"proof file %s not found or missing token %q after %v\ncontainer: %s\ncat output: %q (err: %v)\ntmux pane:\n%s",
				path, token, timeout, containerID, content, err, strings.TrimSpace(string(pane)),
			)
		}
		if ctx.Err() != nil {
			t.Fatalf("context cancelled waiting for proof file: %v", ctx.Err())
		}
		time.Sleep(500 * time.Millisecond)
	}
}

// Ensure runtime is imported for label constants used by findAgentContainer (defined in datafs_perms_e2e_test.go).
var _ = runtime.LabelSpawnID
