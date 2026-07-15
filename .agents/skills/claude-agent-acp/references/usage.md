# Claude Agent ACP Usage

## Purpose

`claude-agent-acp` lets an ACP-compatible client call Claude Code through the official Claude Agent SDK.

## Run As ACP Server

Run the binary with no `--cli` flag to start the ACP server over stdin/stdout:

```bash
claude-agent-acp
```

An ACP client should spawn this command as a child process and speak ACP JSON-RPC over stdio.

Minimal client flow:

1. Send `initialize`.
2. Send `session/new` with an absolute `cwd`.
3. Optionally send `session/set_config_option` to choose `model`, `mode`, or `effort`.
4. Send `session/prompt`.
5. Consume `session/update` notifications and the final prompt response.

The `cwd` must be an absolute path to an existing directory.

## Local `.env` Configuration

Use project-local `.env` for private Claude Code, provider, and gateway settings needed by the `claude-agent-acp` process.

Important behavior:

- `claude-agent-acp` inherits environment variables from its parent process.
- The adapter passes the inherited environment to the Claude Agent SDK and Claude Code runtime.
- The adapter does not automatically read `.env` from the repository.
- The ACP client, launcher script, or shell must load `.env` before spawning `claude-agent-acp`.
- `.env` must stay ignored by git because it can contain API keys, gateway headers, tokens, and provider configuration.

Safe shell launch pattern:

```bash
set -a
. ./.env
set +a
claude-agent-acp
```

Use this pattern only from a trusted shell. Do not print `.env` contents, and do not include secrets in command examples or logs.

Common `.env` entries:

```dotenv
ANTHROPIC_MODEL=claude-sonnet-4-6
MAX_THINKING_TOKENS=0
CLAUDE_MODEL_CONFIG={"availableModels":["sonnet","opus"]}
```

Gateway-style deployments may also need provider-specific variables such as `ANTHROPIC_BASE_URL`, `ANTHROPIC_AUTH_TOKEN`, `ANTHROPIC_CUSTOM_HEADERS`, `CLAUDE_CODE_USE_BEDROCK`, AWS region variables, or Bedrock endpoint variables. Keep concrete secret values out of committed examples.

## Model Selection

Initial model priority:

1. `ANTHROPIC_MODEL` environment variable.
2. Claude settings `model`.
3. Claude Agent SDK default.

Example process-level default:

```bash
ANTHROPIC_MODEL=claude-sonnet-4-6 claude-agent-acp
```

For per-session switching, use ACP config options after creating a session:

```json
{
  "method": "session/set_config_option",
  "params": {
    "sessionId": "<session-id>",
    "configId": "model",
    "value": "claude-sonnet-4-6"
  }
}
```

The adapter also accepts model aliases when they can be resolved against the advertised model list, such as `opus` or `sonnet`.

## Model Availability And Provider Overrides

Use `CLAUDE_MODEL_CONFIG` for deployment-level model configuration when the ACP client does not provide SDK settings in session metadata.

Restrict available models:

```bash
CLAUDE_MODEL_CONFIG='{"availableModels":["opus","sonnet"]}' claude-agent-acp
```

Provider model override example:

```bash
CLAUDE_MODEL_CONFIG='{"modelOverrides":{"claude-sonnet-4-5":"provider-specific-model-id"}}' claude-agent-acp
```

For Bedrock-style deployments, the adapter understands the Claude Code Bedrock environment variables, including `CLAUDE_CODE_USE_BEDROCK` and related provider settings.

## Permission Modes

The adapter exposes ACP session modes backed by Claude Code permission modes.

Common modes:

- `default`: prompt for dangerous operations.
- `acceptEdits`: auto-accept file edits.
- `plan`: plan without executing tools.
- `dontAsk`: do not prompt; deny when not pre-approved.
- `auto`: use model-based permission classification when available for the active model.
- `bypassPermissions`: bypass checks when supported by the runtime.

`bypassPermissions` is not available when running as root unless the runtime declares sandbox support.

## Gateway Authentication

ACP clients that support gateway auth can offer Anthropic-protocol or Bedrock-protocol gateways. The adapter maps gateway metadata to environment variables for the Claude Agent SDK.

Do not expose gateway headers or bearer tokens in logs. Redact values when summarizing configuration.

## Integration Notes

- This package is not an MCP server. Do not add it to Codex MCP configuration unless a separate MCP bridge is introduced.
- This package is not a Codex skill provider. This repository skill only documents how to install and use the adapter.
- The adapter can merge ACP-provided MCP server definitions into the Claude SDK session, but that is part of the ACP session request, not Codex MCP registration.
- The adapter can surface Claude tool permission requests back to the ACP client through `requestPermission`.
- The adapter can load, resume, list, close, delete, and fork sessions when the client uses the corresponding ACP methods.

## Smoke Check

For a real end-to-end check, use an ACP client that can spawn `claude-agent-acp` and send `initialize`, `session/new`, and `session/prompt`.

If building a small custom smoke client, keep logs on stderr and reserve stdout/stdin for the ACP transport. The adapter itself redirects normal console output away from stdout so ACP framing is not corrupted.
