package main

// Translation between the Anthropic Messages API (what Claude Code speaks) and
// the OpenAI Chat Completions API (what most other model servers speak).
// Everything here is pure: bytes in, bytes out, no HTTP.

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// oaOptions controls a translation to the OpenAI Chat Completions API.
type oaOptions struct {
	Model          string // model name sent upstream (always set)
	MaxTokensField string // "max_tokens" (default when empty) or "max_completion_tokens"
	EffortField    string // upstream field for Claude Code's output_config.effort; empty drops the effort
	CacheKeyField  string // upstream field for CacheKey (e.g. "prompt_cache_key"); empty sends none
	CacheKey       string // stable key for one conversation, so the server reuses its prompt cache
}

// ---- Anthropic request shapes (only the fields we translate) ----

type anRequest struct {
	System        json.RawMessage `json:"system"`
	Messages      []anMessage     `json:"messages"`
	Tools         []anTool        `json:"tools"`
	ToolChoice    *anToolChoice   `json:"tool_choice"`
	MaxTokens     *int64          `json:"max_tokens"`
	Temperature   json.RawMessage `json:"temperature"`
	TopP          json.RawMessage `json:"top_p"`
	StopSequences []string        `json:"stop_sequences"`
	Stream        bool            `json:"stream"`
	Metadata      *struct {
		UserID string `json:"user_id"`
	} `json:"metadata"`
	OutputConfig *struct {
		Effort string `json:"effort"`
	} `json:"output_config"`
}

type anMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

type anBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text"`
	Source    *anSource       `json:"source"`
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Input     json.RawMessage `json:"input"`
	ToolUseID string          `json:"tool_use_id"`
	Content   json.RawMessage `json:"content"`
	IsError   bool            `json:"is_error"`
}

type anSource struct {
	Type      string `json:"type"`
	MediaType string `json:"media_type"`
	Data      string `json:"data"`
	URL       string `json:"url"`
}

type anTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"input_schema"`
}

type anToolChoice struct {
	Type                   string `json:"type"`
	Name                   string `json:"name"`
	DisableParallelToolUse bool   `json:"disable_parallel_tool_use"`
}

// ---- OpenAI request shapes ----

type oaMessage struct {
	Role       string       `json:"role"`
	Content    any          `json:"content"` // string, []any of parts, or nil (null)
	ToolCalls  []oaToolCall `json:"tool_calls,omitempty"`
	ToolCallID string       `json:"tool_call_id,omitempty"`
}

type oaTextPart struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type oaImagePart struct {
	Type     string `json:"type"`
	ImageURL struct {
		URL string `json:"url"`
	} `json:"image_url"`
}

type oaToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

type oaTool struct {
	Type     string `json:"type"`
	Function struct {
		Name        string          `json:"name"`
		Description string          `json:"description,omitempty"`
		Parameters  json.RawMessage `json:"parameters"`
	} `json:"function"`
}

// part is one piece of user-visible content: text or an image URL.
type part struct {
	text  string
	image string // URL (possibly a data: URL); empty for text
}

// toOpenAI converts an Anthropic Messages request body to a Chat Completions request body.
func toOpenAI(body []byte, o oaOptions) ([]byte, error) {
	var in anRequest
	if err := json.Unmarshal(body, &in); err != nil {
		return nil, fmt.Errorf("request: %w", err)
	}
	var msgs []oaMessage
	sys, err := textOf(in.System)
	if err != nil {
		return nil, fmt.Errorf("system: %w", err)
	}
	if sys != "" {
		msgs = append(msgs, oaMessage{Role: "system", Content: sys})
	}
	for i, m := range in.Messages {
		if msgs, err = appendMessage(msgs, m); err != nil {
			return nil, fmt.Errorf("messages[%d]: %w", i, err)
		}
	}
	if msgs == nil {
		msgs = []oaMessage{}
	}

	out := map[string]any{"model": o.Model, "messages": msgs}
	var tools []oaTool
	for _, t := range in.Tools {
		if len(t.InputSchema) == 0 || string(t.InputSchema) == "null" {
			continue // Anthropic server tool (web_search, ...): cannot run elsewhere
		}
		var ot oaTool
		ot.Type = "function"
		ot.Function.Name = t.Name
		ot.Function.Description = t.Description
		ot.Function.Parameters = t.InputSchema
		tools = append(tools, ot)
	}
	if len(tools) > 0 {
		out["tools"] = tools
		if tc := in.ToolChoice; tc != nil {
			switch tc.Type {
			case "auto":
				out["tool_choice"] = "auto"
			case "any":
				out["tool_choice"] = "required"
			case "none":
				out["tool_choice"] = "none"
			case "tool":
				out["tool_choice"] = map[string]any{"type": "function", "function": map[string]string{"name": tc.Name}}
			}
			if tc.DisableParallelToolUse {
				out["parallel_tool_calls"] = false
			}
		}
	}
	if in.MaxTokens != nil {
		field := o.MaxTokensField
		if field == "" {
			field = "max_tokens"
		}
		out[field] = *in.MaxTokens
	}
	if isSet(in.Temperature) {
		out["temperature"] = in.Temperature
	}
	if isSet(in.TopP) {
		out["top_p"] = in.TopP
	}
	if len(in.StopSequences) > 0 {
		out["stop"] = in.StopSequences
	}
	if in.Stream {
		out["stream"] = true
		out["stream_options"] = map[string]bool{"include_usage": true}
	}
	if in.Metadata != nil && in.Metadata.UserID != "" {
		out["user"] = in.Metadata.UserID
	}
	if o.CacheKeyField != "" && o.CacheKey != "" {
		out[o.CacheKeyField] = o.CacheKey
	}
	if o.EffortField != "" && in.OutputConfig != nil && in.OutputConfig.Effort != "" {
		out[o.EffortField] = in.OutputConfig.Effort
	}
	// Maps marshal with sorted keys and structs in field order, so the same
	// request always gives the same bytes and upstream prefix caches hit.
	return marshal(out)
}

func isSet(r json.RawMessage) bool { return len(r) > 0 && string(r) != "null" }

// appendMessage translates one Anthropic message into zero or more OpenAI messages.
func appendMessage(msgs []oaMessage, m anMessage) ([]oaMessage, error) {
	blocks, err := parseBlocks(m.Content)
	if err != nil {
		return nil, err
	}
	switch m.Role {
	case "system":
		var parts []part
		for _, b := range blocks {
			parts = appendPart(parts, b)
		}
		return append(msgs, oaMessage{Role: "system", Content: joinText(parts)}), nil

	case "assistant":
		var parts []part
		var calls []oaToolCall
		for _, b := range blocks {
			if b.Type == "tool_use" {
				var c oaToolCall
				c.ID = b.ID
				c.Type = "function"
				c.Function.Name = b.Name
				c.Function.Arguments = compactArgs(b.Input)
				calls = append(calls, c)
				continue
			}
			parts = appendPart(parts, b)
		}
		om := oaMessage{Role: "assistant", ToolCalls: calls}
		if text := joinText(parts); text != "" {
			om.Content = text
		} else if len(calls) == 0 {
			om.Content = "" // e.g. only thinking blocks; keep the turn so roles still alternate
		}
		return append(msgs, om), nil

	default: // user
		var toolMsgs []oaMessage
		var toolImages, rest []part
		for _, b := range blocks {
			if b.Type != "tool_result" {
				rest = appendPart(rest, b)
				continue
			}
			text, images, err := toolResult(b)
			if err != nil {
				return nil, err
			}
			toolMsgs = append(toolMsgs, oaMessage{Role: "tool", ToolCallID: b.ToolUseID, Content: text})
			if len(images) > 0 {
				toolImages = append(toolImages, part{text: fmt.Sprintf("[Image(s) from tool result %s]", b.ToolUseID)})
				toolImages = append(toolImages, images...)
			}
		}
		// Tool messages must directly follow the assistant message with the
		// tool calls, so they come before anything else in this turn.
		msgs = append(msgs, toolMsgs...)
		parts := append(toolImages, rest...)
		if len(parts) > 0 || len(toolMsgs) == 0 {
			msgs = append(msgs, oaMessage{Role: "user", Content: content(parts)})
		}
		return msgs, nil
	}
}

// toolResult returns a tool_result's text and the images it carries.
func toolResult(b anBlock) (string, []part, error) {
	inner, err := parseBlocks(b.Content)
	if err != nil {
		return "", nil, fmt.Errorf("tool_result %s: %w", b.ToolUseID, err)
	}
	var texts, images []part
	for _, ib := range inner {
		for _, p := range appendPart(nil, ib) {
			if p.image != "" {
				images = append(images, p)
			} else {
				texts = append(texts, p)
			}
		}
	}
	text := joinText(texts)
	if text == "" && len(images) > 0 {
		text = "(image output follows)"
	}
	if b.IsError {
		l := strings.ToLower(strings.TrimSpace(text))
		if !strings.HasPrefix(l, "error") && !strings.HasPrefix(l, "<tool_use_error>") {
			text = "Error: " + text
		}
	}
	return text, images, nil
}

// appendPart appends the user-visible content of an Anthropic block. Blocks
// that cannot be represented (thinking, unknown types) are dropped.
func appendPart(parts []part, b anBlock) []part {
	switch b.Type {
	case "text":
		if b.Text != "" {
			parts = append(parts, part{text: b.Text})
		}
	case "image":
		if u := imageURL(b.Source); u != "" {
			parts = append(parts, part{image: u})
		} else {
			parts = append(parts, part{text: "[image omitted]"})
		}
	case "document":
		if b.Source != nil && b.Source.Type == "text" {
			parts = append(parts, part{text: b.Source.Data})
		} else {
			parts = append(parts, part{text: "[document omitted]"})
		}
	}
	return parts
}

func imageURL(s *anSource) string {
	if s == nil {
		return ""
	}
	switch s.Type {
	case "base64":
		return "data:" + s.MediaType + ";base64," + s.Data
	case "url":
		return s.URL
	}
	return ""
}

// content is the OpenAI message content for parts: a plain string when there
// are only texts, the array form when there are images.
func content(parts []part) any {
	hasImage := false
	for _, p := range parts {
		if p.image != "" {
			hasImage = true
			break
		}
	}
	if !hasImage {
		return joinText(parts)
	}
	out := make([]any, 0, len(parts))
	for _, p := range parts {
		if p.image != "" {
			var ip oaImagePart
			ip.Type = "image_url"
			ip.ImageURL.URL = p.image
			out = append(out, ip)
		} else {
			out = append(out, oaTextPart{Type: "text", Text: p.text})
		}
	}
	return out
}

func joinText(parts []part) string {
	var texts []string
	for _, p := range parts {
		if p.image == "" {
			texts = append(texts, p.text)
		}
	}
	return strings.Join(texts, "\n\n")
}

// textOf joins the text of a string-or-blocks value.
func textOf(raw json.RawMessage) (string, error) {
	blocks, err := parseBlocks(raw)
	if err != nil {
		return "", err
	}
	var parts []part
	for _, b := range blocks {
		parts = appendPart(parts, b)
	}
	return joinText(parts), nil
}

// parseBlocks reads Anthropic content: a string (one text block), an array of
// blocks, or nothing.
func parseBlocks(raw json.RawMessage) ([]anBlock, error) {
	if !isSet(raw) {
		return nil, nil
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return []anBlock{{Type: "text", Text: s}}, nil
	}
	var blocks []anBlock
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return nil, fmt.Errorf("content: %w", err)
	}
	return blocks, nil
}

// compactArgs renders a tool_use input as the compact JSON string OpenAI
// expects in function.arguments.
func compactArgs(input json.RawMessage) string {
	if !isSet(input) {
		return "{}"
	}
	var buf bytes.Buffer
	if err := json.Compact(&buf, input); err != nil {
		return "{}"
	}
	return buf.String()
}

// ---- OpenAI response shapes ----

type oaUsage struct {
	PromptTokens        int64 `json:"prompt_tokens"`
	CompletionTokens    int64 `json:"completion_tokens"`
	PromptTokensDetails *struct {
		CachedTokens int64 `json:"cached_tokens"`
	} `json:"prompt_tokens_details"`
	PromptCacheHitTokens int64 `json:"prompt_cache_hit_tokens"` // DeepSeek
}

type oaRespToolCall struct {
	ID       string `json:"id"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

