package piadapter

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"sync"

	"spawnery/internal/acp"
)

// launchFunc creates a pi --mode rpc subprocess and returns its stdin/stdout.
// The adapter owns the process for the life of the connection.
type launchFunc func(model, dir string) (piIn io.WriteCloser, piOut io.ReadCloser, err error)

// realLaunch returns a launchFunc that runs the real pi binary.
func realLaunch(bin string) launchFunc {
	return func(model, dir string) (io.WriteCloser, io.ReadCloser, error) {
		args := []string{"--mode", "rpc", "--provider", "spawnery", "--approve"}
		if model != "" {
			args = append(args, "--model", "spawnery/"+model)
		}
		cmd := exec.Command(bin, args...)
		if dir != "" {
			cmd.Dir = dir
		}
		stdin, err := cmd.StdinPipe()
		if err != nil {
			return nil, nil, fmt.Errorf("pi stdin pipe: %w", err)
		}
		stdout, err := cmd.StdoutPipe()
		if err != nil {
			stdin.Close()
			return nil, nil, fmt.Errorf("pi stdout pipe: %w", err)
		}
		if err := cmd.Start(); err != nil {
			stdin.Close()
			stdout.Close()
			return nil, nil, fmt.Errorf("start pi: %w", err)
		}
		return stdin, stdout, nil
	}
}

// Option configures an Adapter.
type Option func(*Adapter)

// WithBinary overrides the pi binary path/name (default "pi"). Used in tests
// and for explicit binary paths (e.g. /usr/local/bin/pi).
func WithBinary(bin string) Option {
	return func(a *Adapter) { a.launch = realLaunch(bin) }
}

// withLaunch injects a custom launch function (used by tests).
func withLaunch(fn launchFunc) Option {
	return func(a *Adapter) { a.launch = fn }
}

// Adapter presents a canonical-ACP agent to the node, backed by a
// `pi --mode rpc` child process the adapter owns over stdio. One Adapter
// serves one node connection; the connection maps to a single pi process.
// Mirrors internal/ocadapter.Adapter but for a subprocess rather than HTTP.
type Adapter struct {
	model  string
	dir    string
	launch launchFunc

	mu          sync.Mutex
	piIn        io.WriteCloser // pi's stdin (write commands here)
	inflightID  int
	hasInflight bool
	turnErr     *PiError
	turnUsage   *PiUsage
	sentTool   map[string]bool            // toolCallId -> true once tool_call creation emitted
	toolInputs map[string]json.RawMessage  // toolCallId -> rawInput stored at start
}

// New creates an Adapter that will spawn the pi binary (default "pi") to serve
// ACP sessions. model is the "spawnery/<id>" model passed to pi (optional).
// dir is the working directory for pi (defaults to "/app").
func New(model, dir string, opts ...Option) *Adapter {
	if dir == "" {
		dir = "/app"
	}
	a := &Adapter{
		model:      model,
		dir:        dir,
		sentTool:   map[string]bool{},
		toolInputs: map[string]json.RawMessage{},
	}
	a.launch = realLaunch("pi")
	for _, o := range opts {
		o(a)
	}
	return a
}

// Serve runs the ACP agent loop on the given node connection until it ends. It
// starts the pi subprocess, pumps its stdout into ACP notifications, and
// processes incoming node requests.
func (a *Adapter) Serve(r io.Reader, w io.Writer) error {
	srv := acp.NewServer(r, w)

	piIn, piOut, err := a.launch(a.model, a.dir)
	if err != nil {
		return fmt.Errorf("launch pi: %w", err)
	}
	defer piIn.Close()

	a.mu.Lock()
	a.piIn = piIn
	a.mu.Unlock()

	go a.pumpLoop(srv, piOut)

	for {
		m, err := srv.Read()
		if err != nil {
			return err
		}
		switch m.Method {
		case "initialize":
			a.handleInitialize(srv, m)
		case "session/new":
			a.handleNewSession(srv, m)
		case "session/prompt":
			a.handlePrompt(srv, m)
		case "session/cancel", "cancel":
			a.sendAbort()
		}
	}
}

func (a *Adapter) handleInitialize(srv *acp.Server, m acp.Message) {
	id, ok := m.ID.AsInt()
	if !ok {
		return
	}
	_ = srv.Respond(id, map[string]any{"protocolVersion": 1, "agentCapabilities": map[string]any{}})
}

// sessionID is a static identifier returned on session/new. pi --mode rpc does
// not have a session-management layer; the process IS the session.
const sessionID = "pi-session-1"

func (a *Adapter) handleNewSession(srv *acp.Server, m acp.Message) {
	id, ok := m.ID.AsInt()
	if !ok {
		return
	}
	_ = srv.Respond(id, map[string]any{"sessionId": sessionID})
}

