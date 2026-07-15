# Claude Agent ACP Client Integration

## Purpose

Use this reference when an ACP-compatible client needs to call Claude Code through `claude-agent-acp`.

## Client Workflow

1. Install or verify the adapter with `npm install -g @agentclientprotocol/claude-agent-acp`, then `command -v claude-agent-acp`.
2. Load private configuration from `.env` in the launcher process without printing values.
3. Spawn `claude-agent-acp` as a child process with stdio pipes. Do not run it as a one-shot command; default mode waits for ACP messages on stdin/stdout.
4. Use `@agentclientprotocol/sdk` exports such as `ClientSideConnection` and `ndJsonStream` to speak ACP over the child process stdio.
5. Send `initialize`, then `session/new` with an absolute `cwd`, then `session/prompt` with text content.
6. Implement `sessionUpdate` to observe assistant chunks and tool updates.
7. Implement `requestPermission` if the task may need tool execution. Select safe allow options deliberately; do not auto-approve secrets exposure.
8. Kill the child process after the prompt returns or on error.

## Installed Files To Inspect

Before writing a custom client, inspect the installed reference files instead of relying on memory:

- Adapter entrypoint: `<global-node-modules>/@agentclientprotocol/claude-agent-acp/dist/index.js`.
- Agent implementation: `<global-node-modules>/@agentclientprotocol/claude-agent-acp/dist/acp-agent.js`.
- SDK client example: `<global-node-modules>/@agentclientprotocol/claude-agent-acp/node_modules/@agentclientprotocol/sdk/dist/examples/client.js`.
- SDK connection implementation and exports: `<global-node-modules>/@agentclientprotocol/claude-agent-acp/node_modules/@agentclientprotocol/sdk/dist/acp.js`.

## Prompt Script

Use `scripts/prompt-claude-agent-acp.mjs` for ad hoc prompt execution instead of copying client code into each task.

Basic usage:

```bash
node /absolute/path/to/.agents/skills/claude-agent-acp/scripts/prompt-claude-agent-acp.mjs \
  --cwd /absolute/workspace/path \
  --prompt "Implement the requested task. Do not read or print .env."
```

Read the prompt from a file:

```bash
node /absolute/path/to/.agents/skills/claude-agent-acp/scripts/prompt-claude-agent-acp.mjs \
  --cwd /absolute/workspace/path \
  --prompt-file /absolute/path/to/prompt.txt
```

Set session config options before prompting:

```bash
node /absolute/path/to/.agents/skills/claude-agent-acp/scripts/prompt-claude-agent-acp.mjs \
  --cwd /absolute/workspace/path \
  --model claude-sonnet-4-6 \
  --mode default \
  --prompt "Summarize the repository status."
```

If the adapter cannot be found on `PATH`, pass the absolute binary path from `command -v claude-agent-acp`:

```bash
node /absolute/path/to/.agents/skills/claude-agent-acp/scripts/prompt-claude-agent-acp.mjs \
  --command /opt/nodejs/bin/claude-agent-acp \
  --cwd /absolute/workspace/path \
  --prompt "Check the build configuration."
```

The script inherits environment variables from the parent process, but does not load or print `.env`. Load private variables in the launcher shell when needed.

Permission requests are denied by default. Use `--auto-approve-first-permission` only when the requested operation is safe and the prompt explicitly avoids exposing credentials.