type oaResponse struct {
	ID      string `json:"id"`
	Choices []struct {
		Message struct {
			Content      json.RawMessage  `json:"content"`
			ToolCalls    []oaRespToolCall `json:"tool_calls"`
			FunctionCall *struct {
				Name      string `json:"name"`
				Arguments string `json:"arguments"`
			} `json:"function_call"`
		} `json:"message"`
		FinishReason *string         `json:"finish_reason"`
		StopReason   json.RawMessage `json:"stop_reason"` // vLLM: the matched stop string
	} `json:"choices"`
	Usage *oaUsage        `json:"usage"`
	Error json.RawMessage `json:"error"`
}

// ---- Anthropic response shapes ----

type anUsage struct {
	InputTokens              int64 `json:"input_tokens"`
	OutputTokens             int64 `json:"output_tokens"`
	CacheCreationInputTokens int64 `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     int64 `json:"cache_read_input_tokens"`
}

type anTextBlock struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type anToolUseBlock struct {
	Type  string          `json:"type"`
	ID    string          `json:"id"`
	Name  string          `json:"name"`
	Input json.RawMessage `json:"input"`
}

type anResponse struct {
	ID           string  `json:"id"`
	Type         string  `json:"type"`
	Role         string  `json:"role"`
	Model        string  `json:"model"`
	Content      []any   `json:"content"`
	StopReason   string  `json:"stop_reason"`
	StopSequence *string `json:"stop_sequence"`
	Usage        anUsage `json:"usage"`
}

// fromOpenAI converts a non-streaming Chat Completions response body to an Anthropic Messages response;
// the "model" field of the result is `presented`. A body carrying an "error" object (some servers answer
// errors with status 200) is returned as an error.
func fromOpenAI(body []byte, presented string) ([]byte, error) {
	var in oaResponse
	if err := json.Unmarshal(body, &in); err != nil {
		return nil, fmt.Errorf("response: %w", err)
	}
	if isSet(in.Error) {
		return nil, fmt.Errorf("upstream error: %s", errorMessage(in.Error))
	}
	if len(in.Choices) == 0 {
		return nil, errors.New("response has no choices")
	}
	ch := in.Choices[0]
	out := anResponse{
		ID:      messageID(in.ID),
		Type:    "message",
		Role:    "assistant",
		Model:   presented,
		Content: []any{},
		Usage:   usage(in.Usage),
	}
	if text := contentText(ch.Message.Content); text != "" {
		out.Content = append(out.Content, anTextBlock{Type: "text", Text: text})
	}
	calls := ch.Message.ToolCalls
	if fc := ch.Message.FunctionCall; fc != nil && len(calls) == 0 {
		var c oaRespToolCall
		c.Function.Name, c.Function.Arguments = fc.Name, fc.Arguments
		calls = append(calls, c)
	}
	for _, c := range calls {
		out.Content = append(out.Content, anToolUseBlock{
			Type:  "tool_use",
			ID:    toolID(c.ID),
			Name:  c.Function.Name,
			Input: toolInput(c.Function.Arguments),
		})
	}
	finish := ""
	if ch.FinishReason != nil {
		finish = *ch.FinishReason
	}
	out.StopReason, out.StopSequence = stopReason(finish, stopString(ch.StopReason), len(calls) > 0)
	return marshal(out)
}

// contentText reads message content: a string, or an array of parts whose
// text parts are concatenated.
func contentText(raw json.RawMessage) string {
	if !isSet(raw) {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	_ = json.Unmarshal(raw, &parts)
	var b strings.Builder
	for _, p := range parts {
		if p.Type == "text" {
			b.WriteString(p.Text)
		}
	}
	return b.String()
}

// toolInput turns function.arguments into an Anthropic tool input object.
// Empty arguments become {}. A JSON object is used as is, also when it was
// double-encoded as a JSON string. Anything else that is not an object (the
// model wrote broken JSON) is kept as {"_raw": "<arguments>"}, so the tool
// call reaches Claude Code, fails schema validation there and the model sees
// its mistake, instead of the call silently vanishing.
func toolInput(args string) json.RawMessage {
	args = strings.TrimSpace(args)
	if args == "" {
		return json.RawMessage("{}")
	}
	if obj := jsonObject([]byte(args)); obj != nil {
		return obj
	}
	var s string
	if json.Unmarshal([]byte(args), &s) == nil {
		if obj := jsonObject([]byte(s)); obj != nil {
			return obj
		}
	}
	b, _ := marshal(map[string]string{"_raw": args})
	return b
}

// jsonObject returns b compacted if it is a JSON object, else nil.
func jsonObject(b []byte) json.RawMessage {
	var obj map[string]json.RawMessage
	if json.Unmarshal(b, &obj) != nil || obj == nil {
		return nil
	}
	var buf bytes.Buffer
	if json.Compact(&buf, b) != nil {
		return nil
	}
	return buf.Bytes()
}

func usage(u *oaUsage) anUsage {
	if u == nil {
		return anUsage{}
	}
	cached := u.PromptCacheHitTokens
	if u.PromptTokensDetails != nil && u.PromptTokensDetails.CachedTokens > 0 {
		cached = u.PromptTokensDetails.CachedTokens
	}
	return anUsage{
		InputTokens:          max(u.PromptTokens-cached, 0),
		OutputTokens:         u.CompletionTokens,
		CacheReadInputTokens: cached,
	}
}

// stopReason maps an OpenAI finish_reason to an Anthropic stop_reason and
// stop_sequence. A truncated response is max_tokens even when it contains a
// (probably incomplete) tool call; otherwise any tool call makes it tool_use.
// tool_calls without any emitted tool_use block is end_turn, since Claude
// Code would otherwise wait for tool results it has no calls for.
func stopReason(finish, seq string, hasTool bool) (string, *string) {
	switch finish {
	case "length":
		return "max_tokens", nil
	case "content_filter":
		return "refusal", nil
	}
	if hasTool {
		return "tool_use", nil
	}
	if finish == "stop" && seq != "" {
		return "stop_sequence", &seq
	}
	return "end_turn", nil
}

// stopString reads vLLM's choice.stop_reason, which is the matched stop
// string (or a stop token id, which we ignore).
func stopString(raw json.RawMessage) string {
	var s string
	if isSet(raw) && json.Unmarshal(raw, &s) == nil {
		return s
	}
	return ""
}

func messageID(upstream string) string {
	if upstream == "" {
		return "msg_" + rand.Text()
	}
	return "msg_" + upstream
}

// toolID returns the upstream tool call id with characters outside
// [A-Za-z0-9_-] replaced (the Anthropic API rejects them, which would break
// the conversation if it later moves to a Claude model), or a new random id
// when the server sent none.
func toolID(upstream string) string {
	if upstream == "" {
		return "toolu_" + rand.Text()
	}
	return strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' || r == '-' {
			return r
		}
		return '_'
	}, upstream)
}

// ---- Streaming ----

type oaToolDelta struct {
	Index    *int   `json:"index"`
	ID       string `json:"id"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

type oaChunk struct {
	ID      string `json:"id"`
	Choices []struct {
		Index int `json:"index"`
		Delta struct {
			Content   json.RawMessage `json:"content"`
			ToolCalls []oaToolDelta   `json:"tool_calls"`
		} `json:"delta"`
		FinishReason *string         `json:"finish_reason"`
		StopReason   json.RawMessage `json:"stop_reason"`
	} `json:"choices"`
	Usage *oaUsage        `json:"usage"`
	Error json.RawMessage `json:"error"`
}

// toolState is one upstream tool call being streamed.
type toolState struct {
	id, name string
	args     strings.Builder // all argument fragments so far
	started  bool            // content_block_start sent
	closed   bool
	block    int
}

type oaStream struct {
	w     io.Writer
	flush func()
	model string

	started   bool
	next      int  // next content block index
	textOpen  bool // a text block is open at index next-1
	cur       *toolState
	calls     []*toolState // by internal key
	alias     map[int]int  // upstream tool_calls index -> internal key
	toolsSent int          // tool_use blocks emitted
	finish    string       // last finish_reason
	stopSeq   string       // vLLM stop_reason string
	usage     anUsage
}

// streamFromOpenAI reads a Chat Completions SSE stream from src and writes the equivalent Anthropic Messages SSE
// stream (event: + data: lines, blank line after each event) to dst, calling flush() after each event.
// It returns when src ends ([DONE] or EOF) or on error; on a mid-stream upstream error it writes an Anthropic
// `event: error` before returning. A stream that ends without any chunk is reported as an error too.
func streamFromOpenAI(dst io.Writer, flush func(), src io.Reader, presented string) error {
	s := &oaStream{w: dst, flush: flush, model: presented, alias: map[int]int{}}
	sc := bufio.NewScanner(src)
	sc.Buffer(make([]byte, 0, 64<<10), 16<<20)
	var pending, event string
	for sc.Scan() {
		line := strings.TrimSuffix(sc.Text(), "\r")
		switch {
		case line == "":
			if pending != "" {
				return s.fail(fmt.Errorf("malformed stream chunk: %.200s", pending))
			}
			event = ""
		case strings.HasPrefix(line, ":"): // SSE comment / keep-alive
		case strings.HasPrefix(line, "event:"):
			event = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
		case strings.HasPrefix(line, "data:"):
			d := strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " ")
			if pending != "" {
				d = pending + "\n" + d // multi-line data field
			}
			if strings.TrimSpace(d) == "[DONE]" {
				return s.end()
			}
			if !json.Valid([]byte(d)) {
				pending = d
				continue
			}
			pending = ""
			if err := s.chunk([]byte(d), event == "error"); err != nil {
				return err
			}
		}
	}
	if err := sc.Err(); err != nil {
		return s.fail(fmt.Errorf("reading upstream stream: %w", err))
	}
	if pending != "" {
		return s.fail(fmt.Errorf("malformed stream chunk: %.200s", pending))
	}
	return s.end()
}

