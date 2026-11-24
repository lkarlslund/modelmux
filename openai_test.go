package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
)

// jsonEqual reports whether a and b are the same JSON value.
func jsonEqual(t *testing.T, got []byte, want string) bool {
	t.Helper()
	var g, w any
	if err := json.Unmarshal(got, &g); err != nil {
		t.Fatalf("output is not JSON: %v\n%s", err, got)
	}
	if err := json.Unmarshal([]byte(want), &w); err != nil {
		t.Fatalf("bad want JSON: %v\n%s", err, want)
	}
	return reflect.DeepEqual(g, w)
}

func TestToOpenAI(t *testing.T) {
	const img = `{"type":"image","source":{"type":"base64","media_type":"image/png","data":"AAAA"}}`
	tests := []struct {
		name string
		opts oaOptions
		in   string
		want string
	}{
		{
			name: "system string",
			in:   `{"model":"x","max_tokens":100,"system":"be brief","messages":[{"role":"user","content":"hi"}]}`,
			want: `{"model":"m","max_tokens":100,"messages":[{"role":"system","content":"be brief"},{"role":"user","content":"hi"}]}`,
		},
		{
			name: "system blocks with cache_control, user text blocks joined",
			in: `{"system":[{"type":"text","text":"a"},{"type":"text","text":"b","cache_control":{"type":"ephemeral"}}],` +
				`"messages":[{"role":"user","content":[{"type":"text","text":"one"},{"type":"text","text":"two","cache_control":{"type":"ephemeral"}}]}]}`,
			want: `{"model":"m","messages":[{"role":"system","content":"a\n\nb"},{"role":"user","content":"one\n\ntwo"}]}`,
		},
		{
			name: "mid-conversation system message stays in place",
			in: `{"messages":[{"role":"user","content":"task"},{"role":"system","content":[{"type":"text","text":"env"}]},` +
				`{"role":"assistant","content":"ok"}]}`,
			want: `{"model":"m","messages":[{"role":"user","content":"task"},{"role":"system","content":"env"},{"role":"assistant","content":"ok"}]}`,
		},
		{
			name: "text and images",
			in: `{"messages":[{"role":"user","content":[{"type":"text","text":"look"},` + img + `,` +
				`{"type":"image","source":{"type":"url","url":"https://x/y.png"}}]}]}`,
			want: `{"model":"m","messages":[{"role":"user","content":[{"type":"text","text":"look"},` +
				`{"type":"image_url","image_url":{"url":"data:image/png;base64,AAAA"}},` +
				`{"type":"image_url","image_url":{"url":"https://x/y.png"}}]}]}`,
		},
		{
			name: "assistant text and tool_use, thinking dropped",
			in: `{"messages":[{"role":"assistant","content":[{"type":"thinking","thinking":"hmm","signature":"s"},` +
				`{"type":"text","text":"running"},` +
				`{"type":"tool_use","id":"toolu_1","name":"Bash","input":{ "command" : "ls", "n":[1, 2] }},` +
				`{"type":"tool_use","id":"toolu_2","name":"Read","input":{}}]}]}`,
			want: `{"model":"m","messages":[{"role":"assistant","content":"running","tool_calls":[` +
				`{"id":"toolu_1","type":"function","function":{"name":"Bash","arguments":"{\"command\":\"ls\",\"n\":[1,2]}"}},` +
				`{"id":"toolu_2","type":"function","function":{"name":"Read","arguments":"{}"}}]}]}`,
		},
		{
			name: "assistant tool_use only has null content",
			in:   `{"messages":[{"role":"assistant","content":[{"type":"tool_use","id":"t","name":"X","input":{"a":"<&>"}}]}]}`,
			want: `{"model":"m","messages":[{"role":"assistant","content":null,"tool_calls":[` +
				`{"id":"t","type":"function","function":{"name":"X","arguments":"{\"a\":\"<&>\"}"}}]}]}`,
		},
		{
			name: "tool results come before the turn's text",
			in: `{"messages":[{"role":"user","content":[{"type":"text","text":"also this"},` +
				`{"type":"tool_result","tool_use_id":"t1","content":"out1"},` +
				`{"type":"tool_result","tool_use_id":"t2","content":[{"type":"text","text":"a"},{"type":"text","text":"b"}]}]}]}`,
			want: `{"model":"m","messages":[{"role":"tool","tool_call_id":"t1","content":"out1"},` +
				`{"role":"tool","tool_call_id":"t2","content":"a\n\nb"},{"role":"user","content":"also this"}]}`,
		},
		{
			name: "tool result errors",
			in: `{"messages":[{"role":"user","content":[` +
				`{"type":"tool_result","tool_use_id":"t1","content":"file not found","is_error":true},` +
				`{"type":"tool_result","tool_use_id":"t2","content":"<tool_use_error>nope</tool_use_error>","is_error":true},` +
				`{"type":"tool_result","tool_use_id":"t3","content":"Error: already","is_error":true}]}]}`,
			want: `{"model":"m","messages":[{"role":"tool","tool_call_id":"t1","content":"Error: file not found"},` +
				`{"role":"tool","tool_call_id":"t2","content":"<tool_use_error>nope</tool_use_error>"},` +
				`{"role":"tool","tool_call_id":"t3","content":"Error: already"}]}`,
		},
		{
			name: "tool result image goes to a following user message",
			in: `{"messages":[{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":[` + img + `]},` +
				`{"type":"text","text":"next"}]}]}`,
			want: `{"model":"m","messages":[{"role":"tool","tool_call_id":"t1","content":"(image output follows)"},` +
				`{"role":"user","content":[{"type":"text","text":"[Image(s) from tool result t1]"},` +
				`{"type":"image_url","image_url":{"url":"data:image/png;base64,AAAA"}},{"type":"text","text":"next"}]}]}`,
		},
		{
			name: "documents and unknown blocks",
			in: `{"messages":[{"role":"user","content":[` +
				`{"type":"document","source":{"type":"text","media_type":"text/plain","data":"doc text"}},` +
				`{"type":"document","source":{"type":"base64","media_type":"application/pdf","data":"JVBE"}},` +
				`{"type":"mystery","x":1}]}]}`,
			want: `{"model":"m","messages":[{"role":"user","content":"doc text\n\n[document omitted]"}]}`,
		},
		{
			name: "tools, server tool dropped, tool_choice any, parallel off",
			in: `{"messages":[],"tools":[{"name":"Read","description":"read a file","input_schema":{"type":"object"},"cache_control":{"type":"ephemeral"}},` +
				`{"type":"web_search_20250305","name":"web_search","max_uses":5}],` +
				`"tool_choice":{"type":"any","disable_parallel_tool_use":true}}`,
			want: `{"model":"m","messages":[],"tools":[{"type":"function","function":{"name":"Read","description":"read a file","parameters":{"type":"object"}}}],` +
				`"tool_choice":"required","parallel_tool_calls":false}`,
		},
		{
			name: "only server tools: no tools, no tool_choice",
			in:   `{"messages":[],"tools":[{"type":"web_search_20250305","name":"web_search"}],"tool_choice":{"type":"auto","disable_parallel_tool_use":true}}`,
			want: `{"model":"m","messages":[]}`,
		},
		{
			name: "tool_choice auto",
			in:   `{"messages":[],"tools":[{"name":"A","input_schema":{}}],"tool_choice":{"type":"auto"}}`,
			want: `{"model":"m","messages":[],"tools":[{"type":"function","function":{"name":"A","parameters":{}}}],"tool_choice":"auto"}`,
		},
		{
			name: "tool_choice none",
			in:   `{"messages":[],"tools":[{"name":"A","input_schema":{}}],"tool_choice":{"type":"none"}}`,
			want: `{"model":"m","messages":[],"tools":[{"type":"function","function":{"name":"A","parameters":{}}}],"tool_choice":"none"}`,
		},
		{
			name: "tool_choice tool",
			in:   `{"messages":[],"tools":[{"name":"A","input_schema":{}}],"tool_choice":{"type":"tool","name":"A"}}`,
			want: `{"model":"m","messages":[],"tools":[{"type":"function","function":{"name":"A","parameters":{}}}],` +
				`"tool_choice":{"type":"function","function":{"name":"A"}}}`,
		},
		{
			name: "sampling, stop, stream, user; extras dropped",
			opts: oaOptions{MaxTokensField: "max_completion_tokens"},
			in: `{"model":"x","max_tokens":32000,"temperature":0.7,"top_p":0.95,"top_k":40,"stop_sequences":["END"],"stream":true,` +
				`"metadata":{"user_id":"u1"},"thinking":{"type":"enabled","budget_tokens":1000},"output_config":{"effort":"high"},` +
				`"context_management":{},"service_tier":"auto","messages":[]}`,
			want: `{"model":"m","max_completion_tokens":32000,"temperature":0.7,"top_p":0.95,"stop":["END"],"stream":true,` +
				`"stream_options":{"include_usage":true},"user":"u1","messages":[]}`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.opts.Model == "" {
				tt.opts.Model = "m"
			}
			got, err := toOpenAI([]byte(tt.in), tt.opts)
			if err != nil {
				t.Fatal(err)
			}
			if !jsonEqual(t, got, tt.want) {
				t.Errorf("got  %s\nwant %s", got, tt.want)
			}
		})
	}
}

