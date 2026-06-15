package sidecar

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// --- Task 1: request translation + non-streaming response ---

func TestAnthropicToOpenAIRequest(t *testing.T) {
	req := []byte(`{
	  "model":"openai/gpt-4o-mini",
	  "max_tokens":256,
	  "temperature":0.5,
	  "stop_sequences":["STOP"],
	  "stream":true,
	  "system":"You are a helpful assistant.",
	  "tools":[
	    {"name":"get_weather","description":"Get weather","input_schema":{"type":"object","properties":{"city":{"type":"string"}},"required":["city"]}}
	  ],
	  "tool_choice":{"type":"auto"},
	  "messages":[
	    {"role":"user","content":"What is the weather in Paris?"},
	    {"role":"assistant","content":[
	      {"type":"text","text":"Let me check."},
	      {"type":"tool_use","id":"toolu_1","name":"get_weather","input":{"city":"Paris"}}
	    ]},
	    {"role":"user","content":[
	      {"type":"tool_result","tool_use_id":"toolu_1","content":"sunny, 25C"}
	    ]}
	  ]
	}`)

	out, stream, err := anthropicToOpenAI(req)
	if err != nil {
		t.Fatalf("anthropicToOpenAI: %v", err)
	}
	if !stream {
		t.Fatalf("expected stream=true")
	}

	var oai map[string]any
	if err := json.Unmarshal(out, &oai); err != nil {
		t.Fatalf("output not valid JSON: %v\n%s", err, out)
	}

	if oai["model"] != "openai/gpt-4o-mini" {
		t.Errorf("model = %v", oai["model"])
	}
	if oai["max_tokens"].(float64) != 256 {
		t.Errorf("max_tokens = %v", oai["max_tokens"])
	}
	if oai["temperature"].(float64) != 0.5 {
		t.Errorf("temperature = %v", oai["temperature"])
	}
	// stop_sequences -> stop
	stop, ok := oai["stop"].([]any)
	if !ok || len(stop) != 1 || stop[0] != "STOP" {
		t.Errorf("stop = %v", oai["stop"])
	}
	// stream must be present and true
	if s, ok := oai["stream"].(bool); !ok || !s {
		t.Errorf("stream = %v", oai["stream"])
	}

	// messages: system + user + assistant(tool_calls) + tool
	msgs, ok := oai["messages"].([]any)
	if !ok {
		t.Fatalf("messages not array: %T", oai["messages"])
	}
	if len(msgs) != 4 {
		t.Fatalf("expected 4 messages (system,user,assistant,tool), got %d: %s", len(msgs), out)
	}
	sys := msgs[0].(map[string]any)
	if sys["role"] != "system" || !strings.Contains(sys["content"].(string), "helpful assistant") {
		t.Errorf("system message = %v", sys)
	}
	user := msgs[1].(map[string]any)
	if user["role"] != "user" {
		t.Errorf("user role = %v", user["role"])
	}
	asst := msgs[2].(map[string]any)
	if asst["role"] != "assistant" {
		t.Errorf("assistant role = %v", asst["role"])
	}
	tcs, ok := asst["tool_calls"].([]any)
	if !ok || len(tcs) != 1 {
		t.Fatalf("assistant tool_calls = %v", asst["tool_calls"])
	}
	tc := tcs[0].(map[string]any)
	if tc["id"] != "toolu_1" || tc["type"] != "function" {
		t.Errorf("tool_call = %v", tc)
	}
	fn := tc["function"].(map[string]any)
	if fn["name"] != "get_weather" {
		t.Errorf("fn name = %v", fn["name"])
	}
	// arguments must be a JSON string
	args, ok := fn["arguments"].(string)
	if !ok {
		t.Fatalf("arguments not a string: %T", fn["arguments"])
	}
	var parsedArgs map[string]any
	if err := json.Unmarshal([]byte(args), &parsedArgs); err != nil {
		t.Fatalf("arguments not JSON: %v (%q)", err, args)
	}
	if parsedArgs["city"] != "Paris" {
		t.Errorf("args city = %v", parsedArgs["city"])
	}
	tool := msgs[3].(map[string]any)
	if tool["role"] != "tool" || tool["tool_call_id"] != "toolu_1" {
		t.Errorf("tool message = %v", tool)
	}
	if !strings.Contains(tool["content"].(string), "sunny") {
		t.Errorf("tool content = %v", tool["content"])
	}

	// tools -> function tools
	tools, ok := oai["tools"].([]any)
	if !ok || len(tools) != 1 {
		t.Fatalf("tools = %v", oai["tools"])
	}
	tdef := tools[0].(map[string]any)
	if tdef["type"] != "function" {
		t.Errorf("tool type = %v", tdef["type"])
	}
	tfn := tdef["function"].(map[string]any)
	if tfn["name"] != "get_weather" {
		t.Errorf("tool fn name = %v", tfn["name"])
	}
	params, ok := tfn["parameters"].(map[string]any)
	if !ok || params["type"] != "object" {
		t.Errorf("tool parameters = %v", tfn["parameters"])
	}

	// tool_choice auto -> "auto"
	if oai["tool_choice"] != "auto" {
		t.Errorf("tool_choice = %v", oai["tool_choice"])
	}
}