func (s *oaStream) chunk(data []byte, isError bool) error {
	var c oaChunk
	if err := json.Unmarshal(data, &c); err != nil {
		if isError {
			return s.fail(errors.New(errorMessage(data)))
		}
		return s.fail(fmt.Errorf("unexpected stream chunk: %.200s", data))
	}
	if isSet(c.Error) {
		return s.fail(errors.New(errorMessage(c.Error)))
	}
	if isError {
		return s.fail(errors.New(errorMessage(data)))
	}
	if !s.started {
		s.started = true
		err := s.emit("message_start", map[string]any{
			"type": "message_start",
			"message": map[string]any{
				"id": messageID(c.ID), "type": "message", "role": "assistant", "model": s.model,
				"content": []any{}, "stop_reason": nil, "stop_sequence": nil,
				"usage": map[string]int64{"input_tokens": 0, "output_tokens": 0},
			},
		})
		if err != nil {
			return err
		}
	}
	if c.Usage != nil {
		s.usage = usage(c.Usage)
	}
	for _, ch := range c.Choices {
		if ch.Index != 0 {
			continue
		}
		var text string
		if isSet(ch.Delta.Content) {
			_ = json.Unmarshal(ch.Delta.Content, &text)
		}
		if err := s.text(text); err != nil {
			return err
		}
		for _, tc := range ch.Delta.ToolCalls {
			if err := s.toolDelta(tc); err != nil {
				return err
			}
		}
		if ch.FinishReason != nil && *ch.FinishReason != "" {
			s.finish = *ch.FinishReason
		}
		if seq := stopString(ch.StopReason); seq != "" {
			s.stopSeq = seq
		}
	}
	return nil
}

