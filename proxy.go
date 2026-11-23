package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"
)

// maxBody matches the Messages API request limit.
const maxBody = 32 << 20

type modelKey struct{}

type proxy struct {
	cfg         *Config
	passthrough *httputil.ReverseProxy // Claude models: api.anthropic.com, untouched
	anthropic   *httputil.ReverseProxy // foreign models on an Anthropic-compatible API
	client      *http.Client           // foreign models on an OpenAI-compatible API
	seq         atomic.Int64
}

func newProxy(cfg *Config) (*proxy, error) {
	def, err := url.Parse(cfg.Anthropic)
	if err != nil {
		return nil, fmt.Errorf("anthropic: %w", err)
	}
	p := &proxy{cfg: cfg, client: &http.Client{}}
	// Rewrite (not Director) adds no X-Forwarded-* headers: the default
	// upstream sees the client's request as sent.
	p.passthrough = &httputil.ReverseProxy{
		Rewrite:       func(pr *httputil.ProxyRequest) { pr.SetURL(def) },
		FlushInterval: -1,
	}
	p.anthropic = &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			m := pr.In.Context().Value(modelKey{}).(*Model)
			pr.SetURL(m.upstream)
			setUpstreamHeaders(pr.Out.Header, m)
		},
		ModifyResponse: func(resp *http.Response) error {
			m := resp.Request.Context().Value(modelKey{}).(*Model)
			return presentModel(resp, m.ID)
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			m := r.Context().Value(modelKey{}).(*Model)
			log.Printf("%s: %v", m.ID, err)
			apiError(w, http.StatusBadGateway, "api_error", fmt.Sprintf("ccroute: %s at %s: %v", m.ID, m.upstream.Host, err))
		},
		FlushInterval: -1,
	}
	return p, nil
}

// setUpstreamHeaders replaces the client's Anthropic credentials with the
// model's own (if any): Claude credentials never leave for a foreign server.
func setUpstreamHeaders(h http.Header, m *Model) {
	h.Del("Authorization")
	h.Del("X-Api-Key")
	h.Del("Accept-Encoding") // responses may be rewritten
	if k, _ := m.key(); k != "" {
		if m.Auth == "bearer" {
			h.Set("Authorization", "Bearer "+k)
		} else {
			h.Set("X-Api-Key", k)
		}
	}
	for k, v := range m.Headers {
		h.Set(k, v)
	}
}

func (p *proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/ccroute/health" {
		p.health(w)
		return
	}
	if r.Method != http.MethodPost || (r.URL.Path != "/v1/messages" && r.URL.Path != "/v1/messages/count_tokens") {
		p.passthrough.ServeHTTP(w, r)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxBody+1))
	if err != nil {
		apiError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	if len(body) > maxBody {
		apiError(w, http.StatusRequestEntityTooLarge, "request_too_large", "request body too large")
		return
	}
	var head struct {
		Model  string `json:"model"`
		Stream bool   `json:"stream"`
	}
	_ = json.Unmarshal(body, &head)

	m := p.cfg.lookup(head.Model)
	if m == nil {
		r.Body = io.NopCloser(bytes.NewReader(body))
		r.ContentLength = int64(len(body))
		p.passthrough.ServeHTTP(w, r)
		return
	}
	if _, err := m.key(); err != nil {
		apiError(w, http.StatusInternalServerError, "api_error", "ccroute: "+err.Error())
		return
	}
	countTokens := strings.HasSuffix(r.URL.Path, "/count_tokens")
	start := time.Now()
	defer func() {
		log.Printf("%s %s -> %s %s (%s)", r.URL.Path, head.Model, m.API, m.upstream.Host, time.Since(start).Round(time.Millisecond))
	}()

	if m.API == "openai" {
		p.serveOpenAI(w, r, m, body, head.Stream, countTokens)
		return
	}
	body, err = adapt(body, m)
	if err != nil {
		apiError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	p.dump(body)
	r.Body = io.NopCloser(bytes.NewReader(body))
	r.ContentLength = int64(len(body))
	p.anthropic.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), modelKey{}, m)))
}