func TestToOpenAIErrors(t *testing.T) {
	for _, in := range []string{`not json`, `{"messages":[{"role":"user","content":42}]}`} {
		if _, err := toOpenAI([]byte(in), oaOptions{Model: "m"}); err == nil {
			t.Errorf("%s: no error", in)
		}
	}
}

func TestToOpenAIDeterministic(t *testing.T) {
	in := []byte(`{"model":"x","max_tokens":5,"temperature":1,"stream":true,"metadata":{"user_id":"u"},` +
		`"system":[{"type":"text","text":"<sys> & co"}],"tools":[{"name":"B","input_schema":{"z":1,"a":2}},{"name":"A","input_schema":{}}],` +
		`"tool_choice":{"type":"tool","name":"A"},"messages":[{"role":"user","content":"hi"},` +
		`{"role":"assistant","content":[{"type":"tool_use","id":"t","name":"B","input":{"z":1,"a":{"y":2,"b":3}}}]},` +
		`{"role":"user","content":[{"type":"tool_result","tool_use_id":"t","content":"ok"}]}]}`)
	a, err := toOpenAI(in, oaOptions{Model: "m"})
	if err != nil {
		t.Fatal(err)
	}
	for range 20 {
		b, _ := toOpenAI(in, oaOptions{Model: "m"})
		if !bytes.Equal(a, b) {
			t.Fatalf("%s\n!=\n%s", a, b)
		}
	}
	if !bytes.Contains(a, []byte(`"<sys> & co"`)) {
		t.Errorf("HTML escaped: %s", a)
	}
}

