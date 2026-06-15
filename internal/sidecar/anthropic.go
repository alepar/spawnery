package sidecar

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
)

// anthropic.go translates the Anthropic Messages API (which Claude Code speaks) to the
// OpenAI Chat Completions API (which OpenRouter exposes) and back, including streaming SSE
// and tool_use. The sidecar serves this at POST /v1/messages; everything else stays on the
// transparent OpenAI passthrough (opencode/goose).

// --- Anthropic request shapes (the subset Claude Code sends) ---

type anthropicRequest struct {
	Model         string             `json:"model"`
	MaxTokens     int                `json:"max_tokens"`
	System        json.RawMessage    `json:"system,omitempty"` // string OR []textBlock
	Messages      []anthropicMessage `json:"messages"`
	Tools         []anthropicTool    `json:"tools,omitempty"`
	ToolChoice    json.RawMessage    `json:"tool_choice,omitempty"`
	Temperature   *float64           `json:"temperature,omitempty"`
	TopP          *float64           `json:"top_p,omitempty"`
	StopSequences []string           `json:"stop_sequences,omitempty"`
	Stream        bool               `json:"stream,omitempty"`
}

type anthropicMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"` // string OR []contentBlock
}

type anthropicTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"input_schema,omitempty"`
}

// anthropicBlock covers every content-block kind we map (text, tool_use, tool_result, image).
type anthropicBlock struct {
	Type string `json:"type"`
	// text
	Text string `json:"text,omitempty"`
	// tool_use
	ID    string          `json:"id,omitempty"`
	Name  string          `json:"name,omitempty"`
	Input json.RawMessage `json:"input,omitempty"`
	// tool_result
	ToolUseID string          `json:"tool_use_id,omitempty"`
	Content   json.RawMessage `json:"content,omitempty"` // string OR []block
	IsError   bool            `json:"is_error,omitempty"`
	// image
	Source json.RawMessage `json:"source,omitempty"`
}

// --- OpenAI request shapes ---

type openAIRequest struct {
	Model       string         `json:"model"`
	Messages    []openAIMsg    `json:"messages"`
	Tools       []openAITool   `json:"tools,omitempty"`
	ToolChoice  any            `json:"tool_choice,omitempty"`
	MaxTokens   int            `json:"max_tokens,omitempty"`
	Temperature *float64       `json:"temperature,omitempty"`
	TopP        *float64       `json:"top_p,omitempty"`
	Stop        []string       `json:"stop,omitempty"`
	Stream      bool           `json:"stream,omitempty"`
	StreamOpts  map[string]any `json:"stream_options,omitempty"`
}

type openAIMsg struct {
	Role       string           `json:"role"`
	Content    any              `json:"content,omitempty"` // string OR []part OR null
	ToolCalls  []openAIToolCall `json:"tool_calls,omitempty"`
	ToolCallID string           `json:"tool_call_id,omitempty"`
}

type openAIToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Index    *int   `json:"index,omitempty"`
	Function struct {
		Name      string `json:"name,omitempty"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

type openAITool struct {
	Type     string `json:"type"`
	Function struct {
		Name        string          `json:"name"`
		Description string          `json:"description,omitempty"`
		Parameters  json.RawMessage `json:"parameters,omitempty"`
	} `json:"function"`
}

// anthropicToOpenAI converts an Anthropic Messages request body to an OpenAI Chat Completions
// request body. Returns the OpenAI JSON, whether streaming was requested, and an error.
func anthropicToOpenAI(reqJSON []byte) ([]byte, bool, error) {
	var ar anthropicRequest
	if err := json.Unmarshal(reqJSON, &ar); err != nil {
		return nil, false, fmt.Errorf("decode anthropic request: %w", err)
	}

	oai := openAIRequest{
		Model:       ar.Model,
		MaxTokens:   ar.MaxTokens,
		Temperature: ar.Temperature,
		TopP:        ar.TopP,
		Stop:        ar.StopSequences,
		Stream:      ar.Stream,
	}
	if ar.Stream {
		// Ask OpenRouter to include usage in the final streaming chunk.
		oai.StreamOpts = map[string]any{"include_usage": true}
	}

	// system -> leading system message
	if sys := flattenAnthropicSystem(ar.System); sys != "" {
		oai.Messages = append(oai.Messages, openAIMsg{Role: "system", Content: sys})
	}

	// messages
	for _, m := range ar.Messages {
		msgs, err := convertAnthropicMessage(m)
		if err != nil {
			return nil, false, err
		}
		oai.Messages = append(oai.Messages, msgs...)
	}

	// tools
	for _, t := range ar.Tools {
		var ot openAITool
		ot.Type = "function"
		ot.Function.Name = t.Name
		ot.Function.Description = t.Description
		ot.Function.Parameters = t.InputSchema
		oai.Tools = append(oai.Tools, ot)
	}

	// tool_choice
	if len(ar.ToolChoice) > 0 {
		tc, err := convertToolChoice(ar.ToolChoice)
		if err != nil {
			return nil, false, err
		}
		oai.ToolChoice = tc
	}

	out, err := json.Marshal(oai)
	if err != nil {
		return nil, false, err
	}
	return out, ar.Stream, nil
}