func (p *proxy) serveOpenAI(w http.ResponseWriter, r *http.Request, m *Model, body []byte, stream, countTokens bool) {
	if countTokens {
		// Claude Code falls back to its own estimate.
		apiError(w, http.StatusNotFound, "not_found_error", "count_tokens is not available for "+m.ID)
		return
	}
	body, err := prepareOpenAI(body, m)
	if err == nil {
		body, err = toOpenAI(body, oaOptions{Model: m.UpstreamModel, MaxTokensField: m.MaxTokensKey})
	}
	if err != nil {
		apiError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	p.dump(body)
	u := m.upstream.JoinPath("chat/completions")
	req, err := http.NewRequestWithContext(r.Context(), http.MethodPost, u.String(), bytes.NewReader(body))
	if err != nil {
		apiError(w, http.StatusInternalServerError, "api_error", err.Error())
		return
	}
	req.Header.Set("Content-Type", "application/json")
	if stream {
		req.Header.Set("Accept", "text/event-stream")
	}
	setUpstreamHeaders(req.Header, m)
	resp, err := p.client.Do(req)
	if err != nil {
		apiError(w, http.StatusBadGateway, "api_error", "ccroute: "+err.Error())
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		writeJSON(w, resp.StatusCode, openAIError(resp.StatusCode, b))
		return
	}
	if stream && strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.WriteHeader(http.StatusOK)
		rc := http.NewResponseController(w)
		if err := streamFromOpenAI(w, func() { _ = rc.Flush() }, resp.Body, m.ID); err != nil {
			log.Printf("%s: stream: %v", m.ID, err)
		}
		return
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err == nil {
		b, err = fromOpenAI(b, m.ID)
	}
	if err != nil {
		apiError(w, http.StatusBadGateway, "api_error", "ccroute: upstream response: "+err.Error())
		return
	}
	if stream {
		// The upstream ignored stream:true; Claude Code still needs events.
		writeMessageAsStream(w, b)
		return
	}
	writeJSON(w, http.StatusOK, b)
}

func (p *proxy) health(w http.ResponseWriter) {
	type row struct {
		ID, API, Upstream, UpstreamModel string
		KeyMissing                       bool `json:",omitempty"`
	}
	rows := []row{}
	for _, m := range p.cfg.Models {
		_, err := m.key()
		rows = append(rows, row{m.ID, m.API, m.Upstream, m.UpstreamModel, err != nil})
	}
	b, _ := json.Marshal(map[string]any{"ok": true, "models": rows})
	writeJSON(w, http.StatusOK, b)
}

func (p *proxy) dump(body []byte) {
	if p.cfg.Dump == "" {
		return
	}
	name := fmt.Sprintf("%s-%04d.json", time.Now().Format("20060102-150405"), p.seq.Add(1))
	if err := os.WriteFile(filepath.Join(p.cfg.Dump, name), body, 0o600); err != nil {
		log.Printf("dump: %v", err)
	}
}

func writeJSON(w http.ResponseWriter, status int, b []byte) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(b)
}

func apiError(w http.ResponseWriter, status int, typ, msg string) {
	b, _ := json.Marshal(map[string]any{"type": "error", "error": map[string]string{"type": typ, "message": msg}})
	writeJSON(w, status, b)
}