func TestFromOpenAI(t *testing.T) {
	tests := []struct {
		name, in, want string
	}{
		{
			name: "text, tool call, cached usage",
			in: `{"id":"chatcmpl-1","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant",` +
				`"content":"Let me look.","reasoning_content":"secret","tool_calls":[` +
				`{"id":"call_1","type":"function","function":{"name":"Bash","arguments":"{\"command\": \"ls\"}"}},` +
				`{"type":"function","function":{"name":"Read","arguments":"not json"}}]},"finish_reason":"tool_calls"}],` +
				`"usage":{"prompt_tokens":100,"completion_tokens":20,"prompt_tokens_details":{"cached_tokens":80}}}`,
			want: `{"id":"msg_chatcmpl-1","type":"message","role":"assistant","model":"qwen","content":[` +
				`{"type":"text","text":"Let me look."},` +
				`{"type":"tool_use","id":"call_1","name":"Bash","input":{"command":"ls"}},` +
				`{"type":"tool_use","id":"*","name":"Read","input":{"_raw":"not json"}}],` +
				`"stop_reason":"tool_use","stop_sequence":null,` +
				`"usage":{"input_tokens":20,"output_tokens":20,"cache_creation_input_tokens":0,"cache_read_input_tokens":80}}`,
		},
		{
			name: "length, no usage",
			in:   `{"id":"x","choices":[{"message":{"content":"partial"},"finish_reason":"length"}]}`,
			want: `{"id":"msg_x","type":"message","role":"assistant","model":"qwen","content":[{"type":"text","text":"partial"}],` +
				`"stop_reason":"max_tokens","stop_sequence":null,` +
				`"usage":{"input_tokens":0,"output_tokens":0,"cache_creation_input_tokens":0,"cache_read_input_tokens":0}}`,
		},
		{
			name: "empty content, vLLM stop string",
			in: `{"id":"y","choices":[{"message":{"content":""},"finish_reason":"stop","stop_reason":"END"}],` +
				`"usage":{"prompt_tokens":5,"completion_tokens":1}}`,
			want: `{"id":"msg_y","type":"message","role":"assistant","model":"qwen","content":[],` +
				`"stop_reason":"stop_sequence","stop_sequence":"END",` +
				`"usage":{"input_tokens":5,"output_tokens":1,"cache_creation_input_tokens":0,"cache_read_input_tokens":0}}`,
		},
		{
			name: "content filter, odd tool id",
			in: `{"id":"z","choices":[{"message":{"content":null,"tool_calls":[{"id":"functions.Bash:0","function":{"name":"Bash",` +
				`"arguments":"\"{\\\"a\\\":1}\""}},{"id":"c2","function":{"name":"Nop","arguments":""}}]},"finish_reason":"content_filter"}]}`,
			want: `{"id":"msg_z","type":"message","role":"assistant","model":"qwen","content":[` +
				`{"type":"tool_use","id":"functions_Bash_0","name":"Bash","input":{"a":1}},{"type":"tool_use","id":"c2","name":"Nop","input":{}}],` +
				`"stop_reason":"refusal","stop_sequence":null,` +
				`"usage":{"input_tokens":0,"output_tokens":0,"cache_creation_input_tokens":0,"cache_read_input_tokens":0}}`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := fromOpenAI([]byte(tt.in), "qwen")
			if err != nil {
				t.Fatal(err)
			}
			// Generated tool ids are random: check their shape, then mask them.
			var v map[string]any
			_ = json.Unmarshal(got, &v)
			for _, c := range v["content"].([]any) {
				b := c.(map[string]any)
				if id, ok := b["id"].(string); ok && strings.HasPrefix(id, "toolu_") && len(id) > 10 {
					b["id"] = "*"
				}
			}
			masked, _ := json.Marshal(v)
			if !jsonEqual(t, masked, tt.want) {
				t.Errorf("got  %s\nwant %s", got, tt.want)
			}
		})
	}
}

