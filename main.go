// ccroute is a model-name router for Claude Code. Point ANTHROPIC_BASE_URL at
// it: Messages API requests whose "model" matches a local route go to that
// Anthropic-compatible server, everything else is passed through to
// api.anthropic.com byte for byte, with the client's own headers and
// credentials.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
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

type route struct {
	prefix   string   // matched against the request's model, case-insensitive
	upstream *url.URL // Anthropic-compatible base URL
	model    string   // model name sent upstream ("" keeps the request's)
}

type routes []route

func (r *routes) String() string { return fmt.Sprint(*r) }

// Set parses prefix=upstream[,model].
func (r *routes) Set(s string) error {
	prefix, rest, ok := strings.Cut(s, "=")
	if !ok || prefix == "" {
		return fmt.Errorf("want prefix=upstream[,model], got %q", s)
	}
	up, model, _ := strings.Cut(rest, ",")
	u, err := url.Parse(up)
	if err != nil {
		return err
	}
	if u.Scheme == "" || u.Host == "" {
		return fmt.Errorf("upstream %q needs a scheme and host", up)
	}
	*r = append(*r, route{strings.ToLower(prefix), u, model})
	return nil
}

// maxBody matches the Messages API request limit.
const maxBody = 32 << 20

type routeKey struct{}

func main() {
	var rs routes
	listen := flag.String("listen", "127.0.0.1:8098", "listen address")
	def := flag.String("default", "https://api.anthropic.com", "upstream for unmatched models")
	dump := flag.String("dump", "", "directory to write routed request bodies to (debugging)")
	flag.Var(&rs, "route", "prefix=upstream[,model], repeatable")
	flag.Parse()

	defURL, err := url.Parse(*def)
	if err != nil {
		log.Fatal(err)
	}
	log.Printf("listening on %s, default %s, %d local route(s)", *listen, defURL, len(rs))
	log.Fatal(http.ListenAndServe(*listen, newHandler(defURL, rs, *dump)))
}

func newHandler(def *url.URL, rs routes, dump string) http.Handler {
	// Rewrite (not Director) adds no X-Forwarded-* headers: the default
	// upstream sees the client's request as sent.
	passthrough := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(def)
		},
		FlushInterval: -1,
	}
	local := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			rt := pr.In.Context().Value(routeKey{}).(*route)
			pr.SetURL(rt.upstream)
			// Never send Anthropic credentials to a local server.
			pr.Out.Header.Del("Authorization")
			pr.Out.Header.Del("X-Api-Key")
			pr.Out.Header.Del("Accept-Encoding")
		},
		FlushInterval: -1,
	}

	var seq atomic.Int64
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || !strings.HasPrefix(r.URL.Path, "/v1/messages") {
			passthrough.ServeHTTP(w, r)
			return
		}
		body, err := io.ReadAll(io.LimitReader(r.Body, maxBody+1))
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if len(body) > maxBody {
			http.Error(w, "request body too large", http.StatusRequestEntityTooLarge)
			return
		}
		var head struct {
			Model string `json:"model"`
		}
		_ = json.Unmarshal(body, &head)

		rt := match(rs, head.Model)
		if rt == nil {
			r.Body = io.NopCloser(bytes.NewReader(body))
			r.ContentLength = int64(len(body))
			passthrough.ServeHTTP(w, r)
			return
		}

		body, err = adapt(body, rt)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if dump != "" {
			name := fmt.Sprintf("%s-%04d.json", time.Now().Format("20060102-150405"), seq.Add(1))
			if err := os.WriteFile(filepath.Join(dump, name), body, 0o600); err != nil {
				log.Printf("dump: %v", err)
			}
		}
		r.Body = io.NopCloser(bytes.NewReader(body))
		r.ContentLength = int64(len(body))
		log.Printf("%s %s -> %s", r.URL.Path, head.Model, rt.upstream.Host)
		local.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), routeKey{}, rt)))
	})
}

func match(rs routes, model string) *route {
	m := strings.ToLower(model)
	for i := range rs {
		if strings.HasPrefix(m, rs[i].prefix) {
			return &rs[i]
		}
	}
	return nil
}

// adapt rewrites a Claude Code request for a local server: the served model
// name, and only client-side tools (Anthropic server tools such as web_search
// cannot run locally). The result is deterministic, so a conversation's
// prompt prefix stays stable for the local server's context cache.
func adapt(body []byte, rt *route) ([]byte, error) {
	var req map[string]json.RawMessage
	if err := json.Unmarshal(body, &req); err != nil {
		return nil, err
	}
	if rt.model != "" {
		req["model"], _ = json.Marshal(rt.model)
	}
	if raw, ok := req["messages"]; ok {
		msgs, err := foldSystemMessages(raw)
		if err != nil {
			return nil, err
		}
		req["messages"] = msgs
	}
	if raw, ok := req["tools"]; ok {
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
	return json.Marshal(req)
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