func TestAnthropicToOpenAIToolChoice(t *testing.T) {
	cases := []struct {
		in   string
		want any
	}{
		{`{"type":"auto"}`, "auto"},
		{`{"type":"any"}`, "required"},
		{`{"type":"tool","name":"get_weather"}`, map[string]any{"type": "function", "function": map[string]any{"name": "get_weather"}}},
	}
	for _, c := range cases {
		req := []byte(`{"model":"m","max_tokens":10,"tool_choice":` + c.in + `,"messages":[{"role":"user","content":"hi"}]}`)
		out, _, err := anthropicToOpenAI(req)
		if err != nil {
			t.Fatalf("%s: %v", c.in, err)
		}
		var oai map[string]any
		_ = json.Unmarshal(out, &oai)
		got, _ := json.Marshal(oai["tool_choice"])
		want, _ := json.Marshal(c.want)
		if string(got) != string(want) {
			t.Errorf("tool_choice(%s) = %s, want %s", c.in, got, want)
		}
	}
}

func TestOpenAIToAnthropicNonStreaming(t *testing.T) {
	resp := []byte(`{
	  "id":"chatcmpl-123",
	  "model":"openai/gpt-4o-mini",
	  "choices":[{
	    "index":0,
	    "message":{
	      "role":"assistant",
	      "content":"Let me check the weather.",
	      "tool_calls":[
	        {"id":"call_1","type":"function","function":{"name":"get_weather","arguments":"{\"city\":\"Paris\"}"}}
	      ]
	    },
	    "finish_reason":"tool_calls"
	  }],
	  "usage":{"prompt_tokens":12,"completion_tokens":7}
	}`)

	out, err := openAIToAnthropic(resp)
	if err != nil {
		t.Fatalf("openAIToAnthropic: %v", err)
	}
	var an map[string]any
	if err := json.Unmarshal(out, &an); err != nil {
		t.Fatalf("output not JSON: %v\n%s", err, out)
	}

	if an["type"] != "message" || an["role"] != "assistant" {
		t.Errorf("type/role = %v/%v", an["type"], an["role"])
	}
	id, _ := an["id"].(string)
	if !strings.HasPrefix(id, "msg_") {
		t.Errorf("id = %v, want msg_ prefix", an["id"])
	}
	if an["stop_reason"] != "tool_use" {
		t.Errorf("stop_reason = %v, want tool_use", an["stop_reason"])
	}

	content, ok := an["content"].([]any)
	if !ok || len(content) != 2 {
		t.Fatalf("content = %v", an["content"])
	}
	tb := content[0].(map[string]any)
	if tb["type"] != "text" || !strings.Contains(tb["text"].(string), "check the weather") {
		t.Errorf("text block = %v", tb)
	}
	tu := content[1].(map[string]any)
	if tu["type"] != "tool_use" || tu["id"] != "call_1" || tu["name"] != "get_weather" {
		t.Errorf("tool_use block = %v", tu)
	}
	input, ok := tu["input"].(map[string]any)
	if !ok || input["city"] != "Paris" {
		t.Errorf("tool_use input = %v", tu["input"])
	}

	usage, ok := an["usage"].(map[string]any)
	if !ok || usage["input_tokens"].(float64) != 12 || usage["output_tokens"].(float64) != 7 {
		t.Errorf("usage = %v", an["usage"])
	}
}