func (s *oaStream) text(t string) error {
	if t == "" {
		return nil
	}
	if err := s.closeTool(); err != nil {
		return err
	}
	if !s.textOpen {
		err := s.emit("content_block_start", map[string]any{
			"type": "content_block_start", "index": s.next,
			"content_block": map[string]string{"type": "text", "text": ""},
		})
		if err != nil {
			return err
		}
		s.textOpen = true
		s.next++
	}
	return s.emit("content_block_delta", map[string]any{
		"type": "content_block_delta", "index": s.next - 1,
		"delta": map[string]string{"type": "text_delta", "text": t},
	})
}

func (s *oaStream) closeText() error {
	if !s.textOpen {
		return nil
	}
	s.textOpen = false
	return s.emit("content_block_stop", map[string]any{"type": "content_block_stop", "index": s.next - 1})
}

// key maps a fragment to an internal tool call. Servers differ: most send
// index, some omit it, some send every call with index 0 but distinct ids.
func (s *oaStream) key(tc oaToolDelta) int {
	differentID := func(k int) bool {
		id := s.calls[k].id
		return tc.ID != "" && id != "" && tc.ID != id
	}
	if tc.Index == nil {
		if s.cur != nil && !differentID(s.keyOf(s.cur)) {
			return s.keyOf(s.cur)
		}
		return s.newCall()
	}
	k, ok := s.alias[*tc.Index]
	if !ok || differentID(k) {
		k = s.newCall()
		s.alias[*tc.Index] = k
	}
	return k
}

