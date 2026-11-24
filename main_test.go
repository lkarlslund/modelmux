package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type seen struct {
	path   string
	header http.Header
	body   []byte
}

// recorder is an upstream that records the request and answers with reply
// (default {"ok":true}) and content type ct (default application/json).
func recorder(t *testing.T, got *seen, ct, reply string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got.path = r.URL.Path
		got.header = r.Header.Clone()
		got.body, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", firstNonEmpty(ct, "application/json"))
		_, _ = io.WriteString(w, firstNonEmpty(reply, `{"ok":true}`))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func newTestProxy(t *testing.T, cfg *Config) http.Handler {
	t.Helper()
	if err := cfg.validate(); err != nil {
		t.Fatal(err)
	}
	p, err := newProxy(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func setup(t *testing.T) (h http.Handler, anthropic, local *seen) {
	t.Helper()
	anthropic, local = &seen{}, &seen{}
	cfg := &Config{
		Anthropic: recorder(t, anthropic, "", "").URL,
		Models: []*Model{{
			ID: "qwen3.8-flash-next", Match: []string{"qwen*"},
			Upstream: recorder(t, local, "", "").URL, UpstreamModel: "Qwen/Qwen3.8-Flash-Next",
		}},
	}
	return newTestProxy(t, cfg), anthropic, local
}

func post(h http.Handler, path, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewBufferString(body))
	req.Header.Set("Authorization", "Bearer secret")
	req.Header.Set("X-Api-Key", "secret")
	req.Header.Set("Anthropic-Beta", "oauth-2025-04-20")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w
}

func TestPassthroughIsVerbatim(t *testing.T) {
	h, anthropic, local := setup(t)
	body := `{"model":"claude-opus-5-5", "messages":[],"tools":[{"type":"web_search_20250305","name":"web_search"}]}`
	if w := post(h, "/v1/messages?beta=true", body); w.Code != http.StatusOK {
		t.Fatalf("status %d", w.Code)
	}
	if string(anthropic.body) != body {
		t.Errorf("body changed:\n%s", anthropic.body)
	}
	if anthropic.path != "/v1/messages" {
		t.Errorf("path %q", anthropic.path)
	}
	if anthropic.header.Get("Authorization") != "Bearer secret" || anthropic.header.Get("X-Api-Key") != "secret" ||
		anthropic.header.Get("Anthropic-Beta") != "oauth-2025-04-20" {
		t.Errorf("headers not forwarded: %v", anthropic.header)
	}
	if v := anthropic.header.Get("X-Forwarded-For"); v != "" {
		t.Errorf("X-Forwarded-For added: %q", v)
	}
	if local.path != "" {
		t.Error("local upstream was called")
	}
}

func TestOtherPathsPassThrough(t *testing.T) {
	h, anthropic, _ := setup(t)
	req := httptest.NewRequest(http.MethodGet, "/api/oauth/profile", nil)
	req.Header.Set("Authorization", "Bearer secret")
	h.ServeHTTP(httptest.NewRecorder(), req)
	if anthropic.path != "/api/oauth/profile" || anthropic.header.Get("Authorization") != "Bearer secret" {
		t.Errorf("got %q %v", anthropic.path, anthropic.header)
	}
}

func TestHealthIsLocal(t *testing.T) {
	h, anthropic, _ := setup(t)
	req := httptest.NewRequest(http.MethodGet, "/modelmux/health", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "qwen3.8-flash-next") || anthropic.path != "" {
		t.Errorf("%d %s (anthropic saw %q)", w.Code, w.Body, anthropic.path)
	}
}

func TestLocalRoute(t *testing.T) {
	h, anthropic, local := setup(t)
	body := `{"model":"qwen3.8-flash-next","messages":[],"tools":[` +
		`{"name":"Read","input_schema":{"type":"object"}},` +
		`{"type":"web_search_20250305","name":"web_search"}]}`
	if w := post(h, "/v1/messages", body); w.Code != http.StatusOK {
		t.Fatalf("status %d", w.Code)
	}
	if anthropic.path != "" {
		t.Error("anthropic upstream was called")
	}
	if local.header.Get("Authorization") != "" || local.header.Get("X-Api-Key") != "" {
		t.Errorf("credentials leaked to local upstream: %v", local.header)
	}
	var req struct {
		Model string           `json:"model"`
		Tools []map[string]any `json:"tools"`
	}
	if err := json.Unmarshal(local.body, &req); err != nil {
		t.Fatal(err)
	}
	if req.Model != "Qwen/Qwen3.8-Flash-Next" {
		t.Errorf("model %q", req.Model)
	}
	if len(req.Tools) != 1 || req.Tools[0]["name"] != "Read" {
		t.Errorf("tools %v", req.Tools)
	}
}

func TestGlobAndExactMatch(t *testing.T) {
	cfg := &Config{Models: []*Model{
		{ID: "a", Match: []string{"qwen*"}, Upstream: "http://x"},
		{ID: "qwen-special", Upstream: "http://y"},
	}}
	if err := cfg.validate(); err != nil {
		t.Fatal(err)
	}
	for in, want := range map[string]string{"QWEN3": "a", "qwen-special": "qwen-special", "a[1m]": "a", "claude-opus-5-5": ""} {
		got := ""
		if m := cfg.lookup(in); m != nil {
			got = m.ID
		}
		if got != want {
			t.Errorf("lookup(%q) = %q, want %q", in, got, want)
		}
	}
}

// The model a foreign server reports back is replaced by the ID Claude Code
// asked for, in plain and in streamed responses.
func TestPresentedModel(t *testing.T) {
	stream := "event: message_start\r\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"m\",\"model\":\"upstream/name\",\"content\":[]}}\r\n\r\n" +
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"model upstream/name\"}}\n\n"
	for _, tc := range []struct{ ct, reply, want, keep string }{
		{"application/json", `{"id":"m","model":"upstream/name","content":[]}`, `"model":"shown-as"`, ""},
		{"text/event-stream", stream, `"model":"shown-as"`, `"text":"model upstream/name"`},
	} {
		var local seen
		cfg := &Config{Models: []*Model{{ID: "shown-as", Upstream: recorder(t, &local, tc.ct, tc.reply).URL, UpstreamModel: "upstream/name"}}}
		w := post(newTestProxy(t, cfg), "/v1/messages", `{"model":"shown-as","messages":[]}`)
		out := w.Body.String()
		if !strings.Contains(out, tc.want) || strings.Contains(out, `"model":"upstream/name"`) || !strings.Contains(out, tc.keep) {
			t.Errorf("%s: got %s", tc.ct, out)
		}
		if !strings.Contains(string(local.body), `"model":"upstream/name"`) {
			t.Errorf("upstream got %s", local.body)
		}
	}
}

func TestUpstreamKey(t *testing.T) {
	t.Setenv("TEST_KEY", "k123")
	for _, tc := range []struct{ api, auth, header, want string }{
		{"anthropic", "", "X-Api-Key", "k123"},
		{"anthropic", "bearer", "Authorization", "Bearer k123"},
	} {
		var local seen
		cfg := &Config{Models: []*Model{{ID: "m", API: tc.api, Auth: tc.auth, KeyEnv: "TEST_KEY",
			Upstream: recorder(t, &local, "", "").URL, Headers: map[string]string{"X-Extra": "1"}}}}
		post(newTestProxy(t, cfg), "/v1/messages", `{"model":"m","messages":[]}`)
		if got := local.header.Get(tc.header); got != tc.want {
			t.Errorf("%s/%s: %s = %q", tc.api, tc.auth, tc.header, got)
		}
		if local.header.Get("X-Extra") != "1" || strings.Contains(local.header.Get("Authorization")+local.header.Get("X-Api-Key"), "secret") {
			t.Errorf("headers %v", local.header)
		}
	}
	cfg := &Config{Models: []*Model{{ID: "m", KeyEnv: "TEST_KEY_UNSET", Upstream: "http://127.0.0.1:1"}}}
	if w := post(newTestProxy(t, cfg), "/v1/messages", `{"model":"m"}`); w.Code != 500 || !strings.Contains(w.Body.String(), "TEST_KEY_UNSET") {
		t.Errorf("missing key: %d %s", w.Code, w.Body)
	}
}

func TestStripAndClamp(t *testing.T) {
	m := &Model{ID: "m", Upstream: "http://x", Strip: []string{"thinking"}, MaxTokens: 1000}
	if err := m.validate(); err != nil {
		t.Fatal(err)
	}
	out, err := adapt([]byte(`{"model":"m","max_tokens":32000,"thinking":{"type":"enabled"},"messages":[]}`), m)
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != `{"max_tokens":1000,"messages":[],"model":"m"}` {
		t.Errorf("got %s", out)
	}
}

func TestFoldSystemMessages(t *testing.T) {
	in := `[{"role":"user","content":[{"type":"text","text":"task"}]},` +
		`{"role":"system","content":"env"},` +
		`{"role":"assistant","content":"ok"},` +
		`{"role":"system","content":[{"type":"text","text":"later"}]}]`
	out, err := foldSystemMessages(json.RawMessage(in))
	if err != nil {
		t.Fatal(err)
	}
	var msgs []message
	if err := json.Unmarshal(out, &msgs); err != nil {
		t.Fatal(err)
	}
	want := []struct{ role, content string }{
		{"user", `[{"type":"text","text":"task"},{"type":"text","text":"env"}]`},
		{"assistant", `"ok"`},
		{"user", `[{"type":"text","text":"later"}]`},
	}
	if len(msgs) != len(want) {
		t.Fatalf("got %s", out)
	}
	for i, w := range want {
		if msgs[i].Role != w.role || string(msgs[i].Content) != w.content {
			t.Errorf("message %d: %s %s", i, msgs[i].Role, msgs[i].Content)
		}
	}
}

func TestAdaptIsDeterministic(t *testing.T) {
	m := &Model{ID: "qwen", UpstreamModel: "m"}
	in := []byte(`{"z":1,"model":"qwen","a":{"y":2,"x":[3,"<&>"]}}`)
	a, err := adapt(in, m)
	if err != nil {
		t.Fatal(err)
	}
	for range 10 {
		b, _ := adapt(in, m)
		if !bytes.Equal(a, b) {
			t.Fatalf("%s != %s", a, b)
		}
	}
}

func TestRouteFlag(t *testing.T) {
	m, err := routeFlag("qwen=http://127.0.0.1:8003,Qwen/Qwen3.8-Flash-Next")
	if err != nil {
		t.Fatal(err)
	}
	cfg := &Config{Models: []*Model{m}}
	if err := cfg.validate(); err != nil {
		t.Fatal(err)
	}
	if got := cfg.lookup("qwen3.8-flash-next"); got != m || m.UpstreamModel != "Qwen/Qwen3.8-Flash-Next" {
		t.Errorf("lookup %v, upstream model %q", got, m.UpstreamModel)
	}
}

func TestUnreachableUpstreamIsAnAPIError(t *testing.T) {
	h := newTestProxy(t, &Config{Models: []*Model{{ID: "m", Upstream: "http://127.0.0.1:1"}}})
	w := post(h, "/v1/messages", `{"model":"m","messages":[]}`)
	if w.Code != 502 || !strings.Contains(w.Body.String(), `"type":"api_error"`) || !strings.Contains(w.Body.String(), "127.0.0.1:1") {
		t.Errorf("%d %s", w.Code, w.Body)
	}
}