// writeMessageAsStream sends a complete Messages response as the SSE events a
// streaming client expects.
func writeMessageAsStream(w http.ResponseWriter, msg []byte) {
	var m map[string]json.RawMessage
	_ = json.Unmarshal(msg, &m)
	var blocks []json.RawMessage
	_ = json.Unmarshal(m["content"], &blocks)
	usage, stop := m["usage"], m["stop_reason"]
	start := map[string]json.RawMessage{}
	for k, v := range m {
		start[k] = v
	}
	start["content"] = json.RawMessage("[]")
	start["stop_reason"] = json.RawMessage("null")

	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	ev := func(name string, v any) {
		b, _ := json.Marshal(v)
		fmt.Fprintf(w, "event: %s\ndata: %s\n\n", name, b)
	}
	ev("message_start", map[string]any{"type": "message_start", "message": start})
	for i, raw := range blocks {
		var b map[string]any
		_ = json.Unmarshal(raw, &b)
		switch b["type"] {
		case "text":
			ev("content_block_start", map[string]any{"type": "content_block_start", "index": i, "content_block": map[string]any{"type": "text", "text": ""}})
			ev("content_block_delta", map[string]any{"type": "content_block_delta", "index": i, "delta": map[string]any{"type": "text_delta", "text": b["text"]}})
		case "tool_use":
			input, _ := json.Marshal(b["input"])
			ev("content_block_start", map[string]any{"type": "content_block_start", "index": i, "content_block": map[string]any{"type": "tool_use", "id": b["id"], "name": b["name"], "input": map[string]any{}}})
			ev("content_block_delta", map[string]any{"type": "content_block_delta", "index": i, "delta": map[string]any{"type": "input_json_delta", "partial_json": string(input)}})
		}
		ev("content_block_stop", map[string]any{"type": "content_block_stop", "index": i})
	}
	ev("message_delta", map[string]any{"type": "message_delta", "delta": map[string]any{"stop_reason": stop, "stop_sequence": nil}, "usage": usage})
	ev("message_stop", map[string]any{"type": "message_stop"})
}

// presentModel rewrites the "model" a foreign server reports back to the ID
// Claude Code asked for, in a JSON response or in a stream's message_start.
func presentModel(resp *http.Response, id string) error {
	ct := resp.Header.Get("Content-Type")
	switch {
	case strings.HasPrefix(ct, "text/event-stream"):
		resp.Body = &sseRewriter{src: bufio.NewReaderSize(resp.Body, 64<<10), closer: resp.Body, id: id}
		resp.ContentLength = -1
		resp.Header.Del("Content-Length")
	case strings.HasPrefix(ct, "application/json") && resp.StatusCode < 300:
		b, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
		resp.Body.Close()
		if err != nil {
			return err
		}
		b = setModel(b, id, false)
		resp.Body = io.NopCloser(bytes.NewReader(b))
		resp.ContentLength = int64(len(b))
		resp.Header.Set("Content-Length", fmt.Sprint(len(b)))
	}
	return nil
}

// setModel sets the top-level "model" of a message object, or of its nested
// "message" for a message_start event. Unparseable input is returned as is.
func setModel(b []byte, id string, event bool) []byte {
	var m map[string]json.RawMessage
	if json.Unmarshal(b, &m) != nil {
		return b
	}
	if event {
		raw, ok := m["message"]
		if !ok {
			return b
		}
		m["message"] = setModel(raw, id, false)
	} else {
		if _, ok := m["model"]; !ok {
			return b
		}
		m["model"], _ = json.Marshal(id)
	}
	out, err := json.Marshal(m)
	if err != nil {
		return b
	}
	return out
}

// sseRewriter passes an SSE stream through line by line, rewriting the model
// in the message_start event's data line.
type sseRewriter struct {
	src    *bufio.Reader
	closer io.Closer
	id     string
	buf    []byte
	done   bool // message_start seen: the rest is copied untouched
}

func (s *sseRewriter) Read(p []byte) (int, error) {
	if len(s.buf) == 0 {
		if s.done {
			return s.src.Read(p)
		}
		line, err := s.src.ReadSlice('\n')
		if err == bufio.ErrBufferFull {
			s.buf = append(s.buf[:0], line...)
			return s.drain(p), nil
		}
		if d, ok := bytes.CutPrefix(line, []byte("data:")); ok && bytes.Contains(d, []byte(`"message_start"`)) {
			nl := len(d) - len(bytes.TrimRight(d, "\r\n"))
			body := bytes.TrimSpace(d)
			line = append(append([]byte("data: "), setModel(body, s.id, true)...), d[len(d)-nl:]...)
			s.done = true
		}
		s.buf = append(s.buf[:0], line...)
		if len(s.buf) == 0 {
			return 0, err
		}
		n := s.drain(p)
		if err != nil && len(s.buf) == 0 {
			return n, err
		}
		return n, nil
	}
	return s.drain(p), nil
}