func (s *oaStream) keyOf(t *toolState) int {
	for i, c := range s.calls {
		if c == t {
			return i
		}
	}
	return -1
}

func (s *oaStream) newCall() int {
	s.calls = append(s.calls, &toolState{})
	return len(s.calls) - 1
}

func (s *oaStream) toolDelta(tc oaToolDelta) error {
	t := s.calls[s.key(tc)]
	if t.closed {
		return nil // late fragment for a call whose block is already closed: cannot be delivered
	}
	if t != s.cur {
		if err := s.closeTool(); err != nil {
			return err
		}
		s.cur = t
	}
	if t.id == "" {
		t.id = tc.ID
	}
	switch n := tc.Function.Name; {
	case n == "" || n == t.name:
	case t.name == "" || strings.HasPrefix(n, t.name):
		t.name = n // first, or cumulative
	default:
		t.name += n // fragmented
	}
	args := tc.Function.Arguments
	t.args.WriteString(args)
	if t.started {
		return s.argsDelta(t, args)
	}
	// Start the block once the name is known and arguments have begun (so a
	// name sent in pieces is complete); arguments seen earlier were buffered.
	if t.name != "" && t.args.Len() > 0 {
		return s.startTool(t)
	}
	return nil
}

func (s *oaStream) startTool(t *toolState) error {
	if err := s.closeText(); err != nil {
		return err
	}
	t.started = true
	t.block = s.next
	s.next++
	s.toolsSent++
	err := s.emit("content_block_start", map[string]any{
		"type": "content_block_start", "index": t.block,
		"content_block": anToolUseBlock{Type: "tool_use", ID: toolID(t.id), Name: t.name, Input: json.RawMessage("{}")},
	})
	if err != nil {
		return err
	}
	return s.argsDelta(t, t.args.String())
}

