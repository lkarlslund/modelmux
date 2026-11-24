# modelmux

Use any model from Claude Code, next to Claude: a local model (vLLM, SGLang, llama.cpp, Ollama, LM Studio, ...)
or a hosted one (OpenAI, Gemini, DeepSeek, Kimi, GLM, OpenRouter, ...), as a subagent, in the `/model` picker, or
in place of a Claude alias such as `haiku`. Everything else keeps going to Anthropic, untouched.

You decide how each model is presented to Claude Code: the model ID it sees, its name and description in the
picker, which Claude model it behaves like, how large its context is and which alias it answers to.

A typical setup: your main session stays on Claude and hands simple, well-specified jobs (searches, mechanical
edits, running builds) to a cheap or local model through a subagent.

```
                         ┌─ model is a configured id ──▶ your model's server
Claude Code ──▶ modelmux ┤   (Anthropic API, or OpenAI Chat Completions translated both ways)
                         └─ anything else ─────────────▶ api.anthropic.com, byte for byte
```

## How it works

Claude Code sends every request to one base URL, `ANTHROPIC_BASE_URL`. modelmux listens there and looks at the
`model` of each Messages API request:

- **A configured model** goes to that model's server, under the name the server knows it by and with that
  model's own key; Claude credentials are removed. Anthropic-compatible servers get the request almost as is.
  OpenAI-compatible servers (`/v1/chat/completions`) get it translated, and their responses are translated back,
  streamed or not, tool calls included. In every response the model name is rewritten to the ID Claude Code
  asked for.
- **Everything else**, including every path other than `/v1/messages`, is passed through to
  `https://api.anthropic.com` unchanged: the same body bytes and the same headers and credentials (API key or
  claude.ai sign-in), with no `X-Forwarded-*` headers added.

A single TOML file describes both sides: where each model lives, and how Claude Code sees it. `modelmux install`
sets up Claude Code from it, and `modelmux uninstall` undoes exactly that.

## Install

You need Go 1.27 or later. The installer sets up a systemd user service on Linux. On other systems, use
`install -no-service` and run `modelmux serve` yourself.

```sh
go install github.com/lkarlslund/modelmux@latest
mkdir -p ~/.config/modelmux
curl -o ~/.config/modelmux/config.toml \
  https://raw.githubusercontent.com/lkarlslund/modelmux/main/examples/config.toml
$EDITOR ~/.config/modelmux/config.toml
modelmux check        # sends a one-line prompt to each model, plain and streamed
modelmux install      # sets up the user service, Claude Code settings and agents
```

Start new Claude Code sessions afterwards, because Claude Code reads its settings at startup.

`install` does three things, in this order:

1. **Service.** It writes `~/.config/systemd/user/modelmux.service`, which runs `modelmux serve -config <file>`
   with the config's `env_files` as `EnvironmentFile`s. It then enables and restarts the service and waits until
   the proxy answers. If the new proxy doesn't come up, it restores the previous unit and changes nothing else.
2. **Settings.** It edits `~/.claude/settings.json`, keeping the key order and everything it doesn't own, and
   saves the old file as `settings.json.bak-modelmux`. It sets:
   - `env.ANTHROPIC_BASE_URL`
   - the alias variables for `tier` models
   - the config's `[env]`
   - `modelPicker` rows
   - `modelSettings.<id>.autoCompactWindow` for each model
3. **Agents.** It writes `~/.claude/agents/<name>.md` for each `[[agent]]`, marked as generated. If a hand-written
   file of the same name exists, it saves that file as `.bak-modelmux` first.

`~/.config/modelmux/installed.json` records what install changed. So after you edit the config, running `install`
again replaces only its own entries, and `uninstall` removes them and restores any values it overwrote before
stopping the service.

Other options:

- `install -dry-run` shows what would change.
- `install -no-service` leaves systemd alone.
- `modelmux env` prints the environment variables for a shell, instead of writing them to settings.json.

