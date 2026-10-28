# ccroute

A model-name router for Claude Code, so subagents can run on a local model while the main session stays on
Anthropic.

Claude Code sends every request to one base URL (`ANTHROPIC_BASE_URL`). ccroute sits there and looks at the
`model` of each Messages API request:

- A model matching a `-route` prefix goes to that Anthropic-compatible server (quench6k, llama.cpp, vLLM, …),
  with Anthropic credentials removed.
- Everything else, including every non-Messages path, is passed through to `https://api.anthropic.com`
  unchanged: same body bytes, same headers and credentials (API key or claude.ai sign-in), no
  `X-Forwarded-*` headers.

For local routes the request is adapted:

- `model` is replaced with the served model name.
- Server tools (entries in `tools` without an `input_schema`, such as web search) are dropped.
- `system` entries inside `messages` (Claude Code puts its environment block there) are folded into the
  preceding user turn, as most local chat templates only accept a system message at the start.

The rewrite is deterministic, so a conversation's prompt prefix stays byte-stable and the local server's
prefix cache keeps hitting across turns and across subagent runs.

## Run

```
go install github.com/lkarlslund/ccroute@latest
ccroute -listen 127.0.0.1:8098 -route 'qwen=http://127.0.0.1:8003,Qwen/Qwen3.8-Flash-Next'
```

`-route prefix=upstream[,model]` is repeatable; the prefix is matched case-insensitively against the request's
model. `-dump dir` writes each routed request body to `dir` for debugging. A user unit is in
[`contrib/ccroute.service`](contrib/ccroute.service).

## Use from Claude Code

Start Claude Code with `ANTHROPIC_BASE_URL=http://127.0.0.1:8098` and give a subagent a model name that
matches a route, either in `.claude/agents/*.md` (`model: qwen3.8-flash-next`) or with `--agents`:

```
ANTHROPIC_BASE_URL=http://127.0.0.1:8098 claude --agents '{"qwen-worker": {
  "description": "Fast local worker for simple, well-specified tasks.",
  "prompt": "Do exactly the task given and report the result concisely.",
  "model": "qwen3.8-flash-next", "tools": ["Read", "Grep", "Glob"]}}'
```

Claude Code logs `unrecognized_model` for the name and otherwise uses it as given.

In auto mode Claude Code asks the Anthropic API for a safety verdict on each proposed tool call (a
`safeguards` block in the request, `safeguard_results` in the response). A local server returns none, and
Claude Code fails closed: it falls back to its local checks, or stops the agent with "Auto mode is
unavailable" after repeated responses without a verdict. Tool calls from a local subagent are not approved
by default; ones that need a verdict may be refused.