func (s *oaStream) argsDelta(t *toolState, frag string) error {
	if frag == "" {
		return nil
	}
	return s.emit("content_block_delta", map[string]any{
		"type": "content_block_delta", "index": t.block,
		"delta": map[string]string{"type": "input_json_delta", "partial_json": frag},
	})
}

// closeTool ends the current tool call. A call that never got a name cannot
// be represented and is dropped.
func (s *oaStream) closeTool() error {
	t := s.cur
	if t == nil {
		return nil
	}
	s.cur = nil
	t.closed = true
	if !t.started {
		if t.name == "" {
			return nil
		}
		if err := s.startTool(t); err != nil {
			return err
		}
	}
	return s.emit("content_block_stop", map[string]any{"type": "content_block_stop", "index": t.block})
}

func (s *oaStream) end() error {
	if !s.started {
		return s.fail(errors.New("upstream stream ended without any data"))
	}
	if err := s.closeTool(); err != nil {
		return err
	}
	if err := s.closeText(); err != nil {
		return err
	}
	reason, seq := stopReason(s.finish, s.stopSeq, s.toolsSent > 0)
	err := s.emit("message_delta", map[string]any{
		"type":  "message_delta",
		"delta": map[string]any{"stop_reason": reason, "stop_sequence": seq},
		"usage": s.usage,
	})
	if err != nil {
		return err
	}
	return s.emit("message_stop", map[string]string{"type": "message_stop"})
}