func TestFromOpenAIErrors(t *testing.T) {
	for _, in := range []string{`nope`, `{"error":{"message":"boom"}}`, `{"id":"x","choices":[]}`} {
		if _, err := fromOpenAI([]byte(in), "m"); err == nil {
			t.Errorf("%s: no error", in)
		}
	}
	id := func() string {
		b, err := fromOpenAI([]byte(`{"choices":[{"message":{"content":"a"}}]}`), "m")
		if err != nil {
			t.Fatal(err)
		}
		var v struct{ ID string }
		_ = json.Unmarshal(b, &v)
		return v.ID
	}
	if a, b := id(), id(); !strings.HasPrefix(a, "msg_") || len(a) < 10 || a == b {
		t.Errorf("generated ids %q %q", a, b)
	}
}

type sseEvent struct {
	name string
	data map[string]any
}

func parseSSE(t *testing.T, out string) []sseEvent {
	t.Helper()
	if !strings.HasSuffix(out, "\n\n") {
		t.Errorf("output does not end with a blank line: %q", out)
	}
	var evs []sseEvent
	for _, chunk := range strings.Split(strings.TrimSuffix(out, "\n\n"), "\n\n") {
		lines := strings.Split(chunk, "\n")
		if len(lines) != 2 || !strings.HasPrefix(lines[0], "event: ") || !strings.HasPrefix(lines[1], "data: ") {
			t.Fatalf("bad event %q", chunk)
		}
		e := sseEvent{name: strings.TrimPrefix(lines[0], "event: ")}
		if err := json.Unmarshal([]byte(strings.TrimPrefix(lines[1], "data: ")), &e.data); err != nil {
			t.Fatalf("bad data %q: %v", lines[1], err)
		}
		if e.data["type"] != e.name {
			t.Errorf("event %s has data type %v", e.name, e.data["type"])
		}
		evs = append(evs, e)
	}
	return evs
}

