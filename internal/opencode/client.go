// Package opencode is a thin REST/SSE client for an `opencode serve` instance.
// It targets the shapes pinned in docs/superpowers/notes/opencode-api-pinned.md
// (opencode 1.15.13). The Go SDK lags the server, so we speak raw HTTP.
package opencode

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// Client talks to one opencode server over HTTP + SSE.
type Client struct {
	base     string
	hc       *http.Client
	healthHC *http.Client // short timeout so readiness polling stays responsive
}

// New returns a client for the server at baseURL (e.g. http://127.0.0.1:4096).
func New(baseURL string) *Client {
	return &Client{
		base:     strings.TrimRight(baseURL, "/"),
		hc:       &http.Client{Timeout: 30 * time.Second},
		healthHC: &http.Client{Timeout: 3 * time.Second},
	}
}

// Session is the subset of an opencode session object we use.
type Session struct {
	ID        string `json:"id"`
	Directory string `json:"directory"`
	Title     string `json:"title"`
}

// Health returns nil if the server reports healthy.
func (c *Client) Health() error {
	resp, err := c.healthHC.Get(c.base + "/global/health")
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("opencode health: %s", resp.Status)
	}
	var h struct {
		Healthy bool `json:"healthy"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&h); err != nil {
		return err
	}
	if !h.Healthy {
		return fmt.Errorf("opencode reports unhealthy")
	}
	return nil
}

// ListSessions returns all sessions known to the server (across directories).
func (c *Client) ListSessions() ([]Session, error) {
	resp, err := c.hc.Get(c.base + "/session")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var ss []Session
	if err := json.NewDecoder(resp.Body).Decode(&ss); err != nil {
		return nil, err
	}
	return ss, nil
}

// CreateSession creates a new session (the server roots it at its own cwd).
func (c *Client) CreateSession(title string) (Session, error) {
	body, _ := json.Marshal(map[string]any{"title": title})
	resp, err := c.hc.Post(c.base+"/session", "application/json", bytes.NewReader(body))
	if err != nil {
		return Session{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return Session{}, fmt.Errorf("create session: %s", resp.Status)
	}
	var s Session
	if err := json.NewDecoder(resp.Body).Decode(&s); err != nil {
		return Session{}, err
	}
	return s, nil
}

// DiscoverOrCreateSession reuses the first existing session rooted at dir (so a
// resumed spawn rejoins its restored session), else creates one. dir scopes the
// search because GET /session returns sessions across all directories.
func (c *Client) DiscoverOrCreateSession(dir, title string) (string, error) {
	ss, err := c.ListSessions()
	if err != nil {
		return "", err
	}
	for _, s := range ss {
		if dir == "" || s.Directory == dir {
			return s.ID, nil
		}
	}
	s, err := c.CreateSession(title)
	if err != nil {
		return "", err
	}
	return s.ID, nil
}

// Command is the subset of an opencode command object we use (GET /command, opencode 1.15.13). A
// command is a reusable slash-command/skill/MCP prompt: name + description, an optional template, and
// argument `hints` (e.g. ["$ARGUMENTS"]) surfaced as the ACP input hint. source is command|mcp|skill.
type Command struct {
	Name        string   `json:"name"`
	Description string   `json:"description"`
	Hints       []string `json:"hints"`
	Source      string   `json:"source"`
}

// ListCommands returns the slash commands the server advertises (GET /command). Used to surface
// opencode's command set to the web as an ACP available_commands_update right after session start.
func (c *Client) ListCommands() ([]Command, error) {
	resp, err := c.hc.Get(c.base + "/command")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("list commands: %s", resp.Status)
	}
	var cmds []Command
	if err := json.NewDecoder(resp.Body).Decode(&cmds); err != nil {
		return nil, err
	}
	return cmds, nil
}

// PromptAsync sends a text prompt without waiting; results arrive via SSE.
// model is "providerID/modelID"; if empty, opencode uses its configured default.
func (c *Client) PromptAsync(sessionID, text, model string) error {
	parts := []map[string]any{{"type": "text", "text": text}}
	payload := map[string]any{"parts": parts}
	if providerID, modelID, ok := splitModel(model); ok {
		payload["model"] = map[string]any{"providerID": providerID, "modelID": modelID}
	}
	body, _ := json.Marshal(payload)
	resp, err := c.hc.Post(c.base+"/session/"+sessionID+"/prompt_async", "application/json", bytes.NewReader(body))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("prompt_async: %s", resp.Status)
	}
	return nil
}

// Abort cancels the in-flight turn for a session.
func (c *Client) Abort(sessionID string) error {
	resp, err := c.hc.Post(c.base+"/session/"+sessionID+"/abort", "application/json", nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return nil
}

// RespondPermission answers a permission request: response is once|always|reject.
func (c *Client) RespondPermission(sessionID, permissionID, response string) error {
	body, _ := json.Marshal(map[string]any{"response": response})
	resp, err := c.hc.Post(c.base+"/session/"+sessionID+"/permissions/"+permissionID, "application/json", bytes.NewReader(body))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("respond permission: %s", resp.Status)
	}
	return nil
}

// RawEvent is one decoded SSE event from /event. The full event is
// {"type":"<name>","properties":{...}}; Properties holds the raw payload.
type RawEvent struct {
	Type       string          `json:"type"`
	Properties json.RawMessage `json:"properties"`
}

// Events subscribes to /event and calls fn for each event until the stream ends
// or ctx is done. It does NOT auto-reconnect; the caller wraps it with backoff.
func (c *Client) Events(ctx context.Context, fn func(RawEvent)) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+"/event", nil)
	if err != nil {
		return err
	}
	resp, err := (&http.Client{}).Do(req) // no timeout for a long-lived stream
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for sc.Scan() {
		line := sc.Text()
		data, ok := strings.CutPrefix(line, "data: ")
		if !ok {
			continue
		}
		var ev RawEvent
		if err := json.Unmarshal([]byte(data), &ev); err != nil {
			continue
		}
		fn(ev)
	}
	return sc.Err()
}

func splitModel(model string) (providerID, modelID string, ok bool) {
	if i := strings.Index(model, "/"); i > 0 && i < len(model)-1 {
		return model[:i], model[i+1:], true
	}
	return "", "", false
}
