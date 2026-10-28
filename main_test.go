package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
)

type seen struct {
	path   string
	header http.Header
	body   []byte
}

func recorder(t *testing.T, got *seen) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got.path = r.URL.Path
		got.header = r.Header.Clone()
		got.body, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func setup(t *testing.T) (h http.Handler, anthropic, local *seen) {
	t.Helper()
	anthropic, local = &seen{}, &seen{}
	defURL, _ := url.Parse(recorder(t, anthropic).URL)
	var rs routes
	if err := rs.Set("qwen=" + recorder(t, local).URL + ",Qwen/Qwen3.8-Flash-Next"); err != nil {
		t.Fatal(err)
	}
	return newHandler(defURL, rs, ""), anthropic, local
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
	rt := &route{model: "m"}
	in := []byte(`{"z":1,"model":"qwen","a":{"y":2,"x":[3,"<&>"]}}`)
	a, err := adapt(in, rt)
	if err != nil {
		t.Fatal(err)
	}
	for range 10 {
		b, _ := adapt(in, rt)
		if !bytes.Equal(a, b) {
			t.Fatalf("%s != %s", a, b)
		}
	}
}
