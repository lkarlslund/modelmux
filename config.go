package main

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/BurntSushi/toml"
)

// Config is the modelmux configuration file (TOML). One file describes both
// the proxy (which models go where) and how they are presented to Claude Code
// (picker rows, alias tiers, subagents), so `modelmux install` can set up the
// client side from the same source.
type Config struct {
	Listen    string   `toml:"listen"`    // proxy address, default 127.0.0.1:8098
	Anthropic string   `toml:"anthropic"` // upstream for every model not listed, default https://api.anthropic.com
	Dump      string   `toml:"dump"`      // directory to write translated request bodies to (debugging)
	EnvFiles  []string `toml:"env_files"` // files with KEY=value lines (API keys) loaded by the systemd unit

	// Env is extra environment for Claude Code, written to settings.json by
	// install, such as CLAUDE_CODE_DISABLE_EXPERIMENTAL_BETAS = "1".
	Env map[string]string `toml:"env"`

	Models []*Model `toml:"model"`
	Agents []*Agent `toml:"agent"`
}

// Model is one foreign model. ID is what Claude Code sees: the name it puts
// in requests, the model it reads back in responses, and the row in /model.
type Model struct {
	ID    string   `toml:"id"`    // presented to Claude Code; may be any string, also a Claude model ID to take its place
	Match []string `toml:"match"` // extra case-insensitive globs (path.Match) of requested models that route here

	Upstream      string            `toml:"upstream"`          // base URL: Anthropic API without /v1, OpenAI API with /v1
	API           string            `toml:"api"`               // "anthropic" (default) or "openai" (Chat Completions)
	UpstreamModel string            `toml:"upstream_model"`    // model name the upstream serves, default ID
	KeyEnv        string            `toml:"key_env"`           // environment variable holding the upstream API key
	Auth          string            `toml:"auth"`              // "x-api-key", "bearer" or "none"; default by API when a key is set
	Headers       map[string]string `toml:"headers"`           // extra headers sent upstream
	Strip         []string          `toml:"strip"`             // top-level request fields to remove (e.g. "thinking")
	MaxTokens     int               `toml:"max_tokens"`        // clamp the request's max_tokens to this
	MaxTokensKey  string            `toml:"max_tokens_field"`  // openai: "max_tokens" (default) or "max_completion_tokens"
	EffortField   string            `toml:"effort_field"`      // openai: upstream field that gets Claude Code's effort (e.g. "reasoning_effort")
	KeepServer    bool              `toml:"keep_server_tools"` // keep Anthropic server tools (web search...) for upstreams that run them

	// How Claude Code presents the model.
	Name        string `toml:"name"`        // /model picker label (default: ID)
	Description string `toml:"description"` // /model picker second line
	BehavesAs   string `toml:"behaves_as"`  // a Claude model ID whose capabilities and effort defaults Claude Code applies
	Context     int    `toml:"context"`     // context window; sets the auto-compact window for this model (>= 100000)
	Tier        string `toml:"tier"`        // also make this model the "haiku", "sonnet", "opus", "fable", "subagent" or "default" model
	Hidden      bool   `toml:"hidden"`      // no /model picker row

	upstream *url.URL
}

// Agent is a Claude Code subagent definition, written to ~/.claude/agents/<name>.md.
type Agent struct {
	Name            string            `toml:"name"`
	Description     string            `toml:"description"`
	Model           string            `toml:"model"` // a model ID from this file, or any model Claude Code accepts
	Prompt          string            `toml:"prompt"`
	Tools           []string          `toml:"tools"`
	DisallowedTools []string          `toml:"disallowed_tools"`
	Extra           map[string]string `toml:"extra"` // other frontmatter keys (color, effort, permissionMode, ...)
}

var tiers = map[string]string{
	"haiku":    "ANTHROPIC_DEFAULT_HAIKU_MODEL",
	"sonnet":   "ANTHROPIC_DEFAULT_SONNET_MODEL",
	"opus":     "ANTHROPIC_DEFAULT_OPUS_MODEL",
	"fable":    "ANTHROPIC_DEFAULT_FABLE_MODEL",
	"subagent": "CLAUDE_CODE_SUBAGENT_MODEL",
	"default":  "ANTHROPIC_DEFAULT_MODEL",
}

var agentName = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)

func defaultConfigPath() string {
	if d, err := os.UserConfigDir(); err == nil {
		return filepath.Join(d, "modelmux", "config.toml")
	}
	return "config.toml"
}

func loadConfig(p string) (*Config, error) {
	var c Config
	md, err := toml.DecodeFile(p, &c)
	if err != nil {
		return nil, err
	}
	if und := md.Undecoded(); len(und) > 0 {
		return nil, fmt.Errorf("%s: unknown keys %v", p, und)
	}
	return &c, c.validate()
}

