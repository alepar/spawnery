package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"

	"spawnery/internal/execstream"
)

// runExec runs cmd non-interactively in spawn's agent container via the node's streaming /exec
// endpoint, copying stdout/stderr to the given writers as they arrive and returning the inner
// command's exit code. err is non-nil only for a transport/node failure (unreachable node, non-200
// before streaming, a node error frame, or a truncated stream) — a non-zero command exit is reported
// via the returned code, not as an error.
func runExec(addr, spawn string, cmd []string, stdout, stderr io.Writer) (int, error) {
	body, err := json.Marshal(map[string]any{"cmd": cmd})
	if err != nil {
		return 1, err
	}
	endpoint := addr + "/exec?spawn=" + url.QueryEscape(spawn)
	resp, err := http.Post(endpoint, "application/json", bytes.NewReader(body))
	if err != nil {
		return 1, fmt.Errorf("contacting node: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return 1, fmt.Errorf("node returned %s: %s", resp.Status, bytes.TrimSpace(b))
	}
	return execstream.Demux(resp.Body, stdout, stderr)
}
