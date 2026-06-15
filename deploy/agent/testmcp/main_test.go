// Package main is a minimal stdlib-only stdio MCP server used as a test fixture.
// This test file exercises serve() in-process over in-memory pipes.
package main

import (
	"bufio"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ----------- test wire helpers -----------------------------------------------

// sendRPC writes a JSON-RPC message to w as a newline-terminated JSON line.
// This matches MCP stdio transport (one JSON object per line).
func sendRPC(t *testing.T, w io.Writer, msg interface{}) {
	t.Helper()
	b, err := json.Marshal(msg)
	if err != nil {
		t.Fatalf("sendRPC marshal: %v", err)
	}
	b = append(b, '\n')
	if _, err := w.Write(b); err != nil {
		t.Fatalf("sendRPC write: %v", err)
	}
}

// recvRPC reads one newline-delimited JSON-RPC message from br.
func recvRPC(t *testing.T, br *bufio.Reader) map[string]interface{} {
	t.Helper()
	msg, err := readMessage(br)
	if err != nil {
		t.Fatalf("recvRPC: %v", err)
	}
	return msg
}

// ----------- tests -----------------------------------------------------------

func TestServe_InitializeAndToolsListAndToolsCall(t *testing.T) {
	// Create in-memory pipe pairs:
	//   clientW -> serverR (server reads from clientW via serverR)
	//   serverW -> clientR (client reads from serverW via clientR)
	serverR, clientW := io.Pipe()
	clientR, serverW := io.Pipe()

	// Wrap the client-read side in a bufio.Reader (shared across the test
	// so buffered data is never discarded between reads).
	clientBR := bufio.NewReader(clientR)

	// Configure the server via env vars.
	proofPath := filepath.Join(t.TempDir(), "proof.txt")
	token := "TEST-TOKEN-QUOKKA"
	t.Setenv("SPAWNERY_MCP_PROOF_FILE", proofPath)
	t.Setenv("SPAWNERY_TEST_MCP_TOKEN", token)

	// Run serve in the background; it exits when serverR is closed.
	done := make(chan error, 1)
	go func() {
		done <- serve(serverR, serverW)
	}()

	// 1. initialize → server must respond with protocolVersion + serverInfo.
	sendRPC(t, clientW, map[string]interface{}{
		"jsonrpc": "2.0",
		"method":  "initialize",
		"params": map[string]interface{}{
			"protocolVersion": "2024-11-05",
			"capabilities":    map[string]interface{}{},
			"clientInfo":      map[string]interface{}{"name": "test", "version": "0"},
		},
		"id": 1,
	})
	resp := recvRPC(t, clientBR)
	if resp["id"].(float64) != 1 {
		t.Fatalf("initialize response id: got %v", resp["id"])
	}
	if resp["error"] != nil {
		t.Fatalf("initialize error: %v", resp["error"])
	}
	result, ok := resp["result"].(map[string]interface{})
	if !ok {
		t.Fatalf("initialize result missing or wrong type")
	}
	if result["protocolVersion"] == nil {
		t.Error("protocolVersion missing in initialize result")
	}
	if result["serverInfo"] == nil {
		t.Error("serverInfo missing in initialize result")
	}

	// 2. Send initialized notification (no response expected from server).
	sendRPC(t, clientW, map[string]interface{}{
		"jsonrpc": "2.0",
		"method":  "notifications/initialized",
		"params":  map[string]interface{}{},
	})

	// 3. tools/list → must advertise the "record_proof" tool.
	sendRPC(t, clientW, map[string]interface{}{
		"jsonrpc": "2.0",
		"method":  "tools/list",
		"params":  map[string]interface{}{},
		"id":      2,
	})
	resp = recvRPC(t, clientBR)
	if resp["id"].(float64) != 2 {
		t.Fatalf("tools/list response id: got %v", resp["id"])
	}
	if resp["error"] != nil {
		t.Fatalf("tools/list error: %v", resp["error"])
	}
	listResult, ok := resp["result"].(map[string]interface{})
	if !ok {
		t.Fatalf("tools/list result not a map")
	}
	tools, ok := listResult["tools"].([]interface{})
	if !ok || len(tools) == 0 {
		t.Fatalf("tools/list: no tools returned")
	}
	// (b) tools/list advertises record_proof.
	found := false
	for _, tl := range tools {
		tm, ok := tl.(map[string]interface{})
		if ok && tm["name"] == "record_proof" {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("tools/list: record_proof not found in %v", tools)
	}

	// 4. tools/call record_proof → marker file written with token; result text echoes token.
	sendRPC(t, clientW, map[string]interface{}{
		"jsonrpc": "2.0",
		"method":  "tools/call",
		"params": map[string]interface{}{
			"name":      "record_proof",
			"arguments": map[string]interface{}{},
		},
		"id": 3,
	})
	resp = recvRPC(t, clientBR)
	if resp["id"].(float64) != 3 {
		t.Fatalf("tools/call response id: got %v", resp["id"])
	}
	if resp["error"] != nil {
		t.Fatalf("tools/call error: %v", resp["error"])
	}
	callResult, ok := resp["result"].(map[string]interface{})
	if !ok {
		t.Fatalf("tools/call result not a map")
	}
	content, ok := callResult["content"].([]interface{})
	if !ok || len(content) == 0 {
		t.Fatalf("tools/call: no content in result")
	}
	cm, ok := content[0].(map[string]interface{})
	if !ok {
		t.Fatalf("tools/call: content[0] not a map")
	}
	if cm["type"] != "text" {
		t.Errorf("tools/call: content[0].type = %v, want text", cm["type"])
	}
	// (d) Tool result text echoes the token.
	text, _ := cm["text"].(string)
	if !strings.Contains(text, token) {
		t.Errorf("tools/call: result text %q does not contain token %q", text, token)
	}

	// (c) Marker file must contain the token.
	data, err := os.ReadFile(proofPath)
	if err != nil {
		t.Fatalf("proof file not written: %v", err)
	}
	if !strings.Contains(string(data), token) {
		t.Errorf("proof file content %q does not contain token %q", string(data), token)
	}

	// Done — close the client write-side so serve exits cleanly.
	clientW.Close()
	if err := <-done; err != nil && err != io.EOF {
		t.Errorf("serve returned unexpected error: %v", err)
	}
}

// TestServe_UnknownMethod: an unknown method gets a method-not-found error response.
func TestServe_UnknownMethod(t *testing.T) {
	serverR, clientW := io.Pipe()
	clientR, serverW := io.Pipe()
	clientBR := bufio.NewReader(clientR)

	t.Setenv("SPAWNERY_MCP_PROOF_FILE", filepath.Join(t.TempDir(), "proof.txt"))
	t.Setenv("SPAWNERY_TEST_MCP_TOKEN", "tok")

	done := make(chan error, 1)
	go func() { done <- serve(serverR, serverW) }()

	// Initialize first (required by protocol).
	sendRPC(t, clientW, map[string]interface{}{
		"jsonrpc": "2.0", "method": "initialize",
		"params": map[string]interface{}{
			"protocolVersion": "2024-11-05",
			"capabilities":    map[string]interface{}{},
			"clientInfo":      map[string]interface{}{"name": "t", "version": "0"},
		},
		"id": 1,
	})
	recvRPC(t, clientBR) // consume initialize response

	sendRPC(t, clientW, map[string]interface{}{
		"jsonrpc": "2.0", "method": "no/such/method", "params": map[string]interface{}{}, "id": 42,
	})
	resp := recvRPC(t, clientBR)
	if resp["id"].(float64) != 42 {
		t.Fatalf("unknown method response id: got %v", resp["id"])
	}
	if resp["error"] == nil {
		t.Fatal("expected error for unknown method, got nil")
	}

	clientW.Close()
	<-done
}