type block struct {
	typ, text, id, name, input string
	stopped                    bool
}

// assemble replays content block events, checking indices count up from 0
// and that blocks are opened and closed in order.
func assemble(t *testing.T, evs []sseEvent) []*block {
	t.Helper()
	var blocks []*block
	open := -1
	for _, e := range evs {
		idx := -1
		if f, ok := e.data["index"].(float64); ok {
			idx = int(f)
		}
		switch e.name {
		case "content_block_start":
			if open != -1 || idx != len(blocks) {
				t.Fatalf("start of block %d while %d open, %d blocks", idx, open, len(blocks))
			}
			cb := e.data["content_block"].(map[string]any)
			b := &block{typ: cb["type"].(string)}
			if b.typ == "tool_use" {
				b.id, b.name = cb["id"].(string), cb["name"].(string)
				if in, _ := json.Marshal(cb["input"]); string(in) != "{}" {
					t.Errorf("tool_use start input %s", in)
				}
			}
			blocks = append(blocks, b)
			open = idx
		case "content_block_delta":
			if idx != open {
				t.Fatalf("delta for block %d while %d open", idx, open)
			}
			d := e.data["delta"].(map[string]any)
			switch d["type"] {
			case "text_delta":
				if d["text"] == "" {
					t.Error("empty text delta")
				}
				blocks[idx].text += d["text"].(string)
			case "input_json_delta":
				blocks[idx].input += d["partial_json"].(string)
			default:
				t.Errorf("delta type %v", d["type"])
			}
		case "content_block_stop":
			if idx != open {
				t.Fatalf("stop for block %d while %d open", idx, open)
			}
			blocks[idx].stopped = true
			open = -1
		}
	}
	if open != -1 {
		t.Errorf("block %d left open", open)
	}
	for i, b := range blocks {
		if b.typ == "text" && b.text == "" {
			t.Errorf("block %d: empty text block", i)
		}
	}
	return blocks
}

func names(evs []sseEvent) string {
	var s []string
	for _, e := range evs {
		s = append(s, e.name)
	}
	return strings.Join(s, " ")
}

// data formats OpenAI chunks as an SSE stream body.
func data(chunks ...string) string {
	var b strings.Builder
	for _, c := range chunks {
		b.WriteString("data: " + c + "\n\n")
	}
	return b.String()
}

