// testmcp is a minimal stdlib-only stdio MCP server used as a test fixture for
// the spawnery profiles e2e (profile_mcp_loadproof_e2e_test.go). It exposes one
// tool — record_proof — which writes the value of $SPAWNERY_TEST_MCP_TOKEN to
// the path in $SPAWNERY_MCP_PROOF_FILE (default /tmp/spawnery-mcp-proof) and
// returns the token in the tool result text.
//
// This binary is baked into spawnery/agent:dev so the e2e can reference a
// stdio MCP server that is guaranteed to be present in the pod.
package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
)

func main() {
	if err := serve(os.Stdin, os.Stdout); err != nil && err != io.EOF {
		fmt.Fprintf(os.Stderr, "testmcp: %v\n", err)
		os.Exit(1)
	}
}

// serve is the testable core: reads JSON-RPC messages from r and writes
// responses to w using Content-Length framing (MCP stdio transport wire format,
// identical to LSP).
func serve(r io.Reader, w io.Writer) error {
	br := bufio.NewReader(r)
	for {
		msg, err := readMessage(br)
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return fmt.Errorf("read: %w", err)
		}
		if err := dispatch(msg, w); err != nil {
			return fmt.Errorf("dispatch: %w", err)
		}
	}
}

// ----------- wire transport --------------------------------------------------

// readMessage reads one Content-Length framed JSON message from br.
func readMessage(br *bufio.Reader) (map[string]interface{}, error) {
	var contentLen int
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			return nil, err
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			break
		}
		if strings.HasPrefix(line, "Content-Length:") {
			n, _ := strconv.Atoi(strings.TrimSpace(line[len("Content-Length:"):]))
			contentLen = n
		}
	}
	if contentLen == 0 {
		return nil, fmt.Errorf("missing or zero Content-Length")
	}
	body := make([]byte, contentLen)
	if _, err := io.ReadFull(br, body); err != nil {
		return nil, fmt.Errorf("read body: %w", err)
	}
	var msg map[string]interface{}
	if err := json.Unmarshal(body, &msg); err != nil {
		return nil, fmt.Errorf("unmarshal: %w", err)
	}
	return msg, nil
}

// writeMessage marshals msg to JSON and sends it with Content-Length framing.
func writeMessage(w io.Writer, msg interface{}) error {
	b, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(w, "Content-Length: %d\r\n\r\n%s", len(b), b)
	return err
}

// ----------- dispatch ---------------------------------------------------------

func dispatch(msg map[string]interface{}, w io.Writer) error {
	method, _ := msg["method"].(string)
	id := msg["id"] // nil for notifications

	// Notifications (no id) — no response required.
	if id == nil {
		return nil
	}

	switch method {
	case "initialize":
		return writeMessage(w, map[string]interface{}{
			"jsonrpc": "2.0",
			"id":      id,
			"result": map[string]interface{}{
				"protocolVersion": "2024-11-05",
				"capabilities":    map[string]interface{}{},
				"serverInfo": map[string]interface{}{
					"name":    "spawnery-test-mcp",
					"version": "0.1.0",
				},
				"instructions": "Spawnery test fixture MCP server. Use record_proof to record a proof token.",
			},
		})

	case "tools/list":
		return writeMessage(w, map[string]interface{}{
			"jsonrpc": "2.0",
			"id":      id,
			"result": map[string]interface{}{
				"tools": []interface{}{
					map[string]interface{}{
						"name":        "record_proof",
						"description": "Writes the SPAWNERY_TEST_MCP_TOKEN env var value to the SPAWNERY_MCP_PROOF_FILE and returns it as text.",
						"inputSchema": map[string]interface{}{
							"type":       "object",
							"properties": map[string]interface{}{},
						},
					},
				},
			},
		})

	case "tools/call":
		params, _ := msg["params"].(map[string]interface{})
		toolName, _ := params["name"].(string)
		if toolName != "record_proof" {
			return writeMessage(w, errResponse(id, -32601, fmt.Sprintf("unknown tool: %s", toolName)))
		}
		token := os.Getenv("SPAWNERY_TEST_MCP_TOKEN")
		proofFile := os.Getenv("SPAWNERY_MCP_PROOF_FILE")
		if proofFile == "" {
			proofFile = "/tmp/spawnery-mcp-proof"
		}
		if err := os.WriteFile(proofFile, []byte(token), 0o644); err != nil {
			return writeMessage(w, errResponse(id, -32000, fmt.Sprintf("write proof file: %v", err)))
		}
		return writeMessage(w, map[string]interface{}{
			"jsonrpc": "2.0",
			"id":      id,
			"result": map[string]interface{}{
				"content": []interface{}{
					map[string]interface{}{
						"type": "text",
						"text": token,
					},
				},
			},
		})

	default:
		return writeMessage(w, errResponse(id, -32601, fmt.Sprintf("method not found: %s", method)))
	}
}

func errResponse(id interface{}, code int, msg string) map[string]interface{} {
	return map[string]interface{}{
		"jsonrpc": "2.0",
		"id":      id,
		"error": map[string]interface{}{
			"code":    code,
			"message": msg,
		},
	}
}