func (a *Adapter) handlePrompt(srv *acp.Server, m acp.Message) {
	id, ok := m.ID.AsInt()
	if !ok {
		return
	}
	text := extractPromptText(m.Params)
	a.mu.Lock()
	a.inflightID = id
	a.hasInflight = true
	a.turnErr = nil
	a.turnUsage = nil
	piIn := a.piIn
	a.mu.Unlock()

	if err := a.writeCmd(piIn, PromptCommand(text)); err != nil {
		a.mu.Lock()
		a.hasInflight = false
		a.mu.Unlock()
		_ = srv.RespondError(id, -32000, err.Error())
	}
	// Turn-end response is sent when agent_end arrives on pi's stdout (pumpLoop).
}

func (a *Adapter) sendAbort() {
	a.mu.Lock()
	piIn := a.piIn
	a.mu.Unlock()
	if piIn != nil {
		_ = a.writeCmd(piIn, AbortCommand())
	}
}

// writeCmd serializes a command and writes it as a single LF-terminated line.
func (a *Adapter) writeCmd(w io.Writer, cmd any) error {
	b, err := json.Marshal(cmd)
	if err != nil {
		return err
	}
	b = append(b, '\n')
	_, err = w.Write(b)
	return err
}

// pumpLoop reads pi's stdout line-by-line (LF-only, as per rpc.md) and
// translates events into ACP traffic. It runs until pi's stdout closes.
func (a *Adapter) pumpLoop(srv *acp.Server, piOut io.ReadCloser) {
	defer piOut.Close()

	sc := bufio.NewScanner(piOut)
	sc.Split(bufio.ScanLines) // LF-only split; never splits on U+2028/U+2029

	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var e Event
		if err := json.Unmarshal(line, &e); err != nil {
			continue // skip garbled lines; pi may add new event types
		}
		a.onEvent(srv, e)
	}

	// pi's stdout closed (process exited or pipe broken). End any in-flight turn
	// with a structured error so the node doesn't hang waiting for a response.
	a.mu.Lock()
	id := a.inflightID
	has := a.hasInflight
	a.hasInflight = false
	a.mu.Unlock()
	if has {
		_ = srv.Respond(id, map[string]any{
			"stopReason": "end_turn",
			"error":      &ACPError{Message: "pi process exited unexpectedly"},
		})
	}
}

// onEvent translates one pi event into ACP traffic.
func (a *Adapter) onEvent(srv *acp.Server, e Event) {
	switch e.Type {
	case "message_update":
		kind := "agent_message_chunk"
		if ch := strings.ToLower(e.Channel); ch == "thinking" || ch == "reasoning" {
			kind = "agent_thought_chunk"
		}
		if e.Delta != "" {
			_ = srv.Notify("session/update", ACPSessionUpdate(sessionID, kind, e.Delta))
		}

	case "tool_execution_start":
		callID := e.ToolCallID
		if callID == "" {
			return
		}
		a.mu.Lock()
		first := !a.sentTool[callID]
		a.sentTool[callID] = true
		if len(e.Input) > 0 {
			a.toolInputs[callID] = e.Input
		}
		a.mu.Unlock()
		if first {
			_ = srv.Notify("session/update", ACPToolCall(sessionID, callID, e.Name, ToolKind(e.Name)))
		}

	case "tool_execution_update":
		// pi may emit partial progress; emit an in_progress update.
		callID := e.ToolCallID
		if callID == "" {
			return
		}
		u := ACPToolUpdateParams{
			SessionID: sessionID,
			Update: ACPToolUpdate{
				SessionUpdate: "tool_call_update",
				ToolCallID:    callID,
				Status:        "in_progress",
			},
		}
		_ = srv.Notify("session/update", u)

	case "tool_execution_end":
		callID := e.ToolCallID
		if callID == "" {
			return
		}
		// Retrieve cached rawInput for this callID (stored at start).
		a.mu.Lock()
		rawIn := a.toolInputs[callID]
		delete(a.toolInputs, callID)
		a.mu.Unlock()
		_ = srv.Notify("session/update", ACPToolCallUpdate(sessionID, callID, e.Output, e.IsError, rawIn))

	case "turn_end":
		a.mu.Lock()
		a.turnUsage = e.Usage
		a.mu.Unlock()

	case "extension_error":
		a.mu.Lock()
		a.turnErr = &PiError{Name: e.Name, Message: e.Message}
		a.mu.Unlock()

	case "agent_end":
		// Ready signal: this turn is complete; respond to the pending prompt.
		a.mu.Lock()
		id := a.inflightID
		has := a.hasInflight
		a.hasInflight = false
		te := a.turnErr
		a.turnErr = nil
		usage := PiUsageToACP(a.turnUsage)
		a.turnUsage = nil
		a.mu.Unlock()
		if has {
			stop, ei := StopReasonForError(te)
			resp := map[string]any{"stopReason": stop}
			if ei != nil {
				resp["error"] = ei
			}
			if usage != nil {
				resp["usage"] = usage
			}
			_ = srv.Respond(id, resp)
		}
	}
}

// extractPromptText concatenates the text content blocks of an ACP session/prompt.
func extractPromptText(params json.RawMessage) string {
	var p struct {
		Prompt []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"prompt"`
	}
	_ = json.Unmarshal(params, &p)
	var sb strings.Builder
	for _, b := range p.Prompt {
		if b.Type == "text" {
			sb.WriteString(b.Text)
		}
	}
	return sb.String()
}