func (s *sseRewriter) drain(p []byte) int {
	n := copy(p, s.buf)
	s.buf = s.buf[n:]
	return n
}

func (s *sseRewriter) Close() error { return s.closer.Close() }

// adapt rewrites a Claude Code request for a foreign Anthropic-compatible
// server: the served model name, client-side tools only (Anthropic server
// tools such as web_search cannot run elsewhere), mid-conversation system
// messages folded into user turns, configured fields stripped and max_tokens
// clamped. The result is deterministic, so a conversation's prompt prefix
// stays stable for the server's context cache.
func adapt(body []byte, m *Model) ([]byte, error) {
	var req map[string]json.RawMessage
	if err := json.Unmarshal(body, &req); err != nil {
		return nil, err
	}
	req["model"], _ = json.Marshal(m.UpstreamModel)
	if raw, ok := req["messages"]; ok {
		msgs, err := foldSystemMessages(raw)
		if err != nil {
			return nil, err
		}
		req["messages"] = msgs
	}
	if raw, ok := req["tools"]; ok && !m.KeepServer {
		var tools []map[string]json.RawMessage
		if err := json.Unmarshal(raw, &tools); err == nil {
			kept := tools[:0]
			for _, t := range tools {
				if _, ok := t["input_schema"]; ok {
					kept = append(kept, t)
				}
			}
			req["tools"], _ = json.Marshal(kept)
		}
	}
	limit(req, m)
	return json.Marshal(req)
}

// prepareOpenAI folds mid-conversation system messages (many chat templates
// reject a system message after the first turn), strips configured fields and
// clamps max_tokens, before the request is translated to Chat Completions.
func prepareOpenAI(body []byte, m *Model) ([]byte, error) {
	var req map[string]json.RawMessage
	if err := json.Unmarshal(body, &req); err != nil {
		return nil, err
	}
	if raw, ok := req["messages"]; ok {
		msgs, err := foldSystemMessages(raw)
		if err != nil {
			return nil, err
		}
		req["messages"] = msgs
	}
	limit(req, m)
	return json.Marshal(req)
}

func limit(req map[string]json.RawMessage, m *Model) {
	for _, k := range m.Strip {
		delete(req, k)
	}
	if m.MaxTokens > 0 {
		var n int
		if json.Unmarshal(req["max_tokens"], &n) == nil && n > m.MaxTokens {
			req["max_tokens"], _ = json.Marshal(m.MaxTokens)
		}
	}
}

type message struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

// foldSystemMessages turns "system" entries inside messages (Claude Code puts
// its environment block there) into user content, appended to the preceding
// user turn when there is one. Most local chat templates accept a system
// message only at the start of the prompt.
func foldSystemMessages(raw json.RawMessage) (json.RawMessage, error) {
	var msgs []message
	if err := json.Unmarshal(raw, &msgs); err != nil {
		return nil, err
	}
	out := msgs[:0]
	changed := false
	for _, m := range msgs {
		if m.Role != "system" {
			out = append(out, m)
			continue
		}
		changed = true
		blocks, err := contentBlocks(m.Content)
		if err != nil {
			return nil, err
		}
		if n := len(out); n > 0 && out[n-1].Role == "user" {
			prev, err := contentBlocks(out[n-1].Content)
			if err != nil {
				return nil, err
			}
			out[n-1].Content, _ = json.Marshal(append(prev, blocks...))
			continue
		}
		m.Role = "user"
		m.Content, _ = json.Marshal(blocks)
		out = append(out, m)
	}
	if !changed {
		return raw, nil
	}
	return json.Marshal(out)
}

// contentBlocks returns message content as a list of blocks; a plain string
// becomes one text block.
func contentBlocks(c json.RawMessage) ([]json.RawMessage, error) {
	var s string
	if json.Unmarshal(c, &s) == nil {
		b, _ := json.Marshal(struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}{"text", s})
		return []json.RawMessage{b}, nil
	}
	var blocks []json.RawMessage
	if err := json.Unmarshal(c, &blocks); err != nil {
		return nil, fmt.Errorf("message content: %w", err)
	}
	return blocks, nil
}
