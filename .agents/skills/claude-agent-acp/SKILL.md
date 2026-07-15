---
name: claude-agent-acp
description: |
  Use claude-agent-acp when installing, running, or integrating the official
  @agentclientprotocol/claude-agent-acp adapter so an ACP client can call Claude
  Code through the Claude Agent SDK.
---

# Claude Agent ACP Skill

Use this skill when the user wants to install, run, verify, or integrate the official Claude Agent ACP adapter.

`claude-agent-acp` is an ACP server backed by the Claude Agent SDK. Treat it as a stdio ACP process, not as a Codex plugin and not as an MCP server.

## Quick Task Routing

- Installation, authentication, and binary verification: read `references/install.md`.
- Startup and smoke checks: read `references/usage.md`.
- Model selection and provider/gateway configuration: read `references/usage.md`.
- ACP client integration flow: read `references/acp-client.md`.

## Core Rules

- Use the HTTPS repository URL: `https://github.com/agentclientprotocol/claude-agent-acp`.
- Do not assume the official project ships Codex skills; it ships an ACP adapter package.
- Keep credentials out of command output. Do not print API keys, auth tokens, or custom gateway headers.
- Use project-local `.env` for private Claude Code and gateway environment variables, but never commit it.
- `.env` is not loaded automatically by `claude-agent-acp`; the launcher or shell must load it before spawning the ACP server.
- After install, verify the real binary path with `command -v claude-agent-acp`. In this workspace it may resolve to `/opt/nodejs/bin/claude-agent-acp`.
- Do not guess ACP JSON-RPC message shapes. Inspect the installed package's SDK examples and generated types before building a client.
- Prefer environment variables or Claude settings for model/provider defaults, and ACP `session/set_config_option` for per-session model switches.
- If you create or modify this skill and Codex does not detect the change, tell the user to restart Codex.

## Output Checklist

- Confirm whether the task was install, runtime configuration, or ACP client integration.
- Name the official package, binary, and repository when relevant.
- Summarize verification without printing credentials or raw environment values.
- Mention that Codex may need to be restarted if it does not detect skill changes.

## Evidence Summary

- The npm package is `@agentclientprotocol/claude-agent-acp` and exposes the `claude-agent-acp` binary.
- The default binary mode runs an ACP agent over stdin/stdout.
- The `--cli` mode delegates to the Claude Code CLI and is mainly useful for login/auth flows.
- The adapter creates Claude sessions through the Claude Agent SDK and exposes ACP session config options for mode, model, and effort.