func TestOpenAIToAnthropicStopReasons(t *testing.T) {
	cases := map[string]string{
		"stop":           "end_turn",
		"length":         "max_tokens",
		"tool_calls":     "tool_use",
		"content_filter": "end_turn",
	}
	for fr, want := range cases {
		resp := []byte(`{"id":"x","model":"m","choices":[{"message":{"role":"assistant","content":"hi"},"finish_reason":"` + fr + `"}],"usage":{}}`)
		out, err := openAIToAnthropic(resp)
		if err != nil {
			t.Fatalf("%s: %v", fr, err)
		}
		var an map[string]any
		_ = json.Unmarshal(out, &an)
		if an["stop_reason"] != want {
			t.Errorf("finish_reason %s -> %v, want %s", fr, an["stop_reason"], want)
		}
	}
}

func TestMessagesHandlerCredentialsOverrideAppliesPerRequest(t *testing.T) {
	var defaultHit bool
	defaultUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defaultHit = true
		io.WriteString(w, `{"id":"default","model":"default","choices":[{"message":{"role":"assistant","content":"wrong"},"finish_reason":"stop"}],"usage":{}}`)
	}))
	defer defaultUpstream.Close()

	var gotAuth, gotPath string
	overrideUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"id":"chatcmpl-1","model":"override","choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`)
	}))
	defer overrideUpstream.Close()

	ov := &Override{}
	if err := ov.SetCredentials(overrideUpstream.URL, "byok-key"); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(NewMessagesHandler(defaultUpstream.URL, "default-key", ov))
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/v1/messages", "application/json",
		strings.NewReader(`{"model":"agent/model","max_tokens":8,"messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 200; body=%s", resp.StatusCode, body)
	}

	if defaultHit {
		t.Fatalf("request unexpectedly reached default upstream")
	}
	if gotAuth != "Bearer byok-key" {
		t.Fatalf("auth = %q, want BYOK bearer", gotAuth)
	}
	if gotPath != "/v1/chat/completions" {
		t.Fatalf("path = %q, want /v1/chat/completions", gotPath)
	}
}

// --- Task 2: streaming SSE translation ---

func TestStreamOpenAIToAnthropic(t *testing.T) {
	// OpenAI SSE: a couple text deltas, then a tool_call (id+name first, then arg fragments),
	// then a finish_reason chunk, then [DONE].
	sse := strings.Join([]string{
		`data: {"id":"c1","choices":[{"index":0,"delta":{"role":"assistant","content":""}}]}`,
		`data: {"id":"c1","choices":[{"index":0,"delta":{"content":"Let me "}}]}`,
		`data: {"id":"c1","choices":[{"index":0,"delta":{"content":"check."}}]}`,
		`data: {"id":"c1","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"get_weather","arguments":""}}]}}]}`,
		`data: {"id":"c1","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"ci"}}]}}]}`,
		`data: {"id":"c1","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"ty\":\"Paris\"}"}}]}}]}`,
		`data: {"id":"c1","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":10,"completion_tokens":5}}`,
		`data: [DONE]`,
		``,
	}, "\n\n")

	var buf bytes.Buffer
	if err := streamOpenAIToAnthropic(&buf, strings.NewReader(sse)); err != nil {
		t.Fatalf("streamOpenAIToAnthropic: %v", err)
	}
	out := buf.String()
	t.Logf("output:\n%s", out)

	// Required event ordering.
	order := []string{
		"event: message_start",
		"event: content_block_start", // text, index 0
		"event: content_block_delta", // text_delta
		"event: content_block_stop",  // close text
		"event: content_block_start", // tool_use, index 1
		"event: content_block_delta", // input_json_delta
		"event: content_block_stop",  // close tool_use
		"event: message_delta",
		"event: message_stop",
	}
	idx := 0
	for _, want := range order {
		i := strings.Index(out[idx:], want)
		if i < 0 {
			t.Fatalf("missing or out-of-order event %q after index %d\nfull:\n%s", want, idx, out)
		}
		idx += i + len(want)
	}

	// text_delta content present
	if !strings.Contains(out, "text_delta") || !strings.Contains(out, "Let me ") || !strings.Contains(out, "check.") {
		t.Errorf("missing text_delta content")
	}
	// tool_use start carries id + name
	if !strings.Contains(out, `"tool_use"`) || !strings.Contains(out, `"call_1"`) || !strings.Contains(out, `"get_weather"`) {
		t.Errorf("missing tool_use id/name in content_block_start")
	}
	// stop_reason tool_use in message_delta
	if !strings.Contains(out, `"stop_reason":"tool_use"`) {
		t.Errorf("missing stop_reason tool_use in message_delta")
	}

	// Concatenated input_json_delta partial_json must parse to the tool input.
	partials := extractPartialJSON(t, out)
	var parsed map[string]any
	if err := json.Unmarshal([]byte(partials), &parsed); err != nil {
		t.Fatalf("concatenated partial_json does not parse: %v (%q)", err, partials)
	}
	if parsed["city"] != "Paris" {
		t.Errorf("accumulated tool input city = %v", parsed["city"])
	}
}