// fail sends an Anthropic error event and returns err.
func (s *oaStream) fail(err error) error {
	if werr := s.emit("error", map[string]any{
		"type":  "error",
		"error": map[string]string{"type": "api_error", "message": err.Error()},
	}); werr != nil {
		return errors.Join(err, werr)
	}
	return err
}

func (s *oaStream) emit(event string, v any) error {
	b, err := marshal(v)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(s.w, "event: %s\ndata: %s\n\n", event, b); err != nil {
		return err
	}
	if s.flush != nil {
		s.flush()
	}
	return nil
}

// ---- Errors ----

// openAIError converts an upstream error response (any body, possibly not JSON) to an Anthropic error body
// {"type":"error","error":{"type":...,"message":...}}; type from status: 400 invalid_request_error,
// 401 authentication_error, 403 permission_error, 404 not_found_error, 413 request_too_large, 429 rate_limit_error,
// 529/503 overloaded_error, else api_error.
func openAIError(status int, body []byte) []byte {
	typ := "api_error"
	switch status {
	case 400:
		typ = "invalid_request_error"
	case 401:
		typ = "authentication_error"
	case 403:
		typ = "permission_error"
	case 404:
		typ = "not_found_error"
	case 413:
		typ = "request_too_large"
	case 429:
		typ = "rate_limit_error"
	case 503, 529:
		typ = "overloaded_error"
	}
	msg := errorMessage(body)
	if msg == "" {
		msg = fmt.Sprintf("upstream returned %d %s", status, http.StatusText(status))
	}
	b, _ := marshal(map[string]any{"type": "error", "error": map[string]string{"type": typ, "message": msg}})
	return b
}

// errorMessage extracts a human-readable message from an error body in any of
// the shapes servers use: {"error":{"message":..}}, {"error":"..."},
// {"message":..}, {"detail":..}, Gemini's [{"error":{..}}], a bare JSON
// string, or plain text.
func errorMessage(body []byte) string {
	body = bytes.TrimSpace(body)
	var v any
	if json.Unmarshal(body, &v) != nil {
		s := strings.TrimSpace(string(body))
		if len(s) > 2000 {
			s = s[:2000] + "..."
		}
		return s
	}
	return messageIn(v, 0)
}

func messageIn(v any, depth int) string {
	if depth > 4 {
		return ""
	}
	switch x := v.(type) {
	case string:
		return x
	case []any:
		if len(x) > 0 {
			return messageIn(x[0], depth+1)
		}
	case map[string]any:
		for _, k := range []string{"error", "message", "detail", "msg"} {
			if m := messageIn(x[k], depth+1); m != "" {
				return m
			}
		}
		b, _ := marshal(x)
		return string(b)
	}
	return ""
}

// marshal is json.Marshal without HTML escaping, so text such as
// <system-reminder> reaches the server (and dumps) as written.
func marshal(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(buf.Bytes(), []byte("\n")), nil
}