> [!WARNING]
> While `ANTHROPIC_BASE_URL` points at modelmux, every Claude Code session depends on it, Claude-only ones
> included. If the proxy is down, Claude Code can't reach Anthropic. `modelmux uninstall`, or deleting
> `env.ANTHROPIC_BASE_URL` from settings.json, gives Claude Code direct access again.

## Add a model

```toml
[[model]]
id = "gpt"                               # what Claude Code sees
api = "openai"                           # or "anthropic" (default)
upstream = "https://api.openai.com/v1"   # Anthropic API base without /v1, OpenAI API base with /v1
upstream_model = "gpt-5-mini"            # what the server calls it (default: id)
key_env = "OPENAI_API_KEY"               # read from the environment; never put keys in the config
name = "GPT (OpenAI)"
context = 400000
```

API keys come from the service's environment. Put `OPENAI_API_KEY=...` in a file listed in `env_files`, and
`chmod 600` that file.

[`examples/config.toml`](examples/config.toml) has every key, with examples for local servers and hosted
providers. These keys control what is sent to the model's server:

| Key | Meaning |
|---|---|
| `upstream` | the server's API base URL |
| `api` | `anthropic` (Messages API, default) or `openai` (Chat Completions) |
| `upstream_model` | the model name the server expects (default: `id`) |
| `match` | extra globs (`qwen*`) of requested model names that route here. `id` always matches, and a `[1m]` suffix is ignored. |
| `key_env` | the environment variable that holds the API key |
| `auth` | `x-api-key` (default for `anthropic`), `bearer` (default for `openai`) or `none` |
| `headers` | extra headers to send to the server |
| `strip` | top-level request fields to drop, for servers that reject `thinking`, `output_config`, ... |
| `max_tokens` | clamp the request's `max_tokens` |
| `max_tokens_field` | for `openai` only: `max_tokens` (default) or `max_completion_tokens` (OpenAI's newer models) |
| `effort_field` | for `openai` only: the request field that gets Claude Code's effort (`output_config.effort`: `low` ... `max`), for example `reasoning_effort`. Unset: the effort is dropped. Claude Code sends an effort only for models it treats as supporting it, so set `behaves_as` too. |
| `keep_server_tools` | keep Anthropic server tools (web search, ...) for a server that runs them. They are dropped by default. |

## How a model is presented to Claude Code

| Key | Effect |
|---|---|
| `id` | The model's name everywhere in Claude Code: agent files, `--model`, `/model` and every response (modelmux rewrites the model name the server reports to `id`). It can be any string, including a Claude model ID, to take that model's place. |
| `name`, `description` | The model's row in the `/model` picker (`modelPicker` in settings.json). |
| `hidden` | `true` leaves the model out of the picker. |
| `behaves_as` | A Claude model ID whose capabilities and effort defaults Claude Code applies to this model (`behavesAs`), instead of treating it as an unknown model. |
| `context` | The model's real context window. Claude Code assumes 1M tokens for unknown models; this sets the auto-compact window for the model and for agents that use it. Claude Code's minimum is 100000. For a smaller model, cap `max_tokens` and keep its tasks small. |
| `tier` | The alias that resolves to this model. See below. |

`tier` makes an alias point at this model:

| Tier | Effect |
|---|---|
| `haiku`, `sonnet`, `opus`, `fable` | Sets `ANTHROPIC_DEFAULT_<TIER>_MODEL`, with `name` and `description` as the picker label. `haiku` also covers Claude Code's background requests, such as titles and summaries. |
| `subagent` | Sets `CLAUDE_CODE_SUBAGENT_MODEL`, for subagents without a model of their own. |
| `default` | Sets `ANTHROPIC_DEFAULT_MODEL`, the model new sessions start on. |

The Agent tool's per-call `model` parameter accepts only aliases. So a tier is the way Claude itself can choose
a foreign model for a single call.

## Agents

An `[[agent]]` becomes a subagent file in `~/.claude/agents/`:

```toml
[[agent]]
name = "local-worker"
model = "qwen3.8-flash-next"             # the id of a [[model]] (or any model Claude Code knows)
description = "Fast local worker for simple, well-specified tasks."
prompt = "Do exactly the task given and report the result concisely."
disallowed_tools = ["Agent", "ListAgents", "SendMessage"]
# tools = ["Read", "Grep", "Glob"]
# extra = { color = "cyan", effort = "low" }   # any other frontmatter key
```

Claude delegates to it like any other subagent ("use local-worker to ..."), or picks it by its description.

## What the translation does

These steps apply to every configured model. They are deterministic, so the prompt prefix of a conversation
stays byte-stable and the server's prefix cache keeps hitting.

- Server tools, the entries in `tools` without an `input_schema` (such as web search), are dropped.
- `system` entries inside `messages`, where Claude Code puts reminders and its environment block, are folded
  into the preceding user turn. Most chat templates accept a system message only at the start.

For `api = "openai"`, requests are also translated:

- The system prompt becomes a system message.
- Text, images (base64 or URL) and text documents are converted.
- `tool_use` becomes `tool_calls`, and `tool_result` becomes `tool` messages placed directly after the call.
  Images from a tool result follow in a user message, and `is_error` is marked.
- `tool_choice`, `stop_sequences`, `temperature`, `top_p` and `metadata.user_id` are mapped.
- The effort (`output_config.effort`) goes to the field named by `effort_field`, when it is set.
- Thinking blocks are dropped in both directions, because they need signatures.

On the way back:

- Text and tool calls are translated, including the dialects that send fragmented, out-of-order, ID-less or
  all-index-0 tool calls.
- Finish reasons and usage, including cached tokens, are translated.
- `count_tokens` returns 404 for these models, and Claude Code falls back to its own estimate.

Known limits:

- Streamed tool arguments are passed on as they arrive, so if a model emits broken JSON, Claude Code fails to
  parse the call.
- Gemini's thought signatures on tool calls are not carried back.
- Images are passed on as they are, so a model without vision will reject them.

## Auto mode

In auto mode Claude Code asks the Anthropic API for a safety verdict on each proposed tool call: a `safeguards`
block in the request, and `safeguard_results` in the response. A foreign server returns no verdict, and Claude
Code fails closed. It falls back to its local checks, or after repeated responses without a verdict it stops
the agent with "Auto mode is unavailable". modelmux doesn't fake verdicts.

## Debugging

- `modelmux serve -dump DIR` (or `dump = "DIR"` in the config) writes each translated request body to `DIR`.
- `GET /modelmux/health` lists the configured models and whether their keys are set.
- The service logs one line per foreign request: `journalctl --user -u modelmux`.
- For a quick run without a config file: `modelmux serve -route 'PATTERN=UPSTREAM[,MODEL]'`, which routes model
  names matching PATTERN to an Anthropic-compatible UPSTREAM.

## Terms and support

modelmux is an independent project, not affiliated with or endorsed by Anthropic.

Pointing Claude Code at a gateway through `ANTHROPIC_BASE_URL` is a documented setup, but Anthropic
[doesn't support routing Claude Code to non-Claude models](https://code.claude.com/docs/en/llm-gateway), so
problems with foreign models are yours to solve.

modelmux runs the unmodified Claude Code binary, and passes your own Claude requests and credentials through
untouched without reading or storing them. Use it within Anthropic's terms
([consumer](https://www.anthropic.com/legal/consumer-terms), [commercial](https://www.anthropic.com/legal/commercial-terms),
[Claude Code credential use](https://code.claude.com/docs/en/legal-and-compliance)) and those of your other
providers:

- Run it for yourself, on localhost. Don't serve other people's requests through your subscription.
- Don't use Claude's output, including request dumps, to train or fine-tune models.

## License

[MIT](LICENSE)
