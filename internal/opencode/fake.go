package opencode

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
)

// Fake is an in-process opencode server emulating the pinned 1.15.13 endpoints
// the adapter uses. It is the opencode analogue of internal/stubagent and is
// shared by the opencode and ocadapter tests.
type Fake struct {
	*httptest.Server

	mu       sync.Mutex
	dir      string
	sessions []Session
	nextSess int
	subs     map[chan string]struct{}

	perms       []PermResponse // recorded permission answers
	aborts      []string       // session IDs aborted
	pendingErr  string         // if set, emitted as a session.error before idle by the next scriptPrompt
	pendingStep []stepScript   // step-finish parts emitted before idle by the next scriptPrompt (cat D)
	commands    []Command      // advertised slash commands returned by GET /command (cat E)
}

// stepScript is one scripted step-finish part (per-turn token usage + cost) to emit before idle.
type stepScript struct {
	partID string
	cost   float64
	tokens string // raw tokens JSON object literal
}

// PermResponse records a permission answer the adapter POSTed.
type PermResponse struct {
	SessionID    string
	PermissionID string
	Response     string
}

// NewFake starts a fake opencode server rooting created sessions at dir
// (default "/app"). Call Close when done.
func NewFake(dir string) *Fake {
	if dir == "" {
		dir = "/app"
	}
	f := &Fake{dir: dir, subs: map[chan string]struct{}{}}
	mux := http.NewServeMux()

	mux.HandleFunc("/global/health", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"healthy": true, "version": "fake"})
	})

	mux.HandleFunc("/session", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		if r.Method == http.MethodPost {
			f.nextSess++
			s := Session{ID: fmt.Sprintf("ses_fake%d", f.nextSess), Directory: f.dir, Title: "t"}
			f.sessions = append(f.sessions, s)
			_ = json.NewEncoder(w).Encode(s)
			return
		}
		_ = json.NewEncoder(w).Encode(f.sessions)
	})

	// /command: the advertised slash-command list (cat E). Empty by default; SetCommands seeds it.
	mux.HandleFunc("/command", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		cmds := append([]Command(nil), f.commands...)
		f.mu.Unlock()
		_ = json.NewEncoder(w).Encode(cmds)
	})

	// /event SSE: register a subscriber channel; stream until the request ends.
	mux.HandleFunc("/event", func(w http.ResponseWriter, r *http.Request) {
		flusher, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "no flush", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		ch := make(chan string, 64)
		f.mu.Lock()
		f.subs[ch] = struct{}{}
		f.mu.Unlock()
		defer func() {
			f.mu.Lock()
			delete(f.subs, ch)
			f.mu.Unlock()
		}()
		// initial connected event
		fmt.Fprintf(w, "data: %s\n\n", `{"type":"server.connected","properties":{}}`)
		flusher.Flush()
		for {
			select {
			case <-r.Context().Done():
				return
			case msg := <-ch:
				fmt.Fprintf(w, "data: %s\n\n", msg)
				flusher.Flush()
			}
		}
	})

	// prompt_async: 204, then asynchronously emit a scripted streaming sequence.
	mux.HandleFunc("/session/", func(w http.ResponseWriter, r *http.Request) {
		switch {
		case hasSuffix(r.URL.Path, "/prompt_async") && r.Method == http.MethodPost:
			sid := pathSeg(r.URL.Path, 1)
			w.WriteHeader(http.StatusNoContent)
			go f.scriptPrompt(sid)
		case hasSuffix(r.URL.Path, "/abort") && r.Method == http.MethodPost:
			f.mu.Lock()
			f.aborts = append(f.aborts, pathSeg(r.URL.Path, 1))
			f.mu.Unlock()
			w.WriteHeader(http.StatusOK)
		case contains(r.URL.Path, "/permissions/") && r.Method == http.MethodPost:
			var body struct {
				Response string `json:"response"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			f.mu.Lock()
			f.perms = append(f.perms, PermResponse{
				SessionID:    pathSeg(r.URL.Path, 1),
				PermissionID: lastSeg(r.URL.Path),
				Response:     body.Response,
			})
			f.mu.Unlock()
			w.WriteHeader(http.StatusOK)
		case hasSuffix(r.URL.Path, "/message") && r.Method == http.MethodGet:
			_ = json.NewEncoder(w).Encode([]any{}) // backfill: empty
		default:
			w.WriteHeader(http.StatusOK)
		}
	})

	f.Server = httptest.NewServer(mux)
	return f
}

// Emit broadcasts one event (type + raw properties JSON) to all /event subscribers.
func (f *Fake) Emit(eventType, propsJSON string) {
	msg := fmt.Sprintf(`{"type":%q,"properties":%s}`, eventType, propsJSON)
	f.mu.Lock()
	for ch := range f.subs {
		select {
		case ch <- msg:
		default:
		}
	}
	f.mu.Unlock()
}

// scriptPrompt emits a text part snapshot, a streamed delta, an optional scripted session.error, then idle.
func (f *Fake) scriptPrompt(sid string) {
	f.Emit("message.part.updated", fmt.Sprintf(`{"sessionID":%q,"part":{"id":"prt_1","type":"text","text":""},"time":1}`, sid))
	f.Emit("message.part.delta", fmt.Sprintf(`{"sessionID":%q,"messageID":"msg_1","partID":"prt_1","field":"text","delta":"hi"}`, sid))
	f.mu.Lock()
	pe := f.pendingErr
	f.pendingErr = ""
	steps := f.pendingStep
	f.pendingStep = nil
	f.mu.Unlock()
	// Per-turn step-finish parts (token usage + cost) ride before idle so the adapter accumulates them.
	for _, s := range steps {
		f.Emit("message.part.updated", fmt.Sprintf(
			`{"sessionID":%q,"part":{"id":%q,"messageID":"msg_1","type":"step-finish","cost":%v,"tokens":%s}}`,
			sid, s.partID, s.cost, s.tokens))
	}
	if pe != "" {
		f.Emit("session.error", fmt.Sprintf(`{"sessionID":%q,"error":%s}`, sid, pe))
	}
	f.Emit("session.idle", fmt.Sprintf(`{"sessionID":%q}`, sid))
}

// ScriptTurnError makes the NEXT prompt's turn emit the given NamedError (raw JSON, e.g.
// `{"name":"MessageAbortedError","data":{}}`) as a session.error just before idle.
func (f *Fake) ScriptTurnError(errJSON string) {
	f.mu.Lock()
	f.pendingErr = errJSON
	f.mu.Unlock()
}

// ScriptStepFinish queues a step-finish part (per-step token usage + cost) to be emitted by the NEXT
// prompt's turn just before idle, so the adapter accumulates per-turn usage (cat D). Call it more than
// once to script a multi-step turn. tokensJSON is a raw token object literal, e.g.
// `{"input":10,"output":5,"reasoning":2,"cache":{"read":3,"write":0}}`.
func (f *Fake) ScriptStepFinish(partID string, cost float64, tokensJSON string) {
	f.mu.Lock()
	f.pendingStep = append(f.pendingStep, stepScript{partID: partID, cost: cost, tokens: tokensJSON})
	f.mu.Unlock()
}

// EmitUserMessage emits the events opencode produces for a user message: message.updated (role)
// then message.part.updated (the text part). Used to simulate a prompt typed in the in-pod TUI.
func (f *Fake) EmitUserMessage(sid, msgID, partID, text string) {
	f.Emit("message.updated", fmt.Sprintf(`{"sessionID":%q,"info":{"id":%q,"role":"user"}}`, sid, msgID))
	f.Emit("message.part.updated", fmt.Sprintf(`{"sessionID":%q,"part":{"id":%q,"messageID":%q,"type":"text","text":%q}}`, sid, partID, msgID, text))
}

// EmitToolPart emits a message.part.updated carrying a ToolPart in the given state. input/output are
// raw fragments (input is a JSON object literal, output a plain string); output is omitted unless set.
func (f *Fake) EmitToolPart(sid, callID, tool, status, input, output string) {
	state := fmt.Sprintf(`{"status":%q,"input":%s`, status, input)
	if output != "" {
		state += fmt.Sprintf(`,"output":%q`, output)
	}
	state += "}"
	f.Emit("message.part.updated", fmt.Sprintf(
		`{"sessionID":%q,"part":{"id":"prt_tool","messageID":"msg_1","type":"tool","callID":%q,"tool":%q,"state":%s}}`,
		sid, callID, tool, state))
}

// EmitTodoUpdated emits a todo.updated event carrying the FULL current todo list for a session.
// todosJSON is a raw JSON array literal of opencode todos (e.g.
// `[{"id":"1","content":"do x","status":"pending","priority":"high"}]`).
func (f *Fake) EmitTodoUpdated(sid, todosJSON string) {
	f.Emit("todo.updated", fmt.Sprintf(`{"sessionID":%q,"todos":%s}`, sid, todosJSON))
}

// EmitPermissionAsked emits a permission.asked event for a session.
func (f *Fake) EmitPermissionAsked(sid, permID string) {
	f.Emit("permission.asked", fmt.Sprintf(`{"id":%q,"sessionID":%q,"permission":"bash","patterns":[],"metadata":{},"always":[],"tool":{"messageID":"msg_1","callID":"c1"}}`, permID, sid))
}

// SetCommands seeds the slash-command list returned by GET /command (cat E).
func (f *Fake) SetCommands(cmds []Command) {
	f.mu.Lock()
	f.commands = cmds
	f.mu.Unlock()
}

// PermResponses returns the recorded permission answers.
func (f *Fake) PermResponses() []PermResponse {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]PermResponse(nil), f.perms...)
}

// Aborts returns the session IDs that were aborted.
func (f *Fake) Aborts() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.aborts...)
}

// --- tiny path helpers (avoid pulling in a router) --------------------------

func hasSuffix(s, suf string) bool { return len(s) >= len(suf) && s[len(s)-len(suf):] == suf }
func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

// pathSeg returns the nth path segment (0-based) of a /a/b/c path.
func pathSeg(path string, n int) string {
	segs := splitNonEmpty(path)
	if n < len(segs) {
		return segs[n]
	}
	return ""
}
func lastSeg(path string) string {
	segs := splitNonEmpty(path)
	if len(segs) == 0 {
		return ""
	}
	return segs[len(segs)-1]
}
func splitNonEmpty(path string) []string {
	var out []string
	cur := ""
	for _, r := range path {
		if r == '/' {
			if cur != "" {
				out = append(out, cur)
				cur = ""
			}
			continue
		}
		cur += string(r)
	}
	if cur != "" {
		out = append(out, cur)
	}
	return out
}