func TestStreamFromOpenAI(t *testing.T) {
	const (
		start = "message_start"
		bs    = "content_block_start"
		bd    = "content_block_delta"
		bstop = "content_block_stop"
		end   = "message_delta message_stop"
	)
	type wantBlock struct{ typ, text, name, input string }
	tests := []struct {
		name    string
		in      string
		events  string // event names, space separated
		blocks  []wantBlock
		stop    string
		usage   map[string]float64
		wantErr bool
		ids     []string // expected tool ids; "*" = generated
	}{
		{
			name: "text only, CRLF, comments, no space after data:, event lines",
			in: ": keep-alive\r\n\r\n" +
				"event: chunk\r\ndata:{\"id\":\"c1\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"\"}}]}\r\n\r\n" +
				"data: {\"id\":\"c1\",\"choices\":[{\"index\":0,\"delta\":{\"reasoning_content\":\"think\"}}]}\r\n\r\n" +
				"data: {\"id\":\"c1\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"Hel\"}}]}\r\n\r\n" +
				"data: {\"id\":\"c1\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"lo\"},\"finish_reason\":null}]}\r\n\r\n" +
				"data: {\"id\":\"c1\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\r\n\r\n" +
				"data: [DONE]\r\n\r\n",
			events: strings.Join([]string{start, bs, bd, bd, bstop, end}, " "),
			blocks: []wantBlock{{typ: "text", text: "Hello"}},
			stop:   "end_turn",
		},
		{
			name: "text then two tool calls with fragmented arguments, usage-only final chunk",
			in: data(
				`{"id":"c2","choices":[{"index":0,"delta":{"content":"Checking."}}]}`,
				`{"id":"c2","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_a","type":"function","function":{"name":"Bash","arguments":""}}]}}]}`,
				`{"id":"c2","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"command\":"}}]}}]}`,
				`{"id":"c2","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"ls\"}"}}]}}]}`,
				`{"id":"c2","choices":[{"index":0,"delta":{"tool_calls":[{"index":1,"id":"call_b","type":"function","function":{"name":"Read","arguments":"{\"file"}}]}}]}`,
				`{"id":"c2","choices":[{"index":0,"delta":{"tool_calls":[{"index":1,"function":{"arguments":"_path\":\"/x\"}"}}]}}]}`,
				`{"id":"c2","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
				`{"id":"c2","choices":[],"usage":{"prompt_tokens":1000,"completion_tokens":30,"total_tokens":1030,"prompt_tokens_details":{"cached_tokens":900}}}`,
				`[DONE]`),
			events: strings.Join([]string{start, bs, bd, bstop, bs, bd, bd, bstop, bs, bd, bd, bstop, end}, " "),
			blocks: []wantBlock{
				{typ: "text", text: "Checking."},
				{typ: "tool_use", name: "Bash", input: `{"command":"ls"}`},
				{typ: "tool_use", name: "Read", input: `{"file_path":"/x"}`},
			},
			ids:   []string{"call_a", "call_b"},
			stop:  "tool_use",
			usage: map[string]float64{"input_tokens": 100, "output_tokens": 30, "cache_read_input_tokens": 900},
		},
		{
			name: "name after an argument fragment, no ids, whole call in one fragment, text after tools",
			in: data(
				`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"a\""}}]}}]}`,
				`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"name":"Grep"}}]}}]}`,
				`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":":1}"}}]}}]}`,
				`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":1,"function":{"name":"Glob","arguments":"{\"p\":\"*\"}"}}]}}]}`,
				`{"choices":[{"index":0,"delta":{"content":"done"},"finish_reason":"stop"}]}`),
			events: strings.Join([]string{start, bs, bd, bd, bstop, bs, bd, bstop, bs, bd, bstop, end}, " "),
			blocks: []wantBlock{
				{typ: "tool_use", name: "Grep", input: `{"a":1}`},
				{typ: "tool_use", name: "Glob", input: `{"p":"*"}`},
				{typ: "text", text: "done"},
			},
			ids:  []string{"*", "*"},
			stop: "tool_use",
		},
		{
			name: "no index, every call index 0 with distinct ids, name-only call, repeated names",
			in: data(
				`{"choices":[{"index":0,"delta":{"tool_calls":[{"id":"x1","function":{"name":"A","arguments":"{}"}}]}}]}`,
				`{"choices":[{"index":0,"delta":{"tool_calls":[{"id":"x2","function":{"name":"B","arguments":"{\"q\":"}}]}}]}`,
				`{"choices":[{"index":0,"delta":{"tool_calls":[{"id":"x2","function":{"name":"B","arguments":"2}"}}]}}]}`,
				`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"x3","function":{"name":"C","arguments":"{}"}},{"index":0,"id":"x4","function":{"name":"D"}}]}}]}`,
				`{"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`),
			events: strings.Join([]string{start, bs, bd, bstop, bs, bd, bd, bstop, bs, bd, bstop, bs, bstop, end}, " "),
			blocks: []wantBlock{
				{typ: "tool_use", name: "A", input: `{}`},
				{typ: "tool_use", name: "B", input: `{"q":2}`},
				{typ: "tool_use", name: "C", input: `{}`},
				{typ: "tool_use", name: "D", input: ``},
			},
			ids:  []string{"x1", "x2", "x3", "x4"},
			stop: "tool_use",
		},
		{
			name: "no finish_reason, no [DONE], open tool block",
			in: data(
				`{"id":"c5","choices":[{"index":0,"delta":{"content":"hi "}}]}`,
				`{"id":"c5","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"k","function":{"name":"T","arguments":"{\"x\":1}"}}]}}]}`),
			events: strings.Join([]string{start, bs, bd, bstop, bs, bd, bstop, end}, " "),
			blocks: []wantBlock{{typ: "text", text: "hi "}, {typ: "tool_use", name: "T", input: `{"x":1}`}},
			ids:    []string{"k"},
			stop:   "tool_use",
		},
		{
			name:   "no finish_reason, text only, usage on the last content chunk",
			in:     data(`{"choices":[{"index":0,"delta":{"content":"x"}}],"usage":{"prompt_tokens":3,"completion_tokens":1}}`),
			events: strings.Join([]string{start, bs, bd, bstop, end}, " "),
			blocks: []wantBlock{{typ: "text", text: "x"}},
			stop:   "end_turn",
			usage:  map[string]float64{"input_tokens": 3, "output_tokens": 1, "cache_read_input_tokens": 0},
		},
		{
			name:   "length",
			in:     data(`{"choices":[{"index":0,"delta":{"content":"x"},"finish_reason":"length"}]}`, `[DONE]`),
			events: strings.Join([]string{start, bs, bd, bstop, end}, " "),
			blocks: []wantBlock{{typ: "text", text: "x"}},
			stop:   "max_tokens",
		},
		{
			name: "mid-stream error",
			in: data(`{"choices":[{"index":0,"delta":{"content":"x"}}]}`,
				`{"error":{"message":"model crashed","type":"server_error"}}`,
				`{"choices":[{"index":0,"delta":{"content":"never"}}]}`),
			events:  strings.Join([]string{start, bs, bd, "error"}, " "),
			wantErr: true,
		},
		{
			name:    "error event with plain data",
			in:      "event: error\ndata: {\"message\":\"overloaded\"}\n\n",
			events:  "error",
			wantErr: true,
		},
		{
			name:    "empty stream",
			in:      "",
			events:  "error",
			wantErr: true,
		},
		{
			name:    "garbage chunk",
			in:      data(`{"choices":[]}`, `{not json`),
			events:  strings.Join([]string{start, "error"}, " "),
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var out bytes.Buffer
			flushes := 0
			err := streamFromOpenAI(&out, func() { flushes++ }, strings.NewReader(tt.in), "qwen")
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
			evs := parseSSE(t, out.String())
			if got := names(evs); got != tt.events {
				t.Fatalf("events\n got %s\nwant %s\n%s", got, tt.events, out.String())
			}
			if flushes != len(evs) {
				t.Errorf("%d flushes for %d events", flushes, len(evs))
			}
			if tt.wantErr {
				last := evs[len(evs)-1].data["error"].(map[string]any)
				if last["type"] != "api_error" || last["message"] == "" {
					t.Errorf("error event %v", last)
				}
				return
			}
			msg := evs[0].data["message"].(map[string]any)
			if msg["model"] != "qwen" || !strings.HasPrefix(msg["id"].(string), "msg_") || msg["role"] != "assistant" {
				t.Errorf("message_start %v", msg)
			}
			blocks := assemble(t, evs)
			if len(blocks) != len(tt.blocks) {
				t.Fatalf("%d blocks, want %d", len(blocks), len(tt.blocks))
			}
			var ids []string
			for i, w := range tt.blocks {
				b := blocks[i]
				if b.typ != w.typ || b.text != w.text || b.name != w.name || b.input != w.input {
					t.Errorf("block %d = %+v, want %+v", i, *b, w)
				}
				if b.typ == "tool_use" {
					id := b.id
					if strings.HasPrefix(id, "toolu_") && len(id) > 10 {
						id = "*"
					}
					ids = append(ids, id)
				}
			}
			if !reflect.DeepEqual(ids, tt.ids) {
				t.Errorf("tool ids %v, want %v", ids, tt.ids)
			}
			md := evs[len(evs)-2].data
			if sr := md["delta"].(map[string]any)["stop_reason"]; sr != tt.stop {
				t.Errorf("stop_reason %v, want %s", sr, tt.stop)
			}
			u := md["usage"].(map[string]any)
			for k, v := range tt.usage {
				if u[k] != v {
					t.Errorf("usage %s = %v, want %v", k, u[k], v)
				}
			}
		})
	}
}