func TestStreamTextOnly(t *testing.T) {
	sse := strings.Join([]string{
		`data: {"id":"c1","choices":[{"index":0,"delta":{"role":"assistant","content":"Hello"}}]}`,
		`data: {"id":"c1","choices":[{"index":0,"delta":{"content":" world"}}]}`,
		`data: {"id":"c1","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
		`data: [DONE]`,
		``,
	}, "\n\n")
	var buf bytes.Buffer
	if err := streamOpenAIToAnthropic(&buf, strings.NewReader(sse)); err != nil {
		t.Fatalf("stream: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "event: message_start") || !strings.Contains(out, "event: message_stop") {
		t.Fatalf("missing message_start/stop:\n%s", out)
	}
	if !strings.Contains(out, "Hello") || !strings.Contains(out, " world") {
		t.Errorf("missing text content:\n%s", out)
	}
	if !strings.Contains(out, `"stop_reason":"end_turn"`) {
		t.Errorf("missing end_turn stop_reason:\n%s", out)
	}
}

// extractPartialJSON pulls every input_json_delta partial_json string out of the SSE output
// and concatenates them (in order).
func extractPartialJSON(t *testing.T, sse string) string {
	t.Helper()
	var sb strings.Builder
	for _, line := range strings.Split(sse, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		var ev struct {
			Type  string `json:"type"`
			Delta struct {
				Type        string `json:"type"`
				PartialJSON string `json:"partial_json"`
			} `json:"delta"`
		}
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &ev); err != nil {
			continue
		}
		if ev.Type == "content_block_delta" && ev.Delta.Type == "input_json_delta" {
			sb.WriteString(ev.Delta.PartialJSON)
		}
	}
	return sb.String()
}

// --- Model override on the Anthropic path ---

func TestMessagesHandlerOverrideSubstitutes(t *testing.T) {
	var upModel string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var obj map[string]any
		_ = json.Unmarshal(b, &obj)
		if m, ok := obj["model"].(string); ok {
			upModel = m
		}
		// minimal non-streaming OpenAI completion
		io.WriteString(w, `{"id":"x","model":"override/model","choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`)
	}))
	defer upstream.Close()

	ov := &Override{}
	ov.Set("override/model")
	srv := httptest.NewServer(NewMessagesHandler(upstream.URL, "k", ov))
	defer srv.Close()

	body := `{"model":"agent/model","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`
	resp, err := http.Post(srv.URL+"/v1/messages", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if upModel != "override/model" {
		t.Fatalf("upstream model = %q, want override/model", upModel)
	}
}

func TestMessagesHandlerOverrideUnsetKeepsAgentModel(t *testing.T) {
	var upModel string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var obj map[string]any
		_ = json.Unmarshal(b, &obj)
		upModel, _ = obj["model"].(string)
		io.WriteString(w, `{"id":"x","model":"agent/model","choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],"usage":{}}`)
	}))
	defer upstream.Close()

	srv := httptest.NewServer(NewMessagesHandler(upstream.URL, "k", &Override{}))
	defer srv.Close()

	body := `{"model":"agent/model","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`
	resp, err := http.Post(srv.URL+"/v1/messages", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if upModel != "agent/model" {
		t.Fatalf("upstream model = %q, want agent/model (passthrough)", upModel)
	}
}
