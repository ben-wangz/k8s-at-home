---
name: claude-agent-acp
description: |
  Use claude-agent-acp when installing, running, or integrating the official
  @agentclientprotocol/claude-agent-acp adapter so an ACP client can call Claude
  Code through the Claude Agent SDK.
compatibility: opencode
metadata:
  npm-package: "@agentclientprotocol/claude-agent-acp"
  github-repo: "https://github.com/agentclientprotocol/claude-agent-acp"
---

# Claude Agent ACP Skill

Use this skill when the user wants to install, run, verify, or integrate the official Claude Agent ACP adapter.

`claude-agent-acp` is an ACP server backed by the Claude Agent SDK. Treat it as a stdio ACP process, not as an opencode skill bundle and not as an MCP server.

## Quick Task Routing

- Installation, authentication, and binary verification: read `references/install.md`.
- Startup and smoke checks: read `references/usage.md`.
- Model selection and provider/gateway configuration: read `references/usage.md`.
- ACP client integration flow: read `references/usage.md`.

## Core Rules

- Use the HTTPS repository URL: `https://github.com/agentclientprotocol/claude-agent-acp`.
- Do not assume the official project ships opencode-style skills; it ships an ACP adapter package.
- Keep credentials out of command output. Do not print API keys, auth tokens, or custom gateway headers.
- Use project-local `.env` for private Claude Code and gateway environment variables, but never commit it.
- `.env` is not loaded automatically by `claude-agent-acp`; the launcher or shell must load it before spawning the ACP server.
- After install, verify the real binary path with `command -v claude-agent-acp`. In this workspace it may resolve to `/opt/nodejs/bin/claude-agent-acp`.
- Do not guess ACP JSON-RPC message shapes. Inspect the installed package's SDK examples and generated types before building a client.
- Prefer environment variables or Claude settings for model/provider defaults, and ACP `session/set_config_option` for per-session model switches.
- If you create or modify this skill, tell the user to restart opencode before expecting the new skill content to load.

## Calling Claude Code Through ACP

Use this workflow when the user asks to use Claude Code through `claude-agent-acp` for an implementation task:

1. Install or verify the adapter: `npm install -g @agentclientprotocol/claude-agent-acp`, then `command -v claude-agent-acp`.
2. Load private configuration from `.env` in the launcher process, without printing values.
3. Spawn `claude-agent-acp` as a child process with stdio pipes. Do not run it as a one-shot CLI command; the default mode waits for ACP messages on stdin/stdout.
4. Use `@agentclientprotocol/sdk`'s `ClientSideConnection` and `ndJsonStream` to speak ACP over the child process stdio.
5. Send `initialize`, then `session/new` with an absolute `cwd`, then `session/prompt` with text content.
6. Implement `sessionUpdate` in the client to observe assistant chunks and tool updates.
7. Implement `requestPermission` if the task may need tool execution. Select safe allow options deliberately; do not auto-approve secrets exposure.
8. Kill the child process after the prompt returns or on error.

Before writing a custom client, inspect the installed reference files instead of relying on memory:

- Adapter entrypoint: `<global-node-modules>/@agentclientprotocol/claude-agent-acp/dist/index.js`.
- Agent implementation: `<global-node-modules>/@agentclientprotocol/claude-agent-acp/dist/acp-agent.js`.
- SDK client example: `<global-node-modules>/@agentclientprotocol/claude-agent-acp/node_modules/@agentclientprotocol/sdk/dist/examples/client.js`.
- SDK connection implementation and exports: `<global-node-modules>/@agentclientprotocol/claude-agent-acp/node_modules/@agentclientprotocol/sdk/dist/acp.js`.

Minimal Node client shape:

```js
import { spawn } from "node:child_process";
import { Readable, Writable } from "node:stream";
import * as acp from "<sdk-dist>/acp.js";

const child = spawn("claude-agent-acp", [], {
  cwd: "/absolute/workspace/path",
  stdio: ["pipe", "pipe", "inherit"],
  env: process.env,
});

class Client {
  async sessionUpdate(params) {
    // Read params.update for agent_message_chunk, tool_call, tool_call_update, etc.
  }

  async requestPermission(params) {
    // Choose an allowed option intentionally, or deny when unsafe.
    return { outcome: { outcome: "selected", optionId: params.options[0].optionId } };
  }
}

const stream = acp.ndJsonStream(Writable.toWeb(child.stdin), Readable.toWeb(child.stdout));
const connection = new acp.ClientSideConnection(() => new Client(), stream);

await connection.initialize({
  protocolVersion: acp.PROTOCOL_VERSION,
  clientCapabilities: {
    fs: { readTextFile: true, writeTextFile: true },
    auth: { terminal: true },
    _meta: { "terminal-auth": true },
  },
});

const session = await connection.newSession({
  cwd: "/absolute/workspace/path",
  mcpServers: [],
});

await connection.prompt({
  sessionId: session.sessionId,
  prompt: [{ type: "text", text: "Implement the requested task. Do not read or print .env." }],
});

child.kill();
```

If the adapter cannot be found after installation, use the absolute path from `command -v claude-agent-acp` as the child process command.

## Evidence Summary

- The npm package is `@agentclientprotocol/claude-agent-acp` and exposes the `claude-agent-acp` binary.
- The default binary mode runs an ACP agent over stdin/stdout.
- The `--cli` mode delegates to the Claude Code CLI and is mainly useful for login/auth flows.
- The adapter creates Claude sessions through the Claude Agent SDK and exposes ACP session config options for mode, model, and effort.