func TestStreamLongLine(t *testing.T) {
	long := strings.Repeat("x", 1<<20)
	in := data(`{"choices":[{"index":0,"delta":{"content":"`+long+`"},"finish_reason":"stop"}]}`, `[DONE]`)
	var out bytes.Buffer
	if err := streamFromOpenAI(&out, func() {}, strings.NewReader(in), "m"); err != nil {
		t.Fatal(err)
	}
	blocks := assemble(t, parseSSE(t, out.String()))
	if len(blocks) != 1 || blocks[0].text != long {
		t.Errorf("long text lost")
	}
}

type failWriter struct{}

func (failWriter) Write([]byte) (int, error) { return 0, errors.New("client gone") }

func TestStreamWriteError(t *testing.T) {
	in := data(`{"choices":[{"index":0,"delta":{"content":"x"}}]}`, `[DONE]`)
	if err := streamFromOpenAI(failWriter{}, func() {}, strings.NewReader(in), "m"); err == nil {
		t.Error("no error")
	}
}

func TestOpenAIError(t *testing.T) {
	tests := []struct {
		status        int
		body          string
		typ, message  string
		messagePrefix bool
	}{
		{400, `{"error":{"message":"bad tool schema","type":"invalid_request_error","code":null}}`, "invalid_request_error", "bad tool schema", false},
		{401, `{"error":"invalid api key"}`, "authentication_error", "invalid api key", false},
		{403, `{"detail":"forbidden"}`, "permission_error", "forbidden", false},
		{404, `{"object":"error","message":"The model foo does not exist.","type":"NotFoundError","code":404}`, "not_found_error", "The model foo does not exist.", false},
		{413, `payload too large`, "request_too_large", "payload too large", false},
		{429, `[{"error":{"code":429,"message":"Resource exhausted","status":"RESOURCE_EXHAUSTED"}}]`, "rate_limit_error", "Resource exhausted", false},
		{503, "<html><body>Service Unavailable</body></html>\n", "overloaded_error", "<html><body>Service Unavailable</body></html>", false},
		{529, ``, "overloaded_error", "upstream returned 529", true},
		{500, `{"error":{"code":500}}`, "api_error", `{"code":500}`, false},
		{422, `{"detail":[{"loc":["body","messages"],"msg":"field required"}]}`, "api_error", "field required", false},
	}
	for _, tt := range tests {
		got := openAIError(tt.status, []byte(tt.body))
		var v struct {
			Type  string `json:"type"`
			Error struct {
				Type    string `json:"type"`
				Message string `json:"message"`
			} `json:"error"`
		}
		if err := json.Unmarshal(got, &v); err != nil {
			t.Fatalf("%d: %v: %s", tt.status, err, got)
		}
		okMsg := v.Error.Message == tt.message || tt.messagePrefix && strings.HasPrefix(v.Error.Message, tt.message)
		if v.Type != "error" || v.Error.Type != tt.typ || !okMsg {
			t.Errorf("%d %s: got %s", tt.status, tt.body, got)
		}
	}
}
