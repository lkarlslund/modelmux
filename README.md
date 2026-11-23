# ccroute

Use any foreign model from Claude Code, transparently: local models (vLLM, SGLang, llama.cpp, Ollama, ...) and
hosted ones (OpenAI, Gemini, DeepSeek, Kimi, GLM, OpenRouter, ...), as subagents, as the main model or in place of
a Claude alias, while everything else keeps going to Anthropic untouched.

Claude Code sends every request to one base URL (`ANTHROPIC_BASE_URL`). ccroute sits there and looks at the
`model` of each Messages API request:

- A model listed in the config goes to that model's server, under the name the server knows it by, with the
  model's own credentials (Claude credentials are removed). Anthropic-compatible servers get the request almost
  as is; OpenAI-compatible servers (`/v1/chat/completions`) get it translated, and their responses, streamed or
  not and tool calls included, are translated back.
- Everything else, including every non-Messages path, is passed through to `https://api.anthropic.com`
  unchanged: same body bytes, same headers and credentials (API key or claude.ai sign-in), no `X-Forwarded-*`.

One TOML file describes both sides: where each model lives, and how Claude Code sees it. `ccroute install` sets
up Claude Code from it.

## Install

```sh
go install .                                   # from a checkout (the repository is private)
mkdir -p ~/.config/ccroute
cp examples/config.toml ~/.config/ccroute/config.toml   # then edit it
ccroute check                                  # one-line prompt to each model, plain and streamed
ccroute install                                # user service + Claude Code settings + agents
```

`install` does, in this order:

1. writes `~/.config/systemd/user/ccroute.service` (running `ccroute serve -config <file>`, with the config's
   `env_files` as `EnvironmentFile`s), enables and restarts it, and waits until it answers. If the new proxy does
   not come up, the previous unit is restored, and nothing else changes;
2. edits `~/.claude/settings.json` (keeping its key order and everything it doesn't own; the old file is kept
   as `settings.json.bak-ccroute`): `env.ANTHROPIC_BASE_URL`, alias variables for `tier` models, the config's
   `[env]`, `modelPicker` rows and per-model `modelSettings.<id>.autoCompactWindow`;
3. writes `~/.claude/agents/<name>.md` for each `[[agent]]` (marked as generated; an existing hand-written file of
   the same name is saved as `.bak-ccroute` first).

What it changed is recorded in `~/.config/ccroute/installed.json`, so rerunning `install` after editing the config
replaces exactly its own entries, and `ccroute uninstall` takes them out again (restoring values it overwrote)
before stopping the service. Restart Claude Code sessions afterwards; they read settings at startup.
`install -dry-run` shows the changes; `-no-service` leaves systemd alone; `ccroute env` prints the environment
for a shell instead of settings.json.

If the proxy is down while `ANTHROPIC_BASE_URL` points at it, every Claude Code session fails: `ccroute uninstall`
(or deleting the `env.ANTHROPIC_BASE_URL` key) goes back to direct Anthropic access.

## Configure a model

```toml
[[model]]
id = "gpt"                               # what Claude Code sees
api = "openai"                           # or "anthropic" (default)
upstream = "https://api.openai.com/v1"   # Anthropic API base without /v1, OpenAI API base with /v1
upstream_model = "gpt-5-mini"            # what the server calls it (default: id)
key_env = "OPENAI_API_KEY"               # from the service's env_files; never in the config
name = "GPT (OpenAI)"
context = 400000
```

[`examples/config.toml`](examples/config.toml) lists every key with examples for local servers and hosted
providers. Request-side keys:

| Key | Meaning |
|---|---|
| `match` | extra globs (`qwen*`) of requested model names that route here; `id` always matches, `[1m]` is ignored |
| `auth` | `x-api-key` (Anthropic API default), `bearer` (OpenAI API default) or `none`; needs `key_env` |
| `headers` | extra headers sent upstream |
| `strip` | top-level request fields to drop, for servers that reject `thinking`, `output_config`, ... |
| `max_tokens` | clamp the request's `max_tokens` |
| `max_tokens_field` | OpenAI API: `max_tokens` (default) or `max_completion_tokens` (OpenAI's newer models) |
| `keep_server_tools` | keep Anthropic server tools (web search, ...) for an upstream that runs them; dropped by default |

## How the model is presented to Claude Code

- **`id`** is the model's name everywhere on the Claude Code side: in agent files, `--model`, `/model`, and in
  every response (ccroute rewrites the `model` the server reports back to the `id`). It can be any string, also
  a Claude model ID, to take that model's place.
- **`name`, `description`**: the model's row in the `/model` picker (`modelPicker` in settings.json). `hidden =
  true` gives no row.
- **`behaves_as`**: a Claude model ID whose capabilities and effort defaults Claude Code applies to the row
  (`modelPicker` `behavesAs`), instead of treating the ID as an unknown model.
- **`context`**: the real context window. Claude Code assumes 1M tokens for unknown IDs; this sets the
  auto-compact window for the model (`modelSettings`) and for agents that use it (`autoCompactWindow`).
  Claude Code's minimum is 100000; for a smaller window cap `max_tokens` and keep the agent's tasks small.
- **`tier`**: make the model what an alias resolves to: `haiku`, `sonnet`, `opus`, `fable` (via
  `ANTHROPIC_DEFAULT_<TIER>_MODEL`, with `name`/`description` as the picker label), `subagent`
  (`CLAUDE_CODE_SUBAGENT_MODEL`, subagents without a model of their own) or `default` (`ANTHROPIC_DEFAULT_MODEL`).
  The Agent tool's per-call `model` parameter only takes aliases, so a tier is how Claude itself can pick a foreign
  model for one call. `haiku` also covers Claude Code's background requests (titles, summaries).

## Agents

```toml
[[agent]]
name = "qwen-worker"
model = "qwen3.8-flash-next"
description = "Fast local worker for simple, well-specified tasks."
prompt = "Do exactly the task given and report the result concisely."
disallowed_tools = ["Agent", "ListAgents", "SendMessage"]
# tools = ["Read", "Grep", "Glob"]
# extra = { color = "cyan", effort = "low" }   # any other frontmatter key
```

## What the translation does

For every foreign model, deterministically (so a conversation's prompt prefix stays byte-stable and the server's
prefix cache keeps hitting):

- Server tools (entries in `tools` without an `input_schema`, such as web search) are dropped.
- `system` entries inside `messages` (Claude Code puts reminders and its environment block there) are folded
  into the preceding user turn: most chat templates accept a system message only at the start.

For `api = "openai"` additionally: system prompt to a system message; text, images (base64 or URL) and documents
with text; `tool_use` to `tool_calls` and `tool_result` to `tool` messages (placed directly after the call, images
from a tool result follow in a user message, `is_error` marked); `tool_choice`, `stop_sequences`, `temperature`,
`top_p`, `metadata.user_id`. Thinking blocks are dropped both ways (they need signatures). Back: text and tool
calls (fragmented, out-of-order, id-less and all-index-0 dialects), finish reasons, and usage with cached tokens.
`count_tokens` returns 404 for these models, and Claude Code falls back to its own estimate.

Known limits: streamed tool arguments are passed through as they arrive, so a model that emits broken JSON fails at
Claude Code's parse; Gemini's thought signatures on tool calls are not carried back; image parts reach models
without vision as is.

## Auto mode

In auto mode Claude Code asks the Anthropic API for a safety verdict on each proposed tool call (a `safeguards`
block in the request, `safeguard_results` in the response). A foreign server returns none, and Claude Code fails
closed: it falls back to its local checks, or stops the agent with "Auto mode is unavailable" after repeated
responses without a verdict.

## Debugging

`ccroute serve -dump DIR` (or `dump = "DIR"`) writes each translated request body to `DIR`. `GET /ccroute/health`
lists the configured models and whether their keys are set. The service logs one line per foreign request
(`journalctl --user -u ccroute`). `-route PATTERN=UPSTREAM[,MODEL]` still works for a quick run without a config.