func (c *Config) validate() error {
	if c.Listen == "" {
		c.Listen = "127.0.0.1:8098"
	}
	if c.Anthropic == "" {
		c.Anthropic = "https://api.anthropic.com"
	}
	var errs []error
	ids := map[string]bool{}
	usedTier := map[string]string{}
	for i, m := range c.Models {
		if err := m.validate(); err != nil {
			errs = append(errs, fmt.Errorf("model %d (%s): %w", i+1, m.ID, err))
			continue
		}
		if ids[strings.ToLower(m.ID)] {
			errs = append(errs, fmt.Errorf("model %s listed twice", m.ID))
		}
		ids[strings.ToLower(m.ID)] = true
		if m.EffortField != "" && m.API != "openai" {
			return errors.New("effort_field needs api = \"openai\" (Anthropic upstreams get output_config as is)")
		}
		if m.Tier != "" {
			if prev, ok := usedTier[m.Tier]; ok {
				errs = append(errs, fmt.Errorf("tier %q set on both %s and %s", m.Tier, prev, m.ID))
			}
			usedTier[m.Tier] = m.ID
		}
	}
	names := map[string]bool{}
	for i, a := range c.Agents {
		switch {
		case !agentName.MatchString(a.Name):
			errs = append(errs, fmt.Errorf("agent %d: name %q must be lowercase letters, digits and dashes", i+1, a.Name))
		case names[a.Name]:
			errs = append(errs, fmt.Errorf("agent %s listed twice", a.Name))
		case a.Description == "":
			errs = append(errs, fmt.Errorf("agent %s: description is required", a.Name))
		}
		names[a.Name] = true
	}
	return errors.Join(errs...)
}

func (m *Model) validate() error {
	if m.ID == "" {
		return errors.New("id is required")
	}
	if strings.ContainsAny(m.ID, " \t\n\"") {
		return errors.New("id may not contain spaces or quotes")
	}
	u, err := url.Parse(m.Upstream)
	if err != nil {
		return fmt.Errorf("upstream: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" || u.Host == "" {
		return fmt.Errorf("upstream %q needs an http(s) scheme and host", m.Upstream)
	}
	m.upstream = u
	switch m.API {
	case "":
		m.API = "anthropic"
	case "anthropic", "openai":
	default:
		return fmt.Errorf("api %q: want anthropic or openai", m.API)
	}
	if m.UpstreamModel == "" {
		m.UpstreamModel = m.ID
	}
	switch m.Auth {
	case "":
		switch {
		case m.KeyEnv == "":
			m.Auth = "none"
		case m.API == "openai":
			m.Auth = "bearer"
		default:
			m.Auth = "x-api-key"
		}
	case "none", "bearer", "x-api-key":
	default:
		return fmt.Errorf("auth %q: want x-api-key, bearer or none", m.Auth)
	}
	if m.Auth != "none" && m.KeyEnv == "" {
		return fmt.Errorf("auth %s needs key_env", m.Auth)
	}
	switch m.MaxTokensKey {
	case "", "max_tokens", "max_completion_tokens":
	default:
		return fmt.Errorf("max_tokens_field %q: want max_tokens or max_completion_tokens", m.MaxTokensKey)
	}
	if m.Tier != "" {
		if _, ok := tiers[m.Tier]; !ok {
			return fmt.Errorf("tier %q: want haiku, sonnet, opus, fable, subagent or default", m.Tier)
		}
	}
	if m.Context != 0 && m.Context < 100000 {
		return fmt.Errorf("context %d: Claude Code compacts no earlier than 100000 tokens; leave it unset and cap max_tokens instead", m.Context)
	}
	for _, g := range m.Match {
		if _, err := path.Match(strings.ToLower(g), ""); err != nil {
			return fmt.Errorf("match %q: %w", g, err)
		}
	}
	return nil
}

// key returns the upstream API key, or "" when the model needs none.
func (m *Model) key() (string, error) {
	if m.Auth == "none" {
		return "", nil
	}
	k := os.Getenv(m.KeyEnv)
	if k == "" {
		return "", fmt.Errorf("model %s: $%s is empty", m.ID, m.KeyEnv)
	}
	return k, nil
}

// lookup returns the model a request for `requested` routes to, or nil for
// the Anthropic passthrough. An exact ID wins over a glob; globs are tried in
// file order.
func (c *Config) lookup(requested string) *Model {
	r := strings.ToLower(strings.TrimSuffix(requested, "[1m]"))
	for _, m := range c.Models {
		if strings.ToLower(m.ID) == r {
			return m
		}
	}
	for _, m := range c.Models {
		for _, g := range m.Match {
			if ok, _ := path.Match(strings.ToLower(g), r); ok {
				return m
			}
		}
	}
	return nil
}