// flattenAnthropicSystem accepts a string OR an array of text blocks and returns plain text.
func flattenAnthropicSystem(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	var blocks []anthropicBlock
	if err := json.Unmarshal(raw, &blocks); err == nil {
		var sb strings.Builder
		for _, b := range blocks {
			if b.Type == "text" || b.Text != "" {
				sb.WriteString(b.Text)
			}
		}
		return sb.String()
	}
	return ""
}

// convertAnthropicMessage maps one Anthropic message to one or more OpenAI messages.
// A user message containing tool_result blocks expands into separate {role:tool} messages.
func convertAnthropicMessage(m anthropicMessage) ([]openAIMsg, error) {
	// content as a plain string
	var asString string
	if err := json.Unmarshal(m.Content, &asString); err == nil {
		return []openAIMsg{{Role: m.Role, Content: asString}}, nil
	}

	var blocks []anthropicBlock
	if err := json.Unmarshal(m.Content, &blocks); err != nil {
		return nil, fmt.Errorf("decode message content: %w", err)
	}

	var out []openAIMsg
	var textParts []string
	var toolCalls []openAIToolCall
	// OpenAI requires tool results as their own messages, and they must come AFTER the
	// assistant message that issued the tool_calls. We collect them and emit them last.
	var toolResults []openAIMsg

	for _, b := range blocks {
		switch b.Type {
		case "text":
			textParts = append(textParts, b.Text)
		case "tool_use":
			tc := openAIToolCall{ID: b.ID, Type: "function"}
			tc.Function.Name = b.Name
			if len(b.Input) > 0 {
				tc.Function.Arguments = string(b.Input)
			} else {
				tc.Function.Arguments = "{}"
			}
			toolCalls = append(toolCalls, tc)
		case "tool_result":
			toolResults = append(toolResults, openAIMsg{
				Role:       "tool",
				ToolCallID: b.ToolUseID,
				Content:    flattenToolResultContent(b.Content),
			})
		case "image":
			// best-effort: drop image content (text models). Leave a placeholder so the turn
			// is not empty.
			textParts = append(textParts, "[image omitted]")
		default:
			// ignore unknown blocks (thinking, etc.)
		}
	}

	if m.Role == "assistant" {
		msg := openAIMsg{Role: "assistant"}
		if len(textParts) > 0 {
			msg.Content = strings.Join(textParts, "")
		}
		msg.ToolCalls = toolCalls
		// An assistant message with tool_calls and no text is valid; content may be empty.
		out = append(out, msg)
	} else {
		// user (or other): emit a text message (if any text), then the tool results.
		if len(textParts) > 0 {
			out = append(out, openAIMsg{Role: m.Role, Content: strings.Join(textParts, "")})
		}
	}
	out = append(out, toolResults...)
	return out, nil
}

// flattenToolResultContent accepts a tool_result content that is a string OR an array of
// blocks and returns a plain string for the OpenAI tool message.
func flattenToolResultContent(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	var blocks []anthropicBlock
	if err := json.Unmarshal(raw, &blocks); err == nil {
		var sb strings.Builder
		for _, b := range blocks {
			if b.Text != "" {
				sb.WriteString(b.Text)
			}
		}
		return sb.String()
	}
	// fall back to the raw JSON
	return string(raw)
}

// convertToolChoice maps Anthropic tool_choice to the OpenAI equivalent.
func convertToolChoice(raw json.RawMessage) (any, error) {
	var tc struct {
		Type string `json:"type"`
		Name string `json:"name"`
	}
	if err := json.Unmarshal(raw, &tc); err != nil {
		return nil, fmt.Errorf("decode tool_choice: %w", err)
	}
	switch tc.Type {
	case "auto":
		return "auto", nil
	case "any":
		return "required", nil
	case "tool":
		return map[string]any{
			"type":     "function",
			"function": map[string]any{"name": tc.Name},
		}, nil
	case "none":
		return "none", nil
	default:
		return "auto", nil
	}
}

// --- Non-streaming response (OpenAI -> Anthropic) ---

