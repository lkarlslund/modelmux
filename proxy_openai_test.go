package main

import (
	"encoding/json"
	"strings"
	"testing"
)

const oaStreamReply = `data: {"id":"c1","choices":[{"index":0,"delta":{"role":"assistant","content":"Let me look."}}]}

data: {"id":"c1","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"Bash","arguments":""}}]}}]}

data: {"id":"c1","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"command\":"}}]}}]}

data: {"id":"c1","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"ls\"}"}}]}}]}

data: {"id":"c1","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}

data: {"id":"c1","choices":[],"usage":{"prompt_tokens":50,"completion_tokens":7,"prompt_tokens_details":{"cached_tokens":40}}}

data: [DONE]

`

const oaPlainReply = `{"id":"c2","choices":[{"index":0,"message":{"role":"assistant","content":"OK"},"finish_reason":"stop"}],"usage":{"prompt_tokens":5,"completion_tokens":1}}`

type event struct {
	Type    string          `json:"type"`
	Message json.RawMessage `json:"message"`
	Delta   struct {
		Type        string `json:"type"`
		Text        string `json:"text"`
		PartialJSON string `json:"partial_json"`
		StopReason  string `json:"stop_reason"`
	} `json:"delta"`
	ContentBlock struct {
		Type, Name, ID string
	} `json:"content_block"`
}

func events(t *testing.T, s string) []event {
	t.Helper()
	var out []event
	for line := range strings.SplitSeq(s, "\n") {
		if d, ok := strings.CutPrefix(line, "data: "); ok {
			var e event
			if err := json.Unmarshal([]byte(d), &e); err != nil {
				t.Fatalf("bad event %s: %v", d, err)
			}
			out = append(out, e)
		}
	}
	return out
}

func openAIProxy(t *testing.T, ct, reply string) (*seen, func(body string) (int, string)) {
	t.Helper()
	t.Setenv("OA_KEY", "sk-test")
	var up seen
	cfg := &Config{Models: []*Model{{ID: "gpt-x", API: "openai", KeyEnv: "OA_KEY", UpstreamModel: "real/gpt",
		Upstream: recorder(t, &up, ct, reply).URL + "/v1"}}}
	h := newTestProxy(t, cfg)
	return &up, func(body string) (int, string) {
		w := post(h, "/v1/messages", body)
		return w.Code, w.Body.String()
	}
}

func TestOpenAIStreamEndToEnd(t *testing.T) {
	up, send := openAIProxy(t, "text/event-stream", oaStreamReply)
	code, out := send(`{"model":"gpt-x","stream":true,"max_tokens":100,"system":"be brief",` +
		`"messages":[{"role":"user","content":"hi"},{"role":"system","content":"env block"}],` +
		`"tools":[{"name":"Bash","input_schema":{"type":"object"}},{"type":"web_search_20250305","name":"web_search"}]}`)
	if code != 200 {
		t.Fatalf("%d %s", code, out)
	}
	if up.path != "/v1/chat/completions" || up.header.Get("Authorization") != "Bearer sk-test" || up.header.Get("X-Api-Key") != "" {
		t.Errorf("upstream saw %s %v", up.path, up.header)
	}
	var req struct {
		Model    string `json:"model"`
		Messages []struct {
			Role    string `json:"role"`
			Content any    `json:"content"`
		} `json:"messages"`
		Tools []any `json:"tools"`
	}
	if err := json.Unmarshal(up.body, &req); err != nil {
		t.Fatal(err)
	}
	if req.Model != "real/gpt" || len(req.Tools) != 1 || len(req.Messages) != 2 ||
		req.Messages[0].Role != "system" || req.Messages[1].Role != "user" ||
		!strings.Contains(req.Messages[1].Content.(string), "env block") {
		t.Errorf("translated request: %s", up.body)
	}

	evs := events(t, out)
	var types []string
	var text, args, stop string
	for _, e := range evs {
		types = append(types, e.Type)
		text += e.Delta.Text
		args += e.Delta.PartialJSON
		if e.Delta.StopReason != "" {
			stop = e.Delta.StopReason
		}
	}
	if !strings.Contains(string(evs[0].Message), `"model":"gpt-x"`) {
		t.Errorf("message_start %s", evs[0].Message)
	}
	want := "message_start content_block_start content_block_delta content_block_stop content_block_start " +
		"content_block_delta content_block_delta content_block_stop message_delta message_stop"
	if strings.Join(types, " ") != want {
		t.Errorf("events %v", types)
	}
	if text != "Let me look." || args != `{"command":"ls"}` || stop != "tool_use" {
		t.Errorf("text %q args %q stop %q", text, args, stop)
	}
}

func TestOpenAIPlainAndUnstreamedFallback(t *testing.T) {
	_, send := openAIProxy(t, "application/json", oaPlainReply)
	code, out := send(`{"model":"gpt-x","max_tokens":10,"messages":[{"role":"user","content":"hi"}]}`)
	var msg struct {
		Model   string `json:"model"`
		Content []struct{ Text string }
	}
	_ = json.Unmarshal([]byte(out), &msg)
	if code != 200 || msg.Model != "gpt-x" || len(msg.Content) != 1 || msg.Content[0].Text != "OK" {
		t.Errorf("%d %s", code, out)
	}
	// The upstream answers JSON although we asked for a stream.
	code, out = send(`{"model":"gpt-x","stream":true,"max_tokens":10,"messages":[{"role":"user","content":"hi"}]}`)
	var text string
	for _, e := range events(t, out) {
		text += e.Delta.Text
	}
	if code != 200 || text != "OK" || !strings.HasPrefix(out, "event: message_start") {
		t.Errorf("%d %s", code, out)
	}
}

func TestOpenAIErrorsAndCountTokens(t *testing.T) {
	_, send := openAIProxy(t, "application/json", `{"error":{"message":"bad key","type":"invalid_request_error"}}`)
	// recorder answers 200; an error object in a 200 body is still an error.
	code, out := send(`{"model":"gpt-x","messages":[{"role":"user","content":"hi"}]}`)
	if code != 502 || !strings.Contains(out, "bad key") {
		t.Errorf("%d %s", code, out)
	}
	h := newTestProxy(t, &Config{Models: []*Model{{ID: "gpt-x", API: "openai", Upstream: "http://127.0.0.1:1/v1"}}})
	if w := post(h, "/v1/messages/count_tokens", `{"model":"gpt-x","messages":[]}`); w.Code != 404 {
		t.Errorf("count_tokens: %d", w.Code)
	}
	if w := post(h, "/v1/messages", `{"model":"gpt-x","messages":[]}`); w.Code != 502 || !strings.Contains(w.Body.String(), `"type":"error"`) {
		t.Errorf("unreachable upstream: %d %s", w.Code, w.Body)
	}
}