type openAIResponse struct {
	ID      string `json:"id"`
	Model   string `json:"model"`
	Choices []struct {
		Index   int `json:"index"`
		Message struct {
			Role      string           `json:"role"`
			Content   string           `json:"content"`
			ToolCalls []openAIToolCall `json:"tool_calls"`
		} `json:"message"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Usage struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
	} `json:"usage"`
}

// openAIToAnthropic converts a non-streaming OpenAI completion to an Anthropic message.
func openAIToAnthropic(respJSON []byte) ([]byte, error) {
	var or openAIResponse
	if err := json.Unmarshal(respJSON, &or); err != nil {
		return nil, fmt.Errorf("decode openai response: %w", err)
	}

	content := []map[string]any{}
	finish := ""
	model := or.Model
	if len(or.Choices) > 0 {
		ch := or.Choices[0]
		finish = ch.FinishReason
		if ch.Message.Content != "" {
			content = append(content, map[string]any{
				"type": "text",
				"text": ch.Message.Content,
			})
		}
		for _, tc := range ch.Message.ToolCalls {
			input := json.RawMessage(tc.Function.Arguments)
			if len(input) == 0 || !json.Valid(input) {
				input = json.RawMessage(`{}`)
			}
			content = append(content, map[string]any{
				"type":  "tool_use",
				"id":    tc.ID,
				"name":  tc.Function.Name,
				"input": input,
			})
		}
	}

	msg := map[string]any{
		"id":            "msg_" + randID(),
		"type":          "message",
		"role":          "assistant",
		"model":         model,
		"content":       content,
		"stop_reason":   mapStopReason(finish),
		"stop_sequence": nil,
		"usage": map[string]any{
			"input_tokens":  or.Usage.PromptTokens,
			"output_tokens": or.Usage.CompletionTokens,
		},
	}
	return json.Marshal(msg)
}

// mapStopReason maps an OpenAI finish_reason to an Anthropic stop_reason.
func mapStopReason(fr string) string {
	switch fr {
	case "stop":
		return "end_turn"
	case "length":
		return "max_tokens"
	case "tool_calls", "function_call":
		return "tool_use"
	case "":
		return "end_turn"
	default:
		return "end_turn"
	}
}

// --- Streaming (OpenAI SSE -> Anthropic SSE) ---

// openAIStreamChunk is one OpenAI streaming chunk.
type openAIStreamChunk struct {
	ID      string `json:"id"`
	Model   string `json:"model"`
	Choices []struct {
		Index int `json:"index"`
		Delta struct {
			Role      string           `json:"role"`
			Content   *string          `json:"content"`
			ToolCalls []openAIToolCall `json:"tool_calls"`
		} `json:"delta"`
		FinishReason *string `json:"finish_reason"`
	} `json:"choices"`
	Usage *struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
	} `json:"usage"`
}

// streamOpenAIToAnthropic reads an OpenAI SSE stream and writes the equivalent Anthropic SSE
// event stream to w, flushing after each event when w is an http.Flusher.
//
// Block indexing: Anthropic content blocks are indexed sequentially. We open a text block at
// index 0 lazily on the first text delta; each OpenAI tool_call (keyed by its OpenAI delta
// index) gets the next Anthropic block index when it first appears.
func streamOpenAIToAnthropic(w io.Writer, r io.Reader) error {
	flush := func() {
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
	}

	msgID := "msg_" + randID()
	model := ""

	// message_start
	writeEvent(w, "message_start", map[string]any{
		"type": "message_start",
		"message": map[string]any{
			"id":            msgID,
			"type":          "message",
			"role":          "assistant",
			"model":         model,
			"content":       []any{},
			"stop_reason":   nil,
			"stop_sequence": nil,
			"usage":         map[string]any{"input_tokens": 0, "output_tokens": 0},
		},
	})
	// An initial ping keeps some clients happy.
	writeEvent(w, "ping", map[string]any{"type": "ping"})
	flush()

	nextBlockIndex := 0
	textBlockIndex := -1 // -1 = not open
	textOpen := false

	// tool block state, keyed by OpenAI tool_call delta index.
	type toolBlock struct {
		anthIndex int
		opened    bool
		closed    bool
	}
	toolBlocks := map[int]*toolBlock{}
	var toolOrder []int // OpenAI indices in first-seen order

	stopReason := "end_turn"
	gotStop := false
	usageOut := 0
	usageIn := 0

	closeText := func() {
		if textOpen {
			writeEvent(w, "content_block_stop", map[string]any{
				"type": "content_block_stop", "index": textBlockIndex,
			})
			textOpen = false
			flush()
		}
	}

	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 1024*1024), 16*1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "[DONE]" {
			break
		}

		var chunk openAIStreamChunk
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			// Skip malformed chunks rather than aborting the whole turn.
			log.Printf("warn: sidecar: skip malformed openai stream chunk: %v", err)
			continue
		}
		if chunk.Usage != nil {
			usageIn = chunk.Usage.PromptTokens
			usageOut = chunk.Usage.CompletionTokens
		}
		if len(chunk.Choices) == 0 {
			continue
		}
		choice := chunk.Choices[0]

		// text delta
		if choice.Delta.Content != nil && *choice.Delta.Content != "" {
			if !textOpen {
				// Close any nothing; text always comes before tools in practice, but open lazily.
				textBlockIndex = nextBlockIndex
				nextBlockIndex++
				textOpen = true
				writeEvent(w, "content_block_start", map[string]any{
					"type":          "content_block_start",
					"index":         textBlockIndex,
					"content_block": map[string]any{"type": "text", "text": ""},
				})
				flush()
			}
			writeEvent(w, "content_block_delta", map[string]any{
				"type":  "content_block_delta",
				"index": textBlockIndex,
				"delta": map[string]any{"type": "text_delta", "text": *choice.Delta.Content},
			})
			flush()
		}

		// tool_call deltas
		for _, tc := range choice.Delta.ToolCalls {
			oaiIdx := 0
			if tc.Index != nil {
				oaiIdx = *tc.Index
			}
			tb, ok := toolBlocks[oaiIdx]
			if !ok {
				// New tool block: close the text block first (Anthropic blocks are sequential).
				closeText()
				tb = &toolBlock{anthIndex: nextBlockIndex}
				nextBlockIndex++
				toolBlocks[oaiIdx] = tb
				toolOrder = append(toolOrder, oaiIdx)
			}
			// Open it once we have an id+name (the first tool chunk carries these).
			if !tb.opened && (tc.ID != "" || tc.Function.Name != "") {
				writeEvent(w, "content_block_start", map[string]any{
					"type":  "content_block_start",
					"index": tb.anthIndex,
					"content_block": map[string]any{
						"type":  "tool_use",
						"id":    tc.ID,
						"name":  tc.Function.Name,
						"input": map[string]any{},
					},
				})
				tb.opened = true
				flush()
			}
			// arguments fragment -> input_json_delta
			if tc.Function.Arguments != "" {
				// Defensive: if a fragment arrives before the open (no id/name yet), open with empties.
				if !tb.opened {
					writeEvent(w, "content_block_start", map[string]any{
						"type":  "content_block_start",
						"index": tb.anthIndex,
						"content_block": map[string]any{
							"type": "tool_use", "id": "", "name": "", "input": map[string]any{},
						},
					})
					tb.opened = true
					flush()
				}
				writeEvent(w, "content_block_delta", map[string]any{
					"type":  "content_block_delta",
					"index": tb.anthIndex,
					"delta": map[string]any{"type": "input_json_delta", "partial_json": tc.Function.Arguments},
				})
				flush()
			}
		}

		if choice.FinishReason != nil && *choice.FinishReason != "" {
			stopReason = mapStopReason(*choice.FinishReason)
			gotStop = true
		}
	}
	if err := sc.Err(); err != nil {
		// We've emitted partial events; close out gracefully so claude doesn't hang.
		log.Printf("warn: sidecar: error reading openai stream: %v", err)
	}

	// Close any open blocks (text first, then tools in first-seen order).
	closeText()
	for _, oaiIdx := range toolOrder {
		tb := toolBlocks[oaiIdx]
		if tb.opened && !tb.closed {
			writeEvent(w, "content_block_stop", map[string]any{
				"type": "content_block_stop", "index": tb.anthIndex,
			})
			tb.closed = true
			flush()
		}
	}
	_ = gotStop

	// message_delta with stop_reason + usage
	writeEvent(w, "message_delta", map[string]any{
		"type": "message_delta",
		"delta": map[string]any{
			"stop_reason":   stopReason,
			"stop_sequence": nil,
		},
		"usage": map[string]any{"input_tokens": usageIn, "output_tokens": usageOut},
	})
	flush()

	// message_stop
	writeEvent(w, "message_stop", map[string]any{"type": "message_stop"})
	flush()
	return nil
}

// writeEvent writes a single Anthropic SSE event in `event: <type>\ndata: <json>\n\n` framing.
func writeEvent(w io.Writer, event string, payload any) {
	b, err := json.Marshal(payload)
	if err != nil {
		log.Printf("warn: sidecar: marshal SSE event %s: %v", event, err)
		return
	}
	var buf bytes.Buffer
	buf.WriteString("event: ")
	buf.WriteString(event)
	buf.WriteByte('\n')
	buf.WriteString("data: ")
	buf.Write(b)
	buf.WriteString("\n\n")
	_, _ = w.Write(buf.Bytes())
}

func randID() string {
	var b [12]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// --- HTTP handler (Task 3) ---

// NewMessagesHandler returns an http.Handler for POST /v1/messages that translates the
// Anthropic Messages API to OpenAI Chat Completions against upstream, injecting the bearer key.
func NewMessagesHandler(upstream, key string, ov *Override) http.Handler {
	client := &http.Client{}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeAnthropicError(w, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed")
			return
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", "read body: "+err.Error())
			return
		}
		oaiBody, stream, err := anthropicToOpenAI(body)
		if err != nil {
			log.Printf("warn: sidecar: /v1/messages translate request: %v", err)
			writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", "translate request: "+err.Error())
			return
		}

		// Force the override model (raw OpenRouter id): patch the already-converted
		// OpenAI body before forwarding. The conversion copies the agent's model
		// verbatim, so patching after it is functionally the same as substituting
		// the model in the Anthropic request first.
		if m := ov.Get(); m != "" {
			if patched, perr := patchModelJSON(oaiBody, m); perr == nil {
				oaiBody = patched
			} else {
				log.Printf("warn: sidecar: /v1/messages model override skipped: %v", perr)
			}
		}

		creds := ov.Credentials(upstream, key)
		upstreamURL, err := chatCompletionsURL(creds.Upstream)
		if err != nil {
			writeAnthropicError(w, http.StatusInternalServerError, "api_error", "build upstream request: "+err.Error())
			return
		}
		upReq, err := http.NewRequestWithContext(r.Context(), http.MethodPost, upstreamURL, bytes.NewReader(oaiBody))
		if err != nil {
			writeAnthropicError(w, http.StatusInternalServerError, "api_error", "build upstream request: "+err.Error())
			return
		}
		upReq.Header.Set("Content-Type", "application/json")
		upReq.Header.Set("Authorization", "Bearer "+creds.Key)
		if stream {
			upReq.Header.Set("Accept", "text/event-stream")
		}

		resp, err := client.Do(upReq)
		if err != nil {
			log.Printf("warn: sidecar: /v1/messages upstream request failed: %v", err)
			writeAnthropicError(w, http.StatusBadGateway, "api_error", "upstream request failed: "+err.Error())
			return
		}
		defer resp.Body.Close()

		if resp.StatusCode >= 400 {
			b, _ := io.ReadAll(resp.Body)
			snippet := strings.TrimSpace(redactCredentialEcho(string(b), creds.Key))
			log.Printf("warn: sidecar: /v1/messages upstream -> %d: %s", resp.StatusCode, truncateStr(snippet, 512))
			// Pass the upstream error through as an Anthropic-shaped error.
			writeAnthropicError(w, resp.StatusCode, "api_error", snippet)
			return
		}

		if stream {
			w.Header().Set("Content-Type", "text/event-stream")
			w.Header().Set("Cache-Control", "no-cache")
			w.Header().Set("Connection", "keep-alive")
			w.WriteHeader(http.StatusOK)
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
			if err := streamOpenAIToAnthropic(w, resp.Body); err != nil {
				log.Printf("warn: sidecar: /v1/messages stream translation error: %v", err)
			}
			return
		}

		b, err := io.ReadAll(resp.Body)
		if err != nil {
			writeAnthropicError(w, http.StatusBadGateway, "api_error", "read upstream body: "+err.Error())
			return
		}
		anthBody, err := openAIToAnthropic(b)
		if err != nil {
			log.Printf("warn: sidecar: /v1/messages translate response: %v", err)
			writeAnthropicError(w, http.StatusBadGateway, "api_error", "translate response: "+err.Error())
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(anthBody)
	})
}

func chatCompletionsURL(upstream string) (string, error) {
	target, err := url.Parse(upstream)
	if err != nil {
		return "", err
	}
	endpoint := &url.URL{Path: "/v1/chat/completions"}
	path, rawpath := joinURLPath(target, endpoint)
	out := *target
	out.Path = path
	out.RawPath = rawpath
	out.Fragment = ""
	return out.String(), nil
}

// writeAnthropicError writes an Anthropic-shaped error JSON.
func writeAnthropicError(w http.ResponseWriter, status int, errType, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	b, _ := json.Marshal(map[string]any{
		"type": "error",
		"error": map[string]any{
			"type":    errType,
			"message": message,
		},
	})
	_, _ = w.Write(b)
}

func truncateStr(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
